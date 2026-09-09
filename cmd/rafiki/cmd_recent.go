package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/mattn/go-runewidth"
	"github.com/spf13/cobra"

	"go.graveland.dev/rafiki/pkg/client"
	"go.graveland.dev/rafiki/pkg/clientstate"
	"go.graveland.dev/rafiki/pkg/costfmt"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/table"
)

func newRecentCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "recent [id|name]",
		Short: "Show recent events from a child's ring buffer",
		Args:  cobra.MaximumNArgs(1),
		RunE:  runRecent,
	}
	cmd.Flags().IntP("limit", "l", 100, "Maximum number of events")
	cmd.Flags().Duration("since", 0, "Only events newer than this (e.g. 5m)")
	cmd.Flags().StringSlice("include", nil, "Include only these event types (repeatable)")
	cmd.Flags().StringSlice("exclude", nil, "Exclude these event types (repeatable)")

	_ = cmd.RegisterFlagCompletionFunc("include", func(_ *cobra.Command, _ []string, _ string) ([]string, cobra.ShellCompDirective) {
		return knownEventTypes, cobra.ShellCompDirectiveNoFileComp
	})
	_ = cmd.RegisterFlagCompletionFunc("exclude", func(_ *cobra.Command, _ []string, _ string) ([]string, cobra.ShellCompDirective) {
		return knownEventTypes, cobra.ShellCompDirectiveNoFileComp
	})

	cmd.ValidArgsFunction = func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		if len(args) > 0 {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		return completeChildren(cmd, toComplete), cobra.ShellCompDirectiveNoFileComp
	}
	return cmd
}

func runRecent(cmd *cobra.Command, args []string) error {
	c := mustDial(cmd)
	defer c.Close()

	ctx := cmdCtx(cmd)
	var input string
	if len(args) > 0 {
		input = args[0]
	}
	childID, err := resolveTarget(ctx, c, mustProfile(cmd).Name, input)
	if err != nil {
		return err
	}

	limit, _ := cmd.Flags().GetInt("limit")
	since, _ := cmd.Flags().GetDuration("since")
	include, _ := cmd.Flags().GetStringSlice("include")
	exclude, _ := cmd.Flags().GetStringSlice("exclude")

	req := protocol.GetRecentRequest{
		Type:    protocol.TypeCtrlGetRecent,
		ChildID: childID,
		Limit:   limit,
		Include: include,
		Exclude: exclude,
	}
	if since > 0 {
		req.Since = time.Now().Add(-since).UnixMilli()
	}

	resp, err := c.Request(ctx, req)
	if err != nil {
		return err
	}
	if !resp.Success {
		return fmt.Errorf("ctrl_get_recent: %s", client.FormatError(resp))
	}

	mode, useColor, err := outputOpts(cmd)
	if err != nil {
		return err
	}
	return emitRecent(os.Stdout, resp.Data, mode, useColor)
}

// emitRecent writes ctrl_get_recent's payload in the resolved mode. The
// payload's rows are the events; JSON mode passes the envelope through
// unchanged (today's shape) while JSONL unwraps it to one event per line.
// A children-list payload (a list-shaped response) renders through the shared
// list renderer instead.
func emitRecent(w io.Writer, data []byte, mode outputMode, useColor bool) error {
	if children, ok := decodeChildrenPayload(data); ok {
		switch mode {
		case outputJSONL:
			return writeJSONL(w, childRows(children))
		case outputJSON:
			enc := json.NewEncoder(w)
			enc.SetIndent("", "  ")
			return enc.Encode(json.RawMessage(data))
		default:
			return renderList(w, children, outputTable, useColor, true)
		}
	}

	switch mode {
	case outputJSON:
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(json.RawMessage(data))
	case outputJSONL:
		var env protocol.GetRecentResponseData
		if err := json.Unmarshal(data, &env); err != nil {
			enc := json.NewEncoder(w)
			return enc.Encode(json.RawMessage(data))
		}
		return writeJSONL(w, rawRows(env.Events))
	default:
		return renderRecentTable(w, data, useColor)
	}
}

// recentDetailMaxCols bounds the DETAIL cell so one wide event cannot blow
// the table past the terminal; the JSON modes carry the full payload.
const recentDetailMaxCols = 80

// recentEvent is the subset of a verbatim pi event the table renders.
type recentEvent struct {
	Type     string          `json:"type"`
	ChildID  string          `json:"childId"`
	Status   string          `json:"status"`
	IsError  bool            `json:"isError"`
	ToolName string          `json:"toolName"`
	Args     json.RawMessage `json:"args"`
	Result   json.RawMessage `json:"result"`
	Message  json.RawMessage `json:"message"`
	Usage    json.RawMessage `json:"usage"`
}

// renderRecentTable renders the event rows as a table. The rows are verbatim
// pi events, so the columns mirror the fields an event actually carries —
// its identity (TYPE, and CHILD where the event names one), its status (the
// error flag, or a lifecycle status change), any reported cost, and whatever
// remains abridged into DETAIL.
func renderRecentTable(w io.Writer, data []byte, useColor bool) error {
	var env protocol.GetRecentResponseData
	if err := json.Unmarshal(data, &env); err != nil {
		// Unexpected payload: pass it through rather than guessing.
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(json.RawMessage(data))
	}

	tb := table.New(w, table.Options{Color: useColor})
	tb.Header(dimHeader(useColor, "TYPE", "CHILD", "STATUS", "COST", "DETAIL")...)

	cur := clientstate.LoadScoped(clientstate.Scope{}).Currency
	for _, ev := range env.Events {
		var hdr recentEvent
		_ = json.Unmarshal(ev, &hdr)

		status := "-"
		switch {
		case hdr.IsError:
			status = "error"
		case hdr.Status != "":
			status = hdr.Status
		}

		tb.Row(
			defaultDash(hdr.Type),
			defaultDash(hdr.ChildID),
			status,
			usageCostCell(hdr.Usage, cur),
			recentDetail(hdr, ev),
		)
	}
	return tb.Render()
}

// usageCostCell formats an event's usage.cost.total, or "-" when the event
// reports none (costfmt renders zero as "-", matching the list table).
func usageCostCell(raw json.RawMessage, cur *clientstate.Currency) string {
	if len(raw) == 0 {
		return "-"
	}
	var u struct {
		Cost struct {
			Total float64 `json:"total"`
		} `json:"cost"`
	}
	if err := json.Unmarshal(raw, &u); err != nil {
		return "-"
	}
	return costfmt.Format(u.Cost.Total, cur)
}

// recentDetail builds the DETAIL cell: a one-line summary of what the event
// carries beyond the columns already shown. Known event shapes get their
// useful text; everything else gets the compact remainder of the event.
func recentDetail(hdr recentEvent, ev json.RawMessage) string {
	shown := []string{"type", "childId", "status", "isError", "toolName",
		"args", "result", "message", "usage"}
	switch hdr.Type {
	case "tool_execution_start":
		return clampDetail(hdr.ToolName + " " + toolPayloadText(hdr.Args))
	case "tool_execution_end":
		return clampDetail(hdr.ToolName + " " + toolPayloadText(hdr.Result))
	case "message_start", "message_end":
		var m struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		}
		if err := json.Unmarshal(hdr.Message, &m); err == nil {
			return clampDetail(m.Role + ": " + messageText(m.Content))
		}
	}
	return clampDetail(compactEventDetail(ev, shown...))
}

// compactEventDetail re-marshals an event minus the named (already-rendered)
// keys, so DETAIL does not repeat them.
func compactEventDetail(ev json.RawMessage, shown ...string) string {
	var m map[string]any
	if err := json.Unmarshal(ev, &m); err != nil {
		return strings.TrimSpace(string(ev))
	}
	for _, k := range shown {
		delete(m, k)
	}
	b, err := json.Marshal(m)
	if err != nil {
		return strings.TrimSpace(string(ev))
	}
	return string(b)
}

// clampDetail folds a summary to one line, bounded to recentDetailMaxCols.
func clampDetail(s string) string {
	s = strings.ReplaceAll(s, "\t", "    ")
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return runewidth.Truncate(strings.TrimSpace(s), recentDetailMaxCols, "…")
}
