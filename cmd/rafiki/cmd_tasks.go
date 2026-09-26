package main

import (
	"io"
	"os"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/table"
)

func newTasksCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "tasks",
		Aliases: []string{"task"},
		Short:   "Query the task ledger",
		RunE:    runTasks,
	}
	cmd.Flags().String("child", "", "Show tasks assigned to this child")
	cmd.Flags().String("status", "", "Filter by status (pending, in_progress, blocked, completed, failed, orphaned, dropped)")
	cmd.Flags().IntP("limit", "l", 0, "Maximum rows to return (0 = server default, max 2000)")
	cmd.Flags().Bool("all", false, "Include dropped tasks")
	_ = cmd.RegisterFlagCompletionFunc("child", func(cmd *cobra.Command, _ []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		return completeChildren(cmd, toComplete), cobra.ShellCompDirectiveNoFileComp
	})
	// The statuses tasks.Task can carry (protocol.Status plus the dropped/
	// orphaned ledger states); the flag's help text names the same set.
	_ = cmd.RegisterFlagCompletionFunc("status", cobra.FixedCompletions(
		[]string{"pending", "in_progress", "blocked", "completed", "failed", "orphaned", "dropped"},
		cobra.ShellCompDirectiveNoFileComp,
	))
	return cmd
}

func runTasks(cmd *cobra.Command, _ []string) error {
	ep, err := newConnectEndpoint(cmd)
	if err != nil {
		return err
	}

	childID, _ := cmd.Flags().GetString("child")
	status, _ := cmd.Flags().GetString("status")
	all, _ := cmd.Flags().GetBool("all")
	limit, _ := cmd.Flags().GetInt("limit")

	resp, err := ep.control().ListTasks(cmdCtx(cmd), connect.NewRequest(&rafikiv1.ListTasksRequest{
		ChildId: childID,
		Status:  status,
		Limit:   int32(limit),
		All:     all,
	}))
	if err != nil {
		return diagnoseConnectError(err, ep.describe)
	}

	mode, useColor, err := outputOpts(cmd)
	if err != nil {
		return err
	}
	return emitTasks(os.Stdout, resp.Msg, mode, useColor)
}

// emitTasks writes the ListTasks response in the resolved mode: the
// response's canonical protojson in JSON mode (the framed plane passed the
// bare row array through; the wire response is now the message), one row
// object per line in JSONL, and a table in text mode.
func emitTasks(w io.Writer, resp *rafikiv1.ListTasksResponse, mode outputMode, useColor bool) error {
	switch mode {
	case outputJSON, outputJSONL:
		if mode == outputJSONL {
			return emitProtoRows(w, resp.GetTasks(), mode)
		}
		return emitProto(w, resp, mode)
	default:
		return renderTasksTable(w, resp.GetTasks(), useColor)
	}
}

// renderTasksTable renders the ledger as a table. Every column renders for
// every row — no per-row dynamic hiding — so a column's presence never
// depends on the rows beneath it. Handle is the dotted ordinal path ("2.1")
// and is what every consumer addresses rows by. CHILD is the row's owning
// conversation (TaskRow.conversation_id — a conversation id, NOT a child id;
// the child working the row rides ASSIGNEE). UPDATED stays dropped: the
// framed plane never carried it either — it rendered a client-side dash.
func renderTasksTable(w io.Writer, rows []*rafikiv1.TaskRow, useColor bool) error {
	tb := table.New(w, table.Options{Color: useColor})
	tb.Header(dimHeader(useColor, "ID", "CHILD", "STATUS", "SUBJECT", "ASSIGNEE")...)

	for _, r := range rows {
		status := r.GetStatus()
		if r.GetDropReason() != "" {
			status = defaultDash(status) + " (" + r.GetDropReason() + ")"
		}
		tb.Row(
			defaultDash(r.GetHandle()),
			defaultDash(r.GetConversationId()),
			defaultDash(status),
			defaultDash(r.GetContent()),
			defaultDash(r.GetAssignee()),
		)
	}
	return tb.Render()
}
