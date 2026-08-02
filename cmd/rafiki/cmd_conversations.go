package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/spf13/cobra"

	"go.graveland.dev/rafiki/pkg/agentcli"
	"go.graveland.dev/rafiki/pkg/client"
	"go.graveland.dev/rafiki/pkg/insights"
	"go.graveland.dev/rafiki/pkg/protocol"
)

func newConversationsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "conversations",
		Short: "Query persisted conversation history from the daemon's agent database",
		Long: `Global stats, search, and transcript export over the conversations schema
the daemon persists to when RAFIKI_DB is set. Unlike "rafiki search" (live,
in-memory, currently-running children only), these query history in Postgres
regardless of whether anything is still running.

Output matches "rafikid agent stats|search|export" exactly — same queries, same
renderers, only the transport differs. --output controls the format: tables at a
terminal, JSON when piped.`,
	}
	cmd.AddCommand(
		newConversationsStatsCmd(),
		newConversationsSearchCmd(),
		newConversationsExportCmd(),
	)
	return cmd
}

// bindConversationFilterFlags registers the filter flags shared by stats and
// search, matching rafiki agent's flag names exactly.
func bindConversationFilterFlags(cmd *cobra.Command) {
	cmd.Flags().String("since", "", "RFC3339 timestamp or duration like 24h")
	cmd.Flags().String("until", "", "RFC3339 timestamp or duration like 24h")
	cmd.Flags().String("owner", "", "filter by owner")
	cmd.Flags().String("persona", "", "filter by persona")
	cmd.Flags().String("source", "", "filter by source")
	cmd.Flags().String("model", "", "filter by model")
	cmd.Flags().String("path", "", `filter by path ("proxy" or "direct")`)
}

// conversationFilterVals reads the shared filter flags into an
// agentcli.FilterVals, the same flag-value bag rafiki agent binds from.
func conversationFilterVals(cmd *cobra.Command) agentcli.FilterVals {
	v := agentcli.FilterVals{}
	v.Since, _ = cmd.Flags().GetString("since")
	v.Until, _ = cmd.Flags().GetString("until")
	v.Owner, _ = cmd.Flags().GetString("owner")
	v.Persona, _ = cmd.Flags().GetString("persona")
	v.Source, _ = cmd.Flags().GetString("source")
	v.Model, _ = cmd.Flags().GetString("model")
	v.Path, _ = cmd.Flags().GetString("path")
	return v
}

// unixOrZero converts a resolved filter timestamp to the wire's Unix-seconds
// convention, where 0 means unset.
func unixOrZero(t *time.Time) int64 {
	if t == nil {
		return 0
	}
	return t.Unix()
}

// conversationsMode maps the global --output flag onto the render mode shared
// with `rafikid agent`. "auto" stays TTY-aware, matching every other rafiki
// command: tables at a terminal, JSON when piped so `| jq` keeps working.
func conversationsMode(cmd *cobra.Command) agentcli.Mode {
	mode, _ := outputOpts(cmd)
	if mode == outputTable {
		return agentcli.ModeTable
	}
	return agentcli.ModeJSON
}

// renderConversationResponse decodes a ctrl_conversation_* payload into the
// domain type the daemon marshalled it from (pkg/control/dispatch.go hands
// *insights.Stats and friends straight to okResponse) and hands it to the same
// renderer `rafikid agent` uses. Both surfaces read the same rows through the
// same pkg/insights queries; routing both through agentcli.Render is what keeps
// them from presenting those rows differently.
func renderConversationResponse[T any](w io.Writer, m agentcli.Mode, resp *protocol.Response, table func(io.Writer, T) error) error {
	var v T
	if err := decodeConversationData(resp, &v); err != nil {
		return err
	}
	return agentcli.Render(w, v, m, table)
}

// decodeConversationData decodes a ctrl_conversation_* payload into v. Split
// out of renderConversationResponse for search, whose payload is an envelope
// around the value it renders rather than the value itself.
func decodeConversationData[T any](resp *protocol.Response, v *T) error {
	if err := json.Unmarshal(resp.Data, v); err != nil {
		return fmt.Errorf("decode %s response: %w", resp.Command, err)
	}
	return nil
}

// ─── stats ──────────────────────────────────────────────────────────────────

func newConversationsStatsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "stats [conv-id]",
		Short: "Global or per-conversation stats over persisted history",
		Args:  cobra.MaximumNArgs(1),
		RunE:  runConversationsStats,
	}
	bindConversationFilterFlags(cmd)
	return cmd
}

func runConversationsStats(cmd *cobra.Command, args []string) error {
	c := mustDial(cmd)
	defer c.Close()

	req := protocol.ConversationStatsRequest{Type: protocol.TypeCtrlConversationStats}
	if len(args) == 1 {
		req.ConversationID = args[0]
	} else {
		f, err := agentcli.BindStatsFilter(conversationFilterVals(cmd))
		if err != nil {
			return err
		}
		req.SinceUnix = unixOrZero(f.Since)
		req.UntilUnix = unixOrZero(f.Until)
		req.Owner = f.Owner
		req.Persona = f.Persona
		req.Source = f.Source
		req.Model = f.Model
		req.Path = string(f.Path)
	}

	resp, err := c.Request(cmdCtx(cmd), req)
	if err != nil {
		return err
	}
	if !resp.Success {
		return fmt.Errorf("ctrl_conversation_stats: %s", client.FormatError(resp))
	}
	return renderConversationResponse(os.Stdout, conversationsMode(cmd), resp, agentcli.RenderStats)
}

// ─── search ─────────────────────────────────────────────────────────────────

func newConversationsSearchCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "search",
		Short: "Search persisted conversation history",
		Args:  cobra.NoArgs,
		RunE:  runConversationsSearch,
	}
	bindConversationFilterFlags(cmd)
	cmd.Flags().String("status", "", "filter by status")
	cmd.Flags().Int64("min-tokens", 0, "minimum total tokens")
	cmd.Flags().String("text", "", "full-text search over first messages")
	cmd.Flags().Int("limit", 0, "max results (0 = default)")
	return cmd
}

func runConversationsSearch(cmd *cobra.Command, _ []string) error {
	c := mustDial(cmd)
	defer c.Close()

	v := conversationFilterVals(cmd)
	v.Status, _ = cmd.Flags().GetString("status")
	v.MinTokens, _ = cmd.Flags().GetInt64("min-tokens")
	v.Text, _ = cmd.Flags().GetString("text")
	v.Limit, _ = cmd.Flags().GetInt("limit")

	f, err := agentcli.BindSearchFilter(v)
	if err != nil {
		return err
	}

	req := protocol.ConversationSearchRequest{
		Type:      protocol.TypeCtrlConversationSearch,
		SinceUnix: unixOrZero(f.Since),
		UntilUnix: unixOrZero(f.Until),
		Owner:     f.Owner,
		Persona:   f.Persona,
		Source:    f.Source,
		Model:     f.Model,
		Path:      string(f.Path),
		Status:    f.Status,
		MinTokens: f.MinTokens,
		Text:      f.Text,
		Limit:     f.Limit,
	}

	resp, err := c.Request(cmdCtx(cmd), req)
	if err != nil {
		return err
	}
	if !resp.Success {
		return fmt.Errorf("ctrl_conversation_search: %s", client.FormatError(resp))
	}
	return renderConversationSearch(os.Stdout, conversationsMode(cmd), resp)
}

// renderConversationSearch unwraps ctrl_conversation_search's payload before
// rendering. Alone among the three verbs it wraps its rows in a {"rows": [...]}
// envelope (control-protocol.md §6.18) where stats and export send the domain
// value bare, so it cannot go through renderConversationResponse. Unwrapping
// here keeps both the table and the JSON matching `rafikid agent search`, which
// prints the rows themselves.
func renderConversationSearch(w io.Writer, m agentcli.Mode, resp *protocol.Response) error {
	var payload struct {
		Rows []insights.ConversationSummary `json:"rows"`
	}
	if err := decodeConversationData(resp, &payload); err != nil {
		return err
	}
	return agentcli.Render(w, payload.Rows, m, agentcli.RenderSearch)
}

// ─── export ─────────────────────────────────────────────────────────────────

func newConversationsExportCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "export <conv-id>",
		Short: "Export a persisted conversation's full transcript",
		Args:  cobra.ExactArgs(1),
		RunE:  runConversationsExport,
	}
}

func runConversationsExport(cmd *cobra.Command, args []string) error {
	c := mustDial(cmd)
	defer c.Close()

	req := protocol.ConversationExportRequest{
		Type:           protocol.TypeCtrlConversationExport,
		ConversationID: args[0],
	}

	resp, err := c.Request(cmdCtx(cmd), req)
	if err != nil {
		return err
	}
	if !resp.Success {
		return fmt.Errorf("ctrl_conversation_export: %s", client.FormatError(resp))
	}
	return renderConversationResponse(os.Stdout, conversationsMode(cmd), resp, agentcli.RenderTranscriptMD)
}
