// SPDX-License-Identifier: Apache-2.0

package webui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/multigres/testkit/assert"
)

// fakeLoginBackend records the Begin and Complete calls it receives and answers
// each from a canned result.
type fakeLoginBackend struct {
	mu sync.Mutex

	beginPorts []uint32
	begin      beginResult

	completes   []completeCall
	completeUp  Upstream
	completeErr error
}

type beginResult struct {
	loginID      string
	authorizeURL string
	pinnedPort   uint32
	err          error
}

type completeCall struct {
	LoginID string
	Query   string
}

func (f *fakeLoginBackend) Begin(_ context.Context, redirectPort uint32) (string, string, uint32, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.beginPorts = append(f.beginPorts, redirectPort)
	return f.begin.loginID, f.begin.authorizeURL, f.begin.pinnedPort, f.begin.err
}

func (f *fakeLoginBackend) Complete(_ context.Context, loginID, query string) (Upstream, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.completes = append(f.completes, completeCall{LoginID: loginID, Query: query})
	if f.completeErr != nil {
		return Upstream{}, f.completeErr
	}
	return f.completeUp, nil
}

func (f *fakeLoginBackend) seenBeginPorts() []uint32 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]uint32(nil), f.beginPorts...)
}

func (f *fakeLoginBackend) seenCompletes() []completeCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]completeCall(nil), f.completes...)
}

// fakeLoginBackend satisfies LoginBackend.
var _ LoginBackend = (*fakeLoginBackend)(nil)

func loginBeginReq(secret string) *http.Request {
	return controlReq(http.MethodPost, "/_rafiki/login/begin", secret, loopbackOrigin(testPort))
}

func loginCallbackReq(target string) *http.Request {
	return newReq(http.MethodGet, target, loopbackHost(testPort), "")
}

func TestWebuiLoginBeginPassesOwnPort(t *testing.T) {
	ck := assert.NewCollecting(t)
	fb := &fakeLoginBackend{begin: beginResult{
		loginID:      "L1",
		authorizeURL: "https://idp.example/auth?x=1",
	}}
	s := New(Config{Port: testPort, Assets: testAssets()})
	s.EnableLogin(fb)
	secret := addSecret(s, "begin-secret")

	w := serve(s, loginBeginReq(secret))
	ck.Eq(http.StatusOK, w.Code, "status")

	var out struct {
		AuthorizeURL string `json:"authorize_url"`
	}
	ck.NoError(json.Unmarshal(w.Body.Bytes(), &out), "decode body")
	ck.Eq("https://idp.example/auth?x=1", out.AuthorizeURL, "authorize url")
	ck.EqDiff([]uint32{testPort}, fb.seenBeginPorts(), "Begin redirect port")
}

func TestWebuiLoginBeginRequiresSecret(t *testing.T) {
	ck := assert.NewCollecting(t)
	fb := &fakeLoginBackend{begin: beginResult{loginID: "L1"}}
	s := New(Config{Port: testPort, Assets: testAssets()})
	s.EnableLogin(fb)

	w := serve(s, loginBeginReq(""))
	ck.Eq(http.StatusUnauthorized, w.Code, "status")
	ck.Eq("refused", w.Header().Get(gatedHeader), "gate header")
	ck.Empty(fb.seenBeginPorts(), "Begin calls")
}

func TestWebuiLoginCallbackWithoutPendingIs404(t *testing.T) {
	ck := assert.NewCollecting(t)
	fb := &fakeLoginBackend{}
	s := New(Config{Port: testPort, Assets: testAssets()})
	s.EnableLogin(fb)

	w := serve(s, loginCallbackReq("/oidc/callback?code=x"))
	ck.Eq(http.StatusNotFound, w.Code, "status")
	ck.Empty(fb.seenCompletes(), "Complete calls")
}

func TestWebuiLoginCallbackCompletesAndSwapsUpstream(t *testing.T) {
	ck := assert.NewCollecting(t)
	var newHits int32
	newUp := fakeUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&newHits, 1)
		_, _ = io.WriteString(w, "new")
	})

	s := New(Config{Port: testPort, Assets: testAssets()})
	fb := &fakeLoginBackend{
		begin:      beginResult{loginID: "L1", authorizeURL: "https://idp.example/a"},
		completeUp: Upstream{Client: &http.Client{}, BaseURL: newUp.URL},
	}
	s.EnableLogin(fb)
	secret := addSecret(s, "swap-secret")

	ck.Eq(http.StatusOK, serve(s, loginBeginReq(secret)).Code, "begin status")

	wc := serve(s, loginCallbackReq("/oidc/callback?code=abc&state=xyz"))
	ck.Eq(http.StatusFound, wc.Code, "callback status")
	ck.Eq(fmt.Sprintf("http://127.0.0.1:%d/#/login-done", testPort), wc.Header().Get("Location"), "redirect target")

	completes := fb.seenCompletes()
	ck.EqDiff([]completeCall{{LoginID: "L1", Query: "code=abc&state=xyz"}}, completes, "Complete calls")

	wp := serve(s, controlReq(http.MethodPost, "/rafiki.v1.Control/X", secret, loopbackOrigin(testPort)))
	ck.Eq(http.StatusOK, wp.Code, "proxied status")
	ck.Eq("new", wp.Body.String(), "proxied body")
	ck.Eq(int32(1), atomic.LoadInt32(&newHits), "new upstream requests")
}

func TestWebuiLoginCallbackFirstQueryWins(t *testing.T) {
	ck := assert.NewCollecting(t)
	s := New(Config{Port: testPort, Assets: testAssets()})
	fb := &fakeLoginBackend{
		begin:      beginResult{loginID: "L1", authorizeURL: "u"},
		completeUp: Upstream{Client: &http.Client{}, BaseURL: "http://up.invalid"},
	}
	s.EnableLogin(fb)
	secret := addSecret(s, "first-wins-secret")

	ck.Eq(http.StatusOK, serve(s, loginBeginReq(secret)).Code, "begin status")

	w1 := serve(s, loginCallbackReq("/oidc/callback?code=first"))
	ck.Eq(http.StatusFound, w1.Code, "first callback status")

	w2 := serve(s, loginCallbackReq("/oidc/callback?code=second"))
	ck.Eq(http.StatusNotFound, w2.Code, "second callback status")

	ck.Eq(1, len(fb.seenCompletes()), "Complete called once")
}

func TestWebuiLoginBeginReplacesPending(t *testing.T) {
	ck := assert.NewCollecting(t)
	s := New(Config{Port: testPort, Assets: testAssets()})
	fb := &fakeLoginBackend{completeUp: Upstream{Client: &http.Client{}, BaseURL: "http://up.invalid"}}
	s.EnableLogin(fb)
	secret := addSecret(s, "replace-secret")

	fb.begin = beginResult{loginID: "A", authorizeURL: "ua"}
	ck.Eq(http.StatusOK, serve(s, loginBeginReq(secret)).Code, "begin A status")
	fb.begin = beginResult{loginID: "B", authorizeURL: "ub"}
	ck.Eq(http.StatusOK, serve(s, loginBeginReq(secret)).Code, "begin B status")

	ck.Eq(http.StatusFound, serve(s, loginCallbackReq("/oidc/callback?code=x")).Code, "callback status")

	completes := fb.seenCompletes()
	ck.Eq(1, len(completes), "Complete calls")
	ck.Eq("B", completes[0].LoginID, "completed login id")
}

func TestWebuiLoginPinnedPortListens(t *testing.T) {
	ck := assert.NewCollecting(t)

	probe, err := net.Listen("tcp", "127.0.0.1:0")
	ck.NoError(err, "probe listen")
	pinned := probe.Addr().(*net.TCPAddr).Port
	ck.NoError(probe.Close(), "probe close")

	s := New(Config{Port: testPort, Assets: testAssets()})
	fb := &fakeLoginBackend{
		begin:      beginResult{loginID: "L1", authorizeURL: "u", pinnedPort: uint32(pinned)},
		completeUp: Upstream{Client: &http.Client{}, BaseURL: "http://up.invalid"},
	}
	s.EnableLogin(fb)
	secret := addSecret(s, "pinned-secret")

	ck.Eq(http.StatusOK, serve(s, loginBeginReq(secret)).Code, "begin status")

	client := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/oidc/callback?code=x", pinned))
	ck.NoError(err, "GET the pinned callback")
	if resp != nil {
		defer resp.Body.Close()
		ck.Eq(http.StatusFound, resp.StatusCode, "callback status")
		ck.Eq(fmt.Sprintf("http://127.0.0.1:%d/#/login-done", testPort), resp.Header.Get("Location"), "redirect target")
	}

	waitForDialRefused(t, ck, pinned)
}

// waitForDialRefused blocks until a TCP dial to 127.0.0.1:port fails, within a
// 5s deadline and on a 10ms ticker. A socket close carries no notification.
func waitForDialRefused(t *testing.T, ck *assert.C, port int) {
	t.Helper()
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	deadline := time.After(5 * time.Second)
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-deadline:
			ck.Fail("the pinned listener never closed")
			return
		case <-tick.C:
			conn, err := net.Dial("tcp", addr)
			if err != nil {
				return
			}
			_ = conn.Close()
		}
	}
}

func TestWebuiLoginCompleteErrorShowsPage(t *testing.T) {
	ck := assert.NewCollecting(t)
	var hits int32
	up := fakeUpstream(t, func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt32(&hits, 1)
		_, _ = io.WriteString(w, "old")
	})
	s := New(Config{
		Port:    testPort,
		Assets:  testAssets(),
		Resolve: staticResolver(Upstream{Client: &http.Client{}, BaseURL: up.URL}),
	})
	fb := &fakeLoginBackend{
		begin:       beginResult{loginID: "L1", authorizeURL: "u"},
		completeErr: errors.New("bad <code> & stuff"),
	}
	s.EnableLogin(fb)
	secret := addSecret(s, "error-page-secret")

	ck.Eq(http.StatusOK, serve(s, loginBeginReq(secret)).Code, "begin status")

	w := serve(s, loginCallbackReq("/oidc/callback?code=x"))
	ck.Eq(http.StatusOK, w.Code, "callback status")
	ck.StrContains(w.Header().Get("Content-Type"), "text/html", "content type")
	ck.StrContains(w.Body.String(), "login failed: bad &lt;code&gt; &amp; stuff", "escaped message")
	ck.NotStrContains(w.Body.String(), "bad <code> & stuff", "raw message is not emitted")

	// The upstream is unchanged: the proxy still resolves the original daemon.
	wp := serve(s, controlReq(http.MethodPost, "/rafiki.v1.Control/X", secret, loopbackOrigin(testPort)))
	ck.Eq("old", wp.Body.String(), "proxied body")
	ck.Eq(int32(1), atomic.LoadInt32(&hits), "original upstream requests")
}
