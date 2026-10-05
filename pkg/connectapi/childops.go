// SPDX-License-Identifier: Apache-2.0

package connectapi

import (
	"context"
	"errors"
	"log/slog"

	"connectrpc.com/connect"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
)

// ChildOps is the operator-side slice of the daemon behind the Resume,
// CloseAllExited, SetLabels, Status, Search, ShutdownDaemon, ModelInfo and
// ConversationStats RPCs. It is a seam because the daemon's Controller
// — the only thing that can answer any of it — is built after the proxy face
// that owns this Server, so it attaches post-construction, and because
// depending on the interface keeps the handler testable without a database.
type ChildOps interface {
	Resume(ctx context.Context, childID, apiKey string) (string, error)
	CloseAllExited(ctx context.Context, olderThanMs int64) ([]string, error)
	SetLabels(ctx context.Context, childID string, set map[string]string, remove []string) (map[string]string, error)
	Status(ctx context.Context) (*rafikiv1.StatusResponse, error)
	Search(ctx context.Context, req *rafikiv1.SearchRequest) (*rafikiv1.SearchResponse, error)
	ShutdownDaemon(ctx context.Context) error
	ModelInfo(ctx context.Context, model string) (*rafikiv1.ModelInfoResponse, error)
	ConversationStats(ctx context.Context, req *rafikiv1.ConversationStatsRequest) (string, error) // JSON
}

// SetChildOps attaches the child-operator backend. Post-construction setter
// for the same reason as SetSkillManager: the Controller is built after this
// Server. A nil backend is refused rather than stored, the same rule as
// SetSkillManager: storing &o for a nil interface would defeat the handler's
// Unavailable path and nil-panic the first handler call.
func (s *Server) SetChildOps(o ChildOps) {
	if o == nil {
		return
	}
	s.childOps.Store(&o)
}

// childOp returns the wired ChildOps backend. It fails closed with
// CodeUnavailable until the daemon attaches it — the Controller is built
// after this Server (see SetChildOps), so a request that arrives during that
// window must report "not ready", not panic or hang.
func (s *Server) childOp() (ChildOps, error) {
	p := s.childOps.Load()
	if p == nil {
		return nil, connect.NewError(connect.CodeUnavailable,
			errors.New("child ops not yet wired"))
	}
	return *p, nil
}

// mapChildOpsErr is childops.go's shared error mapping. It fails in the same
// three ways every handler in this file can:
//
//   - An already-coded *connect.Error passes through untouched — the
//     ConversationStats adapter's scopeFor refusal arrives as
//     CodePermissionDenied, and ConnectErr would re-wrap it as internal.
//   - A *ControllerError keeps its authored message under its
//     protocol code (ConnectErr) — the code the daemon attached at the source
//     IS the classification.
//   - Anything else is infrastructure text ConnectErr redacts; its cause is
//     logged here first or it is lost (the same discipline as close.go).
func mapChildOpsErr(err error, logMsg string, logArgs ...any) error {
	var cerr *connect.Error
	if errors.As(err, &cerr) {
		return err
	}
	var ce *ControllerError
	if !errors.As(err, &ce) {
		// ConnectErr redacts this below; log the cause here or lose it.
		slog.Error(logMsg, append(logArgs, "error", err)...)
	}
	return ConnectErr(err)
}

// Resume serves the Resume RPC: re-spawn an
// exited child against its persisted state record. api_key is used at spawn
// time only and is never persisted — the adapter passes it through to the
// the daemon's own Controller.Resume.
func (s *Server) Resume(
	ctx context.Context,
	req *connect.Request[rafikiv1.ResumeRequest],
) (*connect.Response[rafikiv1.ResumeResponse], error) {
	childID := req.Msg.GetChildId()
	if childID == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument,
			errors.New("child_id is required"))
	}
	o, err := s.childOp()
	if err != nil {
		return nil, err
	}
	id, err := o.Resume(ctx, childID, req.Msg.GetApiKey())
	if err != nil {
		return nil, mapChildOpsErr(err, "connect: resume failed", "child_id", childID)
	}
	return connect.NewResponse(&rafikiv1.ResumeResponse{ChildId: id}), nil
}

// CloseAllExited serves the CloseAllExited RPC
// verb: close (forget) every exited child, optionally older than
// older_than_ms. The count is derivable from the id list, so the wire carries
// proto shape drops it as derivable from the repeated field.
func (s *Server) CloseAllExited(
	ctx context.Context,
	req *connect.Request[rafikiv1.CloseAllExitedRequest],
) (*connect.Response[rafikiv1.CloseAllExitedResponse], error) {
	o, err := s.childOp()
	if err != nil {
		return nil, err
	}
	closed, err := o.CloseAllExited(ctx, req.Msg.GetOlderThanMs())
	if err != nil {
		// A partial result must not read as a bare failure: some children WERE
		// closed before the failure, and the wire carries only the error. Record
		// the count closed here — with the error, whose text names the failure
		// count — before the generic mapping below logs the cause and redacts it.
		if len(closed) > 0 {
			slog.Warn("connect: forget_all_exited closed some children before failing",
				"closed", len(closed), "error", err)
		}
		return nil, mapChildOpsErr(err, "connect: forget_all_exited failed")
	}
	return connect.NewResponse(&rafikiv1.CloseAllExitedResponse{ChildIds: closed}), nil
}

// SetLabels serves the SetLabels RPC: set
// entries apply first, then remove entries are deleted, and the full
// post-mutation map comes back. Keys using the rafiki/ prefix are reserved
// and rejected by the Controller.
func (s *Server) SetLabels(
	ctx context.Context,
	req *connect.Request[rafikiv1.SetLabelsRequest],
) (*connect.Response[rafikiv1.SetLabelsResponse], error) {
	childID := req.Msg.GetChildId()
	if childID == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument,
			errors.New("child_id is required"))
	}
	if len(req.Msg.GetSet()) == 0 && len(req.Msg.GetRemove()) == 0 {
		return nil, connect.NewError(connect.CodeInvalidArgument,
			errors.New("at least one of set or remove is required"))
	}
	o, err := s.childOp()
	if err != nil {
		return nil, err
	}
	labels, err := o.SetLabels(ctx, childID, req.Msg.GetSet(), req.Msg.GetRemove())
	if err != nil {
		return nil, mapChildOpsErr(err, "connect: set_labels failed", "child_id", childID)
	}
	return connect.NewResponse(&rafikiv1.SetLabelsResponse{Labels: labels}), nil
}

// Status serves the Status RPC: the
// daemon's process vitals and live/exited child counts.
func (s *Server) Status(
	ctx context.Context,
	req *connect.Request[rafikiv1.StatusRequest],
) (*connect.Response[rafikiv1.StatusResponse], error) {
	o, err := s.childOp()
	if err != nil {
		return nil, err
	}
	resp, err := o.Status(ctx)
	if err != nil {
		return nil, mapChildOpsErr(err, "connect: status failed")
	}
	return connect.NewResponse(resp), nil
}

// Search serves the Search RPC: in-memory
// content search across children's session buffers. The query is the one
// required field; limit's default is the Controller's own floor, applied
// inside the adapter's delegated call, unchanged.
func (s *Server) Search(
	ctx context.Context,
	req *connect.Request[rafikiv1.SearchRequest],
) (*connect.Response[rafikiv1.SearchResponse], error) {
	if req.Msg.GetQuery() == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument,
			errors.New("query is required"))
	}
	o, err := s.childOp()
	if err != nil {
		return nil, err
	}
	resp, err := o.Search(ctx, req.Msg)
	if err != nil {
		return nil, mapChildOpsErr(err, "connect: search failed")
	}
	return connect.NewResponse(resp), nil
}

// ShutdownDaemon serves the ShutdownDaemon RPC
// verb — as a fail-closed stub this wave. The full contract (broadcast →
// drain → close listeners → exit) turns on Controller's one-way stopping
// latch: a drain fired while the daemon keeps running flips that latch (its
// contract is "the daemon is dying"), permanently suppressing child status
// persists, which makes the next daemon start auto-resume children the
// operator explicitly killed. So the daemon shutdown path lands with the
// daemon shutdown, wired into main.go's signal path — not
// from an RPC whose context dies with its response. Until then this handler
// refuses BEFORE its seam is consulted (no wiring state can reach the
// adapter), and the refusal is answered Unimplemented, not an uncoded error
// mapped through mapChildOpsErr: an unimplemented RPC is a wire fact, not an
// infrastructure failure to redact.
func (s *Server) ShutdownDaemon(
	ctx context.Context,
	req *connect.Request[rafikiv1.ShutdownDaemonRequest],
) (*connect.Response[rafikiv1.ShutdownDaemonResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented,
		errors.New("ShutdownDaemon: not yet wired to the daemon's shutdown path"))
}

// ModelInfo serves the ModelInfo RPC: the
// daemon's own catalog answer for one model, so the client never reads
// OpenRouter itself. Never an error: an unknown model and an unconfigured
// catalog are both known=false, which is what every caller degrades on.
func (s *Server) ModelInfo(
	ctx context.Context,
	req *connect.Request[rafikiv1.ModelInfoRequest],
) (*connect.Response[rafikiv1.ModelInfoResponse], error) {
	model := req.Msg.GetModel()
	if model == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument,
			errors.New("model is required"))
	}
	o, err := s.childOp()
	if err != nil {
		return nil, err
	}
	resp, err := o.ModelInfo(ctx, model)
	if err != nil {
		return nil, mapChildOpsErr(err, "connect: model_info failed", "model", model)
	}
	return connect.NewResponse(resp), nil
}

// ConversationStats serves the ConversationStats RPC: global (filtered)
// stats when conversation_id
// is empty, scoped to one conversation otherwise. The stats come back as the
// daemon's own insights.Stats JSON, opaque by design (precedent:
// ToolUse.input_json) — the caller asked for an aggregate, not a schema.
func (s *Server) ConversationStats(
	ctx context.Context,
	req *connect.Request[rafikiv1.ConversationStatsRequest],
) (*connect.Response[rafikiv1.ConversationStatsResponse], error) {
	o, err := s.childOp()
	if err != nil {
		return nil, err
	}
	statsJSON, err := o.ConversationStats(ctx, req.Msg)
	if err != nil {
		return nil, mapChildOpsErr(err, "connect: conversation_stats failed")
	}
	return connect.NewResponse(&rafikiv1.ConversationStatsResponse{StatsJson: statsJSON}), nil
}
