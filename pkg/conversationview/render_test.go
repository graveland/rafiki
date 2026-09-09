// SPDX-License-Identifier: Apache-2.0

package conversationview

import (
	"bytes"
	"os"
	"strings"
	"testing"
	"time"

	"go.graveland.dev/rafiki/pkg/clientstate"
	"go.graveland.dev/rafiki/pkg/insightstypes"
)

// The renderers read client state (display currency) directly, so the whole
// package runs with XDG_STATE_HOME pointed at a temp dir: a developer's real
// client-state.json must not decide what these tests assert.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "conversationview-state")
	if err != nil {
		panic(err)
	}
	os.Setenv("XDG_STATE_HOME", dir)
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

func sampleRenderStats() *insightstypes.Stats {
	return &insightstypes.Stats{
		Volume:   insightstypes.VolumeStats{Conversations: 3, Turns: 17},
		Adoption: insightstypes.AdoptionStats{PerOwner: []insightstypes.OwnerCount{{Owner: "alice", Conversations: 2, Turns: 11}}},
		Tokens:   insightstypes.TokenStats{InputTokens: 1000, OutputTokens: 200, CacheReadTokens: 3000, CacheCreationTokens: 500, CacheHitRatio: 0.75},
		Cost: []insightstypes.CostRow{
			{Model: "claude-sonnet-5", Turns: 12, InputTokens: 800, CostUSD: 0.042},
			{Model: "claude-haiku-4-5", Turns: 5, InputTokens: 200},
		},
		ByPath: map[string]insightstypes.TokenStats{
			"proxy": {InputTokens: 700, OutputTokens: 140, CacheReadTokens: 2400, CacheHitRatio: 0.77},
		},
	}
}

func sampleRenderSearch() []insightstypes.ConversationSummary {
	return []insightstypes.ConversationSummary{{
		ID: "conv-abc", Owner: "alice", Persona: "reviewer", Source: "cli", Model: "claude-sonnet-5",
		Status: "completed", DrivenBy: "client", CreatedAt: time.Unix(1716000000, 0),
		Turns: 7, InputTokens: 900, OutputTokens: 120, CacheReadTokens: 2400,
		CacheHitRatio: 0.727, TotalCostUSD: 0.042,
		FirstMessage: "why do the stats disagree",
	}}
}

// The migration onto pkg/table pins the one shared style: single-line
// borders. Rounded runes (go-pretty's StyleRounded) must be gone from every
// table, and no color is requested, so no ANSI escapes either.
func TestRenderTablesUseSingleLineBorders(t *testing.T) {
	for name, out := range map[string]string{
		"stats":  renderToString(t, func(w *bytes.Buffer) { _ = RenderStats(w, sampleRenderStats()) }),
		"search": renderToString(t, func(w *bytes.Buffer) { _ = RenderSearch(w, sampleRenderSearch()) }),
	} {
		if !strings.Contains(out, "┌") || !strings.Contains(out, "│") || !strings.Contains(out, "└") {
			t.Errorf("%s: expected single-line borders, got:\n%s", name, out)
		}
		for _, rounded := range []string{"╭", "╮", "╰", "╯"} {
			if strings.Contains(out, rounded) {
				t.Errorf("%s: rounded border rune %q survived the migration:\n%s", name, rounded, out)
			}
		}
		if strings.ContainsRune(out, '\x1b') {
			t.Errorf("%s: no color is requested, so no ANSI escapes expected:\n%q", name, out)
		}
	}
}

// Cell strings are computed exactly as before the migration; only the
// renderer underneath changed.
func TestRenderStatsKeepsContentAndStructure(t *testing.T) {
	out := renderToString(t, func(w *bytes.Buffer) { _ = RenderStats(w, sampleRenderStats()) })
	for _, want := range []string{
		"Conversations: 3", "Owners", "alice", "1.0K", "75.0%", "Tokens",
		"Cost by model", "claude-sonnet-5", "claude-haiku-4-5", "TOTAL", "$0.04",
		"Reliability", "Latency", "Prefix cache",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("stats output missing %q:\n%s", want, out)
		}
	}
	// Section titles sit on their own line above each table, and the TOTAL
	// row closes the cost table (pkg/table has no footer row).
	for _, line := range []string{"Owners", "Tokens", "Cost by model"} {
		if !hasOwnLine(out, line) {
			t.Errorf("expected %q as a standalone title line:\n%s", line, out)
		}
	}
	var totalRow bool
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(strings.Trim(line, "│ "), "TOTAL") && strings.Contains(line, "$0.04") {
			totalRow = true
		}
	}
	if !totalRow {
		t.Errorf("expected a TOTAL row carrying the summed cost:\n%s", out)
	}
}

func TestRenderSearchKeepsContentAndStructure(t *testing.T) {
	out := renderToString(t, func(w *bytes.Buffer) { _ = RenderSearch(w, sampleRenderSearch()) })
	for _, want := range []string{
		"Conversations (1)", "conv-abc", "alice", "reviewer", "claude-sonnet-5",
		"2024-05-17 20:40", "900", "72.7%", "why do the stats disagree",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("search output missing %q:\n%s", want, out)
		}
	}
	if !hasOwnLine(out, "Conversations (1)") {
		t.Errorf("expected the count line standalone above the table:\n%s", out)
	}
}

func TestRenderEmptyInputsUnchanged(t *testing.T) {
	var b bytes.Buffer
	if err := RenderStats(&b, &insightstypes.Stats{}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.String(), "no captured turns") {
		t.Errorf("empty stats should say so, got: %q", b.String())
	}
	b.Reset()
	if err := RenderSearch(&b, nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(b.String(), "no conversations found") {
		t.Errorf("empty search should say so, got: %q", b.String())
	}
}

// The scoped currency preference must reach cost cells: save one with a
// distinctive rate, render, and restore the unset state.
func TestRenderSearchFormatsCostInScopedCurrency(t *testing.T) {
	clientstate.SaveScoped(clientstate.Scope{}, clientstate.State{
		Currency: &clientstate.Currency{Code: "CAD", Rate: 2},
	})
	defer clientstate.SaveScoped(clientstate.Scope{}, clientstate.State{})

	out := renderToString(t, func(w *bytes.Buffer) { _ = RenderSearch(w, sampleRenderSearch()) })
	if !strings.Contains(out, "$0.08 CAD") {
		t.Errorf("expected CAD-converted cost cell, got:\n%s", out)
	}
}

func renderToString(t *testing.T, f func(*bytes.Buffer)) string {
	t.Helper()
	var b bytes.Buffer
	f(&b)
	return b.String()
}

func hasOwnLine(out, line string) bool {
	for _, l := range strings.Split(out, "\n") {
		if strings.TrimSpace(l) == line {
			return true
		}
	}
	return false
}
