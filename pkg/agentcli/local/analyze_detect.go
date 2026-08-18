// SPDX-License-Identifier: Apache-2.0

package local

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"go.graveland.dev/rafiki/pkg/agentcli"
	"go.graveland.dev/rafiki/pkg/analyze"
	"go.graveland.dev/rafiki/pkg/insights"
	"go.graveland.dev/rafiki/pkg/store"
)

// analyzeOutcome is analyzeOne's result for one conversation. A nil
// *analyzeOutcome (with a nil error) means the stop_after=="compact" halt:
// neither a success nor a failure, so runAnalyze's batch loop doesn't count
// it either way.
type analyzeOutcome struct {
	analysis   *analyze.Analysis
	analysisID string
	prior      map[store.FindingKey]string
	failed     bool
}

// analyzeOne runs Export→Compact→Detect→store for one conversation (or, for
// a corpus item, Compact→Detect straight from its preloaded transcript,
// skipping Export). The returned error is always fatal — a channel send
// failed because ctx was cancelled — and aborts the whole batch; an
// Export/Detect problem that isn't fatal is instead reported through send
// and folded into the returned outcome's failed field.
func (b *Backend) analyzeOne(ctx context.Context, send func(agentcli.AnalyzeEvent) bool, it analyzeItem, profile *analyze.Profile, promptHash, stopAfter string, noStore bool) (*analyzeOutcome, error) {
	tr := it.transcript
	if tr == nil {
		var err error
		tr, err = b.ins.Export(ctx, it.id)
		if err != nil {
			// insights.ErrNotFound means the conversation row itself is
			// gone: there is nothing for a failed-analysis row to FK
			// against, so — unlike any other export error — we don't try
			// to record one.
			if !errors.Is(err, insights.ErrNotFound) && !noStore {
				if recErr := b.recordFailure(ctx, it.id, profile, promptHash, fmt.Errorf("export: %w", err)); recErr != nil {
					return nil, fmt.Errorf("agentcli/local: record failure: %w", recErr)
				}
			}
			// The trailing 0, 0, 0 is a known limitation, not an oversight:
			// insights.Export doesn't return partial usage on a failed export
			// (there's no LLM call in Export to begin with — it's a straight
			// DB read), so there is no real usage figure to report here.
			if !send(progressEvent(it.id, agentcli.StateFailed, err.Error(), 0, 0, 0)) {
				return nil, ctx.Err()
			}
			return &analyzeOutcome{failed: true}, nil
		}
	}

	compacted := analyze.Compact(tr, profile.Compact)
	// Pin ConversationID to it.id here too (mirroring the same pin on
	// analysis below): a corpus file's transcript.ConversationID can be
	// empty, and WriteArtifacts rejects an empty conversation id outright —
	// without this, a --compact run over a conversation_id-less corpus file
	// fails to write artifacts even though it.id (derived from the
	// filename) is a perfectly good identifier.
	compacted.ConversationID = it.id

	if stopAfter == "compact" {
		if !send(agentcli.AnalyzeEvent{Kind: agentcli.EventAnalysis, Transcript: compacted}) {
			return nil, ctx.Err()
		}
		return nil, nil
	}

	analysis, err := analyze.Detect(ctx, b.llm, compacted, profile, analyzerOwnerUserID, b.pricer)
	if err != nil {
		if !noStore {
			if recErr := b.recordFailure(ctx, it.id, profile, promptHash, fmt.Errorf("detect: %w", err)); recErr != nil {
				return nil, fmt.Errorf("agentcli/local: record failure: %w", recErr)
			}
		}
		// The trailing 0, 0, 0 is a known limitation, not an oversight:
		// analyze.Detect returns only (*Analysis, error) — on a failed call
		// (malformed tool use, retry exhaustion, transport error) there is no
		// usage available to attach to this event, since Detect never
		// constructs a partial *Analysis to report it from. Fixing this
		// properly would mean widening Detect's error return to also carry
		// whatever usage the failed attempt's response(s) reported, which is
		// out of scope for agentcli/local alone (Detect lives in the analyze
		// package and is called from multiple places).
		if !send(progressEvent(it.id, agentcli.StateFailed, err.Error(), 0, 0, 0)) {
			return nil, ctx.Err()
		}
		return &analyzeOutcome{failed: true}, nil
	}
	// Export always sets ConversationID to it.id already, and Detect copies
	// it through from the transcript — but pin it explicitly here too, since
	// a corpus file's transcript.ConversationID can differ from (or be
	// absent, falling back to) the filename-derived it.id used everywhere
	// else in this run (progress events, the skip-key, ...).
	analysis.ConversationID = it.id

	var analysisID string
	var prior map[store.FindingKey]string
	if !noStore {
		raw, jerr := json.Marshal(analysis)
		if jerr != nil {
			return nil, fmt.Errorf("agentcli/local: marshal analysis: %w", jerr)
		}
		// Keyed on profile.DetectorModel, not analysis.Model: a
		// catalog-mediated failover can serve a different concrete model
		// than the one requested, and the skip-key must stay stable across
		// that drift or every run would treat itself as never-analyzed.
		id, pr, uerr := store.UpsertAnalysis(ctx, b.pool, store.AnalysisRow{
			ConversationID: it.id, DetectorVersion: analyze.DetectorVersion, Model: profile.DetectorModel,
			Profile: profile.Name, Status: "ok", PromptHash: promptHash, Analysis: raw,
			InputTokens: analysis.InputTokens, OutputTokens: analysis.OutputTokens, CostUSD: analysis.CostUSD,
		})
		if uerr != nil {
			return nil, fmt.Errorf("agentcli/local: store analysis: %w", uerr)
		}
		analysisID, prior = id, pr
	}

	if !send(progressEvent(it.id, agentcli.StateDone, "", analysis.InputTokens, analysis.OutputTokens, analysis.CostUSD)) {
		return nil, ctx.Err()
	}
	if !send(agentcli.AnalyzeEvent{Kind: agentcli.EventAnalysis, Analysis: analysis}) {
		return nil, ctx.Err()
	}

	return &analyzeOutcome{analysis: analysis, analysisID: analysisID, prior: prior}, nil
}

// progressEvent builds an EventProgress AnalyzeEvent.
func progressEvent(id, state, detail string, in, out int64, cost float64) agentcli.AnalyzeEvent {
	return agentcli.AnalyzeEvent{Kind: agentcli.EventProgress, Progress: &agentcli.Progress{
		ConversationID: id, State: state, Detail: detail, InputTokens: in, OutputTokens: out, CostUSD: cost,
	}}
}

// recordFailure writes a status='failed' conversation_analysis row for a
// conversation whose Export or Detect step errored, so a later run's
// AnalyzedSet check (and a human browsing findings) can see the attempt
// happened without re-deriving it from progress-event logs.
func (b *Backend) recordFailure(ctx context.Context, convID string, profile *analyze.Profile, promptHash string, cause error) error {
	_, _, err := store.UpsertAnalysis(ctx, b.pool, store.AnalysisRow{
		ConversationID: convID, DetectorVersion: analyze.DetectorVersion, Model: profile.DetectorModel,
		Profile: profile.Name, Status: "failed", Error: cause.Error(), PromptHash: promptHash,
	})
	return err
}
