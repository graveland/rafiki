// SPDX-License-Identifier: Apache-2.0

package conversationview

import (
	"fmt"
	"io"

	"github.com/jedib0t/go-pretty/v6/table"

	"go.graveland.dev/rafiki/pkg/insightstypes"
)

// RenderSearch renders search results as a rounded table: identity columns,
// then per-conversation turn/token aggregates, then cache hit ratio, total
// cost, and the first message snippet.
func RenderSearch(w io.Writer, rows []insightstypes.ConversationSummary) error {
	if len(rows) == 0 {
		_, err := fmt.Fprintln(w, "no conversations found")
		return err
	}

	t := table.NewWriter()
	t.SetOutputMirror(w)
	t.SetStyle(table.StyleRounded)
	t.SetTitle("Conversations (%d)", len(rows))
	t.AppendHeader(table.Row{"Id", "Name", "Created At", "Owner", "Persona", "Source", "Model", "Driven By",
		"Status", "Turns", "Input Tokens", "Output Tokens", "Cache %", "Cost", "First Message"})
	for _, r := range rows {
		t.AppendRow(table.Row{
			r.ID, r.Name, r.CreatedAt.Local().Format("2006-01-02 15:04"), r.Owner, r.Persona, r.Source, r.Model,
			r.DrivenBy, r.Status, r.Turns, r.InputTokens, r.OutputTokens,
			pct(r.CacheHitRatio), dollars(r.TotalCostUSD),
			truncateCell(r.FirstMessage),
		})
	}
	t.Render()
	return nil
}
