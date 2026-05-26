package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"graveland.dev/pi-controller/internal/client"
	"graveland.dev/pi-controller/internal/protocol"
)

func newSearchCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "search <query>",
		Short: "Search across live children's events",
		Args:  cobra.ExactArgs(1),
		RunE:  runSearch,
	}
	cmd.Flags().Bool("regex", false, "Treat query as a regular expression")
	cmd.Flags().Int("limit", 50, "Maximum hits to return")
	cmd.Flags().Int("context", 2, "Context lines around each hit")
	cmd.Flags().String("cwd-contains", "", "Restrict to children whose cwd contains this")
	cmd.Flags().String("name-contains", "", "Restrict to children whose name contains this")
	return cmd
}

func runSearch(cmd *cobra.Command, args []string) error {
	c := mustDial(cmd)
	defer c.Close()

	regex, _ := cmd.Flags().GetBool("regex")
	limit, _ := cmd.Flags().GetInt("limit")
	context, _ := cmd.Flags().GetInt("context")
	cwd, _ := cmd.Flags().GetString("cwd-contains")
	name, _ := cmd.Flags().GetString("name-contains")

	req := protocol.SearchRequest{
		Type:    protocol.TypeCtrlSearch,
		Query:   args[0],
		Regex:   regex,
		Limit:   limit,
		Context: context,
	}
	if cwd != "" || name != "" {
		req.SessionFilter = &protocol.SearchSessionFilter{
			CwdContains:  cwd,
			NameContains: name,
		}
	}

	resp, err := c.Request(cmdCtx(cmd), req)
	if err != nil {
		return err
	}
	if !resp.Success {
		return fmt.Errorf("ctrl_search: %s", client.FormatError(resp))
	}

	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(json.RawMessage(resp.Data))
}
