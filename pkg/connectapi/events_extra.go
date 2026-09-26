// SPDX-License-Identifier: Apache-2.0

package connectapi

import (
	"context"
	"encoding/json"
	"errors"

	"connectrpc.com/connect"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
)

// RawChildIO is the debug/operator slice of the daemon behind the framed
// ctrl_get_streams and ctrl_send verbs: raw, uninterpreted access to a live
// child's stdin/stderr capture and stdin pipe. Deliberately raw — no
// event model, no parsing — because these are the faces a human debugging a
// wedged child uses, not the ones an agent talks through (children use Send).
type RawChildIO interface {
	GetStreams(ctx context.Context, childID, which string) (*rafikiv1.GetStreamsResponse, error)
	SendFrame(ctx context.Context, childID string, frame json.RawMessage) error
}

// SetRawChildIO attaches the raw child-I/O backend. Post-construction setter
// for the same reason as SetSkillManager: the Controller is built after this
// Server. A nil backend is refused rather than stored, the same rule as
// SetSkillManager: storing &r for a nil interface would defeat the handler's
// Unavailable path and nil-panic the first handler call.
func (s *Server) SetRawChildIO(r RawChildIO) {
	if r == nil {
		return
	}
	s.rawIO.Store(&r)
}

// GetStreams serves the framed ctrl_get_streams face: a live child's raw
// stdin/stderr capture.
func (s *Server) GetStreams(
	ctx context.Context,
	req *connect.Request[rafikiv1.GetStreamsRequest],
) (*connect.Response[rafikiv1.GetStreamsResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, errors.New("GetStreams: not yet implemented"))
}

// SendFrame serves the framed ctrl_send face: a raw child-protocol frame,
// for debugging/scripting. userOnly — children use Send.
func (s *Server) SendFrame(
	ctx context.Context,
	req *connect.Request[rafikiv1.SendFrameRequest],
) (*connect.Response[rafikiv1.SendFrameResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, errors.New("SendFrame: not yet implemented"))
}
