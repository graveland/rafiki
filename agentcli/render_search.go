// SPDX-License-Identifier: Apache-2.0

package agentcli

import (
	"fmt"
	"io"

	"github.com/jedib0t/go-pretty/v6/table"

	"git.graveland.dev/brent/rafiki/insights"
)

// RenderSearch renders search results as a rounded table: identity columns,
// then per-conversation turn/token aggregates, then the first message
// snippet.
func RenderSearch(w io.Writer, rows []insights.ConversationSummary) error {
	if len(rows) == 0 {
		_, err := fmt.Fprintln(w, "no conversations found")
		return err
	}

	t := table.NewWriter()
	t.SetOutputMirror(w)
	t.SetStyle(table.StyleRounded)
	t.SetTitle("Conversations (%d)", len(rows))
	t.AppendHeader(table.Row{"Id", "Created At", "Owner", "Persona", "Source", "Model", "Driven By",
		"Status", "Turns", "Input Tokens", "Output Tokens", "Cache Read Tokens", "First Message"})
	for _, r := range rows {
		t.AppendRow(table.Row{
			r.ID, r.CreatedAt.Local().Format("2006-01-02 15:04"), r.Owner, r.Persona, r.Source, r.Model,
			r.DrivenBy, r.Status, r.Turns, r.InputTokens, r.OutputTokens, r.CacheReadTokens,
			truncateCell(r.FirstMessage),
		})
	}
	t.Render()
	return nil
}
