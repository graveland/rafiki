// SPDX-License-Identifier: Apache-2.0

// Package agentcli defines the transport-agnostic seam between the CLI and
// backend services, plus the typed renderers for stats/search/export output.
package conversationview

import (
	"cmp"
	"encoding/json"
	"fmt"
	"io"
	"slices"

	"github.com/jedib0t/go-pretty/v6/table"

	"go.graveland.dev/rafiki/pkg/clientstate"
	"go.graveland.dev/rafiki/pkg/costfmt"
	"go.graveland.dev/rafiki/pkg/insightstypes"
)

// RenderStats renders a stats bundle as a designed, human-first layout:
// headline volume, owners, a token table with per-path rows, per-model cost
// with a total, and one-line reliability/latency/prefix summaries.
func RenderStats(w io.Writer, st *insightstypes.Stats) error {
	if st == nil || st.Volume.Turns == 0 {
		_, err := fmt.Fprintln(w, "no captured turns match the filter")
		return err
	}

	cur := clientstate.Load().Currency

	ew := &errWriter{w: w}
	ew.printf("Conversations: %s    Turns: %s    Cache hit: %s\n",
		insightstypes.CompactTokens(st.Volume.Conversations), insightstypes.CompactTokens(st.Volume.Turns), pct(st.Tokens.CacheHitRatio))

	if len(st.Adoption.PerOwner) > 0 {
		t := newAgentTable(w, "Owners")
		t.AppendHeader(table.Row{"Owner", "Convs", "Turns"})
		for _, o := range st.Adoption.PerOwner {
			owner := o.Owner
			if owner == "" {
				owner = "(unattributed)"
			}
			t.AppendRow(table.Row{owner, insightstypes.CompactTokens(o.Conversations), insightstypes.CompactTokens(o.Turns)})
		}
		t.Render()
	}

	t := newAgentTable(w, "Tokens")
	t.AppendHeader(table.Row{"", "Input", "Output", "Cache Read", "Cache Write", "Hit"})
	t.AppendRow(tokenRow("overall", st.Tokens))
	for _, path := range []string{"proxy", "direct"} {
		if ts, ok := st.ByPath[path]; ok {
			t.AppendRow(tokenRow(path, ts))
		}
	}
	t.Render()

	if len(st.Cost) > 0 {
		t = newAgentTable(w, "Cost by model")
		t.AppendHeader(table.Row{"Model", "Turns", "Input", "Output", "Cache Read", "Cost"})
		// The server orders by token volume; cost is what the reader ranks by.
		rows := slices.Clone(st.Cost)
		slices.SortStableFunc(rows, func(a, b insightstypes.CostRow) int {
			if c := cmp.Compare(b.CostUSD, a.CostUSD); c != 0 {
				return c
			}
			return cmp.Compare(b.Turns, a.Turns)
		})
		var total float64
		for _, c := range rows {
			t.AppendRow(table.Row{c.Model, insightstypes.CompactTokens(c.Turns), insightstypes.CompactTokens(c.InputTokens),
				insightstypes.CompactTokens(c.OutputTokens), insightstypes.CompactTokens(c.CacheReadTokens), costfmt.Format(c.CostUSD, cur)})
			total += c.CostUSD
		}
		t.AppendFooter(table.Row{"TOTAL", "", "", "", "", costfmt.Format(total, cur)})
		t.Render()
	}

	ew.printf("Reliability    %s errors / %s turns (%s) · failover %s · cache waste %s turns / %s tokens\n",
		insightstypes.CompactTokens(st.Failures.Errors), insightstypes.CompactTokens(st.Failures.Turns), pct(st.Failures.ErrorRate),
		pct(st.Failures.FailoverRate), insightstypes.CompactTokens(st.CacheWaste.WastedTurns), insightstypes.CompactTokens(st.CacheWaste.WastedInputTokens))
	ew.printf("Latency        p50 %s · p95 %s · p99 %s\n", secs(st.Latency.P50), secs(st.Latency.P95), secs(st.Latency.P99))
	ew.printf("Prefix cache   %s distinct · reuse %.1f× · %s drifted convs · %s cross-user\n",
		insightstypes.CompactTokens(st.Prefix.DistinctPrefixes), st.Prefix.ReuseRatio,
		insightstypes.CompactTokens(st.Prefix.DriftedConversations), insightstypes.CompactTokens(st.Prefix.CrossUserPrefixes))
	return ew.err
}

func newAgentTable(w io.Writer, title string) table.Writer {
	t := table.NewWriter()
	t.SetOutputMirror(w)
	t.SetStyle(table.StyleRounded)
	t.SetTitle(title)
	return t
}

func tokenRow(label string, ts insightstypes.TokenStats) table.Row {
	return table.Row{label, insightstypes.CompactTokens(ts.InputTokens), insightstypes.CompactTokens(ts.OutputTokens),
		insightstypes.CompactTokens(ts.CacheReadTokens), insightstypes.CompactTokens(ts.CacheCreationTokens), pct(ts.CacheHitRatio)}
}

// pct formats a 0..1 ratio as a percentage with one decimal.
func pct(r float64) string { return fmt.Sprintf("%.1f%%", r*100) }

// secs renders a millisecond latency as seconds with one decimal.
func secs(ms float64) string { return fmt.Sprintf("%.1fs", ms/1000) }

// RenderJSON writes v as JSON, indented when indent is true and compact
// otherwise.
func RenderJSON(w io.Writer, v any, indent bool) error {
	var (
		b   []byte
		err error
	)
	if indent {
		b, err = json.MarshalIndent(v, "", "  ")
	} else {
		b, err = json.Marshal(v)
	}
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(w, string(b))
	return err
}
