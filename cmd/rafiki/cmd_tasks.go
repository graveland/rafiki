package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	"go.graveland.dev/rafiki/pkg/client"
	"go.graveland.dev/rafiki/pkg/protocol"
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
	c := mustDial(cmd)
	defer c.Close()

	childID, _ := cmd.Flags().GetString("child")
	status, _ := cmd.Flags().GetString("status")
	all, _ := cmd.Flags().GetBool("all")
	limit, _ := cmd.Flags().GetInt("limit")

	req := protocol.TaskListRequest{
		Type:    protocol.TypeCtrlTaskList,
		ChildID: childID,
		Status:  status,
		Limit:   limit,
		All:     all,
	}

	resp, err := c.Request(context.Background(), req)
	if err != nil {
		return fmt.Errorf("tasks: %w", err)
	}
	if !resp.Success {
		return fmt.Errorf("tasks: %s", client.FormatError(resp))
	}

	mode, useColor, err := outputOpts(cmd)
	if err != nil {
		return err
	}
	return emitTasks(os.Stdout, resp.Data, mode, useColor)
}

// taskRow mirrors the fields of a ctrl_task_list row the CLI renders. The
// daemon serializes tasks.Task without JSON tags, so the wire keys are the Go
// field names. Handle is the dotted ordinal path ("2.1") and is what every
// consumer addresses rows by, so it fronts the ID column when present.
// UpdatedAt is accepted in the daemon's lowerCamel convention for forward
// compatibility and renders as a dash until the ledger publishes it.
type taskRow struct {
	ID             string `json:"ID"`
	Handle         string `json:"Handle"`
	ConversationID string `json:"ConversationID"`
	Content        string `json:"Content"`
	Status         string `json:"Status"`
	DropReason     string `json:"DropReason"`
	Assignee       string `json:"Assignee"`
	UpdatedAt      string `json:"updatedAt"`
}

// emitTasks writes ctrl_task_list's payload in the resolved mode. The JSON
// shape is today's raw passthrough of the daemon's response (a bare row
// array), unchanged; JSONL unwraps the array to one row object per line.
func emitTasks(w io.Writer, data []byte, mode outputMode, useColor bool) error {
	switch mode {
	case outputJSON:
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(json.RawMessage(data))
	case outputJSONL:
		var rows []json.RawMessage
		if err := json.Unmarshal(data, &rows); err != nil {
			// Not a row array (unexpected payload): pass it through on one
			// line rather than guessing at a decode.
			enc := json.NewEncoder(w)
			return enc.Encode(json.RawMessage(data))
		}
		return writeJSONL(w, rawRows(rows))
	default:
		var rows []taskRow
		if err := json.Unmarshal(data, &rows); err != nil {
			enc := json.NewEncoder(w)
			enc.SetIndent("", "  ")
			return enc.Encode(json.RawMessage(data))
		}
		return renderTasksTable(w, rows, useColor)
	}
}

// renderTasksTable renders the ledger as a table. All six columns render for
// every row — no per-row dynamic hiding — so a column's presence never
// depends on the rows beneath it.
func renderTasksTable(w io.Writer, rows []taskRow, useColor bool) error {
	tb := table.New(w, table.Options{Color: useColor})
	tb.Header(dimHeader(useColor, "ID", "CHILD", "STATUS", "SUBJECT", "ASSIGNEE", "UPDATED")...)

	for _, r := range rows {
		id := r.Handle
		if id == "" {
			id = r.ID
		}
		status := r.Status
		if r.DropReason != "" {
			status = defaultDash(status) + " (" + r.DropReason + ")"
		}
		tb.Row(
			defaultDash(id),
			defaultDash(r.ConversationID),
			defaultDash(status),
			defaultDash(r.Content),
			defaultDash(r.Assignee),
			defaultDash(r.UpdatedAt),
		)
	}
	return tb.Render()
}
