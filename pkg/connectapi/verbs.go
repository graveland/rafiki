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
	"go.graveland.dev/rafiki/pkg/protocol"
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
	// childScoped: a per-child credential may steer only its own subtree, and
	// the check runs before the message is accepted anywhere.
	if sc := s.childScope(ctx); sc != nil {
		if err := sc.Authorize(childID); err != nil {
			return nil, err
		}
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
	var attachments []inbox.Attachment
	if mode != inbox.ModeAbort {
		var err error
		text, attachments, err = contentFromBlocks(req.Msg.GetBlocks())
		if err != nil {
			return nil, err
		}
	}

	id, err := (*inboxP).Accept(ctx, inbox.Inbound{
		ChildID:     childID,
		Mode:        mode,
		Text:        text,
		Attachments: attachments,
	})
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&rafikiv1.SendResponse{MessageId: id}), nil
}

// contentFromBlocks splits content blocks into the text and the attachments the
// inbox carries separately.
//
// A block type with nowhere to go is still REFUSED rather than skipped, which
// is what the text-only version of this did for every non-text block: silently
// dropping a payload looks to the sender exactly like delivering it.
func contentFromBlocks(blocks []*rafikiv1.ContentBlock) (string, []inbox.Attachment, error) {
	var sb strings.Builder
	var atts []inbox.Attachment
	for _, b := range blocks {
		switch v := b.Block.(type) {
		case *rafikiv1.ContentBlock_Text:
			sb.WriteString(v.Text.GetText())
		case *rafikiv1.ContentBlock_Image:
			if len(v.Image.GetData()) == 0 {
				return "", nil, connect.NewError(connect.CodeInvalidArgument,
					errors.New("image block carries no data"))
			}
			atts = append(atts, inbox.Attachment{
				MediaType: v.Image.GetMediaType(),
				Data:      v.Image.GetData(),
			})
		default:
			return "", nil, connect.NewError(connect.CodeUnimplemented,
				fmt.Errorf("Send does not carry %T blocks", b.Block))
		}
	}
	return sb.String(), atts, nil
}

// ListChildren returns the daemon's children, optionally filtered by status.
//
// childScoped: a per-child credential gets ONLY its own subtree, resolved by
// the wired ChildScope (the same descendants controllerSpawner.List answers),
// never the operator's fleet view.
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
	if sc := s.childScope(ctx); sc != nil {
		summaries := sc.Subtree(req.Msg.GetStatuses())
		out := make([]*rafikiv1.ChildSummary, 0, len(summaries))
		for _, c := range summaries {
			out = append(out, toProtoChild(c, elog, ctx))
		}
		return connect.NewResponse(&rafikiv1.ListChildrenResponse{Children: out}), nil
	}
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
	// childScoped: the subtree boundary runs before the lookup, so a caller
	// cannot even confirm a sibling exists.
	if sc := s.childScope(ctx); sc != nil {
		if err := sc.Authorize(childID); err != nil {
			return nil, err
		}
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
//
// childScoped: a per-child credential spawns into its OWN position —
// ParentChildID is forced to the caller's child id, overwriting whatever the
// wire carried, which is the existing child-spawn admission (depth, budget
// and children checks read the PARENT's grant through that field, the same
// admission fundi's agent_spawn gets). The owner stamp then comes from the
// same source fundi's spawner uses — the caller's own stored row — resolved
// in cmd/rafikid's lifecycle adapter from the credential.
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
	if sc := s.childScope(ctx); sc != nil {
		sp.ParentChildID = sc.ChildID()
	}
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
		Preset:           m.GetPreset(),
		Prefill:          prefillFromProto(m.GetPrefill()),
		ParentChildID:    m.GetParentChildId(),
		ExecutorSelector: m.GetExecutorSelector(),
		ExecutorRef:      m.GetExecutorRef(),
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

// prefillFromProto maps the wire pre-fill onto protocol.PrefillRead entries,
// nil when the request carries none. Validation happens in the controller;
// this layer only carries the list, so it duplicates no prefill.Validate rule.
func prefillFromProto(rows []*rafikiv1.PrefillRead) []protocol.PrefillRead {
	if len(rows) == 0 {
		return nil
	}
	out := make([]protocol.PrefillRead, 0, len(rows))
	for _, r := range rows {
		out = append(out, protocol.PrefillRead{
			Path:  r.GetPath(),
			Start: int(r.GetStart()),
			End:   int(r.GetEnd()),
		})
	}
	return out
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
	// childScoped: the subtree boundary runs before the kill is attempted.
	if sc := s.childScope(ctx); sc != nil {
		if err := sc.Authorize(childID); err != nil {
			return nil, err
		}
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
