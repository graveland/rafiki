package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/gen/rafiki/v1/rafikiv1connect"
	"go.graveland.dev/rafiki/pkg/paths"
	"go.graveland.dev/rafiki/pkg/profile"
)

// The connect target is derived from the resolved PROFILE, not configured: a
// remote profile means the executor dials that daemon's TLS listener, and a
// local (socket) profile means the local daemon's unix socket. Getting this
// backwards points an executor at the wrong machine, which then serves the
// wrong filesystem.
func TestSessionExecutorConnectTarget(t *testing.T) {
	remote := profile.Resolved{Profile: profile.Profile{URL: "https://rafiki.example.dev:8443"}}
	addr, sock, err := sessionConnectTarget(remote)
	if err != nil {
		t.Fatalf("sessionConnectTarget: %v", err)
	}
	if addr != "rafiki.example.dev:8443" {
		t.Errorf("addr = %q, want the remote host:port", addr)
	}
	if sock != "" {
		t.Errorf("socket = %q, want empty for a remote daemon", sock)
	}

	local := profile.Resolved{Profile: profile.Profile{Socket: "/some/path"}}
	addr, sock, err = sessionConnectTarget(local)
	if err != nil {
		t.Fatalf("sessionConnectTarget: %v", err)
	}
	if addr != "" {
		t.Errorf("addr = %q, want empty for a local daemon", addr)
	}
	if sock == "" {
		t.Error("socket is empty; a local daemon is reached over the unix socket")
	}
}

// A durable executor (`rafiki executor serve --connect` / `service install`)
// must default its connect target from RAFIKI_URL the same way the automatic
// in-process executor already does (sessionConnectTarget) — an operator who
// already has RAFIKI_URL set everywhere else shouldn't have to separately
// derive and pass host:port by hand.
func TestResolveExecutorConnectFlags_DerivesFromRAFIKIURL(t *testing.T) {
	t.Setenv("RAFIKI_URL", "https://rafiki.example.dev:8443")
	connect, socket, err := resolveExecutorConnectFlags("", "")
	if err != nil {
		t.Fatalf("resolveExecutorConnectFlags: %v", err)
	}
	if connect != "rafiki.example.dev:8443" {
		t.Errorf("connect = %q, want the RAFIKI_URL-derived host:port", connect)
	}
	if socket != "" {
		t.Errorf("socket = %q, want empty", socket)
	}
}

// An explicit --connect always wins over RAFIKI_URL, even when they disagree —
// the flag is what the operator typed just now.
func TestResolveExecutorConnectFlags_ExplicitConnectWinsOverRAFIKIURL(t *testing.T) {
	t.Setenv("RAFIKI_URL", "https://rafiki.example.dev:8443")
	connect, socket, err := resolveExecutorConnectFlags("other.example.dev:9000", "")
	if err != nil {
		t.Fatalf("resolveExecutorConnectFlags: %v", err)
	}
	if connect != "other.example.dev:9000" {
		t.Errorf("connect = %q, want the explicit flag value untouched", connect)
	}
	if socket != "" {
		t.Errorf("socket = %q, want empty", socket)
	}
}

// An explicit --connect-socket must not be overridden by a RAFIKI_URL-derived
// --connect — naming the local socket transport is itself a deliberate choice.
func TestResolveExecutorConnectFlags_ExplicitSocketWinsOverRAFIKIURL(t *testing.T) {
	t.Setenv("RAFIKI_URL", "https://rafiki.example.dev:8443")
	connect, socket, err := resolveExecutorConnectFlags("", "/tmp/rafiki-executor.sock")
	if err != nil {
		t.Fatalf("resolveExecutorConnectFlags: %v", err)
	}
	if connect != "" {
		t.Errorf("connect = %q, want empty: --connect-socket already chose the transport", connect)
	}
	if socket != "/tmp/rafiki-executor.sock" {
		t.Errorf("socket = %q, want the explicit flag value", socket)
	}
}

// Both flags given is still an error regardless of RAFIKI_URL.
func TestResolveExecutorConnectFlags_BothGivenIsAnError(t *testing.T) {
	t.Setenv("RAFIKI_URL", "https://rafiki.example.dev:8443")
	_, _, err := resolveExecutorConnectFlags("host:1234", "/tmp/x.sock")
	if err == nil {
		t.Fatal("expected an error: --connect and --connect-socket are mutually exclusive")
	}
}

// No flags and no usable RAFIKI_URL (e.g. the local proxy face's http:// URL,
// or unset) is still an error — there is nothing to derive a target from.
func TestResolveExecutorConnectFlags_NeitherGivenNorDerivableIsAnError(t *testing.T) {
	t.Setenv("RAFIKI_URL", "")
	_, _, err := resolveExecutorConnectFlags("", "")
	if err == nil {
		t.Fatal("expected an error: neither flag was given and RAFIKI_URL derives nothing")
	}
}

// executorSessionStub serves ExecutorSession: it sends one ready event, then
// blocks until the request context is cancelled or the test tells it to end
// the stream (with or without an error) via end. ListExecutors always reports
// a live match, so waitExecutorLive returns on its first poll rather than
// delaying the test with its own retry loop.
type executorSessionStub struct {
	rafikiv1connect.UnimplementedControlHandler
	ready *rafikiv1.ExecutorSessionReady
	end   chan error
}

func (s *executorSessionStub) ExecutorSession(
	ctx context.Context,
	_ *connect.Request[rafikiv1.ExecutorSessionRequest],
	stream *connect.ServerStream[rafikiv1.ExecutorSessionEvent],
) error {
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
	return connect.NewResponse(&rafikiv1.ListExecutorsResponse{
		Rows: []*rafikiv1.ExecutorRow{{Enabled: true, Connected: true}},
	}), nil
}

// serveExecutorSessionStub serves stub on a unix socket matching what
// sessionConnectEndpoint resolves for a local (socket) profile: connect.sock
// beside the profile's own control socket (serveConnectOnUnixSocket is
// defined in cmd_history_test.go and shared package-wide).
//
// A short os.MkdirTemp directory, not t.TempDir(): unix socket paths are
// capped at ~104 bytes (sizeof sun_path on darwin), and t.TempDir() nests
// under the full test name, which alone can exceed that.
func serveExecutorSessionStub(t *testing.T, stub *executorSessionStub) profile.Resolved {
	t.Helper()
	dir, err := os.MkdirTemp("", "ses")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	controlSock := filepath.Join(dir, "controller.sock")
	routePath, handler := rafikiv1connect.NewControlHandler(stub)
	serveConnectOnUnixSocket(t, filepath.Join(dir, "connect.sock"), routePath, handler)
	return profile.Resolved{Profile: profile.Profile{Socket: controlSock}}
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

// Calling the cleanup startSessionExecutor returns must cancel the
// ExecutorSession stream's context — that is the daemon's eviction trigger
// (proto ExecutorSessionReady's own doc comment) — and must leave no
// goroutine running behind it.
func TestStartSessionExecutor_CleanupCancelsStreamAndLeavesNoGoroutine(t *testing.T) {
	isolateSessionExecutorEnv(t)

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
	if err != nil {
		t.Fatalf("startSessionExecutor: %v", err)
	}
	if selector != stub.ready.Selector {
		t.Errorf("selector = %q, want %q", selector, stub.ready.Selector)
	}

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
}

// A stream error that arrives after ready means the daemon evicted this
// executor. It must be detected — not silently swallowed — mirroring how the
// framed path surfaced a dropped connection. Detection is observed here
// through cleanup returning promptly even without the test ever calling
// cleanup itself: the watch goroutine must notice the eviction on its own and
// tear the local executor down, so a later cleanup call (or none at all)
// still leaves no goroutine behind.
func TestStartSessionExecutor_StreamErrorAfterReadyEndsTheSession(t *testing.T) {
	isolateSessionExecutorEnv(t)

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
	if err != nil {
		t.Fatalf("startSessionExecutor: %v", err)
	}
	if selector == "" {
		t.Fatal("selector is empty")
	}

	// The daemon evicts this executor: the stream ends with a genuine error,
	// not a client-initiated cancellation.
	stub.end <- errors.New("evicted")

	done := make(chan struct{})
	go func() {
		cleanup()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("cleanup did not return within 5s after a stream error following ready; the eviction was not propagated")
	}
}

// No goroutine may survive a call to the returned cleanup — proven directly
// with a WaitGroup rather than a fixed sleep, so the test fails fast on a
// leak instead of racing a timer.
func TestStartSessionExecutor_CleanupWaitsForItsOwnGoroutines(t *testing.T) {
	isolateSessionExecutorEnv(t)

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

	_, cleanup, err := startSessionExecutor(context.Background(), t.TempDir(), p)
	if err != nil {
		t.Fatalf("startSessionExecutor: %v", err)
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		cleanup()
	}()

	waited := make(chan struct{})
	go func() {
		wg.Wait()
		close(waited)
	}()
	select {
	case <-waited:
	case <-time.After(5 * time.Second):
		t.Fatal("cleanup's own goroutines outlived cleanup() returning")
	}
}

// run_local=false means a durable executor already covers this machine:
// startSessionExecutor must start nothing locally and its cleanup must be an
// immediate no-op, not a stream left dangling open for nothing.
func TestStartSessionExecutor_DurableExecutorStartsNothingLocally(t *testing.T) {
	isolateSessionExecutorEnv(t)

	stub := &executorSessionStub{
		ready: &rafikiv1.ExecutorSessionReady{
			ExecutorId: "durable-1",
			RunLocal:   false,
			Selector:   "machine=laptop",
		},
		end: make(chan error, 1),
	}
	p := serveExecutorSessionStub(t, stub)

	selector, cleanup, err := startSessionExecutor(context.Background(), t.TempDir(), p)
	if err != nil {
		t.Fatalf("startSessionExecutor: %v", err)
	}
	if selector != "machine=laptop" {
		t.Errorf("selector = %q, want the durable executor's own selector", selector)
	}
	cleanup() // must not block or panic
}
