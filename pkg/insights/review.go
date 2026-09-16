// SPDX-License-Identifier: Apache-2.0

package insights

import (
	"context"
	"fmt"
	"strings"
	"time"
)

const (
	defaultReviewLimit = 50
	maxReviewLimit     = 500
)

// clampReviewLimit applies the server-side limit convention shared by the
// review reads: 0 or negative means the default (50), anything above the cap
// (500) clamps to the cap -- the same spirit as pkg/connectapi's
// maxConversationSearchLimit, re-declared here because pkg/insights does not
// import that package. The server-side LIMIT is deliberate: a wire caller
// cannot be trusted to bound its own page.
func clampReviewLimit(limit int) int {
	if limit <= 0 {
		return defaultReviewLimit
	}
	if limit > maxReviewLimit {
		return maxReviewLimit
	}
	return limit
}

// AnalysisRow mirrors connectapi.ReviewAnalysis (that package cannot import
// this one's Scope type into its own signature, so the adapter in
// cmd/rafikid maps between them).
type AnalysisRow struct {
	ID, ConversationID, Model, Profile, Status, Error string
	InputTokens, OutputTokens                         int64
	CostUSD                                           float64
	CreatedAt                                         time.Time
}

// RecentAnalyses returns conversation_analysis rows scoped to scope, most
// recent first, capped at limit (0 or negative means a default of 50; values
// above 500 clamp to 500). An id in conversationIDs that scope excludes
// contributes no row and no error -- the same not-found-shaped scope miss
// ConversationExport uses, never a permission error naming the id. An invalid
// (zero-value) scope renders as 1=0 via scope.cond, so it returns no rows
// rather than behaving as "no filter"; a malformed (non-UUID) id errors, the
// same loud failure Export gives one.
func (i *Insights) RecentAnalyses(ctx context.Context, scope Scope, conversationIDs []string, limit int) ([]AnalysisRow, error) {
	limit = clampReviewLimit(limit)

	var a argList
	conds := []string{scope.cond(&a, "c.owner_user_id")}
	if len(conversationIDs) > 0 {
		conds = append(conds, "ca.conversation_id = ANY("+a.next(conversationIDs)+")")
	}

	rows, err := i.pool.Query(ctx, `
SELECT ca.id::text, ca.conversation_id::text, ca.model, coalesce(ca.profile, ''),
       ca.status, coalesce(ca.error, ''), ca.input_tokens, ca.output_tokens,
       ca.cost_usd, ca.created_at
  FROM conversations.conversation_analysis ca
  JOIN conversations.conversation c ON c.id = ca.conversation_id
 WHERE `+strings.Join(conds, "\n   AND ")+`
 ORDER BY ca.created_at DESC
 LIMIT `+a.next(limit), a.args...)
	if err != nil {
		return nil, fmt.Errorf("recent analyses: %w", err)
	}
	defer rows.Close()

	var out []AnalysisRow
	for rows.Next() {
		var r AnalysisRow
		if err := rows.Scan(&r.ID, &r.ConversationID, &r.Model, &r.Profile,
			&r.Status, &r.Error, &r.InputTokens, &r.OutputTokens, &r.CostUSD, &r.CreatedAt); err != nil {
			return nil, fmt.Errorf("recent analyses: scan: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Finding mirrors connectapi.ReviewFinding.
type Finding struct {
	ID, AnalysisID, ConversationID, Axis, TopicKey, SkillName, Title string
	ExpectedSavingsTokens                                            int64
	Status                                                           string
}

// FindingsFilter narrows Findings. Status "" means "open", matching
// store.FindingFilter's existing convention -- kept identical on purpose so
// the wire behavior matches `rafikid agent findings`'s default exactly. Zero
// Axis/Skill values are ignored; an out-of-scope id in ConversationIDs is
// silently absent from the results, never an error. Limit follows the same
// server-side convention as RecentAnalyses (0/negative -> 50, cap 500).
type FindingsFilter struct {
	Axis, Skill, Status string
	ConversationIDs     []string
	Limit               int
}

// Findings returns analysis_finding rows scoped to scope (joined through
// conversation_analysis to conversation on owner_user_id), matching f,
// most-impactful first -- same ordering as store.ListFindings
// (expected_savings_tokens DESC, then the joined analysis's created_at DESC).
// An invalid (zero-value) scope renders as 1=0 via scope.cond and returns no
// rows rather than behaving as "no filter".
func (i *Insights) Findings(ctx context.Context, scope Scope, f FindingsFilter) ([]Finding, error) {
	limit := clampReviewLimit(f.Limit)
	status := f.Status
	if status == "" {
		status = "open"
	}

	var a argList
	conds := []string{scope.cond(&a, "c.owner_user_id"), "af.status = " + a.next(status)}
	if f.Axis != "" {
		conds = append(conds, "af.axis = "+a.next(f.Axis))
	}
	if f.Skill != "" {
		conds = append(conds, "af.skill_name = "+a.next(f.Skill))
	}
	if len(f.ConversationIDs) > 0 {
		conds = append(conds, "ca.conversation_id = ANY("+a.next(f.ConversationIDs)+")")
	}

	rows, err := i.pool.Query(ctx, `
SELECT af.id::text, af.analysis_id::text, ca.conversation_id::text, af.axis,
       af.topic_key, coalesce(af.skill_name, ''), af.title,
       af.expected_savings_tokens, af.status
  FROM conversations.analysis_finding af
  JOIN conversations.conversation_analysis ca ON ca.id = af.analysis_id
  JOIN conversations.conversation c ON c.id = ca.conversation_id
 WHERE `+strings.Join(conds, "\n   AND ")+`
 ORDER BY af.expected_savings_tokens DESC, ca.created_at DESC
 LIMIT `+a.next(limit), a.args...)
	if err != nil {
		return nil, fmt.Errorf("findings: %w", err)
	}
	defer rows.Close()

	var out []Finding
	for rows.Next() {
		var r Finding
		if err := rows.Scan(&r.ID, &r.AnalysisID, &r.ConversationID, &r.Axis,
			&r.TopicKey, &r.SkillName, &r.Title, &r.ExpectedSavingsTokens, &r.Status); err != nil {
			return nil, fmt.Errorf("findings: scan: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// FilterByScope returns the subset of ids that scope admits, checked against
// conversations.conversation directly (no join through conversation_analysis
// -- ids passed in have not necessarily been analyzed yet). Consumed by
// cmd/rafikid's ConversationReview (Task 2.2, same wave) to drop
// out-of-scope conversation_ids from a review request before any job is
// built -- that task does not touch this file and must call this exact
// method rather than re-deriving scope-to-SQL translation itself, which
// pkg/insights alone owns.
//
// The result preserves the caller's order (and duplicates): it is a filtered
// view of the input, not a re-ordered set. An id that scope excludes — and
// one that names no conversation at all — is dropped silently: a scope miss
// reads exactly like not-found, never a permission error. An invalid
// (zero-value) scope admits nothing, so it returns an empty result without
// error. A malformed (non-UUID) id errors loudly, like every other uuid-cast
// path here.
func (i *Insights) FilterByScope(ctx context.Context, scope Scope, ids []string) ([]string, error) {
	if len(ids) == 0 {
		return nil, nil
	}

	var a argList
	conds := []string{
		"c.id = ANY(" + a.next(ids) + ")",
		scope.cond(&a, "c.owner_user_id"),
	}
	rows, err := i.pool.Query(ctx, `
SELECT c.id::text
  FROM conversations.conversation c
 WHERE `+strings.Join(conds, "\n   AND "), a.args...)
	if err != nil {
		return nil, fmt.Errorf("filter by scope: %w", err)
	}
	defer rows.Close()

	admitted := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("filter by scope: scan: %w", err)
		}
		admitted[id] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("filter by scope: %w", err)
	}

	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if admitted[id] {
			out = append(out, id)
		}
	}
	return out, nil
}
