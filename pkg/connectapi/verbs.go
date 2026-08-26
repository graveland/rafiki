// SPDX-License-Identifier: Apache-2.0

package connectapi

import (
	"context"
	"errors"
	"fmt"
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
	inboxP := s.inbox.Load()
	if inboxP == nil {
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

	id, err := (*inboxP).Accept(ctx, inbox.Inbound{
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

// ListChildren returns the daemon's children, optionally filtered by status.
func (s *Server) ListChildren(
	ctx context.Context,
	req *connect.Request[rafikiv1.ListChildrenRequest],
) (*connect.Response[rafikiv1.ListChildrenResponse], error) {
	p := s.children.Load()
	if p == nil {
		return nil, connect.NewError(connect.CodeUnavailable,
			errors.New("child lister not yet wired"))
	}
	elog := s.eventLog()
	summaries := (*p).ListChildren(req.Msg.GetStatuses())
	out := make([]*rafikiv1.ChildSummary, 0, len(summaries))
	for _, c := range summaries {
		out = append(out, toProtoChild(c, elog, ctx))
	}
	return connect.NewResponse(&rafikiv1.ListChildrenResponse{Children: out}), nil
}

// GetChild returns one child by id.
func (s *Server) GetChild(
	ctx context.Context,
	req *connect.Request[rafikiv1.GetChildRequest],
) (*connect.Response[rafikiv1.GetChildResponse], error) {
	childID := req.Msg.GetChildId()
	if childID == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument,
			errors.New("child_id is required"))
	}
	p := s.children.Load()
	if p == nil {
		return nil, connect.NewError(connect.CodeUnavailable,
			errors.New("child lister not yet wired"))
	}
	summary, ok := (*p).GetChild(childID)
	if !ok {
		return nil, connect.NewError(connect.CodeNotFound,
			fmt.Errorf("no such child %q", childID))
	}
	elog := s.eventLog()
	return connect.NewResponse(&rafikiv1.GetChildResponse{Child: toProtoChild(summary, elog, ctx)}), nil
}

// Spawn creates a child. The budget pointers are copied as pointers, never
// dereferenced into values, so "unset" survives the trip to the daemon.
func (s *Server) Spawn(
	ctx context.Context,
	req *connect.Request[rafikiv1.SpawnRequest],
) (*connect.Response[rafikiv1.SpawnResponse], error) {
	if req.Msg.GetCwd() == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument,
			errors.New("cwd is required"))
	}
	p := s.lifecycle.Load()
	if p == nil {
		return nil, connect.NewError(connect.CodeUnavailable,
			errors.New("child lifecycle not yet wired"))
	}

	sp := connectapiSpawnParams(req.Msg)
	id, err := (*p).Spawn(ctx, sp)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&rafikiv1.SpawnResponse{ChildId: id}), nil
}

// connectapiSpawnParams maps the wire request onto SpawnParams, preserving
// pointer-ness on the three budgets.
func connectapiSpawnParams(m *rafikiv1.SpawnRequest) SpawnParams {
	p := SpawnParams{
		Cwd:              m.GetCwd(),
		Name:             m.GetName(),
		Model:            m.GetModel(),
		Kind:             m.GetKind(),
		ParentChildID:    m.GetParentChildId(),
		ExecutorSelector: m.GetExecutorSelector(),
		Labels:           m.GetLabels(),
	}
	if m.MaxDepth != nil {
		v := int(*m.MaxDepth)
		p.MaxDepth = &v
	}
	if m.MaxCost != nil {
		v := *m.MaxCost
		p.MaxCost = &v
	}
	if m.MaxChildren != nil {
		v := int(*m.MaxChildren)
		p.MaxChildren = &v
	}
	return p
}

// Kill ends a child and reports the status it settled on.
func (s *Server) Kill(
	ctx context.Context,
	req *connect.Request[rafikiv1.KillRequest],
) (*connect.Response[rafikiv1.KillResponse], error) {
	childID := req.Msg.GetChildId()
	if childID == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument,
			errors.New("child_id is required"))
	}
	p := s.lifecycle.Load()
	if p == nil {
		return nil, connect.NewError(connect.CodeUnavailable,
			errors.New("child lifecycle not yet wired"))
	}
	out, err := (*p).Kill(ctx, childID,
		req.Msg.GetShutdownTimeoutMs(), req.Msg.GetKillTimeoutMs())
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	resp := &rafikiv1.KillResponse{
		ChildId:    childID,
		Signal:     out.Signal,
		DurationMs: out.DurationMs,
		Escalated:  out.Escalated,
	}
	if out.ExitCode != nil {
		code := int32(*out.ExitCode)
		resp.ExitCode = &code
	}
	return connect.NewResponse(resp), nil
}
