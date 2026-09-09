// SPDX-License-Identifier: Apache-2.0

package agentcli

import (
	"fmt"
	"io"

	"go.graveland.dev/rafiki/pkg/insights"
	"go.graveland.dev/rafiki/pkg/insightstypes"
)

// RenderSearch renders search results as a single-line table: identity
// columns, then per-conversation turn/token aggregates, then cache hit
// ratio, total cost, and the first message snippet.
func RenderSearch(w io.Writer, rows []insights.ConversationSummary) error {
	if len(rows) == 0 {
		_, err := fmt.Fprintln(w, "no conversations found")
		return err
	}

	header := []string{"Id", "Name", "Created At", "Owner", "Persona", "Source", "Model", "Driven By",
		"Status", "Turns", "Input Tokens", "Output Tokens", "Cache %", "Cost", "First Message"}
	out := make([][]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, []string{
			r.ID, r.Name, r.CreatedAt.Local().Format("2006-01-02 15:04"), r.Owner, r.Persona, r.Source, r.Model,
			r.DrivenBy, r.Status, insightstypes.CompactTokens(int64(r.Turns)),
			insightstypes.CompactTokens(r.InputTokens), insightstypes.CompactTokens(r.OutputTokens),
			pct(r.CacheHitRatio), dollars(r.TotalCostUSD),
			truncateCell(r.FirstMessage),
		})
	}
	return WriteTable(w, fmt.Sprintf("Conversations (%d)", len(rows)), nil, header, out)
}
