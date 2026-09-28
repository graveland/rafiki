// SPDX-License-Identifier: Apache-2.0

package darajapool

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"

	adminpb "go.graveland.dev/rafiki/pkg/adminpb"
	"go.graveland.dev/rafiki/pkg/adminpb/adminpbconnect"
	"go.graveland.dev/rafiki/pkg/darajapb"

	"github.com/multigres/testkit/assert"
)

// fakeAdminClient is a minimal adminpbconnect.AdminServiceClient. launchFn is
// called synchronously by Launch itself; a test that wants the reverse dial
// to actually happen calls pool.installLive-equivalent (via a real
// connectFakeDaraja daraja) or fires OnConnect directly, from inside launchFn
// or from a separate goroutine, depending on what it's testing. statusFn is
// what the launch wait's 1s Status poll gets; nil means the fake predates the
// RPC and answers an error, the old-executor shape.
type fakeAdminClient struct {
	launchFn func(context.Context, *connect.Request[adminpb.LaunchRequest]) (*connect.Response[adminpb.LaunchResponse], error)
	statusFn func(context.Context, *connect.Request[adminpb.StatusRequest]) (*connect.Response[adminpb.StatusResponse], error)
}

func (f *fakeAdminClient) Launch(ctx context.Context, req *connect.Request[adminpb.LaunchRequest]) (*connect.Response[adminpb.LaunchResponse], error) {
	return f.launchFn(ctx, req)
}

func (f *fakeAdminClient) Status(ctx context.Context, req *connect.Request[adminpb.StatusRequest]) (*connect.Response[adminpb.StatusResponse], error) {
	if f.statusFn == nil {
		return nil, errors.New("fakeAdminClient: Status not implemented")
	}
	return f.statusFn(ctx, req)
}

func (f *fakeAdminClient) Reap(context.Context, *connect.Request[adminpb.ReapRequest]) (*connect.Response[adminpb.ReapResponse], error) {
	return nil, errors.New("fakeAdminClient: Reap not implemented")
}

// fakeExecPool implements adminLauncher, handing back a fixed client for any
// executor ID.
type fakeExecPool struct {
	client adminpbconnect.AdminServiceClient
	err    error
}

func (f *fakeExecPool) AdminClientFor(executorID string) (adminpbconnect.AdminServiceClient, error) {
	return f.client, f.err
}

func validSpec() *darajapb.ChildSpec {
	return &darajapb.ChildSpec{
		Kind:   darajapb.Kind_KIND_CLAUDE,
		Claude: &darajapb.ClaudeParams{Model: "claude-sonnet-5"},
	}
}

func TestLaunchReturnsPidPgidOnceTheReverseDialArrives(t *testing.T) {
	c := assert.NewAborting(t)
	reg := NewRegistry()
	pool := New(reg)

	execPool := &fakeExecPool{client: &fakeAdminClient{
		launchFn: func(ctx context.Context, req *connect.Request[adminpb.LaunchRequest]) (*connect.Response[adminpb.LaunchResponse], error) {
			if req.Msg.GetChildId() == "" || req.Msg.GetTicket() == "" || req.Msg.GetDialAddr() == "" {
				t.Errorf("launch request missing a required field: %+v", req.Msg)
			}
			// Simulate the daraja's reverse dial completing right after the
			// executor accepts the launch — installLive is what a real
			// connection does; FireConnect is the test-only equivalent.
			go pool.FireConnect(req.Msg.GetChildId())
			return connect.NewResponse(&adminpb.LaunchResponse{Pid: 4242, Pgid: 4242}), nil
		},
	}}

	result, err := Launch(context.Background(), LaunchParams{
		ExecPool:   execPool,
		Pool:       pool,
		Registry:   reg,
		DialAddr:   "/tmp/whatever.sock",
		ExecutorID: "exec-1",
		ChildID:    "c1",
		Cwd:        "/tmp",
		Spec:       validSpec(),
		Timeout:    2 * time.Second,
	})
	c.NoError(err, "Launch")
	c.False(result.Pid != 4242 || result.Pgid != 4242, "got %+v, want Pid=4242 Pgid=4242", result)
}

func TestLaunchTimesOutAndEvictsIfNoReverseDialArrives(t *testing.T) {
	c := assert.NewAborting(t)
	reg := NewRegistry()
	pool := New(reg)

	execPool := &fakeExecPool{client: &fakeAdminClient{
		launchFn: func(ctx context.Context, req *connect.Request[adminpb.LaunchRequest]) (*connect.Response[adminpb.LaunchResponse], error) {
			// Never fires OnConnect — the daraja "never dials back".
			return connect.NewResponse(&adminpb.LaunchResponse{Pid: 1, Pgid: 1}), nil
		},
	}}

	start := time.Now()
	_, err := Launch(context.Background(), LaunchParams{
		ExecPool:   execPool,
		Pool:       pool,
		Registry:   reg,
		DialAddr:   "/tmp/whatever.sock",
		ExecutorID: "exec-1",
		ChildID:    "c1",
		Cwd:        "/tmp",
		Spec:       validSpec(),
		Timeout:    100 * time.Millisecond,
	})
	elapsed := time.Since(start)
	c.Error(err, "want a timeout error, got nil")
	c.StrContains(err.Error(), "did not connect within", "want a timeout-shaped error, got: %v", err)
	c.LessOrEqual(time.Second, elapsed, "took")
}

// TestLaunchUnregistersItsOnConnectCallback proves Launch's one-shot
// registration does not accumulate: without unsubscribing, every claude spawn
// registers one closure on Pool.onConnect that lives forever after firing,
// growing the list by one per spawn and costing every SUBSEQUENT daraja
// connect one dead call per prior spawn — see the KNOWN, ACCEPTED LEAK note
// launch.go used to carry. Covers both exit paths: a successful connect and a
// timeout.
func TestLaunchUnregistersItsOnConnectCallback(t *testing.T) {
	reg := NewRegistry()
	pool := New(reg)

	connecting := &fakeExecPool{client: &fakeAdminClient{
		launchFn: func(ctx context.Context, req *connect.Request[adminpb.LaunchRequest]) (*connect.Response[adminpb.LaunchResponse], error) {
			go pool.FireConnect(req.Msg.GetChildId())
			return connect.NewResponse(&adminpb.LaunchResponse{Pid: 1, Pgid: 1}), nil
		},
	}}
	timingOut := &fakeExecPool{client: &fakeAdminClient{
		launchFn: func(ctx context.Context, req *connect.Request[adminpb.LaunchRequest]) (*connect.Response[adminpb.LaunchResponse], error) {
			return connect.NewResponse(&adminpb.LaunchResponse{Pid: 1, Pgid: 1}), nil
		},
	}}

	for i, execPool := range []*fakeExecPool{connecting, connecting, timingOut, connecting} {
		if _, err := Launch(context.Background(), LaunchParams{
			ExecPool:   execPool,
			Pool:       pool,
			Registry:   reg,
			DialAddr:   "/tmp/whatever.sock",
			ExecutorID: "exec-1",
			ChildID:    "c1",
			Cwd:        "/tmp",
			Spec:       validSpec(),
			Timeout:    100 * time.Millisecond,
		}); err != nil && execPool != timingOut {
			t.Fatalf("Launch #%d: %v", i, err)
		}
	}

	pool.onConnectMu.Lock()
	got := len(pool.onConnect)
	pool.onConnectMu.Unlock()
	assert.NewAborting(t).Eq(0, got, "Pool.onConnect holds")
}

func TestLaunchRejectsASpecWithNoClaudeParams(t *testing.T) {
	reg := NewRegistry()
	pool := New(reg)
	_, err := Launch(context.Background(), LaunchParams{
		ExecPool:   &fakeExecPool{},
		Pool:       pool,
		Registry:   reg,
		DialAddr:   "/tmp/whatever.sock",
		ExecutorID: "exec-1",
		ChildID:    "c1",
		Spec:       &darajapb.ChildSpec{Kind: darajapb.Kind_KIND_CLAUDE},
	})
	assert.NewAborting(t).Error(err, "want an error for a spec with no Claude params")
}

// A SCRIPT spec is the second valid launch payload: the executor resolves the
// script from its synced cache and Launch ships the params to it. The empty
// ScriptParams message (a kind without either variant) stays refused.
func TestLaunchAcceptsAScriptSpec(t *testing.T) {
	c := assert.NewAborting(t)
	reg := NewRegistry()
	pool := New(reg)
	_, err := Launch(context.Background(), LaunchParams{
		ExecPool:   &fakeExecPool{err: errors.New("no admin client")},
		Pool:       pool,
		Registry:   reg,
		DialAddr:   "/tmp/whatever.sock",
		ExecutorID: "exec-1",
		ChildID:    "c1",
		Spec: &darajapb.ChildSpec{
			Kind:   darajapb.Kind_KIND_SCRIPT,
			Script: &darajapb.ScriptParams{Repo: "local", Script: "driver"},
		},
	})
	// The fake pool refuses every admin client, so Launch fails there — the
	// assertion is only that the spec VALIDATED: the failure is the fake's,
	// not Launch's payload check.
	c.False(err == nil || !strings.Contains(err.Error(), "no admin client"), "a script spec must pass Launch's payload check (the fake pool refuses next), got: %v", err)

	_, err = Launch(context.Background(), LaunchParams{
		ExecPool: &fakeExecPool{}, Pool: pool, Registry: reg,
		DialAddr: "/tmp/whatever.sock", ExecutorID: "exec-1", ChildID: "c2",
		Spec: &darajapb.ChildSpec{Kind: darajapb.Kind_KIND_SCRIPT},
	})
	c.False(err == nil || !strings.Contains(err.Error(), "requires spec"), "an empty script spec must still be refused, got: %v", err)
}

func TestLaunchPropagatesAdminClientForFailure(t *testing.T) {
	reg := NewRegistry()
	pool := New(reg)
	wantErr := errors.New("no admin client")
	_, err := Launch(context.Background(), LaunchParams{
		ExecPool:   &fakeExecPool{err: wantErr},
		Pool:       pool,
		Registry:   reg,
		DialAddr:   "/tmp/whatever.sock",
		ExecutorID: "exec-1",
		ChildID:    "c1",
		Spec:       validSpec(),
	})
	assert.NewAborting(t).False(err == nil || !errors.Is(err, wantErr), "want an error wrapping %v, got %v", wantErr, err)
}

// The fail-fast arm: a daraja that exits before its reverse dial arrives must
// end the launch wait at ONCE with its exit code and stderr tail — not burn
// the whole timeout on a machine that will never dial back. The timeout here
// is deliberately far beyond the ~2s ceiling, so only the Status poll can be
// what ended the wait.
func TestLaunchFailsFastWhenDarajaExits(t *testing.T) {
	c := assert.NewAborting(t)
	reg := NewRegistry()
	pool := New(reg)
	tail := "daraja: connect failed: dial 127.0.0.1:1: connection refused"

	execPool := &fakeExecPool{client: &fakeAdminClient{
		launchFn: func(ctx context.Context, req *connect.Request[adminpb.LaunchRequest]) (*connect.Response[adminpb.LaunchResponse], error) {
			// Never fires OnConnect — the daraja is alive at launch and dies
			// before dialling back.
			return connect.NewResponse(&adminpb.LaunchResponse{Pid: 9, Pgid: 9}), nil
		},
		statusFn: func(ctx context.Context, req *connect.Request[adminpb.StatusRequest]) (*connect.Response[adminpb.StatusResponse], error) {
			code := int32(3)
			return connect.NewResponse(&adminpb.StatusResponse{
				Known: true, Running: false, ExitCode: &code, StderrTail: tail,
			}), nil
		},
	}}

	start := time.Now()
	_, err := Launch(context.Background(), LaunchParams{
		ExecPool:   execPool,
		Pool:       pool,
		Registry:   reg,
		DialAddr:   "/tmp/whatever.sock",
		ExecutorID: "exec-1",
		ChildID:    "c1",
		Cwd:        "/tmp",
		Spec:       validSpec(),
		Timeout:    30 * time.Second,
	})
	elapsed := time.Since(start)
	c.Error(err, "want the exited-before-connecting error, got nil")
	c.StrContains(err.Error(), "exited (code 3) before connecting", "error")
	c.StrContains(err.Error(), tail, "the stderr tail must ride the error")
	c.LessOrEqual(2*time.Second, elapsed, "Launch returned after")
}

// An executor that predates the Status RPC answers Unimplemented. That must
// be "keep waiting", not failure: the wait runs out its timeout exactly as it
// did before the poll existed, and the error is the timeout-shaped one.
func TestLaunchStatusUnimplementedKeepsWaiting(t *testing.T) {
	reg := NewRegistry()
	pool := New(reg)

	execPool := &fakeExecPool{client: &fakeAdminClient{
		launchFn: func(ctx context.Context, req *connect.Request[adminpb.LaunchRequest]) (*connect.Response[adminpb.LaunchResponse], error) {
			return connect.NewResponse(&adminpb.LaunchResponse{Pid: 9, Pgid: 9}), nil
		},
		statusFn: func(ctx context.Context, req *connect.Request[adminpb.StatusRequest]) (*connect.Response[adminpb.StatusResponse], error) {
			return nil, connect.NewError(connect.CodeUnimplemented, errors.New("unknown method Status"))
		},
	}}

	_, err := Launch(context.Background(), LaunchParams{
		ExecPool:   execPool,
		Pool:       pool,
		Registry:   reg,
		DialAddr:   "/tmp/whatever.sock",
		ExecutorID: "exec-1",
		ChildID:    "c1",
		Cwd:        "/tmp",
		Spec:       validSpec(),
		// Two Status polls land inside this window (1s, 2s), both refused;
		// the wait must outlive them and end on the timeout arm.
		Timeout: 2500 * time.Millisecond,
	})
	c := assert.NewAborting(t)
	c.Error(err, "want the timeout error, got nil")
	c.StrContains(err.Error(), "did not connect within", "an Unimplemented Status must not end the wait, got: %v", err)
	c.False(strings.Contains(err.Error(), "exited"), "the fail-fast error leaked through an Unimplemented poll: %v", err)
}

// countingHandler counts slog records by message, so a test can pin "logged
// once" without parsing formatted output.
type countingHandler struct {
	mu     sync.Mutex
	counts map[string]int
}

func newCountingHandler() *countingHandler { return &countingHandler{counts: map[string]int{}} }

func (h *countingHandler) Enabled(context.Context, slog.Level) bool { return true }
func (h *countingHandler) WithAttrs([]slog.Attr) slog.Handler       { return h }
func (h *countingHandler) WithGroup(string) slog.Handler            { return h }

func (h *countingHandler) Handle(_ context.Context, r slog.Record) error {
	h.mu.Lock()
	h.counts[r.Message]++
	h.mu.Unlock()
	return nil
}

func (h *countingHandler) count(msg string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.counts[msg]
}

// A refused Status poll is reported ONCE per wait, not once per tick: two
// Unimplemented polls inside one wait must produce exactly one warning.
func TestLaunchStatusErrorIsLoggedOncePerWait(t *testing.T) {
	h := newCountingHandler()
	old := slog.Default()
	slog.SetDefault(slog.New(h))
	t.Cleanup(func() { slog.SetDefault(old) })

	reg := NewRegistry()
	pool := New(reg)
	execPool := &fakeExecPool{client: &fakeAdminClient{
		launchFn: func(ctx context.Context, req *connect.Request[adminpb.LaunchRequest]) (*connect.Response[adminpb.LaunchResponse], error) {
			return connect.NewResponse(&adminpb.LaunchResponse{Pid: 9, Pgid: 9}), nil
		},
		statusFn: func(ctx context.Context, req *connect.Request[adminpb.StatusRequest]) (*connect.Response[adminpb.StatusResponse], error) {
			return nil, connect.NewError(connect.CodeUnimplemented, errors.New("unknown method Status"))
		},
	}}

	_, err := Launch(context.Background(), LaunchParams{
		ExecPool: execPool, Pool: pool, Registry: reg,
		DialAddr: "/tmp/whatever.sock", ExecutorID: "exec-1", ChildID: "c1",
		Cwd: "/tmp", Spec: validSpec(),
		Timeout: 2500 * time.Millisecond, // two polls, both refused
	})
	c := assert.NewAborting(t)
	c.Error(err, "want the timeout error")
	c.Eq(1, h.count("daraja launch: Status poll failed; waiting on the connect signal"), "Status failure warnings")
}
