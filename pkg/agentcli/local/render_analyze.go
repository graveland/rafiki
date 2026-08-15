// SPDX-License-Identifier: Apache-2.0

package local

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/dustin/go-humanize"
	"github.com/jedib0t/go-pretty/v6/table"

	"go.graveland.dev/rafiki/pkg/agentcli"

	"go.graveland.dev/rafiki/pkg/analyze"
	"go.graveland.dev/rafiki/pkg/insights"
	"go.graveland.dev/rafiki/pkg/store"
)

// errWriter accumulates the first write error so long render paths can
// stay linear instead of checking every Fprintf.
type errWriter struct {
	w   io.Writer
	err error
}

func (e *errWriter) printf(format string, args ...any) {
	if e.err != nil {
		return
	}
	_, e.err = fmt.Fprintf(e.w, format, args...)
}

func (e *errWriter) println(args ...any) {
	if e.err != nil {
		return
	}
	_, e.err = fmt.Fprintln(e.w, args...)
}

// RenderProgress renders one Analyze progress event as a single line: the
// conversation id, its state, token usage, and — for a failed or skipped
// conversation — the detail explaining why.
func RenderProgress(w io.Writer, p *agentcli.Progress) error {
	if p == nil {
		return nil
	}
	ew := &errWriter{w: w}
	ew.printf("%s  %s", shortAnalyzeID(p.ConversationID), p.State)
	if p.Detail != "" {
		ew.printf("  %s", p.Detail)
	}
	if p.InputTokens != 0 || p.OutputTokens != 0 {
		ew.printf("  tokens=%s/%s", humanize.Comma(p.InputTokens), humanize.Comma(p.OutputTokens))
	}
	ew.println()
	return ew.err
}

// shortAnalyzeID truncates a conversation id for a progress line; ids are
// UUIDs and the first 8 characters are enough to eyeball distinctness.
func shortAnalyzeID(id string) string {
	if len(id) <= 8 {
		return id
	}
	return id[:8]
}

// RenderAnalyzeSummary renders the terminal agentcli.Summary of an Analyze run: a
// ranked-findings table, a totals line, and a "remaining" note when the
// run's limit left eligible conversations untouched.
func RenderAnalyzeSummary(w io.Writer, s *agentcli.Summary) error {
	if s == nil {
		_, err := fmt.Fprintln(w, "no summary received")
		return err
	}

	if len(s.Ranked) > 0 {
		t := agentcli.NewAgentTable(w, "Findings")
		t.AppendHeader(table.Row{"Axis", "Title", "Occurrences", "Savings", "Draft"})
		for _, rf := range s.Ranked {
			draft := ""
			if rf.Draft != nil {
				draft = "yes"
			}
			t.AppendRow(table.Row{rf.Axis, rf.Title, rf.Occurrences, humanize.Comma(rf.Score), draft})
		}
		t.Render()
	}

	ew := &errWriter{w: w}
	ew.printf("Analyzed %d/%d  skipped %d  failed %d  tokens %s/%s  cost %s\n",
		s.Analyzed, s.Population, s.Skipped, s.Failed,
		humanize.Comma(s.Totals.InputTokens), humanize.Comma(s.Totals.OutputTokens), agentcli.Dollars(s.Totals.CostUSD))
	if s.Remaining > 0 {
		ew.printf("remaining %d conversation(s) — re-run to continue, raise --limit, or pass --force\n", s.Remaining)
	}
	return ew.err
}

// RenderFindings renders a []store.FindingRow as a rounded table.
func RenderFindings(w io.Writer, rows []store.FindingRow) error {
	if len(rows) == 0 {
		_, err := fmt.Fprintln(w, "no findings")
		return err
	}
	t := agentcli.NewAgentTable(w, "Findings")
	t.AppendHeader(table.Row{"Axis", "Skill", "Title", "Savings", "Status", "ID"})
	for _, r := range rows {
		t.AppendRow(table.Row{r.Axis, r.SkillName, r.Title, humanize.Comma(r.ExpectedSavingsTokens), r.Status, r.ID})
	}
	t.Render()
	return nil
}

// WriteArtifacts writes payload's raw JSON plus a rendered markdown summary
// to <outDir>/<sanitized convID>.<stage>.{json,md}. convID is sanitized with
// filepath.Base and rejected outright if that changes the string — defense
// in depth against a conversation id smuggling path separators into outDir.
// payload's markdown rendering dispatches on its concrete type:
// *analyze.Analysis renders outcome/verdicts/findings-with-evidence,
// *insights.Transcript renders via agentcli.RenderTranscriptMD.
func WriteArtifacts(outDir, convID, stage string, payload any) error {
	safeID := filepath.Base(convID)
	if safeID == "." || safeID == string(filepath.Separator) || safeID != convID {
		return fmt.Errorf("invalid conversation id %q for %s artifacts", convID, stage)
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return err
	}
	base := filepath.Join(outDir, safeID+"."+stage)

	b, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal %s artifact: %w", stage, err)
	}
	if err := os.WriteFile(base+".json", b, 0o644); err != nil {
		return err
	}

	var md bytes.Buffer
	if err := renderArtifactMD(&md, payload); err != nil {
		return fmt.Errorf("render %s markdown artifact: %w", stage, err)
	}
	return os.WriteFile(base+".md", md.Bytes(), 0o644)
}

// renderArtifactMD dispatches an Analyze artifact payload to its markdown
// renderer by concrete type.
func renderArtifactMD(w io.Writer, payload any) error {
	switch v := payload.(type) {
	case *analyze.Analysis:
		return renderAnalysisMD(w, v)
	case *insights.Transcript:
		return agentcli.RenderTranscriptMD(w, v)
	default:
		return fmt.Errorf("unsupported artifact payload type %T", payload)
	}
}

// renderAnalysisMD renders an analyze.Analysis: outcome, per-axis verdicts,
// then each finding with its evidence quotes.
func renderAnalysisMD(w io.Writer, a *analyze.Analysis) error {
	ew := &errWriter{w: w}
	ew.printf("# Analysis %s\n\n", a.ConversationID)
	ew.printf("**Outcome:** %s\n\n", a.Outcome)

	if len(a.Verdicts) > 0 {
		ew.println("## Verdicts")
		for _, axis := range slices.Sorted(maps.Keys(a.Verdicts)) {
			ew.printf("- %s: %s\n", axis, a.Verdicts[axis])
		}
		ew.println()
	}

	for _, f := range a.Findings {
		ew.printf("## %s — %s\n\n", f.Axis, f.Title)
		ew.printf("- topic: %s\n- confidence: %.2f\n- recommendation: %s %s — %s\n",
			f.TopicKey, f.Confidence, f.Recommendation.Kind, f.Recommendation.SkillName, f.Recommendation.Summary)
		for _, ev := range f.Evidence {
			ew.printf("  - [%d] %s\n", ev.Ordinal, ev.Quote)
		}
		ew.println()
	}
	return ew.err
}

// WriteSkillEdits writes every ranked finding's drafted skill file(s) in
// s.Ranked under outDir, in ranked order, creating parent directories as
// needed, and returns the absolute paths written. A finding with no Draft
// (never reached the top-N drafted, or its own Draft call failed) is
// skipped. Mirrors the server's writeSkillEditFiles/writeSummaryDrafts
// (client/pkg/sc/agent_repo.go) — the CLI equivalent of "apply drafted
// skill edits to disk" — but writes under the run's own --out directory
// rather than a --repo working tree; wiring this into a CLI flag (e.g.
// --repo) is left to the caller.
//
// analyze.Draft's own validateRelativePath already rejects an absolute path
// or a ".." segment before a SkillEdit is ever returned, but this is the
// last line of defense before anything touches disk: every proposed file
// path is resolved against outDir and rejected if it would escape it,
// reusing the same escape-check WriteArtifacts relies on for conversation
// ids (filepath.Rel plus an explicit ".." prefix check).
func WriteSkillEdits(outDir string, s *agentcli.Summary) ([]string, error) {
	if s == nil {
		return nil, nil
	}
	outAbs, err := filepath.Abs(outDir)
	if err != nil {
		return nil, fmt.Errorf("resolve out dir %s: %w", outDir, err)
	}

	var written []string
	for _, rf := range s.Ranked {
		if rf.Draft == nil {
			continue
		}
		for _, f := range rf.Draft.Files {
			cleaned := filepath.Clean(f.Path)
			if filepath.IsAbs(cleaned) {
				return written, fmt.Errorf("finding %q: skill edit file %q: absolute paths are not allowed", rf.Title, f.Path)
			}
			full, err := filepath.Abs(filepath.Join(outAbs, cleaned))
			if err != nil {
				return written, fmt.Errorf("finding %q: resolve skill edit file %q: %w", rf.Title, f.Path, err)
			}
			rel, err := filepath.Rel(outAbs, full)
			if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				return written, fmt.Errorf("finding %q: skill edit file %q escapes %s", rf.Title, f.Path, outDir)
			}
			if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
				return written, fmt.Errorf("mkdir for %q: %w", f.Path, err)
			}
			if err := os.WriteFile(full, []byte(f.Content), 0o644); err != nil {
				return written, fmt.Errorf("write %q: %w", f.Path, err)
			}
			written = append(written, full)
		}
	}
	return written, nil
}

// WritePromptsSidecar writes a once-per-run <outDir>/_prompts.md documenting
// exactly what governed the run: the effective detector/draft prompt text
// (profile override > analyzer-dir base > rafiki builtin, via
// analyze.Profile's Effective*Prompt), the stage models, and the profile's
// PromptHash — the same value gating --force re-analysis.
func WritePromptsSidecar(outDir string, p *analyze.Profile) error {
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return err
	}
	detector := p.EffectiveDetectorPrompt(analyze.BuiltinDetectorPrompt())
	draft := p.EffectiveDraftPrompt(analyze.BuiltinDraftPrompt())
	hash := p.PromptHash()
	if hash == "" {
		hash = "(none — no prompt overrides/bases configured; skip-key matching uses rafiki's builtin prompts)"
	}
	var md bytes.Buffer
	ew := &errWriter{w: &md}
	ew.printf("# Analyze prompts\n\nprompt_hash: %s\n\n"+
		"## Models\n\n- detector: %s\n- rank: %s\n- draft: %s\n\n"+
		"## Detector prompt\n\n%s\n\n## Draft prompt\n\n%s\n",
		hash, p.DetectorModel, p.RankModel, p.DraftModel, detector, draft)
	if ew.err != nil {
		return ew.err
	}
	return os.WriteFile(filepath.Join(outDir, "_prompts.md"), md.Bytes(), 0o644)
}
