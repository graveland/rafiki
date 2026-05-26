package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"graveland.dev/pi-controller/internal/client"
	"graveland.dev/pi-controller/internal/protocol"
)

func newResumeCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "resume <id|name>",
		Short: "Resume an exited child",
		Args:  cobra.ExactArgs(1),
		RunE:  runResume,
	}
	cmd.Flags().String("api-key", "", "Optional API key override for this resume")
	return cmd
}

func runResume(cmd *cobra.Command, args []string) error {
	c := mustDial(cmd)
	defer c.Close()

	ctx := cmdCtx(cmd)
	childID, err := c.Resolve(ctx, args[0])
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
