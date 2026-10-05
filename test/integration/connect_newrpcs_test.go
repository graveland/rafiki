// SPDX-License-Identifier: Apache-2.0

package integration_test

// Wiring proof for the Wave-1 Connect seams, on the real daemon: every new
// Control RPC must answer over the daemon's control socket through the backend main.go
// wires (cmd/rafikid: SetChildOps, SetExecutorAdmin, SetUserAdmin,
// SetRawChildIO, SetExecutorSessions) instead of the generated handlers'
// "not yet wired" Unavailable.
//
// This test checks WIRING, not behaviour — the unit suites pin behaviour —
// so each call only asserts its error is neither Unimplemented nor
// Unavailable. Deliberate stubs are excluded: ShutdownDaemon answers
// Unimplemented by design this wave. The calls still use values that would
// SUCCEED if the seam works (a valid machine name for EnrollExecutor and
// ExecutorSession, RAFIKI_EXECUTORS_ENABLED=1 so the daemon builds the
// executor store and pool), because a wire that answers is the point.

import (
	"context"
	"os"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/durationpb"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
)

func TestConnectNewRPCsWired(t *testing.T) {
	t.Parallel()
	// RAFIKI_EXECUTORS_ENABLED=1: bootDaemonDB defaults the executor plane
	// OFF so plain spawns stay toolless; EnrollExecutor needs the executor
	// store and ExecutorSession the pool, so this boot turns it back on.
	d := bootDaemonDB(t, nextDaemonID(),
		append(noRealProviderEnv(), "RAFIKI_EXECUTORS_ENABLED=1")...)
	t.Cleanup(func() { os.RemoveAll(d.homeDir) })

	client := d.connectClient()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// unwired reports whether err is one of the two codes an UNWIRED seam
	// answers: the handlers' "not yet wired" Unavailable, or Unimplemented
	// (a fail-closed stub, or a method the daemon never mounted).
	unwired := func(where string, err error) {
		t.Helper()
		if err == nil {
			return
		}
		switch connect.CodeOf(err) {
		case connect.CodeUnimplemented, connect.CodeUnavailable:
			t.Errorf("%s came back %s: the seam is not wired (%v)", where, connect.CodeOf(err), err)
		default:
			// Wired but refusing on this fixture — wiring is all that is
			// asserted here, so anything else is an observation, not a
			// failure.
			t.Logf("%s returned %s (accepted: wiring, not behaviour, is asserted): %v",
				where, connect.CodeOf(err), err)
		}
	}

	// Status: connectChildOps → Controller.Status.
	resp, err := client.Status(ctx, connect.NewRequest(&rafikiv1.StatusRequest{}))
	unwired("Status", err)
	if err == nil {
		t.Logf("Status: version=%s live=%d exited=%d",
			resp.Msg.GetVersion(), resp.Msg.GetChildren().GetLive(), resp.Msg.GetChildren().GetExited())
	}

	// ModelInfo with an unknown model: connectChildOps → the catalog, which
	// answers known=false rather than an error for anything unconfigured.
	resp2, err := client.ModelInfo(ctx, connect.NewRequest(&rafikiv1.ModelInfoRequest{Model: "x"}))
	unwired("ModelInfo", err)
	if err == nil {
		t.Logf("ModelInfo(\"x\"): known=%v", resp2.Msg.GetKnown())
	}

	// ListUsers: connectUserAdmin, admitted because the local-socket caller
	// is anonymous (UDS local trust — the socket is the credential).
	_, err = client.ListUsers(ctx, connect.NewRequest(&rafikiv1.ListUsersRequest{}))
	unwired("ListUsers", err)

	// GetStreams on a child that does not exist: connectRawChildIO; the
	// refusal (child not found) proves the wire, not the lookup.
	_, err = client.GetStreams(ctx, connect.NewRequest(&rafikiv1.GetStreamsRequest{
		ChildId: "bogus-child-id",
		Which:   "in",
	}))
	unwired("GetStreams(bogus)", err)

	// EnrollExecutor with a valid machine name: connectExecutorAdmin mints
	// a one-time enrollment token. A short TTL keeps the shared test DB
	// free of residue if the token is never claimed.
	_, err = client.EnrollExecutor(ctx, connect.NewRequest(&rafikiv1.EnrollExecutorRequest{
		Name: "wired-test-machine",
		Ttl:  durationpb.New(300 * time.Second),
	}))
	unwired("EnrollExecutor", err)

	// ExecutorSession: open, receive the ready message, cancel — the stream
	// ending is the eviction trigger for the transient executor.
	sctx, scancel := context.WithTimeout(ctx, 10*time.Second)
	defer scancel()
	stream, err := client.ExecutorSession(sctx, connect.NewRequest(&rafikiv1.ExecutorSessionRequest{
		Name: "wired-test-machine",
	}))
	unwired("ExecutorSession(open)", err)
	if err == nil {
		if !stream.Receive() {
			t.Logf("ExecutorSession stream ended without a ready message: %v (accepted: wiring, not behaviour, is asserted)", stream.Err())
		} else if ready := stream.Msg().GetReady(); ready != nil {
			t.Logf("ExecutorSession ready: executor=%s run_local=%v", ready.GetExecutorId(), ready.GetRunLocal())
		}
		scancel() // the eviction trigger: the handler returns on ctx.Done
		for stream.Receive() {
			t.Logf("unexpected ExecutorSession message after cancel: %+v", stream.Msg())
		}
		_ = stream.Close()
	}
}
