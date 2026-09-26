// SPDX-License-Identifier: Apache-2.0

package connectapi

import (
	"context"
	"errors"
	"log/slog"

	"connectrpc.com/connect"

	"go.graveland.dev/rafiki/pkg/control"
	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
)

// ExecutorSessions mints (or finds) the caller's own session executor — the
// face of the framed ctrl_executor_session verb. The framed verb's
// connection-scoped signature (a credential minted for one control
// connection, revoked when it closes) is what kept the framed listener
// alive; here the session rides a server-streaming RPC instead, so the
// stream's lifetime IS the connection's.
type ExecutorSessions interface {
	// Open mints (or finds) the caller's session executor. The transient
	// executor lives until ctx ends.
	Open(ctx context.Context, req *rafikiv1.ExecutorSessionRequest) (*rafikiv1.ExecutorSessionReady, error)
}

// SetExecutorSessions attaches the executor-session backend.
// Post-construction setter for the same reason as SetSkillManager: the
// Controller is built after this Server. A nil backend is refused rather
// than stored, the same rule as SetSkillManager: storing &e for a nil
// interface would defeat the handler's Unavailable path and nil-panic the
// first handler call.
func (s *Server) SetExecutorSessions(e ExecutorSessions) {
	if e == nil {
		return
	}
	s.execSessions.Store(&e)
}

// ExecutorSession serves the framed ctrl_executor_session face: the first
// streamed message is ready. The stream stays open for the session's
// lifetime; the daemon evicts a transient executor when the stream ends.
//
// Open is called with the STREAM's context, not req's: it is what the
// backend watches to know the session has ended, and it outlives the initial
// call. Once ready is sent, the handler blocks until the stream ends — from
// the client's side (disconnect or cancellation, which is the eviction
// trigger) or from the daemon's (Server.Stop during shutdown, which must not
// be held up by a live stream). Open's caller (the backend) is what actually
// releases the executor; net/http cancels a request's context once its
// handler returns, so a Stop-ended stream still reaches the backend's ctx
// watcher.
func (s *Server) ExecutorSession(
	ctx context.Context,
	req *connect.Request[rafikiv1.ExecutorSessionRequest],
	stream *connect.ServerStream[rafikiv1.ExecutorSessionEvent],
) error {
	p := s.execSessions.Load()
	if p == nil {
		return connect.NewError(connect.CodeUnavailable,
			errors.New("executor sessions not yet wired"))
	}
	ready, err := (*p).Open(ctx, req.Msg)
	if err != nil {
		var ce *control.ControllerError
		if !errors.As(err, &ce) {
			// ConnectErr redacts this below; log the cause here or lose it.
			slog.Error("connect: executor session open failed", "error", err)
		}
		return ConnectErr(err)
	}
	if err := stream.Send(&rafikiv1.ExecutorSessionEvent{
		Event: &rafikiv1.ExecutorSessionEvent_Ready{Ready: ready},
	}); err != nil {
		return err
	}
	// A nil stopped channel (a zero-value Server; NewServer always creates
	// it) blocks forever in a select, which is exactly the client-end-only
	// behavior, so a zero value needs no special case.
	select {
	case <-ctx.Done():
	case <-s.stopped:
	}
	return nil
}
