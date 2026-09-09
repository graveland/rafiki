// SPDX-License-Identifier: Apache-2.0

// Package agentcli defines the transport-agnostic seam between the CLI and
// backend services, plus the typed renderers for stats/search/export output.
package agentcli

import (
	"cmp"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strings"

	"go.graveland.dev/rafiki/pkg/insights"
	"go.graveland.dev/rafiki/pkg/insightstypes"
	"go.graveland.dev/rafiki/pkg/table"
)

// RenderStats renders a stats bundle as a designed, human-first layout:
// headline volume, owners, a token table with per-path rows, per-model cost
// with a total, and one-line reliability/latency/prefix summaries.
func RenderStats(w io.Writer, st *insights.Stats) error {
	if st == nil || st.Volume.Turns == 0 {
		_, err := fmt.Fprintln(w, "no captured turns match the filter")
		return err
	}

	ew := &errWriter{w: w}
	ew.printf("Conversations: %s    Turns: %s    Cache hit: %s\n",
		insightstypes.CompactTokens(st.Volume.Conversations), insightstypes.CompactTokens(st.Volume.Turns), pct(st.Tokens.CacheHitRatio))

	if len(st.Adoption.PerOwner) > 0 {
		header := []string{"Owner", "Convs", "Turns"}
		rows := make([][]string, 0, len(st.Adoption.PerOwner))
		for _, o := range st.Adoption.PerOwner {
			owner := o.Owner
			if owner == "" {
				owner = "(unattributed)"
			}
			rows = append(rows, []string{owner, insightstypes.CompactTokens(o.Conversations), insightstypes.CompactTokens(o.Turns)})
		}
		ew.table("Owners", []int{1, 2}, header, rows)
	}

	{
		header := []string{"", "Input", "Output", "Cache Read", "Cache Write", "Hit"}
		rows := [][]string{tokenRow("overall", st.Tokens)}
		for _, path := range []string{"proxy", "direct"} {
			if ts, ok := st.ByPath[path]; ok {
				rows = append(rows, tokenRow(path, ts))
			}
		}
		ew.table("Tokens", []int{1, 2, 3, 4, 5}, header, rows)
	}

	if len(st.Cost) > 0 {
		header := []string{"Model", "Turns", "Input", "Output", "Cache Read", "Cost"}
		// The server orders by token volume; cost is what the reader ranks by.
		rows := slices.Clone(st.Cost)
		slices.SortStableFunc(rows, func(a, b insights.CostRow) int {
			if c := cmp.Compare(b.CostUSD, a.CostUSD); c != 0 {
				return c
			}
			return cmp.Compare(b.Turns, a.Turns)
		})
		out := make([][]string, 0, len(rows)+1)
		var total float64
		for _, c := range rows {
			out = append(out, []string{c.Model, insightstypes.CompactTokens(c.Turns), insightstypes.CompactTokens(c.InputTokens),
				insightstypes.CompactTokens(c.OutputTokens), insightstypes.CompactTokens(c.CacheReadTokens), dollars(c.CostUSD)})
			total += c.CostUSD
		}
		// pkg/table has no footer row: TOTAL rides as the last data row.
		out = append(out, []string{"TOTAL", "", "", "", "", dollars(total)})
		ew.table("Cost by model", []int{1, 2, 3, 4, 5}, header, out)
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

// alignRight right-aligns the given columns by padding every cell — header
// included — on the left to the column's widest cell. pkg/table has no
// per-column alignment, so the padding lives in the cell string. The
// columns it is used on carry plain-ASCII numbers (counts, token
// magnitudes, dollar amounts), so len() is the right width measure. It
// mutates its arguments: header and rows are freshly built by the caller.
func alignRight(header []string, rows [][]string, cols ...int) {
	for _, c := range cols {
		w := len(header[c])
		for _, r := range rows {
			if v := len(r[c]); v > w {
				w = v
			}
		}
		header[c] = padLeft(header[c], w)
		for _, r := range rows {
			r[c] = padLeft(r[c], w)
		}
	}
}

func padLeft(s string, w int) string {
	if len(s) >= w {
		return s
	}
	return strings.Repeat(" ", w-len(s)) + s
}

// WriteTable renders one table in the CLI's one shared style: the title as
// a plain line above the table (go-pretty's boxed title row has no
// equivalent in pkg/table), then the header and rows. Columns named in
// numeric are right-aligned by padding. Exported for pkg/agentcli/local,
// which renders the same Findings tables over its own Summary type; rows
// must be header-width or Render panics on the short index.
func WriteTable(w io.Writer, title string, numeric []int, header []string, rows [][]string) error {
	if _, err := fmt.Fprintln(w, title); err != nil {
		return err
	}
	alignRight(header, rows, numeric...)
	b := table.New(w, table.Options{})
	b.Header(header...)
	for _, r := range rows {
		b.Row(r...)
	}
	return b.Render()
}

func tokenRow(label string, ts insights.TokenStats) []string {
	return []string{label, insightstypes.CompactTokens(ts.InputTokens), insightstypes.CompactTokens(ts.OutputTokens),
		insightstypes.CompactTokens(ts.CacheReadTokens), insightstypes.CompactTokens(ts.CacheCreationTokens), pct(ts.CacheHitRatio)}
}

// pct formats a 0..1 ratio as a percentage with one decimal.
func pct(r float64) string { return fmt.Sprintf("%.1f%%", r*100) }

// dollars formats a best-effort USD amount; sub-cent amounts keep enough
// precision to be visible, 0 renders as "-" (unpriced).
func dollars(v float64) string {
	switch {
	case v == 0:
		return "-"
	case v < 0.01:
		return fmt.Sprintf("$%.4f", v)
	default:
		return fmt.Sprintf("$%.2f", v)
	}
}

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

// Dollars formats a USD value for agent output. Exported for
// pkg/agentcli/local. See dollars.
func Dollars(v float64) string {
	return dollars(v)
}
