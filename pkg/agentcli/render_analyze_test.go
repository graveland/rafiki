// SPDX-License-Identifier: Apache-2.0

package agentcli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.graveland.dev/rafiki/pkg/analyze"
	"go.graveland.dev/rafiki/pkg/store"

	"github.com/multigres/testkit/assert"
)

func TestRenderProgressLine(t *testing.T) {
	c := assert.NewCollecting(t)
	var b bytes.Buffer
	c.Require().NoError(RenderProgress(&b, &Progress{ConversationID: "c1", State: StateFailed, Detail: "detect: boom"}))
	out := b.String()
	c.False(!strings.Contains(out, "c1") || !strings.Contains(out, "failed") || !strings.Contains(out, "boom"), "failure reason must be visible: %q", out)
}

func TestRenderAnalyzeSummary(t *testing.T) {
	c := assert.NewCollecting(t)
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
	c.Require().NoError(RenderAnalyzeSummary(&b, s))
	out := b.String()
	for _, want := range []string{"missing vacuum skill", "skill-gap", "Analyzed 4", "remaining 2"} {
		c.StrContains(out, want, "summary missing")
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
	c.True(foundDraftYes, "should find a finding with 'yes' draft marker:\n%s", out)
	c.True(foundDraftEmpty, "should find a finding without 'yes' draft marker:\n%s", out)
}

// TestRenderAnalyzeSummary_SameTitleDifferentAxis guards against a Title-keyed
// side map for drafts: analyze.Rank groups by (Axis, SkillName‖TopicKey) and
// only tie-breaks equal titles by axis, so two ranked findings CAN share a
// Title while differing in axis. RankedFindingWithDraft attaches the draft to
// the finding itself, so only the entry that was actually drafted shows the
// marker — a Title-keyed map would show it on both.
func TestRenderAnalyzeSummary_SameTitleDifferentAxis(t *testing.T) {
	c := assert.NewCollecting(t)
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
	c.Require().NoError(RenderAnalyzeSummary(&b, s))

	lines := strings.Split(b.String(), "\n")
	var withTitle []string
	for _, line := range lines {
		if strings.Contains(line, sharedTitle) {
			withTitle = append(withTitle, line)
		}
	}
	c.Require().Len(withTitle, 2, "want 2 rows for the shared title, got %d", len(withTitle))

	yesCount := 0
	for _, line := range withTitle {
		cols := strings.Split(line, "│")
		draftCol := strings.TrimSpace(cols[len(cols)-2]) // last column before the closing border
		if draftCol == "yes" {
			yesCount++
		}
	}
	c.Eq(1, yesCount, "exactly one same-titled row should show the draft marker, got %d:\n%s", yesCount, b.String())
}

func TestWriteArtifactsAnalysis(t *testing.T) {
	c := assert.NewCollecting(t)
	dir := t.TempDir()
	a := &analyze.Analysis{ConversationID: "c1", Outcome: "did a thing",
		Verdicts: map[string]string{"grind": "finding"},
		Findings: []analyze.Finding{{Axis: "grind", Title: "loop", Evidence: []analyze.TurnCite{{Ordinal: 3, Quote: "retry"}}}}}
	c.Require().NoError(WriteArtifacts(dir, "c1", "detect", a))
	for _, name := range []string{"c1.detect.json", "c1.detect.md"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("missing artifact %s: %v", name, err)
		}
	}
	md, err := os.ReadFile(filepath.Join(dir, "c1.detect.md"))
	c.Require().NoError(err)
	c.False(!strings.Contains(string(md), "did a thing") || !strings.Contains(string(md), "retry"), "md artifact should carry outcome + evidence: %s", md)
}

func TestWriteArtifactsRejectsTraversal(t *testing.T) {
	dir := t.TempDir()
	assert.NewAborting(t).Error(WriteArtifacts(dir, "../escape", "detect", &analyze.Analysis{}), "conversation ids that change under filepath.Base must be rejected")
}

func TestWritePromptsSidecar(t *testing.T) {
	c := assert.NewCollecting(t)
	dir := t.TempDir()
	p := &analyze.Profile{DetectorModel: "claude-haiku-4-5", DetectorPromptExtra: "also check X"}
	p.Defaults()
	c.Require().NoError(WritePromptsSidecar(dir, p))
	b, err := os.ReadFile(filepath.Join(dir, "_prompts.md"))
	c.Require().NoError(err)
	out := string(b)
	c.StrContains(out, "also check X", "sidecar must include the profile's extra")
	c.StrContains(out, "detector pass", "sidecar must include the effective base prompt text") // from the builtin base
	c.StrContains(out, p.PromptHash(), "sidecar must record the prompt hash")
}

func TestWriteSkillEditsWritesDraftedFiles(t *testing.T) {
	c := assert.NewCollecting(t)
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
	c.Require().NoError(err)
	c.Require().Len(written, 1, "want exactly 1 file written (only the ranked finding with a Draft), got %d", len(written))

	want := filepath.Join(dir, "skills/vacuum-tuning/SKILL.md")
	wantAbs, err := filepath.Abs(want)
	c.Require().NoError(err)
	c.Eq(wantAbs, written[0], "wrote path")
	b, err := os.ReadFile(wantAbs)
	c.Require().NoError(err, "expected file to exist on disk")
	c.Eq("# Vacuum Tuning\n", string(b), "file content mismatch: %q", b)
}

func TestWriteSkillEditsRejectsPathTraversal(t *testing.T) {
	c := assert.NewCollecting(t)
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
	c.Require().Error(err, "a skill edit file path escaping outDir must be rejected")
	c.Empty(written, "no files should have been written before the traversal was caught")
	_, statErr := os.Stat(filepath.Join(filepath.Dir(filepath.Dir(dir)), "escaped.md"))
	c.Error(statErr, "traversal file must not have been written outside outDir")
}

func TestRenderFindings(t *testing.T) {
	c := assert.NewCollecting(t)
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
	c.Require().NoError(RenderFindings(&b, rows))
	out := b.String()
	for _, want := range []string{
		"skill-gap", "distributed-tracing", "missing tracing setup",
		"grind", "query-optimization", "n+1 query pattern",
		"12,000", "45,000", "open", "actioned",
	} {
		c.StrContains(out, want, "findings output missing")
	}
}
