// SPDX-License-Identifier: Apache-2.0

package agentcli

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/dustin/go-humanize"
	"github.com/jedib0t/go-pretty/v6/table"

	"github.com/timescale/rafiki/analyze"
)

// CompareRun is one model's result in a --compare sweep: either a Summary
// (and the individual Analyses that produced it) on success, or Err on
// failure. A failed run never aborts the rest of the sweep.
//
// Analyzed/Failed mirror Summary's own fields when a Summary arrived (the
// normal case), but are also populated by tallying EventProgress failures
// directly when the run never reached a terminal EventSummary at all — e.g.
// an EventError partway through the batch, after some per-conversation
// failures already streamed but before Summary could be built. Without this,
// a run that failed outright (Summary == nil) rendered identically to one
// that analyzed zero conversations because every candidate was skipped: both
// showed "0 findings, ok."
type CompareRun struct {
	Model    string
	Summary  *Summary
	Err      error
	Analyses []*analyze.Analysis
	Analyzed int
	Failed   int
}

// failed reports whether r represents a failed run: an outright error, or a
// summary-less/zero-analyzed run that nonetheless recorded per-conversation
// failures.
func (r CompareRun) failed() bool {
	return r.Err != nil || (r.Analyzed == 0 && r.Failed > 0)
}

// Compare runs base once per model in models, cloning base.Profile (a value
// copy — the caller's Profile is never mutated) and overriding only
// DetectorModel each time. Each model's artifacts land in
// <outDir>/<modelSlug>/; a model's failure is recorded on its CompareRun and
// does not stop the remaining models from running.
func Compare(ctx context.Context, b Backend, base AnalyzeRequest, models []string, outDir string) ([]CompareRun, error) {
	runs := make([]CompareRun, 0, len(models))
	stage := base.StopAfter
	if stage == "" {
		stage = "detect"
	}

	for _, model := range models {
		var profile analyze.Profile
		if base.Profile != nil {
			profile = *base.Profile
		}
		profile.DetectorModel = model

		req := base
		req.Profile = &profile
		run := CompareRun{Model: model}

		events, err := b.Analyze(ctx, req)
		if err != nil {
			run.Err = err
			runs = append(runs, run)
			continue
		}

		runDir := filepath.Join(outDir, modelSlug(model))
		var progressFailed int
		for ev := range events {
			switch ev.Kind {
			case EventAnalysis:
				if ev.Analysis != nil {
					run.Analyses = append(run.Analyses, ev.Analysis)
					if outDir != "" {
						if werr := WriteArtifacts(runDir, ev.Analysis.ConversationID, stage, ev.Analysis); werr != nil && run.Err == nil {
							run.Err = werr
						}
					}
				} else if ev.Transcript != nil && outDir != "" {
					if werr := WriteArtifacts(runDir, ev.Transcript.ConversationID, "compact", ev.Transcript); werr != nil && run.Err == nil {
						run.Err = werr
					}
				}
			case EventProgress:
				if ev.Progress != nil && ev.Progress.State == StateFailed {
					progressFailed++
				}
			case EventSummary:
				run.Summary = ev.Summary
			case EventError:
				if run.Err == nil {
					run.Err = ev.Err
				}
			default:
				// Unrecognized event kinds (future additions) are ignored
				// rather than silently mis-tallied.
			}
		}
		if run.Summary != nil {
			run.Analyzed, run.Failed = run.Summary.Analyzed, run.Summary.Failed
		} else {
			run.Failed = progressFailed
		}
		if outDir != "" && run.Err == nil {
			if werr := WritePromptsSidecar(runDir, &profile); werr != nil {
				run.Err = werr
			}
		}
		runs = append(runs, run)
	}

	// A partial failure (some models succeeded) stays a nil error — each
	// run's own Err/failed() already reports it. But a sweep where EVERY
	// model failed must not report success just because Compare itself never
	// errored: the caller (CLI exit code, any automation reading Compare's
	// error) needs a way to tell "ran fine, all models happened to fail" from
	// "the sweep itself is broken" apart from having to inspect every run.
	if len(runs) > 0 {
		allFailed := true
		for _, r := range runs {
			if !r.failed() {
				allFailed = false
				break
			}
		}
		if allFailed {
			return runs, fmt.Errorf("agentcli: compare: all %d model(s) failed", len(runs))
		}
	}
	return runs, nil
}

// modelSlug turns a model id into a path-safe directory component: "/" and
// "~" (OpenRouter-native id syntax) become "-".
func modelSlug(model string) string {
	return strings.NewReplacer("/", "-", "~", "-").Replace(model)
}

// RenderCompare renders one row per model: findings count, per-axis counts
// (skill-gap/knowledge-to-persist/grind), in/out tokens, cost, and status —
// ERROR plus the failure message when the model's run failed.
func RenderCompare(w io.Writer, runs []CompareRun) error {
	t := newAgentTable(w, "Model Compare")
	t.AppendHeader(table.Row{"Model", "Findings", "Skill-gap", "Knowledge", "Grind", "Analyzed", "Failed", "In", "Out", "Cost", "Status"})
	for _, r := range runs {
		if r.Err != nil {
			t.AppendRow(table.Row{r.Model, "-", "-", "-", "-", "-", "-", "-", "-", "-", "ERROR: " + r.Err.Error()})
			continue
		}

		var skillGap, knowledge, grind int
		var in, out int64
		var cost float64
		if r.Summary != nil {
			for _, rf := range r.Summary.Ranked {
				switch rf.Axis {
				case "skill-gap":
					skillGap++
				case "knowledge-to-persist":
					knowledge++
				case "grind":
					grind++
				}
			}
			in = r.Summary.Totals.InputTokens
			out = r.Summary.Totals.OutputTokens
			cost = r.Summary.Totals.CostUSD
		}
		findings := 0
		if r.Summary != nil {
			findings = len(r.Summary.Ranked)
		}
		// A summary-less/zero-analyzed run with recorded failures (r.failed())
		// is a failed sweep, not a quiet success — rendering it "ok" (the old,
		// unconditional behavior) made a fully-failed model indistinguishable
		// from one that simply skipped every candidate.
		status := "ok"
		if r.failed() {
			status = fmt.Sprintf("FAILED (%d/%d analyzed)", r.Analyzed, r.Analyzed+r.Failed)
		}
		t.AppendRow(table.Row{r.Model, findings, skillGap, knowledge, grind, r.Analyzed, r.Failed, humanize.Comma(in), humanize.Comma(out), dollars(cost), status})
	}
	t.Render()
	return nil
}
