package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"go.graveland.dev/rafiki/pkg/client"
	"go.graveland.dev/rafiki/pkg/protocol"
)

func newResumeCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "resume [id|name]",
		Aliases: []string{"res"},
		Short:   "Resume an exited child",
		Long: `Resume a rafiki-managed child that has exited.

  rafiki resume [id|name]

If id|name is omitted, uses the active marker.`,
		Args: cobra.MaximumNArgs(1),
		RunE: runResume,
	}
	cmd.Flags().String("api-key", "", "Optional API key override for this resume")
	_ = cmd.RegisterFlagCompletionFunc("api-key", cobra.NoFileCompletions)

	cmd.ValidArgsFunction = func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		if len(args) > 0 {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		return completeChildrenByState(cmd, toComplete, func(ch completionChild) bool {
			return ch.Status == string(protocol.StatusExited)
		}), cobra.ShellCompDirectiveNoFileComp
	}
	return cmd
}

func runResume(cmd *cobra.Command, args []string) error {
	c := mustDial(cmd)
	defer c.Close()

	ctx := cmdCtx(cmd)
	var input string
	if len(args) > 0 {
		input = args[0]
	}
	childID, err := resolveTarget(ctx, c, input)
	if err != nil {
		return err
	}

	apiKey, _ := cmd.Flags().GetString("api-key")

	resp, err := c.Request(ctx, protocol.ResumeRequest{
		Type:    protocol.TypeCtrlResume,
		ChildID: childID,
		APIKey:  apiKey,
	})
	if err != nil {
		return err
	}
	if !resp.Success {
		return fmt.Errorf("ctrl_resume: %s", client.FormatError(resp))
	}

	_ = setActive(childID)

	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(json.RawMessage(resp.Data))
}
