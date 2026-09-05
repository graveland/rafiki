// SPDX-License-Identifier: Apache-2.0

package darajapool

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	adminpb "go.graveland.dev/rafiki/pkg/adminpb"
	"go.graveland.dev/rafiki/pkg/adminpb/adminpbconnect"
	"go.graveland.dev/rafiki/pkg/darajapb"
)

// fakeAdminClient is a minimal adminpbconnect.AdminServiceClient. launchFn is
// called synchronously by Launch itself; a test that wants the reverse dial
// to actually happen calls pool.installLive-equivalent (via a real
// connectFakeDaraja daraja) or fires OnConnect directly, from inside launchFn
// or from a separate goroutine, depending on what it's testing.
type fakeAdminClient struct {
	launchFn func(context.Context, *connect.Request[adminpb.LaunchRequest]) (*connect.Response[adminpb.LaunchResponse], error)
}

func (f *fakeAdminClient) Launch(ctx context.Context, req *connect.Request[adminpb.LaunchRequest]) (*connect.Response[adminpb.LaunchResponse], error) {
	return f.launchFn(ctx, req)
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
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if result.Pid != 4242 || result.Pgid != 4242 {
		t.Fatalf("got %+v, want Pid=4242 Pgid=4242", result)
	}
}

func TestLaunchTimesOutAndEvictsIfNoReverseDialArrives(t *testing.T) {
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
	if err == nil {
		t.Fatal("want a timeout error, got nil")
	}
	if !strings.Contains(err.Error(), "did not connect within") {
		t.Fatalf("want a timeout-shaped error, got: %v", err)
	}
	if elapsed > time.Second {
		t.Fatalf("took %s — Timeout override was not honoured", elapsed)
	}
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
	if err == nil {
		t.Fatal("want an error for a spec with no Claude params")
	}
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
	if err == nil || !errors.Is(err, wantErr) {
		t.Fatalf("want an error wrapping %v, got %v", wantErr, err)
	}
}
