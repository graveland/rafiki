// SPDX-License-Identifier: Apache-2.0

package insights

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
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

// splitIDArm partitions ids into the two arms the review reads match on
// (subtree.go's uuidsOnly is the Go-side precedent): a well-formed UUID
// matches conversations.conversation.id directly; anything else is treated as
// a child id and matched through conversations.child. An id matching neither
// arm admits nothing -- silently, the not-found fold.
func splitIDArm(ids []string) (uuids, childIDs []string) {
	uuids = make([]string, 0, len(ids))
	childIDs = make([]string, 0, len(ids))
	for _, id := range ids {
		if _, err := uuid.Parse(id); err == nil {
			uuids = append(uuids, id)
		} else {
			childIDs = append(childIDs, id)
		}
	}
	return uuids, childIDs
}

// idArms renders the two-arm id predicate the review reads share, against the
// conversation column col ("c.id" when the query's row IS the conversation,
// "ca.conversation_id" when it is an analysis joined to one). The uuid arm
// matches col directly; the child arm matches through the authoritative
// conversations.child mapping. Each arm is emitted only when its list is
// non-empty, so an all-UUID or all-child request builds no dead EXISTS. The
// caller must pass at least one non-empty list.
func idArms(a *argList, uuids, childIDs []string, col string) string {
	var arm []string
	if len(uuids) > 0 {
		arm = append(arm, col+" = ANY("+a.next(uuids)+"::uuid[])")
	}
	if len(childIDs) > 0 {
		arm = append(arm, "EXISTS (SELECT 1 FROM conversations.child ch WHERE ch.child_id = ANY("+a.next(childIDs)+"::text[]) AND ch.conversation_id = "+col+")")
	}
	return "(" + strings.Join(arm, " OR ") + ")"
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
// rather than behaving as "no filter". Ids match on two arms: one that parses
// as a UUID matches ca.conversation_id directly, and any other id is matched
// through conversations.child -- the client's Resolve produces child ids, so
// both spellings arrive on the wire. An id matching neither arm (nonexistent
// or out of scope) contributes no row and no error; there is no error for any
// id shape.
func (i *Insights) RecentAnalyses(ctx context.Context, scope Scope, conversationIDs []string, limit int) ([]AnalysisRow, error) {
	limit = clampReviewLimit(limit)

	var a argList
	conds := []string{scope.cond(&a, "c.owner_user_id", "c.id", "c.external_ref")}
	if len(conversationIDs) > 0 {
		uuids, childIDs := splitIDArm(conversationIDs)
		conds = append(conds, idArms(&a, uuids, childIDs, "ca.conversation_id"))
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
// rows rather than behaving as "no filter". Ids in f.ConversationIDs match on
// the same two arms as RecentAnalyses: a UUID against ca.conversation_id,
// anything else through conversations.child; one matching neither arm (an
// out-of-scope or nonexistent id) is silently absent from the results, never
// an error.
func (i *Insights) Findings(ctx context.Context, scope Scope, f FindingsFilter) ([]Finding, error) {
	limit := clampReviewLimit(f.Limit)
	status := f.Status
	if status == "" {
		status = "open"
	}

	var a argList
	conds := []string{scope.cond(&a, "c.owner_user_id", "c.id", "c.external_ref"), "af.status = " + a.next(status)}
	if f.Axis != "" {
		conds = append(conds, "af.axis = "+a.next(f.Axis))
	}
	if f.Skill != "" {
		conds = append(conds, "af.skill_name = "+a.next(f.Skill))
	}
	if len(f.ConversationIDs) > 0 {
		uuids, childIDs := splitIDArm(f.ConversationIDs)
		conds = append(conds, idArms(&a, uuids, childIDs, "ca.conversation_id"))
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

// FilterByScope returns the canonical conversation uuid (c.id::text) of every
// id scope admits, index-aligned with the surviving caller spellings it
// returns alongside: canonical[k] answers spellings[k], both in input order
// (duplicates preserved -- a filtered view of the input, not a re-ordered
// set). Checked against conversations.conversation directly (no join through
// conversation_analysis -- ids passed in have not necessarily been analyzed
// yet). Consumed by cmd/rafikid's ConversationReview, which queues on the
// canonical value and echoes the caller's spelling, so two spellings of one
// conversation (its uuid and its child id) collide correctly in the
// single-flight map. Callers must use this exact method rather than
// re-deriving scope-to-SQL translation themselves, which pkg/insights alone
// owns.
//
// Ids match on two arms, split Go-side like subtree.go's uuidsOnly: one that
// parses as a UUID matches c.id directly, and any other id is treated as a
// child id matched through the authoritative conversations.child mapping (the
// client's Resolve produces child ids, so both spellings arrive on the wire).
// An id matching NEITHER arm -- one that names no conversation at all, or one
// scope excludes -- is dropped silently: a scope miss reads exactly like
// not-found, never a permission error. There is no error for any id shape (a
// malformed id is not a loud failure; it simply matches nothing). Both arms
// are built conditionally, so an all-UUID or all-child request emits no dead
// arm. An invalid (zero-value) scope admits nothing, returning empty results
// without error.
func (i *Insights) FilterByScope(ctx context.Context, scope Scope, ids []string) (canonical, spellings []string, err error) {
	if len(ids) == 0 {
		return nil, nil, nil
	}

	uuids, childIDs := splitIDArm(ids)

	// Each arm projects (canonical, the spelling that matched it): a
	// conversation passed as a uuid is its own spelling; one reached through
	// a child id reports that child id. The child arm is a JOIN rather than
	// an EXISTS because the matched ch.child_id IS the spelling the caller
	// sent, and one row per matching child is what lets a conversation with
	// several children admit every spelling passed for it.
	var a argList
	var arms []string
	if len(uuids) > 0 {
		arms = append(arms, `
SELECT c.id::text, c.id::text
  FROM conversations.conversation c
 WHERE c.id = ANY(`+a.next(uuids)+`::uuid[])
   AND `+scope.cond(&a, "c.owner_user_id", "c.id", "c.external_ref"))
	}
	if len(childIDs) > 0 {
		arms = append(arms, `
SELECT c.id::text, ch.child_id
  FROM conversations.conversation c
  JOIN conversations.child ch ON ch.conversation_id = c.id
 WHERE ch.child_id = ANY(`+a.next(childIDs)+`::text[])
   AND `+scope.cond(&a, "c.owner_user_id", "c.id", "c.external_ref"))
	}
	rows, err := i.pool.Query(ctx, strings.Join(arms, "\nUNION ALL\n"), a.args...)
	if err != nil {
		return nil, nil, fmt.Errorf("filter by scope: %w", err)
	}
	defer rows.Close()

	// Keyed by the caller's spelling; a repeated spelling carries an
	// identical canonical either way.
	admitted := map[string]string{}
	for rows.Next() {
		var id, spelling string
		if err := rows.Scan(&id, &spelling); err != nil {
			return nil, nil, fmt.Errorf("filter by scope: scan: %w", err)
		}
		admitted[spelling] = id
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("filter by scope: %w", err)
	}

	for _, spelling := range ids {
		if id, ok := admitted[spelling]; ok {
			canonical = append(canonical, id)
			spellings = append(spellings, spelling)
		}
	}
	return canonical, spellings, nil
}
