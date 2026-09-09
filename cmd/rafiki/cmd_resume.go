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
	// Resolve the mode before dialing: -j and -J together is a user-input
	// error and must not cost a connection.
	mode, _, err := outputOpts(cmd)
	if err != nil {
		return err
	}

	c := mustDial(cmd)
	defer c.Close()

	ctx := cmdCtx(cmd)
	p := mustProfile(cmd)
	var input string
	if len(args) > 0 {
		input = args[0]
	}
	childID, err := resolveTarget(ctx, c, p.Name, input)
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

	_ = setActive(p.Name, childID)

	// The ctrl_resume payload is SpawnResponseData, shared with ctrl_spawn —
	// it carries no name, so text mode fetches one with a best-effort get. A
	// JSON/JSONL consumer does not need it and pays no extra round trip.
	name := ""
	if mode == outputTable {
		name = resumeChildName(ctx, c, childID)
	}
	return renderResume(os.Stdout, childID, name, json.RawMessage(resp.Data), mode)
}

// renderResume writes the resume result in the requested mode: the raw
// ctrl_resume payload as pretty JSON (byte-identical to the pre-tables
// output), one compact JSONL line, or the text line
// `resumed <childID> (<name>)` — the name is dropped, never printed empty,
// when the follow-up get could not resolve it.
func renderResume(w io.Writer, childID, name string, raw json.RawMessage, mode outputMode) error {
	switch mode {
	case outputJSON:
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(raw)
	case outputJSONL:
		return writeJSONL(w, []any{raw})
	default:
		line := "resumed " + childID
		if name != "" {
			line += " (" + name + ")"
		}
		_, err := fmt.Fprintln(w, line)
		return err
	}
}

// resumeChildName fetches the child's name for text output, best effort: a
// failed lookup degrades to a bare `resumed <childID>` rather than failing a
// resume that succeeded.
func resumeChildName(ctx context.Context, c *client.Client, childID string) string {
	resp, err := c.Request(ctx, protocol.GetRequest{Type: protocol.TypeCtrlGet, ChildID: childID})
	if err != nil || !resp.Success {
		return ""
	}
	var child protocol.ChildSummary
	if err := json.Unmarshal(resp.Data, &child); err != nil {
		return ""
	}
	return child.Name
}
