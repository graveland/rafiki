// SPDX-License-Identifier: Apache-2.0

package local

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"

	"go.graveland.dev/rafiki/pkg/agentcli"
	"go.graveland.dev/rafiki/pkg/analyze"
	"go.graveland.dev/rafiki/pkg/store"
)

// storedAnalysesFor loads previously-stored (status='ok') analyses for ids at
// the given detector key, so a skipped conversation's earlier analysis still
// contributes to this run's Rank: a force-free re-run must not silently drop
// an already-detected finding just because Detect never ran again this time.
// It mirrors the server's storedAnalysesFor exactly: each returned
// analyzedConversation carries conversationID and analysisID (so the
// per-analysis ReplaceFindings loop in rankAndDraft can write ranked findings
// back to a skipped/stored conversation's own row, not just freshly-detected
// ones) plus prior, the analysis's existing non-'open' finding statuses
// carried forward so re-ranking never resurrects a dismissed/actioned finding
// as 'open'.
func storedAnalysesFor(ctx context.Context, pool *pgxpool.Pool, ids []string, detectorVersion int, model, promptHash string) ([]analyzedConversation, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	rows, err := pool.Query(ctx, `
		SELECT ca.conversation_id::text, ca.id::text, ca.analysis,
		       af.axis, af.topic_key, af.status
		  FROM conversations.conversation_analysis ca
		  LEFT JOIN conversations.analysis_finding af
		    ON af.analysis_id = ca.id AND af.status <> 'open'
		 WHERE ca.conversation_id = ANY($1::uuid[]) AND ca.detector_version = $2 AND ca.model = $3 AND ca.prompt_hash = $4
		   AND ca.status = 'ok'`,
		ids, detectorVersion, model, promptHash)
	if err != nil {
		return nil, fmt.Errorf("stored analyses: %w", err)
	}
	defer rows.Close()

	byConv := map[string]*analyzedConversation{}
	var order []string
	for rows.Next() {
		var convID, analysisID string
		var raw []byte
		var axis, topicKey, findingStatus *string
		if err := rows.Scan(&convID, &analysisID, &raw, &axis, &topicKey, &findingStatus); err != nil {
			return nil, fmt.Errorf("stored analyses: scan: %w", err)
		}
		ac, ok := byConv[convID]
		if !ok {
			var a analyze.Analysis
			if err := json.Unmarshal(raw, &a); err != nil {
				return nil, fmt.Errorf("stored analyses: unmarshal %s: %w", convID, err)
			}
			ac = &analyzedConversation{
				conversationID: convID, analysisID: analysisID, analysis: &a,
				prior: map[store.FindingKey]string{},
			}
			byConv[convID] = ac
			order = append(order, convID)
		}
		if axis != nil && topicKey != nil && findingStatus != nil {
			ac.prior[store.FindingKey{Axis: *axis, TopicKey: *topicKey}] = *findingStatus
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("stored analyses: %w", err)
	}
	out := make([]analyzedConversation, 0, len(order))
	for _, id := range order {
		out = append(out, *byConv[id])
	}
	return out, nil
}

// skippedReplace records one conversation's ReplaceFindings call that was
// skipped by the zero-rows guard in replaceFindingsPerAnalysis, rather than
// being allowed to truncate that analysis's existing findings.
type skippedReplace struct {
	conversationID string
	analysisID     string
	existing       int // how many findings the analysis row had before this run
}

// existingFindingCount returns how many analysis_finding rows analysisID
// currently has (any status), so replaceFindingsPerAnalysis can tell "this
// conversation genuinely has zero findings this run" apart from "something
// upstream (e.g. an id case-mismatch) computed zero rows for a conversation
// that actually still has findings."
func existingFindingCount(ctx context.Context, pool *pgxpool.Pool, analysisID string) (int, error) {
	var n int
	err := pool.QueryRow(ctx,
		`SELECT count(*) FROM conversations.analysis_finding WHERE analysis_id = $1::uuid`, analysisID).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("existing finding count: %w", err)
	}
	return n, nil
}

// replaceFindingsPerAnalysis writes each conversation's slice of the ranked
// findings back to its own analysis row: a RankedFinding may span several
// contributing conversations (Rank groups by axis+topic/skill across the
// whole batch), so a finding lands on every analysis row whose conversation
// contributed to it. Mirrors the server's replaceFindingsPerAnalysis exactly,
// including using rf.Score (not any raw per-finding token count) as
// ExpectedSavingsTokens.
//
// Belt-and-braces guard: store.ReplaceFindings is delete-then-insert, so
// calling it with zero rows for a conversation that already has findings
// would silently wipe them. That should never legitimately happen — rows is
// only empty when no ranked finding lists s.conversationID among its
// Conversations — but it's exactly the failure mode an id-canonicalization
// bug (a conversation id compared case-sensitively against Rank's
// slices.Contains) produces: the conversation truly has findings, they just
// don't match by string equality anymore. Rather than trust that this class
// of bug can never recur, check the analysis row's existing finding count
// before an empty-rows call and skip the call (recording it in the returned
// slice) instead of truncating.
func replaceFindingsPerAnalysis(ctx context.Context, pool *pgxpool.Pool, convs []analyzedConversation, ranked []analyze.RankedFinding) ([]skippedReplace, error) {
	var skipped []skippedReplace
	for _, s := range convs {
		var rows []store.FindingRow
		for _, rf := range ranked {
			if !slices.Contains(rf.Conversations, s.conversationID) {
				continue
			}
			rows = append(rows, store.FindingRow{
				AnalysisID:            s.analysisID,
				Axis:                  rf.Axis,
				TopicKey:              rf.TopicKey,
				SkillName:             rf.Recommendation.SkillName,
				Title:                 rf.Title,
				ExpectedSavingsTokens: rf.Score,
			})
		}
		if len(rows) == 0 {
			existing, cerr := existingFindingCount(ctx, pool, s.analysisID)
			if cerr != nil {
				return skipped, cerr
			}
			if existing > 0 {
				skipped = append(skipped, skippedReplace{
					conversationID: s.conversationID, analysisID: s.analysisID, existing: existing,
				})
				continue
			}
		}
		if err := store.ReplaceFindings(ctx, pool, s.analysisID, rows, s.prior); err != nil {
			return skipped, err
		}
	}
	return skipped, nil
}

// draftEligible reports whether f's recommendation is one Draft can act on: a
// proposal to create or edit a skill file.
func draftEligible(f *analyze.RankedFinding) bool {
	return f.Recommendation.Kind == "new-skill" || f.Recommendation.Kind == "skill-edit"
}

// currentSkillFiles returns the subset of skillFiles backing a finding's
// SkillName, mirroring the server's currentSkillFiles exactly: a path
// containing "/skills/<name>/SKILL.md" (the layout every skill directory
// follows), or an exact match on the two bare-relative-path layouts a caller
// might pass instead ("skills/<name>/SKILL.md" or
// ".claude/skills/<name>/SKILL.md") — without these fallbacks a caller
// passing bare relative paths gets an empty current and Draft silently takes
// its new-skill branch for what is really a skill-edit. Returns nil for an
// empty name (a new-skill recommendation has no existing file to match).
func currentSkillFiles(name string, skillFiles []analyze.SkillFile) []analyze.SkillFile {
	if name == "" {
		return nil
	}
	contains := "/skills/" + name + "/SKILL.md"
	exactDotClaude := ".claude/skills/" + name + "/SKILL.md"
	exactBare := "skills/" + name + "/SKILL.md"
	var out []analyze.SkillFile
	for _, sf := range skillFiles {
		if strings.Contains(sf.Path, contains) || sf.Path == exactDotClaude || sf.Path == exactBare {
			out = append(out, sf)
		}
	}
	return out
}

// rankAndDraft is runAnalyze's final stage: it ranks findings across every
// analysis this run touched — freshly-detected plus previously-stored ones
// for skipped conversations — persists the ranked findings back onto every
// contributing analysis row (fresh and carried-over alike), and, unless the
// caller's StopAfter says to stop before drafting, drafts skill edits for the
// top candidates.
func (b *Backend) rankAndDraft(ctx context.Context, req agentcli.AnalyzeRequest, profile *analyze.Profile, successes []analyzedConversation, skippedIDs []string, promptHash string, summary *agentcli.Summary, send func(agentcli.AnalyzeEvent) bool) error {
	var all []*analyze.Analysis
	for _, s := range successes {
		all = append(all, s.analysis)
	}

	var carryOver []analyzedConversation
	if len(skippedIDs) > 0 {
		var err error
		carryOver, err = storedAnalysesFor(ctx, b.pool, skippedIDs, analyze.DetectorVersion, profile.DetectorModel, promptHash)
		if err != nil {
			return fmt.Errorf("agentcli/local: stored analyses: %w", err)
		}
		for _, c := range carryOver {
			all = append(all, c.analysis)
		}
	}

	ranked := analyze.Rank(all)

	// Every conversation whose analysis fed Rank gets the ranked (not raw)
	// findings written back to its own row — freshly-detected ones (unless
	// noStore, which has no conversations.conversation row for an
	// analysis_finding to FK against) and carried-over/skipped ones alike, so
	// cross-conversation recurrence and this run's Score land on every
	// analysis row, not just the ones Detect touched this run.
	//
	// req.NoStore suppresses ALL of that persistence, not just this run's own
	// fresh rows: carryOver conversations were only skipped (not re-detected)
	// because they already have a stored analysis from a prior, separate run —
	// writing this NoStore run's re-ranked scores onto their existing rows
	// would silently rewrite previously-persisted data from a run that asked
	// not to touch the database at all. carryOver still gets loaded above and
	// still feeds Rank (read-only), it just never reaches ReplaceFindings.
	forReplace := make([]analyzedConversation, 0, len(successes)+len(carryOver))
	for _, s := range successes {
		if s.noStore {
			continue
		}
		forReplace = append(forReplace, s)
	}
	if !req.NoStore {
		forReplace = append(forReplace, carryOver...)
	}

	skippedReplaces, err := replaceFindingsPerAnalysis(ctx, b.pool, forReplace, ranked)
	if err != nil {
		return fmt.Errorf("agentcli/local: replace findings: %w", err)
	}
	for _, sr := range skippedReplaces {
		if !send(progressEvent(sr.conversationID, agentcli.StateFailed,
			fmt.Sprintf("replace findings skipped: computed 0 rows but analysis %s already had %d finding(s)", sr.analysisID, sr.existing),
			0, 0, 0)) {
			return ctx.Err()
		}
	}

	// withDrafts nests each ranked finding's (eventual) draft under the
	// finding itself, rather than in a side map keyed by Title: analyze.Rank
	// can produce two distinct findings sharing a Title (grouped by axis+
	// topic/skill, tie-broken by axis on equal titles), and a Title-keyed map
	// would silently attach one draft's presence to both. Mirrors the
	// server's withDrafts exactly.
	withDrafts := make([]agentcli.RankedFindingWithDraft, len(ranked))
	for i, rf := range ranked {
		withDrafts[i] = agentcli.RankedFindingWithDraft{RankedFinding: rf}
	}
	summary.Ranked = withDrafts

	if req.StopAfter != "" && req.StopAfter != "draft" {
		return nil
	}

	failures := b.draftTopFindings(ctx, withDrafts, profile, req.SkillFiles)

	// Fold each successful draft's own LLM usage into the run's Totals: Detect's
	// usage was already summed into summary.Totals well before this point (in
	// runAnalyze, immediately after the per-conversation loop), so without this
	// a drafting pass's cost is silently invisible to any caller reading
	// Summary.Totals as "the whole run's cost."
	for _, rf := range withDrafts {
		if rf.Draft == nil {
			continue
		}
		summary.Totals.InputTokens += rf.Draft.InputTokens
		summary.Totals.OutputTokens += rf.Draft.OutputTokens
		summary.Totals.CostUSD += rf.Draft.CostUSD
	}

	// A per-finding Draft failure never aborts the run (see draftTopFindings):
	// the batch's ranking, persisted findings, and summary all still ship.
	// The server logs the failure and moves on; the CLI has no logger here,
	// so it surfaces the same information as a failed progress event instead,
	// carrying the finding's title (there's no conversation this failure
	// belongs to) in Detail.
	for _, f := range failures {
		if !send(progressEvent("", agentcli.StateFailed, fmt.Sprintf("draft %q: %v", f.title, f.err), 0, 0, 0)) {
			return ctx.Err()
		}
	}
	return nil
}

// draftTopFindings drafts a skill edit for up to the first 10 ranked findings
// whose recommendation is draft-eligible, in Rank's own order (already
// Score-desc), matching each against the caller's current SkillFiles, and
// attaches each successful draft directly to its own entry in ranked (in
// place — ranked's elements are addressed by index, not copied). A
// per-finding Draft failure leaves that entry's Draft nil rather than
// aborting the batch — mirroring the server's draftTopFindings, which logs
// and continues rather than discarding the whole already-computed ranking
// over one bad draft call. The failed findings' titles and errors are
// returned so the caller can surface them (the server logs; the CLI has no
// logger here, so it reports via a progress event instead).
func (b *Backend) draftTopFindings(ctx context.Context, ranked []agentcli.RankedFindingWithDraft, profile *analyze.Profile, skillFiles []analyze.SkillFile) []draftFailure {
	var failures []draftFailure
	count := 0
	for i := range ranked {
		if count >= 10 {
			break
		}
		f := ranked[i].RankedFinding
		if !draftEligible(&f) {
			continue
		}
		count++
		current := currentSkillFiles(f.Recommendation.SkillName, skillFiles)
		edit, err := analyze.Draft(ctx, b.llm, f, current, profile, analyzerOwnerUserID, b.pricer)
		if err != nil {
			failures = append(failures, draftFailure{title: f.Title, err: err})
			continue
		}
		ranked[i].Draft = edit
	}
	return failures
}

// draftFailure is one draft that errored during draftTopFindings: the
// finding's title (for Detail) and the underlying error.
type draftFailure struct {
	title string
	err   error
}
