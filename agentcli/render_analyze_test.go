// SPDX-License-Identifier: Apache-2.0

package agentcli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/timescale/rafiki/analyze"
	"github.com/timescale/rafiki/store"
)

func TestRenderProgressLine(t *testing.T) {
	var b bytes.Buffer
	if err := RenderProgress(&b, &Progress{ConversationID: "c1", State: StateFailed, Detail: "detect: boom"}); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	if !strings.Contains(out, "c1") || !strings.Contains(out, "failed") || !strings.Contains(out, "boom") {
		t.Errorf("failure reason must be visible: %q", out)
	}
}

func TestRenderAnalyzeSummary(t *testing.T) {
	s := &Summary{
		Ranked: []RankedFindingWithDraft{
			{
				RankedFinding: analyze.RankedFinding{
					Finding:     analyze.Finding{Axis: "skill-gap", Title: "missing vacuum skill", TopicKey: "missing-vacuum-skill"},
					Occurrences: 3, Score: 42000,
				},
				Draft: &analyze.SkillEdit{
					FindingTitle: "missing vacuum skill",
					Files:        []analyze.SkillFile{{Path: "test.md", Content: "test"}},
				},
			},
			{
				RankedFinding: analyze.RankedFinding{
					Finding:     analyze.Finding{Axis: "grind", Title: "loop inefficiency", TopicKey: "loop-inefficiency"},
					Occurrences: 1, Score: 5000,
				},
			},
		},
		Analyzed: 4, Skipped: 1, Failed: 0, Remaining: 2, Population: 7,
		Totals: Totals{InputTokens: 1000, OutputTokens: 500, CostUSD: 0.5},
	}
	var b bytes.Buffer
	if err := RenderAnalyzeSummary(&b, s); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	for _, want := range []string{"missing vacuum skill", "skill-gap", "Analyzed 4", "remaining 2"} {
		if !strings.Contains(out, want) {
			t.Errorf("summary missing %q:\n%s", want, out)
		}
	}

	// Verify Draft column: "missing vacuum skill" has draft, "loop inefficiency" does not.
	lines := strings.Split(out, "\n")
	var foundDraftYes, foundDraftEmpty bool
	for _, line := range lines {
		if strings.Contains(line, "missing vacuum skill") {
			if strings.Contains(line, "yes") {
				foundDraftYes = true
			} else {
				t.Errorf("finding with draft should show 'yes': %q", line)
			}
		} else if strings.Contains(line, "loop inefficiency") {
			// Should not have "yes" in draft column (or at least not on the same line).
			if !strings.Contains(line, "yes") || !strings.HasSuffix(strings.TrimRight(line, "\n"), "yes") {
				foundDraftEmpty = true
			} else {
				t.Errorf("finding without draft should not show 'yes': %q", line)
			}
		}
	}
	if !foundDraftYes {
		t.Errorf("should find a finding with 'yes' draft marker:\n%s", out)
	}
	if !foundDraftEmpty {
		t.Errorf("should find a finding without 'yes' draft marker:\n%s", out)
	}
}

// TestRenderAnalyzeSummary_SameTitleDifferentAxis guards against a Title-keyed
// side map for drafts: analyze.Rank groups by (Axis, SkillName‖TopicKey) and
// only tie-breaks equal titles by axis, so two ranked findings CAN share a
// Title while differing in axis. RankedFindingWithDraft attaches the draft to
// the finding itself, so only the entry that was actually drafted shows the
// marker — a Title-keyed map would show it on both.
func TestRenderAnalyzeSummary_SameTitleDifferentAxis(t *testing.T) {
	const sharedTitle = "duplicate title"
	s := &Summary{
		Ranked: []RankedFindingWithDraft{
			{
				RankedFinding: analyze.RankedFinding{
					Finding:     analyze.Finding{Axis: "skill-gap", Title: sharedTitle, TopicKey: "topic-a"},
					Occurrences: 2, Score: 10000,
				},
				Draft: &analyze.SkillEdit{FindingTitle: sharedTitle, Files: []analyze.SkillFile{{Path: "a.md"}}},
			},
			{
				RankedFinding: analyze.RankedFinding{
					Finding:     analyze.Finding{Axis: "grind", Title: sharedTitle, TopicKey: "topic-b"},
					Occurrences: 1, Score: 3000,
				},
				// No Draft: this one was never drafted.
			},
		},
		Analyzed: 2, Population: 2,
	}
	var b bytes.Buffer
	if err := RenderAnalyzeSummary(&b, s); err != nil {
		t.Fatal(err)
	}

	lines := strings.Split(b.String(), "\n")
	var withTitle []string
	for _, line := range lines {
		if strings.Contains(line, sharedTitle) {
			withTitle = append(withTitle, line)
		}
	}
	if len(withTitle) != 2 {
		t.Fatalf("want 2 rows for the shared title, got %d: %v", len(withTitle), withTitle)
	}

	yesCount := 0
	for _, line := range withTitle {
		cols := strings.Split(line, "│")
		draftCol := strings.TrimSpace(cols[len(cols)-2]) // last column before the closing border
		if draftCol == "yes" {
			yesCount++
		}
	}
	if yesCount != 1 {
		t.Errorf("exactly one same-titled row should show the draft marker, got %d:\n%s", yesCount, b.String())
	}
}

func TestWriteArtifactsAnalysis(t *testing.T) {
	dir := t.TempDir()
	a := &analyze.Analysis{ConversationID: "c1", Outcome: "did a thing",
		Verdicts: map[string]string{"grind": "finding"},
		Findings: []analyze.Finding{{Axis: "grind", Title: "loop", Evidence: []analyze.TurnCite{{Ordinal: 3, Quote: "retry"}}}}}
	if err := WriteArtifacts(dir, "c1", "detect", a); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"c1.detect.json", "c1.detect.md"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("missing artifact %s: %v", name, err)
		}
	}
	md, err := os.ReadFile(filepath.Join(dir, "c1.detect.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(md), "did a thing") || !strings.Contains(string(md), "retry") {
		t.Errorf("md artifact should carry outcome + evidence: %s", md)
	}
}

func TestWriteArtifactsRejectsTraversal(t *testing.T) {
	dir := t.TempDir()
	if err := WriteArtifacts(dir, "../escape", "detect", &analyze.Analysis{}); err == nil {
		t.Fatal("conversation ids that change under filepath.Base must be rejected")
	}
}

func TestWritePromptsSidecar(t *testing.T) {
	dir := t.TempDir()
	p := &analyze.Profile{DetectorModel: "claude-haiku-4-5", DetectorPromptExtra: "also check X"}
	p.Defaults()
	if err := WritePromptsSidecar(dir, p); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "_prompts.md"))
	if err != nil {
		t.Fatal(err)
	}
	out := string(b)
	if !strings.Contains(out, "also check X") {
		t.Error("sidecar must include the profile's extra")
	}
	if !strings.Contains(out, "detector pass") { // from the builtin base
		t.Error("sidecar must include the effective base prompt text")
	}
	if !strings.Contains(out, p.PromptHash()) {
		t.Error("sidecar must record the prompt hash")
	}
}

func TestWriteSkillEditsWritesDraftedFiles(t *testing.T) {
	dir := t.TempDir()
	s := &Summary{
		Ranked: []RankedFindingWithDraft{
			{
				RankedFinding: analyze.RankedFinding{Finding: analyze.Finding{Axis: "skill-gap", Title: "missing vacuum skill"}},
				Draft: &analyze.SkillEdit{
					FindingTitle: "missing vacuum skill",
					Files: []analyze.SkillFile{
						{Path: "skills/vacuum-tuning/SKILL.md", Content: "# Vacuum Tuning\n"},
					},
				},
			},
			{
				// No Draft: never reached the top-N, or its own Draft call failed.
				RankedFinding: analyze.RankedFinding{Finding: analyze.Finding{Axis: "grind", Title: "loop inefficiency"}},
			},
		},
	}

	written, err := WriteSkillEdits(dir, s)
	if err != nil {
		t.Fatal(err)
	}
	if len(written) != 1 {
		t.Fatalf("want exactly 1 file written (only the ranked finding with a Draft), got %d: %v", len(written), written)
	}

	want := filepath.Join(dir, "skills/vacuum-tuning/SKILL.md")
	wantAbs, err := filepath.Abs(want)
	if err != nil {
		t.Fatal(err)
	}
	if written[0] != wantAbs {
		t.Errorf("wrote path %q, want %q", written[0], wantAbs)
	}
	b, err := os.ReadFile(wantAbs)
	if err != nil {
		t.Fatalf("expected file to exist on disk: %v", err)
	}
	if string(b) != "# Vacuum Tuning\n" {
		t.Errorf("file content mismatch: %q", b)
	}
}

func TestWriteSkillEditsRejectsPathTraversal(t *testing.T) {
	dir := t.TempDir()
	s := &Summary{
		Ranked: []RankedFindingWithDraft{
			{
				RankedFinding: analyze.RankedFinding{Finding: analyze.Finding{Axis: "grind", Title: "escape attempt"}},
				Draft: &analyze.SkillEdit{
					FindingTitle: "escape attempt",
					Files:        []analyze.SkillFile{{Path: "../../etc/escaped.md", Content: "pwned"}},
				},
			},
		},
	}
	written, err := WriteSkillEdits(dir, s)
	if err == nil {
		t.Fatal("a skill edit file path escaping outDir must be rejected")
	}
	if len(written) != 0 {
		t.Errorf("no files should have been written before the traversal was caught: %v", written)
	}
	if _, statErr := os.Stat(filepath.Join(filepath.Dir(filepath.Dir(dir)), "escaped.md")); statErr == nil {
		t.Error("traversal file must not have been written outside outDir")
	}
}

func TestRenderFindings(t *testing.T) {
	rows := []store.FindingRow{
		{
			ID:                    "f1",
			Axis:                  "skill-gap",
			SkillName:             "distributed-tracing",
			Title:                 "missing tracing setup",
			ExpectedSavingsTokens: 12000,
			Status:                "open",
		},
		{
			ID:                    "f2",
			Axis:                  "grind",
			SkillName:             "query-optimization",
			Title:                 "n+1 query pattern",
			ExpectedSavingsTokens: 45000,
			Status:                "actioned",
		},
	}
	var b bytes.Buffer
	if err := RenderFindings(&b, rows); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	for _, want := range []string{
		"skill-gap", "distributed-tracing", "missing tracing setup",
		"grind", "query-optimization", "n+1 query pattern",
		"12,000", "45,000", "open", "actioned",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("findings output missing %q:\n%s", want, out)
		}
	}
}
