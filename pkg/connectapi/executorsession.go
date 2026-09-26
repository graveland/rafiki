// SPDX-License-Identifier: Apache-2.0

package connectapi

import (
	"context"
	"errors"

	"connectrpc.com/connect"

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
func (s *Server) ExecutorSession(
	ctx context.Context,
	req *connect.Request[rafikiv1.ExecutorSessionRequest],
	stream *connect.ServerStream[rafikiv1.ExecutorSessionEvent],
) error {
	return connect.NewError(connect.CodeUnimplemented, errors.New("ExecutorSession: not yet implemented"))
}
