// SPDX-License-Identifier: Apache-2.0

// Package conversationview defines the transport-agnostic seam between the
// CLI and backend services, plus the typed renderers for stats/search/export
// output.
package conversationview

import (
	"cmp"
	"encoding/json"
	"fmt"
	"io"
	"slices"

	"go.graveland.dev/rafiki/pkg/clientstate"
	"go.graveland.dev/rafiki/pkg/costfmt"
	"go.graveland.dev/rafiki/pkg/insightstypes"
	"go.graveland.dev/rafiki/pkg/table"
)

// RenderStats renders a stats bundle as a designed, human-first layout:
// headline volume, owners, a token table with per-path rows, per-model cost
// with a total, and one-line reliability/latency/prefix summaries.
func RenderStats(w io.Writer, st *insightstypes.Stats) error {
	if st == nil || st.Volume.Turns == 0 {
		_, err := fmt.Fprintln(w, "no captured turns match the filter")
		return err
	}

	cur := clientstate.LoadScoped(clientstate.Scope{}).Currency

	ew := &errWriter{w: w}
	ew.printf("Conversations: %s    Turns: %s    Cache hit: %s\n",
		insightstypes.CompactTokens(st.Volume.Conversations), insightstypes.CompactTokens(st.Volume.Turns), pct(st.Tokens.CacheHitRatio))

	if len(st.Adoption.PerOwner) > 0 {
		t := newTable(ew, "Owners")
		t.Header("OWNER", "CONVS", "TURNS")
		for _, o := range st.Adoption.PerOwner {
			owner := o.Owner
			if owner == "" {
				owner = "(unattributed)"
			}
			t.Row(owner, insightstypes.CompactTokens(o.Conversations), insightstypes.CompactTokens(o.Turns))
		}
		ew.render(t)
	}

	t := newTable(ew, "Tokens")
	t.Header("", "INPUT", "OUTPUT", "CACHE READ", "CACHE WRITE", "HIT")
	t.Row(tokenRow("overall", st.Tokens)...)
	for _, path := range []string{"proxy", "direct"} {
		if ts, ok := st.ByPath[path]; ok {
			t.Row(tokenRow(path, ts)...)
		}
	}
	ew.render(t)

	if len(st.Cost) > 0 {
		t = newTable(ew, "Cost by model")
		t.Header("MODEL", "TURNS", "INPUT", "OUTPUT", "CACHE READ", "COST")
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
			t.Row(c.Model, insightstypes.CompactTokens(c.Turns), insightstypes.CompactTokens(c.InputTokens),
				insightstypes.CompactTokens(c.OutputTokens), insightstypes.CompactTokens(c.CacheReadTokens), costfmt.Format(c.CostUSD, cur))
			total += c.CostUSD
		}
		t.Row("TOTAL", "", "", "", "", costfmt.Format(total, cur))
		ew.render(t)
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

// newTable prints a section title on its own line and returns a builder
// writing through ew. go-pretty rendered the title as a row inside the box;
// pkg/table has no title, so the line sits above the table.
func newTable(ew *errWriter, title string) *table.Builder {
	ew.println(title)
	return table.New(ew, table.Options{})
}

func tokenRow(label string, ts insightstypes.TokenStats) []string {
	return []string{label, insightstypes.CompactTokens(ts.InputTokens), insightstypes.CompactTokens(ts.OutputTokens),
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
