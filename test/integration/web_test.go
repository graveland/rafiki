// SPDX-License-Identifier: Apache-2.0

package integration_test

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"connectrpc.com/connect"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/gen/rafiki/v1/rafikiv1connect"

	"github.com/multigres/testkit/assert"
)

// bootWeb starts `rafiki web` against d and returns the port it bound plus the
// single-use launch code carried in the URL it prints as its first stdout
// line. The command serves until it is signalled, so the cleanup kills it.
//
// --dev-assets is set to a dead address on purpose: it is what lets a Go-only
// test build (whose embedded dist/ holds only .gitkeep) start at all, and no
// static asset is ever requested by these tests.
func bootWeb(t *testing.T, d *daemon) (int, string) {
	t.Helper()
	c := assert.NewAborting(t)

	cmd := cliCmd(t, d, "web", "--no-open", "--port", "0", "--dev-assets", "http://127.0.0.1:1")
	stdout, err := cmd.StdoutPipe()
	c.NoError(err, "stdout pipe")
	c.NoError(cmd.Start(), "start rafiki web")
	t.Cleanup(func() {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		_ = cmd.Wait()
	})

	// The first line is the only thing the command prints before it blocks in
	// Serve, so read exactly it and stop: a scanner reading past the first line
	// would block forever on a pipe that never gets another byte.
	type lineResult struct {
		line string
		err  error
	}
	lineCh := make(chan lineResult, 1)
	go func() {
		sc := bufio.NewScanner(stdout)
		if sc.Scan() {
			lineCh <- lineResult{line: sc.Text()}
			return
		}
		lineCh <- lineResult{err: sc.Err()}
	}()

	select {
	case r := <-lineCh:
		c.NoError(r.err, "read first stdout line")
		return parseWebURL(t, r.line)
	case <-time.After(15 * time.Second):
		t.Fatal("rafiki web printed no URL within 15s")
		return 0, ""
	}
}

// parseWebURL extracts the bound port and the launch code from the
// "rafiki web: http://127.0.0.1:<port>/#k=<code>" line.
func parseWebURL(t *testing.T, line string) (int, string) {
	t.Helper()
	c := assert.NewAborting(t)

	rest, ok := strings.CutPrefix(line, "rafiki web: ")
	c.True(ok, "first stdout line is not \"rafiki web: <url>\": %q", line)
	u, err := url.Parse(rest)
	c.NoError(err, "parse %q", rest)
	_, portStr, err := net.SplitHostPort(u.Host)
	c.NoError(err, "split host %q", u.Host)
	port, err := strconv.Atoi(portStr)
	c.NoError(err, "port %q is not an integer", portStr)
	code, ok := strings.CutPrefix(u.Fragment, "k=")
	c.True(ok, "URL fragment is not #k=<code>: %q", u.Fragment)
	c.NotEq("", code, "empty launch code")
	c.Eq("127.0.0.1", u.Hostname(), "rafiki web must bind 127.0.0.1")
	return port, code
}

// exchangeCode POSTs the launch code to /_rafiki/session and returns the HTTP
// status and, when it succeeded, the per-tab session secret. The Origin header
// is required: the gate refuses a POST without one.
func exchangeCode(t *testing.T, port int, code string) (int, string) {
	t.Helper()
	c := assert.NewAborting(t)

	body, err := json.Marshal(map[string]string{"code": code})
	c.NoError(err, "marshal session request")
	req, err := http.NewRequest(http.MethodPost,
		fmt.Sprintf("http://127.0.0.1:%d/_rafiki/session", port), bytes.NewReader(body))
	c.NoError(err, "build session request")
	req.Header.Set("Origin", fmt.Sprintf("http://127.0.0.1:%d", port))
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	c.NoError(err, "POST /_rafiki/session")
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	c.NoError(err, "read session response")
	if resp.StatusCode != http.StatusOK {
		return resp.StatusCode, ""
	}
	var out struct {
		Secret string `json:"secret"`
	}
	c.NoError(json.Unmarshal(data, &out), "decode session response: %s", data)
	c.NotEq("", out.Secret, "session exchange returned an empty secret")
	return resp.StatusCode, out.Secret
}

// webHeadersTransport adds the two headers the proxy's gate demands on every
// browser request: the per-tab session secret and the loopback Origin. The
// base is the caller's transport (http.DefaultTransport here), so this client
// speaks HTTP/1.1 to the proxy exactly as a browser does.
type webHeadersTransport struct {
	base   http.RoundTripper
	secret string
	origin string
}

func (t *webHeadersTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	r := req.Clone(req.Context())
	r.Header.Set("X-Rafiki-Web", t.secret)
	r.Header.Set("Origin", t.origin)
	return t.base.RoundTrip(r)
}

// TestWebProxiesControlToTheDaemon drives the real `rafiki web` binary against
// a real daemon: it exchanges the launch code for a session secret, then
// proves the proxy carries a unary Control call (ListChildren) and a
// server-stream (StreamEvents) end to end — a child spawned after the stream
// opens is announced through it.
func TestWebProxiesControlToTheDaemon(t *testing.T) {
	t.Parallel()
	c := assert.NewAborting(t)
	d := bootDaemon(t)

	port, code := bootWeb(t, d)
	status, secret := exchangeCode(t, port, code)
	c.Eq(http.StatusOK, status, "session exchange")

	origin := fmt.Sprintf("http://127.0.0.1:%d", port)
	client := rafikiv1connect.NewControlClient(&http.Client{
		Transport: &webHeadersTransport{base: http.DefaultTransport, secret: secret, origin: origin},
	}, origin)

	// A unary Control RPC, proxied without an allowlist.
	listCtx, listCancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer listCancel()
	_, err := client.ListChildren(listCtx, connect.NewRequest(&rafikiv1.ListChildrenRequest{}))
	c.NoError(err, "ListChildren through the proxy")

	// Open the all-subject durable stream of child_spawned events FIRST, then
	// spawn. A from-now StreamEvents sends no response headers until its first
	// live message, so the call itself blocks until an event arrives: it must
	// run in a goroutine or the spawn that produces that event would never
	// happen.
	streamCtx, streamCancel := context.WithCancel(context.Background())
	t.Cleanup(streamCancel)
	streamCh := make(chan *connect.ServerStreamForClient[rafikiv1.Event], 1)
	streamErr := make(chan error, 1)
	go func() {
		s, err := client.StreamEvents(streamCtx, connect.NewRequest(&rafikiv1.StreamEventsRequest{
			Subject: allEventsSubject(),
			Tier:    rafikiv1.EventTier_EVENT_TIER_DURABLE,
			Types:   []string{"child_spawned"},
		}))
		if err != nil {
			streamErr <- err
			return
		}
		streamCh <- s
	}()

	childID := d.spawnChild(t)
	c.NotEq("", childID, "spawn returned an empty childId")

	var stream *connect.ServerStreamForClient[rafikiv1.Event]
	select {
	case stream = <-streamCh:
	case err := <-streamErr:
		t.Fatalf("StreamEvents through the proxy: %v", err)
	case <-time.After(30 * time.Second):
		t.Fatal("StreamEvents never opened through the proxy within 30s")
	}
	defer func() { _ = stream.Close() }()

	// The event for the child we just spawned must arrive through the proxy.
	spawned := make(chan struct{})
	go func() {
		for stream.Receive() {
			if cs := stream.Msg().GetChildSpawned(); cs != nil && cs.GetChildId() == childID {
				close(spawned)
				return
			}
		}
	}()
	select {
	case <-spawned:
	case <-time.After(30 * time.Second):
		t.Fatalf("no child_spawned event for %s arrived through the proxy within 30s", childID)
	}
}

// TestWebSecondCodeUseRefused pins single-use semantics: a launch code
// exchanges once (200) and is refused on the second use (410).
func TestWebSecondCodeUseRefused(t *testing.T) {
	t.Parallel()
	c := assert.NewAborting(t)
	d := bootDaemon(t)

	port, code := bootWeb(t, d)

	first, secret := exchangeCode(t, port, code)
	c.Eq(http.StatusOK, first, "first exchange of a fresh code")
	c.NotEq("", secret, "first exchange returned no secret")

	second, _ := exchangeCode(t, port, code)
	c.Eq(http.StatusGone, second, "second exchange of the same code")
}
