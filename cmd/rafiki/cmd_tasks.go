package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"go.graveland.dev/rafiki/pkg/client"
	"go.graveland.dev/rafiki/pkg/protocol"
)

func newTasksCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "tasks",
		Short: "Query the task ledger",
		RunE:  runTasks,
	}
	cmd.Flags().String("child", "", "Show tasks assigned to this child")
	cmd.Flags().String("status", "", "Filter by status (pending, in_progress, blocked, completed, failed, orphaned, dropped)")
	cmd.Flags().Int("limit", 0, "Maximum rows to return (0 = server default, max 2000)")
	cmd.Flags().Bool("all", false, "Include dropped tasks")
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

	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(json.RawMessage(resp.Data))
}
