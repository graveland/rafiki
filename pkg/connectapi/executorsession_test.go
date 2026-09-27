// SPDX-License-Identifier: Apache-2.0

package connectapi_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"connectrpc.com/connect"

	"go.graveland.dev/rafiki/pkg/connectapi"
	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/gen/rafiki/v1/rafikiv1connect"
	"go.graveland.dev/rafiki/pkg/protocol"

	"github.com/multigres/testkit/assert"
)

// fakeExecSessions is a connectapi.ExecutorSessions fake. When ctxDone is set,
// Open watches its ctx and closes ctxDone the moment the stream ends — the
// handler's promised eviction trigger — so a test can observe it without
// reaching into the daemon's own session bookkeeping.
type fakeExecSessions struct {
	ready   *rafikiv1.ExecutorSessionReady
	err     error
	ctxDone chan struct{}
}

func (f *fakeExecSessions) Open(ctx context.Context, req *rafikiv1.ExecutorSessionRequest) (*rafikiv1.ExecutorSessionReady, error) {
	if f.err != nil {
		return nil, f.err
	}
	if f.ctxDone != nil {
		go func() {
			<-ctx.Done()
			close(f.ctxDone)
		}()
	}
	return f.ready, nil
}

func setupExecSessionServer(t *testing.T, seam connectapi.ExecutorSessions) rafikiv1connect.ControlClient {
	t.Helper()
	s := connectapi.NewServer(nil)
	if seam != nil {
		s.SetExecutorSessions(seam)
	}

	path, h := s.Routes()
	mux := http.NewServeMux()
	mux.Handle(path, h)
	srv := httptest.NewUnstartedServer(mux)
	srv.EnableHTTP2 = true
	srv.StartTLS()
	t.Cleanup(srv.Close)

	return rafikiv1connect.NewControlClient(srv.Client(), srv.URL)
}

// TestExecutorSessionSendsReadyThenBlocksUntilStreamEnds pins the handler
// shape the brief demands: Open is called with the STREAM's context, ready is
// the first (and only) message, and ending the stream — here, the client
// cancelling — is what the backend sees as ctx.Done(), which is the eviction
// trigger.
func TestExecutorSessionSendsReadyThenBlocksUntilStreamEnds(t *testing.T) {
	c := assert.NewAborting(t)
	ctxDone := make(chan struct{})
	seam := &fakeExecSessions{
		ready: &rafikiv1.ExecutorSessionReady{
			ExecutorId: "sess-1",
			RunLocal:   true,
			Ticket:     "tk-1",
			Selector:   "owner=brent,machine=m1",
		},
		ctxDone: ctxDone,
	}
	client := setupExecSessionServer(t, seam)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	stream, err := client.ExecutorSession(ctx, connect.NewRequest(&rafikiv1.ExecutorSessionRequest{Name: "m1"}))
	c.NoError(err, "ExecutorSession")

	if !stream.Receive() {
		t.Fatalf("expected the ready message, got err: %v", stream.Err())
	}
	ready := stream.Msg().GetReady()
	c.False(ready.GetExecutorId() != "sess-1" || !ready.GetRunLocal() ||
		ready.GetTicket() != "tk-1" || ready.GetSelector() != "owner=brent,machine=m1", "got %+v, want the seam's ready message forwarded unchanged", ready)

	select {
	case <-ctxDone:
		t.Fatal("Open's ctx ended before the stream did")
	default:
	}

	cancel()

	select {
	case <-ctxDone:
	case <-time.After(5 * time.Second):
		t.Fatal("stream end never reached Open's ctx; the eviction trigger never fires")
	}
}

// TestExecutorSessionStreamEndsWhenServerStops pins the daemon-shutdown exit:
// http.Server.Shutdown does not cancel handler contexts, so without a stop
// signal the parked handler holds daemon shutdown for the stream's remaining
// lifetime. Server.Stop() must end the live stream promptly and idempotently,
// and the eviction trigger must still fire afterward: net/http cancels the
// request's context once the handler returns, which is what the backend
// watches.
func TestExecutorSessionStreamEndsWhenServerStops(t *testing.T) {
	c := assert.NewAborting(t)
	ctxDone := make(chan struct{})
	seam := &fakeExecSessions{
		ready: &rafikiv1.ExecutorSessionReady{
			ExecutorId: "sess-1",
			RunLocal:   true,
			Ticket:     "tk-1",
			Selector:   "owner=brent,machine=m1",
		},
		ctxDone: ctxDone,
	}

	// Own setup rather than setupExecSessionServer: the test needs the Server
	// itself, to call Stop() the way the daemon's shutdown path will.
	s := connectapi.NewServer(nil)
	s.SetExecutorSessions(seam)
	path, h := s.Routes()
	mux := http.NewServeMux()
	mux.Handle(path, h)
	srv := httptest.NewUnstartedServer(mux)
	srv.EnableHTTP2 = true
	srv.StartTLS()
	t.Cleanup(srv.Close)
	client := rafikiv1connect.NewControlClient(srv.Client(), srv.URL)

	streamCtx, cancelStream := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelStream()

	stream, err := client.ExecutorSession(streamCtx, connect.NewRequest(&rafikiv1.ExecutorSessionRequest{Name: "m1"}))
	c.NoError(err, "ExecutorSession")
	if !stream.Receive() {
		t.Fatalf("expected the ready message, got err: %v", stream.Err())
	}

	s.Stop()
	s.Stop() // a second call must be a no-op, not a close of a closed channel

	// The handler must return promptly: the stream ends cleanly, not with an
	// error and not by the client's own timeout.
	ended := make(chan struct{})
	go func() {
		for stream.Receive() {
		}
		close(ended)
	}()
	select {
	case <-ended:
	case <-time.After(5 * time.Second):
		t.Fatal("the ExecutorSession stream did not end after Server.Stop(); " +
			"a live stream holds daemon shutdown")
	}
	c.NoError(stream.Err(), "expected a clean stream end after Stop(), got")

	// The eviction trigger still fires: the handler returned, so net/http
	// cancels the request ctx and the backend releases the executor.
	select {
	case <-ctxDone:
	case <-time.After(5 * time.Second):
		t.Fatal("Open's ctx never ended after the Stop-ended stream returned; " +
			"the executor would never be released")
	}
}

func TestExecutorSessionUnavailableWhenNotWired(t *testing.T) {
	c := assert.NewAborting(t)
	client := setupExecSessionServer(t, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	stream, err := client.ExecutorSession(ctx, connect.NewRequest(&rafikiv1.ExecutorSessionRequest{Name: "m1"}))
	c.NoError(err, "ExecutorSession")
	c.False(stream.Receive(), "expected no message with no seam wired")
	c.Eq(connect.CodeUnavailable, connect.CodeOf(stream.Err()), "code")
}

// TestExecutorSessionOpenControllerErrorKeepsItsCode mirrors Close's
// connectapi.ControllerError passthrough: the code the daemon attached at the source IS
// the classification, so a missing-name refusal reads as InvalidArgument with
// its authored text, not Internal.
func TestExecutorSessionOpenControllerErrorKeepsItsCode(t *testing.T) {
	c := assert.NewAborting(t)
	seam := &fakeExecSessions{err: &connectapi.ControllerError{
		Code:    protocol.ErrInvalidArgs,
		Message: "this machine has no executor name",
	}}
	client := setupExecSessionServer(t, seam)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	stream, err := client.ExecutorSession(ctx, connect.NewRequest(&rafikiv1.ExecutorSessionRequest{}))
	c.NoError(err, "ExecutorSession")
	c.False(stream.Receive(), "expected no message on error")
	c.Eq(connect.CodeInvalidArgument, connect.CodeOf(stream.Err()), "code")
	c.StrContains(stream.Err().Error(), "this machine has no executor name", "err = %v, want the daemon's authored text", stream.Err())
}

// TestExecutorSessionOpenGenericErrorBecomesInternal is the redaction half of
// the same discipline: an error the daemon did not author (not a
// connectapi.ControllerError) is infrastructure text and must not reach the caller.
func TestExecutorSessionOpenGenericErrorBecomesInternal(t *testing.T) {
	c := assert.NewAborting(t)
	seam := &fakeExecSessions{err: errors.New("dial postgres: connection refused")}
	client := setupExecSessionServer(t, seam)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	stream, err := client.ExecutorSession(ctx, connect.NewRequest(&rafikiv1.ExecutorSessionRequest{Name: "m1"}))
	c.NoError(err, "ExecutorSession")
	c.False(stream.Receive(), "expected no message on error")
	c.Eq(connect.CodeInternal, connect.CodeOf(stream.Err()), "code")
	c.NotStrContains(stream.Err().Error(), "postgres", "err = %v, want the raw cause redacted", stream.Err())
}
