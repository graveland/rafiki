// SPDX-License-Identifier: Apache-2.0

package connectapi

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"

	"connectrpc.com/connect"

	"go.graveland.dev/rafiki/pkg/control"
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

// rawIOOp returns the wired RawChildIO backend. It fails closed with
// CodeUnavailable until the daemon attaches it — the Controller is built
// after this Server (see SetRawChildIO), so a request that arrives during
// that window must report "not ready", not panic or hang.
func (s *Server) rawIOOp() (RawChildIO, error) {
	p := s.rawIO.Load()
	if p == nil {
		return nil, connect.NewError(connect.CodeUnavailable,
			errors.New("raw child io not yet wired"))
	}
	return *p, nil
}

// mapRawChildIOErr is this file's shared error mapping, the same shape as
// childops.go's mapChildOpsErr:
//
//   - An already-coded *connect.Error passes through untouched — ConnectErr
//     would re-wrap it as internal.
//   - A *control.ControllerError keeps its authored message under its
//     protocol code (ConnectErr) — the code the daemon attached at the source
//     IS the classification, the same decision the framed mapErr makes.
//   - Anything else is infrastructure text ConnectErr redacts; its cause is
//     logged here first or it is lost (the same discipline as close.go).
func mapRawChildIOErr(err error, logMsg string, logArgs ...any) error {
	var cerr *connect.Error
	if errors.As(err, &cerr) {
		return err
	}
	var ce *control.ControllerError
	if !errors.As(err, &ce) {
		// ConnectErr redacts this below; log the cause here or lose it.
		slog.Error(logMsg, append(logArgs, "error", err)...)
	}
	return ConnectErr(err)
}

// GetStreams serves the framed ctrl_get_streams face: a live child's raw
// stdin/stderr capture. which is one of "in", "err", "all" (empty means
// "all"); the framed dispatcher validated the same set, and an unlisted value
// stays CodeInvalidArgument. alive=false in the response means the child has
// already exited — the caller falls back to the on-disk dump.
func (s *Server) GetStreams(
	ctx context.Context,
	req *connect.Request[rafikiv1.GetStreamsRequest],
) (*connect.Response[rafikiv1.GetStreamsResponse], error) {
	childID := req.Msg.GetChildId()
	if childID == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument,
			errors.New("child_id is required"))
	}
	switch req.Msg.GetWhich() {
	case "", "in", "err", "all":
	default:
		return nil, connect.NewError(connect.CodeInvalidArgument,
			errors.New(`which must be one of "in", "err", "all"`))
	}
	r, err := s.rawIOOp()
	if err != nil {
		return nil, err
	}
	resp, err := r.GetStreams(ctx, childID, req.Msg.GetWhich())
	if err != nil {
		return nil, mapRawChildIOErr(err, "connect: get_streams failed", "child_id", childID)
	}
	return connect.NewResponse(resp), nil
}

// SendFrame serves the framed ctrl_send face: a raw child-protocol frame,
// for debugging/scripting. userOnly — children use Send. frame_json is
// forwarded verbatim after one syntactic gate: it must parse as a JSON
// OBJECT. The framed dispatcher required a frame at all and delegated the
// rest to Controller.Send; here the request carries the frame as a string, so
// the object check is what stands between a client typo and the child's
// stdin (a JSON array or bare scalar is not a child protocol frame).
func (s *Server) SendFrame(
	ctx context.Context,
	req *connect.Request[rafikiv1.SendFrameRequest],
) (*connect.Response[rafikiv1.SendFrameResponse], error) {
	childID := req.Msg.GetChildId()
	if childID == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument,
			errors.New("child_id is required"))
	}
	frame := json.RawMessage(req.Msg.GetFrameJson())
	if len(frame) == 0 {
		return nil, connect.NewError(connect.CodeInvalidArgument,
			errors.New("frame_json is required"))
	}
	if err := rawFrameIsObject(frame); err != nil {
		return nil, err
	}
	r, err := s.rawIOOp()
	if err != nil {
		return nil, err
	}
	if err := r.SendFrame(ctx, childID, frame); err != nil {
		return nil, mapRawChildIOErr(err, "connect: send_frame failed", "child_id", childID)
	}
	return connect.NewResponse(&rafikiv1.SendFrameResponse{}), nil
}

// rawFrameIsObject refuses any frame_json that is not a JSON object, the one
// shape every child-protocol frame takes on the wire.
func rawFrameIsObject(frame json.RawMessage) error {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(frame, &obj); err != nil || obj == nil {
		return connect.NewError(connect.CodeInvalidArgument,
			errors.New("frame_json is not a JSON object"))
	}
	return nil
}
