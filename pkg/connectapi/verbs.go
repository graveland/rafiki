// SPDX-License-Identifier: Apache-2.0

package connectapi

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/reflect/protoreflect"

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

// listFilterFromWire maps the wire request's non-status filter fields onto
// protocol.ListFilter, the same shape the framed ctrl_list verb applies
// through Controller.List. Status is left zero: the wire's Statuses is a
// plural OR-match with no framed equivalent, and stays a separate parameter
// (see ChildLister.ListChildren and ChildScope.Subtree) rather than folding
// into ListFilter's singular Status field.
func listFilterFromWire(req *rafikiv1.ListChildrenRequest) protocol.ListFilter {
	return protocol.ListFilter{
		Name:         req.GetName(),
		NameContains: req.GetNameContains(),
		CwdContains:  req.GetCwdContains(),
		Since:        req.GetSince(),
		Labels:       req.GetLabels(),
		HasLabel:     req.GetHasLabel(),
	}
}

// matchesChildFilter reports whether c satisfies filter's non-status fields —
// name/name_contains/cwd_contains/since/labels/has_label — with the same
// semantics protocol.ListFilter carries on the framed plane. Applied to both
// ListChildren branches as a post-filter over the already-mapped summaries:
// ChildLister.ListChildren and ChildScope.Subtree take only a status list
// (unchanged interfaces both cmd/rafikid and pkg/connectapi's tests already
// implement), so there is no lower-level function to route these fields
// through instead.
func matchesChildFilter(c protocol.ChildSummary, f protocol.ListFilter) bool {
	if f.Name != "" && c.Name != f.Name {
		return false
	}
	if f.NameContains != "" && !strings.Contains(c.Name, f.NameContains) {
		return false
	}
	if f.CwdContains != "" && !strings.Contains(c.Cwd, f.CwdContains) {
		return false
	}
	if f.Since > 0 && c.StartedAt < f.Since {
		return false
	}
	for k, v := range f.Labels {
		if c.Labels[k] != v {
			return false
		}
	}
	for _, k := range f.HasLabel {
		if _, ok := c.Labels[k]; !ok {
			return false
		}
	}
	return true
}

// ListChildren returns the daemon's children, filtered by status and, for the
// operator path, by name/name_contains/cwd_contains/since/labels/has_label —
// the framed ctrl_list filter set (protocol.ListFilter).
//
// childScoped: a per-child credential gets ONLY its own subtree, resolved by
// the wired ChildScope (the same descendants controllerSpawner.List answers),
// never the operator's fleet view. The subtree is narrowed further by the same
// filter fields; it is never widened by them.
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
	filter := listFilterFromWire(req.Msg)
	if sc := s.childScope(ctx); sc != nil {
		summaries := sc.Subtree(req.Msg.GetStatuses())
		out := make([]*rafikiv1.ChildSummary, 0, len(summaries))
		for _, c := range summaries {
			if !matchesChildFilter(c, filter) {
				continue
			}
			out = append(out, toProtoChild(c, elog, ctx))
		}
		return connect.NewResponse(&rafikiv1.ListChildrenResponse{Children: out}), nil
	}
	summaries := (*p).ListChildren(req.Msg.GetStatuses())
	out := make([]*rafikiv1.ChildSummary, 0, len(summaries))
	for _, c := range summaries {
		if !matchesChildFilter(c, filter) {
			continue
		}
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

// childAllowedSpawnFields is the set of SpawnRequest field numbers a caller
// with child provenance may set (fields 1-14, the child-reachable shape of
// SpawnRequest that predates the Wave 1 OPERATOR-ONLY range). Any other set
// field — including one added in the future — is refused to a child
// credential by default: firstOperatorOnlySet fails CLOSED on everything not
// in this set, rather than open on a literal operator-only list that a new
// field could silently miss. See control.proto:310 and
// TestSpawnRequestFieldsAreClassified, which fails the build the day a new
// SpawnRequest field lands unclassified.
var childAllowedSpawnFields = map[protoreflect.FieldNumber]bool{
	1: true, 2: true, 3: true, 4: true, 5: true, 6: true, 7: true,
	8: true, 9: true, 10: true, 11: true, 12: true, 13: true, 14: true,
}

// firstOperatorOnlySet returns the wire name of the lowest-numbered field req
// has set that is NOT in childAllowedSpawnFields, or "" if every set field is
// child-allowed. Has() supplies the "non-zero" test directly: proto3 fields
// without the "optional" keyword report true only for a non-default scalar
// value and for a repeated/map field only when non-empty — exactly the
// non-empty-string/slice/map, true-bool rule Spawn's child refusal applies.
// The scan walks every field the descriptor knows about (not a numeric
// range), so it also catches a field number the current build doesn't
// recognize as operator-only but that the caller still managed to set.
func firstOperatorOnlySet(req *rafikiv1.SpawnRequest) string {
	m := req.ProtoReflect()
	fields := m.Descriptor().Fields()
	var lowest protoreflect.FieldDescriptor
	for i := 0; i < fields.Len(); i++ {
		fd := fields.Get(i)
		if childAllowedSpawnFields[fd.Number()] {
			continue
		}
		if !m.Has(fd) {
			continue
		}
		if lowest == nil || fd.Number() < lowest.Number() {
			lowest = fd
		}
	}
	if lowest == nil {
		return ""
	}
	return string(lowest.Name())
}

// Spawn creates a child. The budget pointers are copied as pointers, never
// dereferenced into values, so "unset" survives the trip to the daemon.
//
// Security boundary, checked before anything else runs: a caller with child
// provenance may not set any field outside childAllowedSpawnFields (today
// that's fields 15-29 — see firstOperatorOnlySet). This is a
// privilege-escalation guard, not a convenience default, so it runs ahead of
// the cwd check, the lifecycle-wired check, and every other validation — a
// child credential must never learn anything about the daemon's state from a
// request the security boundary alone should have refused.
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
	sc := s.childScope(ctx)
	if sc != nil {
		if field := firstOperatorOnlySet(req.Msg); field != "" {
			return nil, connect.NewError(connect.CodePermissionDenied,
				fmt.Errorf("%s is operator-only: a child credential may not set it", field))
		}
	}
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
	if sc != nil {
		if sc.ChildID() == "" {
			// A per-child credential that names no child must not spawn — and
			// MUST NOT fall through to the forced-empty-parent path below:
			// ParentChildID "" is the top-level spawn shape, so an empty forced
			// parent would grant an operator power to the one caller least
			// entitled to it.
			return nil, refuseChildScope(errors.New(
				"spawn requires a child id your credential does not name"))
		}
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
		Script:           scriptFromProto(m.GetScript()),
		ParentChildID:    m.GetParentChildId(),
		ExecutorSelector: m.GetExecutorSelector(),
		ExecutorRef:      m.GetExecutorRef(),
		Labels:           m.GetLabels(),

		ConfigDir:          m.GetConfigDir(),
		AppendSystemPrompt: m.GetAppendSystemPrompt(),
		Thinking:           m.GetThinking(),
		NoSession:          m.GetNoSession(),
		ResumeSession:      m.GetResumeSession(),
		ForkSession:        m.GetForkSession(),
		Extensions:         m.GetExtensions(),
		NoExtensions:       m.GetNoExtensions(),
		Verbose:            m.GetVerbose(),
		ExtraArgs:          m.GetExtraArgs(),
		SkillsDirs:         m.GetSkillsDirs(),
		MCPConfig:          m.GetMcpConfig(),
		Env:                m.GetEnv(),
		RecordRequests:     m.GetRecordRequests(),
		PassthroughAuth:    m.GetPassthroughAuth(),
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

// scriptFromProto maps the wire ScriptSpec onto the domain type, nil when the
// request carries none. Validation happens in the controller; this layer only
// carries the spec.
func scriptFromProto(m *rafikiv1.SpawnRequest_ScriptSpec) *protocol.ScriptSpec {
	if m == nil {
		return nil
	}
	return &protocol.ScriptSpec{
		Repo:    m.GetRepo(),
		Script:  m.GetScript(),
		Modules: m.GetModules(),
		Args:    m.GetArgs(),
	}
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
