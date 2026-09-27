package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/gen/rafiki/v1/rafikiv1connect"
	"go.graveland.dev/rafiki/pkg/paths"
	"go.graveland.dev/rafiki/pkg/profile"

	"github.com/multigres/testkit/assert"
)

// The connect target is derived from the resolved PROFILE, not configured: a
// remote profile means the executor dials that daemon's TLS listener, and a
// local (socket) profile means the local daemon's unix socket. Getting this
// backwards points an executor at the wrong machine, which then serves the
// wrong filesystem.
func TestSessionExecutorConnectTarget(t *testing.T) {
	c := assert.NewCollecting(t)
	remote := profile.Resolved{Profile: profile.Profile{URL: "https://rafiki.example.dev:8443"}}
	addr, sock, err := sessionConnectTarget(remote)
	c.Require().NoError(err, "sessionConnectTarget")
	c.Eq("rafiki.example.dev:8443", addr, "addr")
	c.Eq("", sock, "socket")

	local := profile.Resolved{Profile: profile.Profile{Socket: "/some/path"}}
	addr, sock, err = sessionConnectTarget(local)
	c.Require().NoError(err, "sessionConnectTarget")
	c.Eq("", addr, "addr")
	c.NotEq("", sock, "socket is empty; a local daemon is reached over the unix socket")
}

// A durable executor (`rafiki executor serve --connect` / `service install`)
// must default its connect target from RAFIKI_URL the same way the automatic
// in-process executor already does (sessionConnectTarget) — an operator who
// already has RAFIKI_URL set everywhere else shouldn't have to separately
// derive and pass host:port by hand.
func TestResolveExecutorConnectFlags_DerivesFromRAFIKIURL(t *testing.T) {
	t.Setenv("RAFIKI_URL", "https://rafiki.example.dev:8443")
	c := assert.NewCollecting(t)
	connect, socket, err := resolveExecutorConnectFlags("", "")
	c.Require().NoError(err, "resolveExecutorConnectFlags")
	c.Eq("rafiki.example.dev:8443", connect, "connect")
	c.Eq("", socket, "socket")
}

// An explicit --connect always wins over RAFIKI_URL, even when they disagree —
// the flag is what the operator typed just now.
func TestResolveExecutorConnectFlags_ExplicitConnectWinsOverRAFIKIURL(t *testing.T) {
	t.Setenv("RAFIKI_URL", "https://rafiki.example.dev:8443")
	c := assert.NewCollecting(t)
	connect, socket, err := resolveExecutorConnectFlags("other.example.dev:9000", "")
	c.Require().NoError(err, "resolveExecutorConnectFlags")
	c.Eq("other.example.dev:9000", connect, "connect")
	c.Eq("", socket, "socket")
}

// An explicit --connect-socket must not be overridden by a RAFIKI_URL-derived
// --connect — naming the local socket transport is itself a deliberate choice.
func TestResolveExecutorConnectFlags_ExplicitSocketWinsOverRAFIKIURL(t *testing.T) {
	t.Setenv("RAFIKI_URL", "https://rafiki.example.dev:8443")
	c := assert.NewCollecting(t)
	connect, socket, err := resolveExecutorConnectFlags("", "/tmp/rafiki-executor.sock")
	c.Require().NoError(err, "resolveExecutorConnectFlags")
	c.Eq("", connect, "connect")
	c.Eq("/tmp/rafiki-executor.sock", socket, "socket")
}

// Both flags given is still an error regardless of RAFIKI_URL.
func TestResolveExecutorConnectFlags_BothGivenIsAnError(t *testing.T) {
	t.Setenv("RAFIKI_URL", "https://rafiki.example.dev:8443")
	_, _, err := resolveExecutorConnectFlags("host:1234", "/tmp/x.sock")
	assert.NewAborting(t).Error(err, "expected an error: --connect and --connect-socket are mutually exclusive")
}

// No flags and no usable RAFIKI_URL (e.g. the local proxy face's http:// URL,
// or unset) is still an error — there is nothing to derive a target from.
func TestResolveExecutorConnectFlags_NeitherGivenNorDerivableIsAnError(t *testing.T) {
	t.Setenv("RAFIKI_URL", "")
	_, _, err := resolveExecutorConnectFlags("", "")
	assert.NewAborting(t).Error(err, "expected an error: neither flag was given and RAFIKI_URL derives nothing")
}

// A remote profile with no token must be refused BEFORE the round trip, with
// the same token-file guidance newConnectEndpoint gives (W4a: the guard was
// silently dropped here, so the session executor sent an empty bearer
// credential and surfaced a bare Unauthenticated instead).
func TestSessionExecutorConnectEndpoint_RemoteWithoutTokenIsRefused(t *testing.T) {
	c := assert.NewCollecting(t)
	isolateSessionExecutorEnv(t)

	_, err := sessionConnectEndpoint(profile.Resolved{Profile: profile.Profile{
		Name: "remote",
		URL:  "https://rafiki.example.dev:8443",
	}})
	c.Require().Error(err, "expected the remote no-token guard to refuse the profile")
	c.StrContains(err.Error(), "token", "error should name the missing token, got: %v", err)

	// With a token the remote endpoint resolves to the profile's URL.
	ep, err := sessionConnectEndpoint(profile.Resolved{
		Profile: profile.Profile{
			Name: "remote",
			URL:  "https://rafiki.example.dev:8443",
		},
		Token: "tok",
	})
	c.Require().NoError(err, "sessionConnectEndpoint")
	c.Eq("https://rafiki.example.dev:8443", ep.baseURL, "baseURL")
}

// executorSessionStub serves ExecutorSession: it sends one ready event, then
// blocks until the request context is cancelled or the test tells it to end
// the stream (with or without an error) via end. ListExecutors always reports
// a live match (unless notLive is set), so waitExecutorLive returns on its
// first poll rather than delaying the test with its own retry loop.
//
// ctxDone is closed once the handler's request context ends — the SERVER-side
// view of the client's stream cancellation, which is the daemon's eviction
// trigger. Nil means the test is not watching it. Zero-value traps avoided on
// purpose: notLive (not live) reads as "live" when unset, so the original
// always-live stub behaviour survives every test that predates it.
type executorSessionStub struct {
	rafikiv1connect.UnimplementedControlHandler
	ready       *rafikiv1.ExecutorSessionReady
	end         chan error
	notLive     bool
	ctxDone     chan struct{}
	ctxDoneOnce sync.Once
}

func (s *executorSessionStub) ExecutorSession(
	ctx context.Context,
	_ *connect.Request[rafikiv1.ExecutorSessionRequest],
	stream *connect.ServerStream[rafikiv1.ExecutorSessionEvent],
) error {
	if s.ctxDone != nil {
		go func() {
			<-ctx.Done()
			s.ctxDoneOnce.Do(func() { close(s.ctxDone) })
		}()
	}
	if err := stream.Send(&rafikiv1.ExecutorSessionEvent{
		Event: &rafikiv1.ExecutorSessionEvent_Ready{Ready: s.ready},
	}); err != nil {
		return err
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-s.end:
		return err
	}
}

func (s *executorSessionStub) ListExecutors(
	_ context.Context, _ *connect.Request[rafikiv1.ListExecutorsRequest],
) (*connect.Response[rafikiv1.ListExecutorsResponse], error) {
	row := &rafikiv1.ExecutorRow{Enabled: true, Connected: true}
	if s.notLive {
		// Connected=false keeps executorLive false, so waitExecutorLive
		// keeps polling until the session ends some other way.
		row.Connected = false
	}
	return connect.NewResponse(&rafikiv1.ListExecutorsResponse{
		Rows: []*rafikiv1.ExecutorRow{row},
	}), nil
}

// serveExecutorSessionStub serves stub on a unix socket matching what
// sessionConnectEndpoint resolves for a local (socket) profile: the profile's
// own socket (serveConnectOnUnixSocket is defined in cmd_history_test.go and
// shared package-wide).
//
// A short os.MkdirTemp directory, not t.TempDir(): unix socket paths are
// capped at ~104 bytes (sizeof sun_path on darwin), and t.TempDir() nests
// under the full test name, which alone can exceed that.
func serveExecutorSessionStub(t *testing.T, stub *executorSessionStub) profile.Resolved {
	t.Helper()
	dir, err := os.MkdirTemp("", "ses")
	assert.NewAborting(t).NoError(err, "MkdirTemp")
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "controller.sock")
	routePath, handler := rafikiv1connect.NewControlHandler(stub)
	serveConnectOnUnixSocket(t, sock, routePath, handler)
	return profile.Resolved{Profile: profile.Profile{Socket: sock}}
}

// isolateSessionExecutorEnv isolates the runtime/data/config dirs a session
// executor touches (the executor socket execpool.Connect dials, and the
// machine-name file paths.MachineName would otherwise read from the real
// developer machine) so the test's outcome does not depend on host state.
func isolateSessionExecutorEnv(t *testing.T) {
	t.Helper()
	isolateProfiles(t)
	resetProfileCache()
	t.Setenv(paths.ExecutorName, "test-machine")
}

// waitUntil polls cond every 25ms until it holds or budget elapses. Tests use
// it to wait for asynchronous teardown they did not trigger themselves, where
// a fixed sleep would either flake or waste time.
func waitUntil(budget time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(budget)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(25 * time.Millisecond)
	}
	return cond()
}

// Calling the cleanup startSessionExecutor returns must cancel the
// ExecutorSession stream's context — that is the daemon's eviction trigger
// (proto ExecutorSessionReady's own doc comment) — and must leave no
// goroutine running behind it. Both halves are asserted: cleanup returns
// promptly (the client-side join, bounded by sessionCleanupJoinCap), and the
// stub's request context actually ended (the server side — cleanup returning
// alone proves nothing about the stream, W4a).
func TestStartSessionExecutor_CleanupCancelsStreamAndLeavesNoGoroutine(t *testing.T) {
	c := assert.NewCollecting(t)
	isolateSessionExecutorEnv(t)

	stub := &executorSessionStub{
		ready: &rafikiv1.ExecutorSessionReady{
			ExecutorId: "exec-1",
			RunLocal:   true,
			Ticket:     "tik-1",
			Selector:   "machine=test-machine",
		},
		end:     make(chan error, 1),
		ctxDone: make(chan struct{}),
	}
	p := serveExecutorSessionStub(t, stub)

	selector, cleanup, err := startSessionExecutor(context.Background(), t.TempDir(), p)
	c.Require().NoError(err, "startSessionExecutor")
	c.Eq(stub.ready.Selector, selector, "selector")

	done := make(chan struct{})
	go func() {
		cleanup()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("cleanup did not return within 5s; a goroutine is stuck (the stream, or the local executor, was not torn down)")
	}

	// Server side: the stub must have seen its request context end.
	select {
	case <-stub.ctxDone:
	case <-time.After(5 * time.Second):
		t.Fatal("the stub's request context never ended; cleanup did not trigger the daemon-side eviction")
	}
}

// A stream error that arrives after ready means the daemon evicted this
// executor. It must be detected WITHOUT cleanup's help: the watch goroutine
// notices the stream end on its own, logs it, and tears the local executor
// down. The test waits for that self-driven teardown (observed through the
// log ring the package routes slog through) and only THEN calls cleanup —
// W4a: calling cleanup immediately proves nothing, it passes identically with
// the watch goroutine's proactive teardown deleted.
func TestStartSessionExecutor_StreamErrorAfterReadyEndsTheSession(t *testing.T) {
	c := assert.NewAborting(t)
	isolateSessionExecutorEnv(t)

	ring := newLogRing(64)
	prev := slog.Default()
	slog.SetDefault(slog.New(ring))
	t.Cleanup(func() { slog.SetDefault(prev) })

	stub := &executorSessionStub{
		ready: &rafikiv1.ExecutorSessionReady{
			ExecutorId: "exec-1",
			RunLocal:   true,
			Ticket:     "tik-1",
			Selector:   "machine=test-machine",
		},
		end: make(chan error, 1),
	}
	p := serveExecutorSessionStub(t, stub)

	selector, cleanup, err := startSessionExecutor(context.Background(), t.TempDir(), p)
	c.NoError(err, "startSessionExecutor")
	c.NotEq("", selector, "selector is empty")

	// The daemon evicts this executor: the stream ends with a genuine error,
	// not a client-initiated cancellation. The watch goroutine must classify
	// and log it; the execpool goroutine's own failure (a dial error against
	// the isolated executor socket) logs the same message with a different
	// error, so the assertion pins the watch goroutine's record — the one
	// carrying the injected "evicted" error — specifically.
	stub.end <- errors.New("evicted")
	if !waitUntil(5*time.Second, func() bool {
		for _, rec := range ring.Records() {
			if strings.Contains(rec, "this machine's executor stopped") && strings.Contains(rec, "evicted") {
				return true
			}
		}
		return false
	}) {
		t.Fatalf("the watch goroutine never logged the stream error; log ring: %v", ring.Records())
	}

	// Only now call cleanup: the eviction was already propagated without it.
	// Cleanup must still return promptly after the watch goroutine's own
	// teardown (idempotent stopOnce, join already drained).
	done := make(chan struct{})
	go func() {
		cleanup()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("cleanup did not return within 5s after the watch goroutine's own teardown")
	}
}

// If the daemon ends the ExecutorSession stream BEFORE the executor reports
// live, startSessionExecutor must fail with the stream's own error — the
// eviction — not with a generic cancellation or the ready-timeout message
// (W4a minor: the liveness wait used to ride the parent context and reported
// evictions as "timed out waiting for executor to connect" after the full
// sessionReadyTimeout).
func TestStartSessionExecutor_StreamEndsBeforeLiveSurfacesTheStreamError(t *testing.T) {
	c := assert.NewCollecting(t)
	isolateSessionExecutorEnv(t)

	stub := &executorSessionStub{
		ready: &rafikiv1.ExecutorSessionReady{
			ExecutorId: "exec-1",
			RunLocal:   true,
			Ticket:     "tik-1",
			Selector:   "machine=test-machine",
		},
		end:     make(chan error, 1),
		notLive: true, // nothing ever connects; waitExecutorLive keeps polling
	}
	p := serveExecutorSessionStub(t, stub)

	// Evict while the liveness wait is still polling.
	go func() {
		time.Sleep(300 * time.Millisecond)
		stub.end <- errors.New("evicted")
	}()

	start := time.Now()
	_, _, err := startSessionExecutor(context.Background(), t.TempDir(), p)
	c.Require().Error(err, "expected startSessionExecutor to fail once the stream was evicted")
	c.StrContains(err.Error(), "evicted", "error should surface the stream's own error, got: %v", err)
	c.NotStrContains(err.Error(), "timed out", "error should not be the ready-timeout message, got: %v", err)
	c.LessOrEqual(10*time.Second, time.Since(start), "startSessionExecutor took")
}

// run_local=false means a durable executor already covers this machine:
// startSessionExecutor must start nothing locally and its cleanup must be an
// immediate no-op, not a stream left dangling open for nothing.
func TestStartSessionExecutor_DurableExecutorStartsNothingLocally(t *testing.T) {
	c := assert.NewCollecting(t)
	isolateSessionExecutorEnv(t)

	stub := &executorSessionStub{
		ready: &rafikiv1.ExecutorSessionReady{
			ExecutorId: "durable-1",
			RunLocal:   false,
			Selector:   "machine=laptop",
		},
		end:     make(chan error, 1),
		ctxDone: make(chan struct{}),
	}
	p := serveExecutorSessionStub(t, stub)

	selector, cleanup, err := startSessionExecutor(context.Background(), t.TempDir(), p)
	c.Require().NoError(err, "startSessionExecutor")
	c.Eq("machine=laptop", selector, "selector")
	cleanup() // must not block or panic

	// Even the durable path must have ended the stream: it was opened for
	// nothing once run_local=false came back (W4a: the server side was never
	// asserted).
	select {
	case <-stub.ctxDone:
	case <-time.After(5 * time.Second):
		t.Fatal("the stub's request context never ended; the durable path left the stream open")
	}
}
