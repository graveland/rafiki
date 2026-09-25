// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
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
			ServedProvider: t.ServedProvider,
		})
	}
	return out, nil
}

func (r *conversationReader) RunQuery(ctx context.Context, name string, f tools.CatalogueFilter) (tools.CatalogueResult, error) {
	res, err := r.ctrl.ConversationQuery(ctx, r.scope, name, insights.StatsFilter{
		Since: unixSecPtr(f.SinceUnix), Until: unixSecPtr(f.UntilUnix),
		Owner: f.Owner, Persona: f.Persona, Source: f.Source, Model: f.Model,
		Path: insights.Path(f.Path),
	})
	if err != nil {
		return tools.CatalogueResult{}, err
	}
	return catalogueResult(res)
}

// catalogueResult maps an insights.QueryResult onto the tool-side mirrors.
// Column kinds collapse to the three the tool can render -- ColString is the
// zero ColumnKind, so it is the default arm. An Entry outside the three
// concrete types cannot occur for a result the admission-checked Query entry
// point produced, so it is a mapping bug and fails loudly rather than
// rendering as an empty cell.
func catalogueResult(res insights.QueryResult) (tools.CatalogueResult, error) {
	cols := make([]tools.CatalogueColumn, 0, len(res.Columns))
	for _, c := range res.Columns {
		kind := "string"
		switch c.Kind {
		case insights.ColInt:
			kind = "int"
		case insights.ColFloat:
			kind = "float"
		}
		cols = append(cols, tools.CatalogueColumn{Name: c.Name, Kind: kind, Format: c.Format})
	}
	rows := make([][]tools.CatalogueEntry, 0, len(res.Rows))
	for _, r := range res.Rows {
		row := make([]tools.CatalogueEntry, 0, len(r))
		for _, e := range r {
			switch v := e.(type) {
			case insights.IntEntry:
				row = append(row, tools.CatalogueEntry{Int: int64(v), IsInt: true})
			case insights.FloatEntry:
				row = append(row, tools.CatalogueEntry{Float: float64(v), IsFloat: true})
			case insights.StringEntry:
				row = append(row, tools.CatalogueEntry{Str: string(v)})
			default:
				return tools.CatalogueResult{}, fmt.Errorf("agent_conversations: unhandled insights.Entry type %T", e)
			}
		}
		rows = append(rows, row)
	}
	return tools.CatalogueResult{Columns: cols, Rows: rows}, nil
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
