// SPDX-License-Identifier: Apache-2.0

package connectapi

import (
	"context"
	"errors"
	"strings"

	"connectrpc.com/connect"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/inbox"
)

// Send submits one message to a child. It routes through the Inbox seam rather
// than calling the controller directly, so the durable queue described in the
// Phase B design §5 can replace the in-memory implementation without touching
// this handler.
func (s *Server) Send(
	ctx context.Context,
	req *connect.Request[rafikiv1.SendRequest],
) (*connect.Response[rafikiv1.SendResponse], error) {
	childID := req.Msg.GetChildId()
	if childID == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument,
			errors.New("child_id is required"))
	}
	if s.inbox == nil {
		return nil, connect.NewError(connect.CodeUnavailable,
			errors.New("inbox not yet wired"))
	}

	var mode inbox.Mode
	switch req.Msg.GetMode() {
	case rafikiv1.SendMode_SEND_MODE_PROMPT:
		mode = inbox.ModePrompt
	case rafikiv1.SendMode_SEND_MODE_STEER:
		mode = inbox.ModeSteer
	case rafikiv1.SendMode_SEND_MODE_ABORT:
		mode = inbox.ModeAbort
	default:
		return nil, connect.NewError(connect.CodeInvalidArgument,
			errors.New("mode must be PROMPT, STEER or ABORT"))
	}

	var text string
	if mode != inbox.ModeAbort {
		var err error
		text, err = textFromBlocks(req.Msg.GetBlocks())
		if err != nil {
			return nil, err
		}
	}

	id, err := s.inbox.Accept(ctx, inbox.Inbound{
		ChildID: childID,
		Mode:    mode,
		Text:    text,
	})
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&rafikiv1.SendResponse{MessageId: id}), nil
}

// textFromBlocks flattens content blocks to the plain string the engine's
// HandlePrompt/HandleSteer accept. A non-text block is REFUSED rather than
// skipped: silently dropping a pasted image would look to the sender like it
// was delivered.
func textFromBlocks(blocks []*rafikiv1.ContentBlock) (string, error) {
	var sb strings.Builder
	for _, b := range blocks {
		t := b.GetText()
		if t == nil {
			return "", connect.NewError(connect.CodeUnimplemented,
				errors.New("only text blocks are supported by Send today"))
		}
		sb.WriteString(t.GetText())
	}
	return sb.String(), nil
}

// The four verbs below are stubs so *Server keeps satisfying the generated
// ControlHandler interface between plan tasks. Tasks 6-8 replace each one.

func (s *Server) ListChildren(context.Context, *connect.Request[rafikiv1.ListChildrenRequest]) (*connect.Response[rafikiv1.ListChildrenResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, errors.New("ListChildren not implemented"))
}

func (s *Server) GetChild(context.Context, *connect.Request[rafikiv1.GetChildRequest]) (*connect.Response[rafikiv1.GetChildResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, errors.New("GetChild not implemented"))
}

func (s *Server) Spawn(context.Context, *connect.Request[rafikiv1.SpawnRequest]) (*connect.Response[rafikiv1.SpawnResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, errors.New("Spawn not implemented"))
}

func (s *Server) Kill(context.Context, *connect.Request[rafikiv1.KillRequest]) (*connect.Response[rafikiv1.KillResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, errors.New("Kill not implemented"))
}
