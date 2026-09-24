// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/table"
)

func newRecallCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "recall <query...>",
		Short: "Search past conversations (summaries, windows) and your saved memories",
		Long: `Search the daemon's recall index by keyword and meaning: conversation
summaries (s: ids), verbatim windows (w: ids) and memories you saved (m: ids).
Conversation results cover what your credential can see (an admin reads the
whole daemon); memories are always your own. Expand one hit with
"rafiki recall context <id>".`,
		Args: cobra.MinimumNArgs(1),
		RunE: runRecall,
	}
	cmd.Flags().StringSlice("source", nil, "restrict to some of: memory, summary, window (repeatable)")
	_ = cmd.RegisterFlagCompletionFunc("source", cobra.FixedCompletions(
		[]string{"memory", "summary", "window"}, cobra.ShellCompDirectiveNoFileComp))
	cmd.Flags().String("under", "", "memory path prefix; only memory hits under it")
	cmd.Flags().String("repo", "", "conversation repo basename filter")
	cmd.Flags().String("since", "", "only hits after this: RFC3339 timestamp or duration like 7d, 36h")
	cmd.Flags().Int("limit", 0, "max results (0 = the daemon default, 10)")
	cmd.AddCommand(newRecallContextCmd())
	return cmd
}

func runRecall(cmd *cobra.Command, args []string) error {
	// Parse user input before any dial: a bad --since is a local error, not a
	// round trip.
	sinceFlag, _ := cmd.Flags().GetString("since")
	since, err := parseSinceArg(sinceFlag)
	if err != nil {
		return err
	}
	sources, _ := cmd.Flags().GetStringSlice("source")
	under, _ := cmd.Flags().GetString("under")
	repo, _ := cmd.Flags().GetString("repo")
	limit, _ := cmd.Flags().GetInt("limit")

	ep, err := newConnectEndpoint(cmd)
	if err != nil {
		return err
	}
	resp, err := ep.control().Recall(cmdCtx(cmd), connect.NewRequest(&rafikiv1.RecallRequest{
		Query:     strings.Join(args, " "),
		Sources:   sources,
		Under:     under,
		Repo:      repo,
		SinceUnix: unixOrZero(since),
		Limit:     int32(limit),
	}))
	if err != nil {
		return diagnoseConnectError(err, ep.describe)
	}
	mode, useColor, err := outputOpts(cmd)
	if err != nil {
		return err
	}
	return emitRecallHits(os.Stdout, resp.Msg.GetHits(), mode, useColor)
}

// parseSinceArg resolves a --since value: RFC3339 first (a timestamp would
// otherwise be misread by ParseDuration), then a duration subtracted from
// now, where a trailing "d" means days — Go's ParseDuration has no day unit.
// Empty resolves to nil (no filter); the wire's 0 then means unbounded.
func parseSinceArg(s string) (*time.Time, error) {
	if s == "" {
		return nil, nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		t = t.UTC()
		return &t, nil
	}
	if d, ok := strings.CutSuffix(s, "d"); ok {
		if days, err := strconv.ParseFloat(d, 64); err == nil {
			t := time.Now().Add(-time.Duration(days * 24 * float64(time.Hour)))
			return &t, nil
		}
	}
	if d, err := time.ParseDuration(s); err == nil {
		t := time.Now().Add(-d)
		return &t, nil
	}
	return nil, fmt.Errorf("invalid --since %q: use RFC3339 or a duration like 7d, 36h", s)
}

// newRecallContextCmd returns `rafiki recall context <id>` — a subcommand, so
// cobra resolves the literal word "context" as the expansion verb, never as a
// query. It prints text, so the three-way --output contract does not apply.
func newRecallContextCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "context <hit-id>",
		Short: "Expand one recall hit (m:/s:/w: prefixed) into its full text",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ep, err := newConnectEndpoint(cmd)
			if err != nil {
				return err
			}
			before, _ := cmd.Flags().GetInt("before")
			after, _ := cmd.Flags().GetInt("after")
			maxChars, _ := cmd.Flags().GetInt("max-chars")
			resp, err := ep.control().RecallContext(cmdCtx(cmd),
				connect.NewRequest(&rafikiv1.RecallContextRequest{
					Id:       args[0],
					Before:   int32(before),
					After:    int32(after),
					MaxChars: int32(maxChars),
				}))
			if err != nil {
				return diagnoseConnectError(err, ep.describe)
			}
			fmt.Println(resp.Msg.GetText())
			return nil
		},
	}
	cmd.Flags().Int("before", 0, "messages of surrounding context before a window hit")
	cmd.Flags().Int("after", 0, "messages of surrounding context after a window hit")
	cmd.Flags().Int("max-chars", 0, "cap the rendered text (0 = uncapped)")
	return cmd
}

// emitRecallHits writes recall's output in the resolved mode. JSON/JSONL rows
// are the wire hits as-is (the brief's "hit array"); the table is ID, SOURCE,
// WHEN, WHERE, SNIPPET.
func emitRecallHits(w io.Writer, hits []*rafikiv1.RecallHit, mode outputMode, useColor bool) error {
	switch mode {
	case outputJSON:
		return writeJSON(w, hits)
	case outputJSONL:
		anyRows := make([]any, len(hits))
		for i, h := range hits {
			anyRows[i] = h
		}
		return writeJSONL(w, anyRows)
	default:
		tb := table.New(w, table.Options{Color: useColor})
		tb.Header(dimHeader(useColor, "ID", "SOURCE", "WHEN", "WHERE", "SNIPPET")...)
		for _, h := range hits {
			tb.Row(h.GetId(), h.GetSource(), formatSavedAt(h.GetWhen()), recallWhere(h), h.GetSnippet())
		}
		return tb.Render()
	}
}

// recallWhere renders the WHERE cell: a memory's path/name, else a
// conversation's repo·name (repo omitted when unknown), else "-".
func recallWhere(h *rafikiv1.RecallHit) string {
	if h.GetPath() != "" {
		if h.GetName() == "" {
			return h.GetPath()
		}
		return h.GetPath() + "/" + h.GetName()
	}
	name := h.GetConversationName()
	if name == "" {
		return defaultDash(h.GetRepo())
	}
	if h.GetRepo() != "" {
		return h.GetRepo() + "·" + name
	}
	return name
}
