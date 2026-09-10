// SPDX-License-Identifier: Apache-2.0

package daraja

import (
	"go.graveland.dev/rafiki/pkg/claudeargv"
	"go.graveland.dev/rafiki/pkg/darajapb"
	"go.graveland.dev/rafiki/pkg/protocol"
)

// ClaudeParamsForRequest maps a daemon SpawnRequest onto the wire ClaudeParams
// the daraja launch path carries. It is the mapping half of
// cmd/rafikid's darajaClaudeParams — which delegates here and then layers the
// Controller-derived proxy fields (ProxyUrl/ProxyToken/PassthroughAuth/
// AutoCompactWindow) on top — so the req→wire mapping is testable outside the
// main package and pinned against the local-subprocess path by
// test/integration's TestClaudeArgvIdenticalAcrossPaths rather than drifting
// from it again (AppendSystemPrompt and ExtraArgs were both dropped here once,
// before the proto carried them, and nothing failed).
//
// PermissionMode is stated here rather than left to claudeargv's default so
// this mapping and the local-subprocess one say the same thing in the same
// words (claudeargv.PermissionModeBypass).
//
// Every field carried here survives into the child's argv via SpecFromProto
// and ChildSpec.Argv. RecordRequests is carried because the wire type does —
// it is launch-only (it shapes the executor's daraja-serve argv and the proxy
// correlation header) and must never reach the claude child's own argv.
func ClaudeParamsForRequest(req protocol.SpawnRequest) *darajapb.ClaudeParams {
	return &darajapb.ClaudeParams{
		Model:         req.Model,
		ResumeSession: req.ResumeSession,
		// Always bypass: a daemon-managed child has no human to answer an
		// interactive permission prompt (see pkg/claudeargv's identical
		// default for the local-subprocess path). No typed field carries
		// a caller override yet — nothing has needed one.
		PermissionMode: claudeargv.PermissionModeBypass,
		// daraja rebuilds argv on every Restart, so both are re-read each
		// time — dropping either here would silently strip the flag from
		// every child spawned through an executor pool.
		AppendSystemPrompt: req.AppendSystemPrompt,
		ExtraArgs:          req.ExtraArgs,
		RecordRequests:     req.RecordRequests,
	}
}
