package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"

	"graveland.dev/pi-controller/internal/client"
	"graveland.dev/pi-controller/internal/protocol"
)

func newKillCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "kill [id|name]",
		Short: "Stop a running child gracefully",
		Long: `Stop a running child gracefully, escalating to SIGKILL only if necessary.

On a clean exit (exit code 0, no signal, no timeout escalation), the child
is also forgotten from the daemon's in-memory store (removed from pic list).
Disk artifacts (logs, state record) are not affected.

Use --no-forget to keep the exited child visible in pic list even after a
clean exit (e.g. for /tree navigation or inspection).`,
		Args: cobra.MaximumNArgs(1),
		RunE: runKill,
	}
	cmd.Flags().Duration("shutdown-timeout", 0, "Override shutdown timeout (e.g. 180s)")
	cmd.Flags().Duration("kill-timeout", 0, "Override kill timeout (e.g. 30s)")
	cmd.Flags().Bool("no-forget", false, "Keep the child in pic list even after a clean exit")
	cmd.ValidArgsFunction = func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		if len(args) > 0 {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		return completeChildren(cmd, toComplete), cobra.ShellCompDirectiveNoFileComp
	}
	return cmd
}

func runKill(cmd *cobra.Command, args []string) error {
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

	st, _ := cmd.Flags().GetDuration("shutdown-timeout")
	kt, _ := cmd.Flags().GetDuration("kill-timeout")
	noForget, _ := cmd.Flags().GetBool("no-forget")

	req := protocol.KillRequest{
		Type:    protocol.TypeCtrlKill,
		ChildID: childID,
	}
	if st > 0 {
		req.ShutdownTimeoutMs = st.Milliseconds()
	}
	if kt > 0 {
		req.KillTimeoutMs = kt.Milliseconds()
	}

	// Allow longer than Go's default RPC timing if both flags push past it.
	total := st + kt
	if total > 30*time.Second {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, total+5*time.Second)
		defer cancel()
	}

	resp, err := c.Request(ctx, req)
	if err != nil {
		return err
	}
	if !resp.Success {
		return fmt.Errorf("ctrl_kill: %s", client.FormatError(resp))
	}

	var killData protocol.KillResponseData
	if err := json.Unmarshal(resp.Data, &killData); err != nil {
		// Couldn't decode kill response — dump raw and skip forget logic.
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(json.RawMessage(resp.Data))
	}

	// Auto-forget on clean exit: exitCode 0, no signal, not escalated.
	cleanExit := killData.ExitCode != nil && *killData.ExitCode == 0 &&
		killData.Signal == "" && !killData.Escalated
	didForget := false
	var forgetErr error
	if cleanExit && !noForget {
		fresp, ferr := c.Request(cmdCtx(cmd), protocol.ForgetRequest{
			Type:    protocol.TypeCtrlForget,
			ChildID: childID,
		})
		if ferr != nil {
			forgetErr = ferr
		} else if !fresp.Success {
			forgetErr = fmt.Errorf("%s", client.FormatError(fresp))
		} else {
			didForget = true
		}
	}

	out := map[string]any{
		"kill":   killData,
		"forgot": didForget,
	}
	if forgetErr != nil {
		out["forgetError"] = forgetErr.Error()
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}
