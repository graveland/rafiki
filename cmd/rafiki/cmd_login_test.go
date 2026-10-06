// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"
	"google.golang.org/protobuf/types/known/timestamppb"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/gen/rafiki/v1/rafikiv1connect"
	"go.graveland.dev/rafiki/pkg/profile"

	"github.com/multigres/testkit/assert"
)

// fakeLogin is a rafikiv1connect.LoginHandler double: it records what the
// client sent — including whether an Authorization header rode along — and
// answers with canned responses.
type fakeLogin struct {
	mu sync.Mutex

	beginCalls    []*rafikiv1.BeginLoginRequest
	completeCalls []*rafikiv1.CompleteLoginRequest
	authHeaders   []string // one per call, "" = no header sent

	beginResp    *rafikiv1.BeginLoginResponse
	beginErr     error
	completeResp *rafikiv1.CompleteLoginResponse
	completeErr  error

	began     chan struct{} // signaled (non-blocking) on each BeginLogin
	completed chan struct{} // signaled (non-blocking) on each CompleteLogin
}

func newFakeLogin(begin *rafikiv1.BeginLoginResponse, complete *rafikiv1.CompleteLoginResponse) *fakeLogin {
	return &fakeLogin{
		beginResp:    begin,
		completeResp: complete,
		began:        make(chan struct{}, 1),
		completed:    make(chan struct{}, 1),
	}
}

func (f *fakeLogin) record(header string, done chan struct{}) {
	f.mu.Lock()
	f.authHeaders = append(f.authHeaders, header)
	f.mu.Unlock()
	select {
	case done <- struct{}{}:
	default:
	}
}

func (f *fakeLogin) BeginLogin(_ context.Context, req *connect.Request[rafikiv1.BeginLoginRequest]) (*connect.Response[rafikiv1.BeginLoginResponse], error) {
	f.mu.Lock()
	f.beginCalls = append(f.beginCalls, req.Msg)
	resp, err := f.beginResp, f.beginErr
	f.mu.Unlock()
	f.record(req.Header().Get("Authorization"), f.began)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(resp), nil
}

func (f *fakeLogin) CompleteLogin(_ context.Context, req *connect.Request[rafikiv1.CompleteLoginRequest]) (*connect.Response[rafikiv1.CompleteLoginResponse], error) {
	f.mu.Lock()
	f.completeCalls = append(f.completeCalls, req.Msg)
	resp, err := f.completeResp, f.completeErr
	f.mu.Unlock()
	f.record(req.Header().Get("Authorization"), f.completed)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(resp), nil
}

func (f *fakeLogin) lastBegin(t *testing.T) *rafikiv1.BeginLoginRequest {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.beginCalls) == 0 {
		t.Fatal("no BeginLogin call recorded")
	}
	return f.beginCalls[len(f.beginCalls)-1]
}

func (f *fakeLogin) completeCallsSnapshot(t *testing.T) []*rafikiv1.CompleteLoginRequest {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]*rafikiv1.CompleteLoginRequest(nil), f.completeCalls...)
}

func (f *fakeLogin) authHeadersSnapshot(t *testing.T) []string {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.authHeaders...)
}

// loginEndpointFor mounts fake on an httptest server and returns an endpoint
// aimed at it, the way a resolved remote profile would (minus TLS).
func loginEndpointFor(t *testing.T, fake *fakeLogin) connectEndpoint {
	t.Helper()
	mux := http.NewServeMux()
	path, handler := rafikiv1connect.NewLoginHandler(fake)
	// Answer with the current epoch, as the daemon's Login mount does, so the
	// CLI's login transport (which verifies the response header) accepts it.
	mux.Handle(path, epochResponseHeader(handler))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return connectEndpoint{httpClient: srv.Client(), baseURL: srv.URL, describe: srv.URL, identity: srv.URL}
}

// serveLoginOnSocket mounts fake on a unix socket the way the daemon's control
// plane serves it — h2c and HTTP/1.1 on one listener (cmd/rafikid's rule) — and
// returns the socket path for a profile's `socket` field.
func serveLoginOnSocket(t *testing.T, fake *fakeLogin) string {
	t.Helper()
	mux := http.NewServeMux()
	path, handler := rafikiv1connect.NewLoginHandler(fake)
	// Answer with the current epoch, as the daemon's Login mount does, so the
	// CLI's login transport (which verifies the response header) accepts it.
	mux.Handle(path, epochResponseHeader(handler))
	proto := &http.Protocols{}
	proto.SetUnencryptedHTTP2(true)
	proto.SetHTTP1(true)
	srv := &http.Server{Handler: mux, Protocols: proto}
	// A short temp dir, not t.TempDir(): the test-name prefix pushes the socket
	// path past macOS's 104-byte unix-socket limit and the bind fails with
	// "invalid argument".
	dir, err := os.MkdirTemp("", "rkl")
	if err != nil {
		t.Fatalf("tempdir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "sso.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen unix: %v", err)
	}
	t.Cleanup(func() {
		_ = srv.Close()
		_ = ln.Close()
	})
	go func() { _ = srv.Serve(ln) }()
	return sock
}

// unassignedPort returns a free TCP port OUTSIDE the ephemeral range, so the
// flow's own 127.0.0.1:0 bind can never land on it and turn the pinned-port
// rebind test into a coincidence.
func unassignedPort(t *testing.T) int {
	t.Helper()
	for range 50 {
		port := 20000 + rand.IntN(10000)
		ln, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(port))
		if err == nil {
			_ = ln.Close()
			return port
		}
	}
	t.Fatal("no free port found in the test range")
	return 0
}

// saveProfile writes a one-profile manifest and selects it.
func saveProfile(t *testing.T, p profile.Profile) {
	t.Helper()
	if err := profile.Save(profile.Set{Profiles: map[string]profile.Profile{p.Name: p}}); err != nil {
		t.Fatalf("profile.Save: %v", err)
	}
	if err := profile.SavePointer(p.Name); err != nil {
		t.Fatalf("profile.SavePointer: %v", err)
	}
}

// runLoginAsync starts the flow the way a browser login runs: stdin at EOF (no
// paste will come) so only the listener can answer. noBrowser is ALWAYS true:
// these tests drive the listener path themselves and must never exec the
// platform opener — a real browser opening per `go test` on a dev machine is
// exactly the failure the shadowed-opener probe caught once.
func runLoginAsync(t *testing.T, ep connectEndpoint, stderr *bytes.Buffer) chan error {
	t.Helper()
	done := make(chan error, 1)
	go func() {
		done <- loginFlow(context.Background(), ep, profile.Resolved{Profile: profile.Profile{Name: "test"}}, loginIO{
			noBrowser: true,
			stdin:     strings.NewReader(""),
			stderr:    stderr,
		})
	}()
	return done
}

// waitBegan waits for BeginLogin to reach the fake.
func waitBegan(t *testing.T, fake *fakeLogin) {
	t.Helper()
	select {
	case <-fake.began:
	case <-time.After(5 * time.Second):
		t.Fatal("BeginLogin never reached the fake server")
	}
}

// waitFlow waits for the flow goroutine to finish.
func waitFlow(t *testing.T, done chan error) {
	t.Helper()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("loginFlow: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("loginFlow did not finish")
	}
}

// The listener path end to end: the flow binds a port, BeginLogin announces it,
// the (test-issued) browser GET delivers the query, CompleteLogin consumes it,
// and the token lands in the isolated profile. No browser is ever opened.
func TestLoginListenerCallbackPathEndToEnd(t *testing.T) {
	c := assert.NewAborting(t)
	isolateProfiles(t)
	expires := time.Now().Add(24 * time.Hour)
	fake := newFakeLogin(
		&rafikiv1.BeginLoginResponse{LoginId: "l-1", AuthorizeUrl: "https://idp.example.net/authorize?client_id=rafiki"},
		&rafikiv1.CompleteLoginResponse{Token: "sk-oidc-listener", Username: "ada", ExpiresAt: timestamppb.New(expires)},
	)
	ep := loginEndpointFor(t, fake)

	var stderr bytes.Buffer
	done := runLoginAsync(t, ep, &stderr)
	waitBegan(t, fake)
	begin := fake.lastBegin(t)
	port := int(begin.GetRedirectPort())
	c.True(port != 0, "BeginLoginRequest.RedirectPort must carry the client's bound port")

	client := &http.Client{Timeout: 5 * time.Second}
	// A stray path 404s — asked first so it cannot race the flow's shutdown.
	resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/other", port))
	c.Require().NoError(err, "GET /other")
	resp.Body.Close()
	c.Eq(http.StatusNotFound, resp.StatusCode, "other paths 404")

	resp, err = client.Get(fmt.Sprintf("http://127.0.0.1:%d/oidc/callback?code=abc&state=def", port))
	c.Require().NoError(err, "GET /oidc/callback")
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	c.Eq(http.StatusOK, resp.StatusCode, "callback status")
	c.True(strings.Contains(string(body), "close this tab"), "callback body = %s", body)

	waitFlow(t, done)

	calls := fake.completeCallsSnapshot(t)
	c.Len(calls, 1, "CompleteLogin calls")
	c.Eq("l-1", calls[0].GetLoginId(), "login id")
	c.Eq("code=abc&state=def", calls[0].GetCallbackQuery(), "callback query")
	c.Eq("sk-oidc-listener", profile.ReadToken("test"), "token written to the isolated profile")
	out := stderr.String()
	c.StrContains(out, "https://idp.example.net/authorize?client_id=rafiki", "authorize URL on stderr")
	c.StrContains(out, "logged in as ada (profile test)", "success line")
	c.StrContains(out, expires.Local().Format("2006-01-02 15:04"), "local expiry")
}

// The daemon pins a callback port (oidc.toml) different from the one the
// client bound: the flow must close its first listener, rebind to the pinned
// port, and receive the redirect there.
func TestLoginRebindsToTheDaemonPinnedPort(t *testing.T) {
	c := assert.NewAborting(t)
	isolateProfiles(t)
	pinned := unassignedPort(t)
	fake := newFakeLogin(
		&rafikiv1.BeginLoginResponse{LoginId: "l-2", AuthorizeUrl: "https://idp.example.net/authorize", RedirectPort: uint32(pinned)},
		&rafikiv1.CompleteLoginResponse{Token: "sk-oidc-pin", Username: "grace"},
	)
	ep := loginEndpointFor(t, fake)

	var stderr bytes.Buffer
	done := runLoginAsync(t, ep, &stderr)
	waitBegan(t, fake)
	begin := fake.lastBegin(t)
	bound := int(begin.GetRedirectPort())
	c.True(bound != pinned, "the flow's first bind must differ from the pinned port for the rebind to be exercised")

	// Poll until the rebind is up: it happens after BeginLogin returns.
	client := &http.Client{Timeout: 2 * time.Second}
	deadline := time.Now().Add(10 * time.Second)
	for {
		resp, err := client.Get(fmt.Sprintf("http://127.0.0.1:%d/oidc/callback?code=pin1&state=s", pinned))
		if err == nil {
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			c.Eq(http.StatusOK, resp.StatusCode, "pinned-port status")
			c.True(strings.Contains(string(body), "close this tab"), "pinned-port body = %s", body)
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("callback on pinned port %d never answered: %v", pinned, err)
		}
		time.Sleep(20 * time.Millisecond)
	}

	waitFlow(t, done)

	calls := fake.completeCallsSnapshot(t)
	c.Len(calls, 1, "CompleteLogin calls")
	c.Eq("code=pin1&state=s", calls[0].GetCallbackQuery(), "query delivered via the pinned port")
	c.Eq("sk-oidc-pin", profile.ReadToken("test"), "token")
	c.StrContains(stderr.String(), "token expires never", "zero expiry renders as never")

	// The FIRST listener must be gone: an implementation that skipped ln.Close()
	// and served only the pinned listener passes everything above identically.
	// A leaked (bound but never served) listener would connect into the kernel
	// backlog and hang, so a plain timeout is not enough — the pin is
	// specifically connection refused.
	stale := &http.Client{Timeout: time.Second}
	_, err := stale.Get(fmt.Sprintf("http://127.0.0.1:%d/oidc/callback?code=stale", bound))
	c.Require().Error(err, "the flow's first listener must not answer after the rebind")
	c.True(errors.Is(err, syscall.ECONNREFUSED),
		"want connection refused on the original port %d (err = %v) — the flow must have closed its first listener", bound, err)
}

// When the pinned port is taken the flow must fail naming the port, why it is
// pinned, and the --no-browser escape.
func TestLoginPinnedPortInUseNamesThePort(t *testing.T) {
	c := assert.NewAborting(t)
	isolateProfiles(t)
	// Hold the port open so the rebind cannot take it.
	held, err := net.Listen("tcp", "127.0.0.1:0")
	c.Require().NoError(err, "listen")
	t.Cleanup(func() { _ = held.Close() })
	port := held.Addr().(*net.TCPAddr).Port

	fake := newFakeLogin(
		&rafikiv1.BeginLoginResponse{LoginId: "l-3", AuthorizeUrl: "https://idp.example.net/authorize", RedirectPort: uint32(port)},
		nil,
	)
	ep := loginEndpointFor(t, fake)

	var stderr bytes.Buffer
	err = loginFlow(context.Background(), ep, profile.Resolved{Profile: profile.Profile{Name: "test"}}, loginIO{
		noBrowser: true, // the rebind fails before any open; never rely on that
		stdin:     strings.NewReader(""),
		stderr:    &stderr,
	})
	c.Require().Error(err, "rebinding to an occupied port must fail")
	msg := err.Error()
	c.StrContains(msg, strconv.Itoa(port), "the port must be named")
	c.StrContains(msg, "pinned by the daemon's oidc.toml", "the reason must be named")
	c.StrContains(msg, "--no-browser", "the escape must be suggested")
	// The failed BeginLogin reached the fake, but no CompleteLogin ever can.
	waitBegan(t, fake)
	c.Len(fake.completeCallsSnapshot(t), 0, "no CompleteLogin on a failed rebind")
}

// The paste path end to end: a URL pasted on stdin supplies the query, parsed
// with url.Parse down to its RawQuery.
func TestLoginPastePathEndToEnd(t *testing.T) {
	c := assert.NewAborting(t)
	isolateProfiles(t)
	fake := newFakeLogin(
		&rafikiv1.BeginLoginResponse{LoginId: "l-4", AuthorizeUrl: "https://idp.example.net/authorize?client_id=rafiki"},
		&rafikiv1.CompleteLoginResponse{Token: "sk-oidc-paste", Username: "linus", ExpiresAt: timestamppb.New(time.Unix(1735689600, 0))},
	)
	ep := loginEndpointFor(t, fake)

	var stderr bytes.Buffer
	err := loginFlow(context.Background(), ep, profile.Resolved{Profile: profile.Profile{Name: "test"}}, loginIO{
		noBrowser: true, // paste-driven; the platform opener must not run
		stdin:     strings.NewReader("http://localhost:9876/oidc/callback?code=pasted&state=xyz\n"),
		stderr:    &stderr,
	})
	c.Require().NoError(err, "loginFlow")

	calls := fake.completeCallsSnapshot(t)
	c.Len(calls, 1, "CompleteLogin calls")
	c.Eq("l-4", calls[0].GetLoginId(), "login id")
	c.Eq("code=pasted&state=xyz", calls[0].GetCallbackQuery(), "raw query from the pasted URL")
	c.Eq("sk-oidc-paste", profile.ReadToken("test"), "token written to the isolated profile")
	out := stderr.String()
	c.StrContains(out, loginPastePrompt, "paste prompt on stderr")
	c.StrContains(out, "logged in as linus (profile test)", "success line")
}

// A paste that parses to nothing is warned about and the wait continues — the
// next line (or the listener) can still answer.
func TestLoginIgnoresAMalformedPaste(t *testing.T) {
	c := assert.NewAborting(t)
	isolateProfiles(t)
	fake := newFakeLogin(
		&rafikiv1.BeginLoginResponse{LoginId: "l-5", AuthorizeUrl: "https://idp.example.net/authorize"},
		&rafikiv1.CompleteLoginResponse{Token: "sk-oidc-ok", Username: "ada"},
	)
	ep := loginEndpointFor(t, fake)

	var stderr bytes.Buffer
	err := loginFlow(context.Background(), ep, profile.Resolved{Profile: profile.Profile{Name: "test"}}, loginIO{
		noBrowser: true, // paste-driven; the platform opener must not run
		stdin:     strings.NewReader("not a url\nhttp://x/oidc/callback?code=ok\n"),
		stderr:    &stderr,
	})
	c.Require().NoError(err, "loginFlow")

	calls := fake.completeCallsSnapshot(t)
	c.Len(calls, 1, "CompleteLogin calls")
	c.Eq("code=ok", calls[0].GetCallbackQuery(), "the good paste supplied the query")
	c.StrContains(stderr.String(), "not a login redirect URL", "the bad paste was warned about")
}

// The browser branch is pinned in both directions WITHOUT ever opening a real
// browser: a logging stub shadows `open`/`xdg-open` at the front of PATH, so
// without --no-browser the opener really runs (and its argv is the authorize
// URL), and with --no-browser nothing is exec'd. This is what makes the
// noBrowser: true in every other flow test meaningful rather than decorative.
func TestLoginBrowserOpeningHonorsNoBrowser(t *testing.T) {
	c := assert.NewAborting(t)
	isolateProfiles(t)

	binDir := t.TempDir()
	logPath := filepath.Join(binDir, "open.log")
	script := fmt.Sprintf("#!/bin/sh\necho \"OPEN-CALLED: $*\" >> %q\n", logPath)
	for _, name := range []string{"open", "xdg-open"} {
		if err := os.WriteFile(filepath.Join(binDir, name), []byte(script), 0o755); err != nil {
			t.Fatalf("write stub %s: %v", name, err)
		}
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	fake := newFakeLogin(
		&rafikiv1.BeginLoginResponse{LoginId: "l-8", AuthorizeUrl: "https://idp.example.net/authorize?client_id=rafiki"},
		&rafikiv1.CompleteLoginResponse{Token: "sk-oidc-browser", Username: "ada"},
	)
	ep := loginEndpointFor(t, fake)
	paste := "http://localhost:1/oidc/callback?code=b&state=s\n"

	// Without --no-browser the opener runs, with the authorize URL as argv.
	var stderr bytes.Buffer
	err := loginFlow(context.Background(), ep, profile.Resolved{Profile: profile.Profile{Name: "test"}}, loginIO{
		stdin:  strings.NewReader(paste),
		stderr: &stderr,
	})
	c.Require().NoError(err, "loginFlow without --no-browser")
	c.True(waitForLog(t, logPath, "OPEN-CALLED: https://idp.example.net/authorize?client_id=rafiki"),
		"the opener must run without --no-browser (log = %q)", readStubLog(t, logPath))

	// With --no-browser nothing is exec'd.
	_ = os.Remove(logPath)
	err = loginFlow(context.Background(), ep, profile.Resolved{Profile: profile.Profile{Name: "test"}}, loginIO{
		noBrowser: true,
		stdin:     strings.NewReader(paste),
		stderr:    &stderr,
	})
	c.Require().NoError(err, "loginFlow with --no-browser")
	// A stray exec would lag the flow's return (Start() does not wait); give it
	// a settle window before declaring absence.
	time.Sleep(time.Second)
	c.False(strings.Contains(readStubLog(t, logPath), "OPEN-CALLED"),
		"the opener must NOT run with --no-browser (log = %q)", readStubLog(t, logPath))
}

// waitForLog polls until the stub log contains sub or the deadline passes.
func waitForLog(t *testing.T, path, sub string) bool {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if strings.Contains(readStubLog(t, path), sub) {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// readStubLog reads the stub's log; a missing file is an empty log.
func readStubLog(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(b)
}

// First request only: a refresh answers 200 but must not overwrite the query
// the flow will consume; other paths 404.
func TestLoginCallbackHandlerFirstQueryOnly(t *testing.T) {
	c := assert.NewAborting(t)
	queries := make(chan string, 1)
	h := loginCallbackHandler(queries)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/oidc/callback?code=one&state=s", nil))
	c.Eq(http.StatusOK, rec.Code, "callback status")
	c.True(strings.Contains(rec.Body.String(), "close this tab"), "body = %s", rec.Body.String())

	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, httptest.NewRequest(http.MethodGet, "/nope", nil))
	c.Eq(http.StatusNotFound, rec2.Code, "other paths 404")

	rec3 := httptest.NewRecorder()
	h.ServeHTTP(rec3, httptest.NewRequest(http.MethodGet, "/oidc/callback?code=two&state=s", nil))
	c.Eq(http.StatusOK, rec3.Code, "a refresh is still answered politely")

	c.Eq("code=one&state=s", <-queries, "first query wins")
	c.Zero(len(queries), "the second query must be ignored")
}

// A remote profile with no token is exactly the caller Login exists for: the
// login endpoint accepts it where the control-plane endpoint still refuses it.
func TestNewLoginEndpointAcceptsTokenlessRemoteProfile(t *testing.T) {
	c := assert.NewAborting(t)
	isolateProfiles(t)
	resetProfileCache()
	saveProfile(t, profile.Profile{Name: "remote", URL: "https://rafiki.example.net"})

	ep, err := newLoginEndpoint(&cobra.Command{})
	c.Require().NoError(err, "newLoginEndpoint")
	c.Eq("https://rafiki.example.net", ep.baseURL, "base URL")
	c.True(ep.httpClient != nil, "httpClient")
	c.NotEq("", ep.describe, "describe")

	// The same profile is still refused for every authenticated verb: only
	// Login is served outside authentication.
	_, err = newConnectEndpoint(&cobra.Command{})
	c.Require().Error(err, "newConnectEndpoint must still refuse a tokenless remote profile")
	c.StrContains(err.Error(), "has no token", "refusal message")
}

// The login client never attaches a bearer — not even when the profile HAS one
// — verified over the real local transport: h2c on a unix socket, the way the
// daemon serves it.
func TestLoginEndpointSendsNoBearerHeader(t *testing.T) {
	c := assert.NewAborting(t)
	isolateProfiles(t)
	fake := newFakeLogin(&rafikiv1.BeginLoginResponse{LoginId: "l-6", AuthorizeUrl: "https://idp.example.net/authorize"}, nil)
	sock := serveLoginOnSocket(t, fake)

	saveProfile(t, profile.Profile{Name: "sso", Socket: sock})
	// A token is present; login must still not attach it.
	if err := profile.WriteToken("sso", "sk-stale"); err != nil {
		t.Fatalf("WriteToken: %v", err)
	}

	resetProfileCache()
	ep, err := newLoginEndpoint(&cobra.Command{})
	c.Require().NoError(err, "newLoginEndpoint")

	resp, err := ep.login().BeginLogin(context.Background(), connect.NewRequest(&rafikiv1.BeginLoginRequest{
		RedirectPort: 45000,
		ClientHost:   "h",
	}))
	c.Require().NoError(err, "BeginLogin")
	c.Eq("l-6", resp.Msg.GetLoginId(), "login id")

	hdrs := fake.authHeadersSnapshot(t)
	c.Len(hdrs, 1, "recorded calls")
	c.Eq("", hdrs[0], "no Authorization header on the login call")
}

// The whole verb through the root command: registration, flag parsing, profile
// resolution and the paste path, against a fake Login service on a unix socket.
func TestLoginCommandEndToEndThroughRootCmd(t *testing.T) {
	c := assert.NewAborting(t)
	isolateProfiles(t)
	fake := newFakeLogin(
		&rafikiv1.BeginLoginResponse{LoginId: "l-7", AuthorizeUrl: "https://idp.example.net/authorize"},
		&rafikiv1.CompleteLoginResponse{Token: "sk-oidc-root", Username: "root-user", ExpiresAt: timestamppb.New(time.Unix(1735689600, 0))},
	)
	sock := serveLoginOnSocket(t, fake)
	saveProfile(t, profile.Profile{Name: "sso", Socket: sock})

	resetProfileCache()
	var stdout, stderr bytes.Buffer
	root := newRootCmd()
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	root.SetIn(strings.NewReader("http://localhost:1/oidc/callback?code=root&state=r\n"))
	root.SetArgs([]string{"login", "--no-browser"})
	c.Require().NoError(root.Execute(), "rafiki login --no-browser")

	calls := fake.completeCallsSnapshot(t)
	c.Len(calls, 1, "CompleteLogin calls")
	c.Eq("code=root&state=r", calls[0].GetCallbackQuery(), "pasted query")
	c.Eq("sk-oidc-root", profile.ReadToken("sso"), "token written to the isolated profile")
	out := stderr.String()
	c.StrContains(out, "https://idp.example.net/authorize", "authorize URL on stderr")
	c.StrContains(out, "logged in as root-user (profile sso)", "success line")
}
