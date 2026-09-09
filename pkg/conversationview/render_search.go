// SPDX-License-Identifier: Apache-2.0

package conversationview

import (
	"fmt"
	"io"

	"go.graveland.dev/rafiki/pkg/clientstate"
	"go.graveland.dev/rafiki/pkg/costfmt"
	"go.graveland.dev/rafiki/pkg/insightstypes"
	"go.graveland.dev/rafiki/pkg/table"
)

// RenderSearch renders search results as a single-line table: a count line,
// then identity columns, per-conversation turn/token aggregates, cache hit
// ratio, total cost, and the first message snippet.
func RenderSearch(w io.Writer, rows []insightstypes.ConversationSummary) error {
	if len(rows) == 0 {
		_, err := fmt.Fprintln(w, "no conversations found")
		return err
	}

	cur := clientstate.LoadScoped(clientstate.Scope{}).Currency

	ew := &errWriter{w: w}
	ew.printf("Conversations (%d)\n", len(rows))

	t := table.New(ew, table.Options{})
	t.Header("ID", "NAME", "CREATED AT", "OWNER", "PERSONA", "SOURCE", "MODEL", "DRIVEN BY",
		"STATUS", "TURNS", "INPUT TOKENS", "OUTPUT TOKENS", "CACHE %", "COST", "FIRST MESSAGE")
	for _, r := range rows {
		t.Row(
			r.ID, r.Name, r.CreatedAt.Local().Format("2006-01-02 15:04"), r.Owner, r.Persona, r.Source, r.Model,
			r.DrivenBy, r.Status, insightstypes.CompactTokens(int64(r.Turns)),
			insightstypes.CompactTokens(r.InputTokens), insightstypes.CompactTokens(r.OutputTokens),
			pct(r.CacheHitRatio), costfmt.Format(r.TotalCostUSD, cur),
			truncateCell(r.FirstMessage),
		)
	}
	ew.render(t)
	return ew.err
}
