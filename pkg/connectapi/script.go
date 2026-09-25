// SPDX-License-Identifier: Apache-2.0

package connectapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"unicode"

	"connectrpc.com/connect"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
)

// Size caps on the script children's free-form JSON payloads. Both are well
// inside the event buffer's own per-fragment truncation
// (RAFIKI_EVENTBUF_MAX_BYTES_PER_FRAGMENT, default 64 KiB), so a report or a
// result that passed the cap here can never be truncated again downstream.
//
// The caps are REFUSALS, not truncations: a silently truncated JSON payload is
// worse than an explicit error, because the parent agent reading the fragment
// cannot tell where the JSON stopped being true.
const (
	// MaxReportDataBytes caps Report's data_json. The proto comment on
	// ReportRequest.data_json names this constant.
	MaxReportDataBytes = 4 * 1024

	// MaxResultBytes caps SetResult's result_json. The proto comment on
	// SetResultRequest.result_json names this constant.
	MaxResultBytes = 4 * 1024
)

// maxReportKindBytes caps Report's kind, which is rendered verbatim inside the
// fragment the parent's injected frame carries. Control characters are refused
// for the same reason: a newline in a kind would forge a second line in a
// frame the coordinator is reading as one report.
const maxReportKindBytes = 64

// ScriptStream is one open Receive stream. Recv blocks until the next message,
// and returns io.EOF when the stream is over — the child exited, an abort
// arrived, or the daemon stopped it. Every other error is a failure.
type ScriptStream interface {
	Recv(ctx context.Context) (*rafikiv1.ScriptMessage, error)
}

// ScriptHub is the daemon-side half of the three script-child verbs. The
// caller's identity has already been resolved when a method runs: callerID is
// the calling child's own id, taken from its credential by the handler — never
// from a request field — and the hub acts only on that position in the tree.
//
// It is wired once (cmd/rafikid connect_script.go, via SetScriptHub) because
// everything the verbs need lives behind the Controller: the event buffer a
// report is pushed to, the inbox a Receive stream pulls from, the childstore a
// result is written to.
type ScriptHub interface {
	// Report publishes one progress report from script child callerID to its
	// parent's event buffer, where it coalesces and defers exactly like a
	// subagent settle. A top-level script (no parent) has its report appended
	// to its own durable event log instead.
	Report(ctx context.Context, callerID, kind, dataJSON string) error

	// SetResult stores callerID's structured final result. Last write wins.
	SetResult(ctx context.Context, callerID, resultJSON string) error

	// Receive opens callerID's message stream: everything addressed to its
	// inbox from now on, ending with the stop variant when the child is
	// stopped.
	Receive(ctx context.Context, callerID string) (ScriptStream, error)
}

// SetScriptHub attaches the script-verb hub. Post-construction setter for the
// same reason as SetChildLister: the Controller is built after this Server.
func (s *Server) SetScriptHub(h ScriptHub) { s.scripts.Store(&h) }

func (s *Server) scriptHub() ScriptHub {
	if p := s.scripts.Load(); p != nil {
		return *p
	}
	return nil
}

// scriptCaller resolves the calling child's own id for the three self-position
// verbs. Every caller that resolves to no child scope — a user credential, or
// the unix socket's anonymous local trust — has no position in the agent tree,
// and all three verbs act on the CALLER'S position, so there is nothing for
// them to act on: refuse rather than guess.
func (s *Server) scriptCaller(ctx context.Context) (string, error) {
	sc := s.childScope(ctx)
	if sc == nil {
		return "", connect.NewError(connect.CodePermissionDenied,
			errors.New("requires a script child credential; these verbs act on the caller's own position in the agent tree"))
	}
	id := sc.ChildID()
	if id == "" {
		return "", connect.NewError(connect.CodePermissionDenied,
			errors.New("the presented child credential names no child"))
	}
	return id, nil
}

// validateReportKind enforces the kind rules the proto comment states:
// required, at most 64 bytes, no control characters — it is rendered verbatim
// inside the parent's injected frame.
func validateReportKind(kind string) error {
	if kind == "" {
		return connect.NewError(connect.CodeInvalidArgument, errors.New("kind is required"))
	}
	if len(kind) > maxReportKindBytes {
		return connect.NewError(connect.CodeInvalidArgument,
			errors.New("kind exceeds 64 bytes"))
	}
	for _, r := range kind {
		if unicode.IsControl(r) {
			return connect.NewError(connect.CodeInvalidArgument,
				errors.New("kind must not contain control characters"))
		}
	}
	return nil
}

// validateJSONValue enforces the payload rules both data_json and result_json
// share: after trimming, the string must parse as a complete JSON value and
// fit its cap. The trimmed string is returned and is what gets stored,
// because json.Valid accepts trailing whitespace and a payload that lands
// verbatim inside the parent's injected frame must not carry padding newlines
// (cosmetic line padding, not content injection — no content can follow the
// value or it would not parse). Oversized or malformed data is refused,
// never truncated.
func validateJSONValue(s string, maxBytes int) (string, error) {
	s = strings.TrimSpace(s)
	if len(s) > maxBytes {
		return "", connect.NewError(connect.CodeInvalidArgument,
			errors.New("payload exceeds the size cap"))
	}
	if !json.Valid([]byte(s)) {
		return "", connect.NewError(connect.CodeInvalidArgument,
			errors.New("payload is not valid JSON"))
	}
	return s, nil
}

// Report serves the Report verb: one progress report from the calling script
// child. Everything after the checks is the hub's — the handler resolves the
// caller from the credential, validates, and forwards.
func (s *Server) Report(
	ctx context.Context,
	req *connect.Request[rafikiv1.ReportRequest],
) (*connect.Response[rafikiv1.ReportResponse], error) {
	callerID, err := s.scriptCaller(ctx)
	if err != nil {
		return nil, err
	}
	if err := validateReportKind(req.Msg.GetKind()); err != nil {
		return nil, err
	}
	dataJSON, err := validateJSONValue(req.Msg.GetDataJson(), MaxReportDataBytes)
	if err != nil {
		return nil, err
	}
	hub := s.scriptHub()
	if hub == nil {
		return nil, connect.NewError(connect.CodeUnavailable, errors.New("script hub not yet wired"))
	}
	if err := hub.Report(ctx, callerID, req.Msg.GetKind(), dataJSON); err != nil {
		return nil, err
	}
	return connect.NewResponse(&rafikiv1.ReportResponse{}), nil
}

// Receive serves the Receive verb: the calling script child's inbox, streamed.
//
// The one childScoped verb whose check is IDENTITY, not subtree:
// ReceiveRequest.child_id is a self-check, not an address. When set it must
// equal the caller's own id, and the check cannot go through
// ChildScope.Authorize, which refuses the caller's own id — a child is not a
// descendant of itself. Empty means the caller's own id.
func (s *Server) Receive(
	ctx context.Context,
	req *connect.Request[rafikiv1.ReceiveRequest],
	stream *connect.ServerStream[rafikiv1.ScriptMessage],
) error {
	callerID, err := s.scriptCaller(ctx)
	if err != nil {
		return err
	}
	if asked := req.Msg.GetChildId(); asked != "" && asked != callerID {
		return refuseChildScope(errors.New(
			"receive is self-only: you may only receive your own inbox"))
	}
	hub := s.scriptHub()
	if hub == nil {
		return connect.NewError(connect.CodeUnavailable, errors.New("script hub not yet wired"))
	}
	src, err := hub.Receive(ctx, callerID)
	if err != nil {
		return err
	}
	for {
		msg, err := src.Recv(ctx)
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if err := stream.Send(msg); err != nil {
			return err
		}
	}
}

// SetResult serves the SetResult verb: the calling script child's final
// result, stored on its own row. Last write wins.
func (s *Server) SetResult(
	ctx context.Context,
	req *connect.Request[rafikiv1.SetResultRequest],
) (*connect.Response[rafikiv1.SetResultResponse], error) {
	callerID, err := s.scriptCaller(ctx)
	if err != nil {
		return nil, err
	}
	resultJSON, err := validateJSONValue(req.Msg.GetResultJson(), MaxResultBytes)
	if err != nil {
		return nil, err
	}
	hub := s.scriptHub()
	if hub == nil {
		return nil, connect.NewError(connect.CodeUnavailable, errors.New("script hub not yet wired"))
	}
	if err := hub.SetResult(ctx, callerID, resultJSON); err != nil {
		return nil, err
	}
	return connect.NewResponse(&rafikiv1.SetResultResponse{}), nil
}
