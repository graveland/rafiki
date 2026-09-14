// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"time"

	"go.graveland.dev/rafiki/pkg/fundi/tools"
	"go.graveland.dev/rafiki/pkg/insights"
	"go.graveland.dev/rafiki/pkg/users"
)

// conversationReader adapts *Controller to tools.ConversationReader, bound
// to one caller's Scope at construction -- the same reasoning as
// quotaReader being bound to one userID.
type conversationReader struct {
	ctrl  *Controller
	scope insights.Scope
}

// newControllerConversationReader binds a fundi child's own owner. An empty
// ownerUserID (anonymous spawn) yields the zero-value Scope, which
// insights.Scope's fail-closed cond() turns into "no rows" rather than a
// special case here -- matching quotaReader's "no data" degrade for the same
// input.
func newControllerConversationReader(c *Controller, ownerUserID string) *conversationReader {
	scope := insights.Scope{}
	if ownerUserID != "" {
		scope = insights.ScopeOwner(ownerUserID)
	}
	return &conversationReader{ctrl: c, scope: scope}
}

// newMCPConversationReader binds the authenticated MCP caller. Unlike the
// fundi-runtime constructor above, this one DOES grant ScopeAll for an admin
// owner -- the MCP caller's own IsAdmin bit is directly available here,
// where a fundi child's is not (agentRuntimeOptions only resolves an owner
// user id, not that user's admin status).
func newMCPConversationReader(ctrl *Controller, owner users.Identity) *conversationReader {
	switch {
	case owner.IsAdmin:
		return &conversationReader{ctrl: ctrl, scope: insights.ScopeAll()}
	case owner.UserID != "":
		return &conversationReader{ctrl: ctrl, scope: insights.ScopeOwner(owner.UserID)}
	default:
		return &conversationReader{ctrl: ctrl, scope: insights.Scope{}}
	}
}

func (r *conversationReader) ConversationSearch(ctx context.Context, q tools.ConversationQuery) ([]tools.ConversationSummaryRow, error) {
	f := insights.SearchFilter{
		Since: unixSecPtr(q.SinceUnix), Until: unixSecPtr(q.UntilUnix),
		Owner: q.Owner, Persona: q.Persona, Source: q.Source, Model: q.Model,
		Status: q.Status, Path: insights.Path(q.Path), MinTokens: q.MinTokens,
		Text: q.Text, Limit: q.Limit,
	}
	rows, err := r.ctrl.ConversationSearch(ctx, r.scope, f)
	if err != nil {
		return nil, err
	}
	out := make([]tools.ConversationSummaryRow, 0, len(rows))
	for _, row := range rows {
		out = append(out, tools.ConversationSummaryRow{
			ID: row.ID, Name: row.Name, Owner: row.Owner, Persona: row.Persona,
			Source: row.Source, Model: row.Model, Status: row.Status, DrivenBy: row.DrivenBy,
			CreatedAtUnix: row.CreatedAt.Unix(), Turns: row.Turns,
			InputTokens: row.InputTokens, OutputTokens: row.OutputTokens, CacheReadTokens: row.CacheReadTokens,
			CacheHitRatio: row.CacheHitRatio, TotalCostUSD: row.TotalCostUSD, FirstMessage: row.FirstMessage,
		})
	}
	return out, nil
}

func (r *conversationReader) ConversationExport(ctx context.Context, conversationID string) (*tools.ConversationTranscript, error) {
	tr, err := r.ctrl.ConversationExport(ctx, r.scope, conversationID)
	if err != nil {
		return nil, err
	}
	out := &tools.ConversationTranscript{
		ConversationID: tr.ConversationID, Owner: tr.Owner, Persona: tr.Persona,
		Source: tr.Source, DrivenBy: tr.DrivenBy, AvailableSkills: tr.AvailableSkills,
	}
	for _, t := range tr.Turns {
		out.Turns = append(out.Turns, tools.ConversationTranscriptTurn{
			Ordinal: t.Ordinal, Role: t.Role, Content: t.Content, Skills: t.Skills,
			InputTokens: t.InputTokens, OutputTokens: t.OutputTokens, CacheReadTokens: t.CacheReadTokens,
			LatencyMS: t.LatencyMS, Model: t.Model, PrefixHash: t.PrefixHash,
		})
	}
	return out, nil
}

// unixSecPtr maps a 0 (unset) Unix-seconds filter onto a nil *time.Time; any
// other value becomes the second boundary. Kept local: pkg/control's
// unixToTime helper is unexported and cmd/rafikid is a different package.
func unixSecPtr(sec int64) *time.Time {
	if sec == 0 {
		return nil
	}
	t := time.Unix(sec, 0)
	return &t
}
