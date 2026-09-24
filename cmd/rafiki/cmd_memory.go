// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/table"
)

func newMemoryCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "memory",
		Short: "Manage your saved memories and the recall index",
		Long: `Saved memories are private to you — an admin's memories are the admin's
own, never the daemon's. tree/get/put/delete are your memory store; backfill
(admin credential) arms the summarizer over historical conversations; status
reads the index.`,
	}
	cmd.AddCommand(newMemoryTreeCmd(), newMemoryGetCmd(), newMemoryPutCmd(),
		newMemoryDeleteCmd(), newMemoryBackfillCmd(), newMemoryStatusCmd())
	return cmd
}

func newMemoryTreeCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "tree <path>",
		Short: "List your memories under a dot-separated path",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ep, err := newConnectEndpoint(cmd)
			if err != nil {
				return err
			}
			depth, _ := cmd.Flags().GetInt("depth")
			resp, err := ep.control().MemoryTree(cmdCtx(cmd),
				connect.NewRequest(&rafikiv1.MemoryTreeRequest{Path: args[0], Depth: int32(depth)}))
			if err != nil {
				return diagnoseConnectError(err, ep.describe)
			}
			mode, useColor, err := outputOpts(cmd)
			if err != nil {
				return err
			}
			return emitMemoryRows(os.Stdout, resp.Msg.GetMemories(), mode, useColor)
		},
	}
	cmd.Flags().Int("depth", 0, "levels below <path> (0 = the whole subtree)")
	return cmd
}

func newMemoryGetCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "get <path> <name>",
		Short: "Print one memory's body",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			ep, err := newConnectEndpoint(cmd)
			if err != nil {
				return err
			}
			resp, err := ep.control().GetMemory(cmdCtx(cmd),
				connect.NewRequest(&rafikiv1.GetMemoryRequest{Path: args[0], Name: args[1]}))
			if err != nil {
				return diagnoseConnectError(err, ep.describe)
			}
			mode, _, err := outputOpts(cmd)
			if err != nil {
				return err
			}
			switch mode {
			case outputJSON:
				return writeJSON(os.Stdout, resp.Msg.GetMemory())
			case outputJSONL:
				return writeJSONL(os.Stdout, []any{resp.Msg.GetMemory()})
			default:
				fmt.Println(resp.Msg.GetMemory().GetBody())
				return nil
			}
		},
	}
	return cmd
}

func newMemoryPutCmd() *cobra.Command {
	var body, file, meta string
	cmd := &cobra.Command{
		Use:   "put <path> <name> [--body TEXT | --file PATH | stdin]",
		Short: "Save a memory; the body comes from --body, --file, or stdin",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			req, err := memoryPutRequest(cmd, args[0], args[1], body, file, meta)
			if err != nil {
				return err
			}
			ep, err := newConnectEndpoint(cmd)
			if err != nil {
				return err
			}
			resp, err := ep.control().PutMemory(cmdCtx(cmd), connect.NewRequest(req))
			if err != nil {
				return diagnoseConnectError(err, ep.describe)
			}
			fmt.Printf("saved %s/%s\n", resp.Msg.GetMemory().GetPath(), resp.Msg.GetMemory().GetName())
			return nil
		},
	}
	cmd.Flags().StringVar(&body, "body", "", "memory body text")
	cmd.Flags().StringVar(&file, "file", "", "read the body from a file (- reads stdin)")
	cmd.Flags().StringVar(&meta, "meta", "", "metadata as a JSON object")
	return cmd
}

// memoryPutRequest builds the put request, resolving the body the way preset
// put resolves its spec file: --body wins, then --file ("-" = stdin), else
// stdin. Extracted so body resolution is testable without a daemon.
func memoryPutRequest(cmd *cobra.Command, path, name, body, file, meta string) (*rafikiv1.PutMemoryRequest, error) {
	if body != "" && file != "" {
		return nil, fmt.Errorf("--body and --file are mutually exclusive")
	}
	switch {
	case body != "": // already resolved
	case file == "-":
		data, err := io.ReadAll(cmd.InOrStdin())
		if err != nil {
			return nil, err
		}
		body = string(data)
	case file != "":
		data, err := os.ReadFile(file)
		if err != nil {
			return nil, err
		}
		body = string(data)
	default:
		data, err := io.ReadAll(cmd.InOrStdin())
		if err != nil {
			return nil, err
		}
		body = string(data)
	}
	if body == "" {
		return nil, fmt.Errorf("empty body: pass --body, --file (- for stdin) or pipe text on stdin")
	}
	if meta != "" && !json.Valid([]byte(meta)) {
		return nil, fmt.Errorf("--meta is not valid JSON")
	}
	return &rafikiv1.PutMemoryRequest{Path: path, Name: name, Body: body, MetaJson: meta}, nil
}

func newMemoryDeleteCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "delete <path> <name>",
		Short: "Remove one of your memories from recall results (tombstoned, not destroyed)",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			ep, err := newConnectEndpoint(cmd)
			if err != nil {
				return err
			}
			_, err = ep.control().DeleteMemory(cmdCtx(cmd),
				connect.NewRequest(&rafikiv1.DeleteMemoryRequest{Path: args[0], Name: args[1]}))
			if err != nil {
				return diagnoseConnectError(err, ep.describe)
			}
			fmt.Printf("deleted %s/%s\n", args[0], args[1])
			return nil
		},
	}
}

// newMemoryBackfillCmd returns `rafiki memory backfill`, the admin verb that
// arms the summarizer over historical conversations. Both flags are REQUIRED
// on the CLI: --since becomes the wire timestamp (so "all history" is an
// explicit operator choice, never a zero default), and --max-cost is refused
// at zero by the daemon — a budgetless backfill cannot be armed at all.
func newMemoryBackfillCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "backfill --since DURATION|RFC3339 --max-cost USD",
		Short: "Summarize historical conversations, under a USD budget (admin credential)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			sinceFlag, _ := cmd.Flags().GetString("since")
			since, err := parseSinceArg(sinceFlag)
			if err != nil {
				return err
			}
			if since == nil {
				return fmt.Errorf("--since is required (e.g. --since 90d or --since 2025-01-01T00:00:00Z)")
			}
			maxCost, _ := cmd.Flags().GetFloat64("max-cost")
			if maxCost <= 0 {
				return fmt.Errorf("--max-cost must be a positive USD budget (the daemon refuses a budgetless backfill)")
			}
			ep, err := newConnectEndpoint(cmd)
			if err != nil {
				return err
			}
			_, err = ep.control().RecallBackfill(cmdCtx(cmd),
				connect.NewRequest(&rafikiv1.RecallBackfillRequest{
					SinceUnix:  unixOrZero(since),
					MaxCostUsd: maxCost,
				}))
			if err != nil {
				return diagnoseConnectError(err, ep.describe)
			}
			fmt.Printf("backfill armed from %s, budget %.2f USD\n", since.Format("2006-01-02 15:04"), maxCost)
			return nil
		},
	}
	cmd.Flags().String("since", "", "summarize conversations active since then (required): RFC3339 or duration like 90d")
	cmd.Flags().Float64("max-cost", 0, "total USD budget for the backfill (required, > 0)")
	return cmd
}

func newMemoryStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Read the recall index: counts, backfill progress, models",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ep, err := newConnectEndpoint(cmd)
			if err != nil {
				return err
			}
			resp, err := ep.control().RecallStatus(cmdCtx(cmd),
				connect.NewRequest(&rafikiv1.RecallStatusRequest{}))
			if err != nil {
				return diagnoseConnectError(err, ep.describe)
			}
			mode, useColor, err := outputOpts(cmd)
			if err != nil {
				return err
			}
			switch mode {
			case outputJSON:
				return writeJSON(os.Stdout, resp.Msg)
			case outputJSONL:
				return writeJSONL(os.Stdout, []any{resp.Msg})
			default:
				tb := table.New(os.Stdout, table.Options{Color: useColor})
				tb.Header(dimHeader(useColor, "FIELD", "VALUE")...)
				for _, cell := range memoryStatusCells(resp.Msg) {
					tb.Row(cell[0], cell[1])
				}
				return tb.Render()
			}
		},
	}
}

// memoryStatusCells renders the status table: every field, as a FIELD/VALUE
// list — zeros are news here (0 conversations = an empty index, spent 0 with
// a budget armed = the backfill has not run yet).
func memoryStatusCells(m *rafikiv1.RecallStatusResponse) [][2]string {
	return [][2]string{
		{"conversations", strconv.FormatInt(m.GetConversations(), 10)},
		{"windows", strconv.FormatInt(m.GetWindows(), 10)},
		{"windows_unembedded", strconv.FormatInt(m.GetWindowsUnembedded(), 10)},
		{"summaries", strconv.FormatInt(m.GetSummaries(), 10)},
		{"summaries_pending", strconv.FormatInt(m.GetSummariesPending(), 10)},
		{"memories", strconv.FormatInt(m.GetMemories(), 10)},
		{"summary_cost_usd", fmt.Sprintf("$%.4f", m.GetSummaryCostUsd())},
		{"backfill_since", defaultDash(m.GetBackfillSince())},
		{"backfill_budget_usd", fmt.Sprintf("$%.2f", m.GetBackfillBudgetUsd())},
		{"backfill_spent_usd", fmt.Sprintf("$%.4f", m.GetBackfillSpentUsd())},
		{"embedding_model", defaultDash(m.GetEmbeddingModel())},
		{"summary_model", defaultDash(m.GetSummaryModel())},
	}
}

// emitMemoryRows writes tree's output in the resolved mode: a table of PATH,
// NAME, UPDATED and the body's first line, or the wire rows as JSON.
func emitMemoryRows(w io.Writer, rows []*rafikiv1.MemoryRow, mode outputMode, useColor bool) error {
	switch mode {
	case outputJSON:
		return writeJSON(w, rows)
	case outputJSONL:
		anyRows := make([]any, len(rows))
		for i, r := range rows {
			anyRows[i] = r
		}
		return writeJSONL(w, anyRows)
	default:
		tb := table.New(w, table.Options{Color: useColor})
		tb.Header(dimHeader(useColor, "PATH", "NAME", "UPDATED", "BODY")...)
		for _, r := range rows {
			tb.Row(r.GetPath(), r.GetName(), formatSavedAt(r.GetUpdatedAt()), firstLineOf(r.GetBody(), 80))
		}
		return tb.Render()
	}
}

// firstLineOf returns s's first line, truncated to max runes.
func firstLineOf(s string, max int) string {
	for i, r := range s {
		if r == '\n' {
			s = s[:i]
			break
		}
	}
	runes := []rune(s)
	if len(runes) > max {
		return string(runes[:max]) + "…"
	}
	return s
}
