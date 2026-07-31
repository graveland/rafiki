package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"

	"go.graveland.dev/rafiki/pkg/client"
	"go.graveland.dev/rafiki/pkg/protocol"
)

func newKillCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "kill [id|name...]",
		Short: "Stop running children gracefully",
		Long: `Stop one or more running children gracefully, escalating to SIGKILL only if necessary.

On a clean exit (exit code 0, no signal, no timeout escalation), the child
is also forgotten from the daemon's in-memory store (removed from fundi list).
Disk artifacts (logs, state record) are not affected.

Use --no-forget to keep exited children visible in fundi list even after a
clean exit (e.g. for /tree navigation or inspection).`,
		Args: cobra.MinimumNArgs(1),
		RunE: runKill,
	}
	cmd.Flags().Duration("shutdown-timeout", 0, "Override shutdown timeout (e.g. 180s)")
	cmd.Flags().Duration("kill-timeout", 0, "Override kill timeout (e.g. 30s)")
	cmd.Flags().Bool("no-forget", false, "Keep children in fundi list even after a clean exit")
	cmd.ValidArgsFunction = func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		return completeChildrenByState(cmd, toComplete, func(ch protocol.ChildSummary) bool {
			return ch.Status != string(protocol.StatusExited)
		}), cobra.ShellCompDirectiveNoFileComp
	}
	return cmd
}

func runKill(cmd *cobra.Command, args []string) error {
	c := mustDial(cmd)
	defer c.Close()

	ctx := cmdCtx(cmd)

	st, _ := cmd.Flags().GetDuration("shutdown-timeout")
	kt, _ := cmd.Flags().GetDuration("kill-timeout")
	noForget, _ := cmd.Flags().GetBool("no-forget")

	// If custom timeouts push past the default RPC window, extend the context.
	total := st + kt
	if total > 30*time.Second {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, total+5*time.Second)
		defer cancel()
	}

	var failures int
	for _, arg := range args {
		if err := killOne(ctx, cmd, c, arg, st, kt, noForget); err != nil {
			fmt.Fprintf(os.Stderr, "error: kill %q: %v\n", arg, err)
			failures++
		}
	}
	if failures > 0 {
		return fmt.Errorf("%d target(s) failed", failures)
	}
	return nil
}

// killOne kills and optionally forgets a single target. Errors are returned to
// the caller for aggregation across multiple targets.
func killOne(ctx context.Context, _ *cobra.Command, c *client.Client, arg string, st, kt time.Duration, noForget bool) error {
	childID, err := c.Resolve(ctx, arg)
	if err != nil {
		return err
	}

	res, err := killAndMaybeForget(ctx, c, childID, st, kt, noForget)
	if err != nil {
		return err
	}

	out := map[string]any{
		"kill":   res.Kill,
		"forgot": res.DidForget,
	}
	if res.ForgetErr != nil {
		out["forgetError"] = res.ForgetErr.Error()
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}

// killAndForgetResult bundles outputs from a kill+(optional)forget operation.
type killAndForgetResult struct {
	Kill      protocol.KillResponseData
	DidForget bool
	ForgetErr error
}

// killAndMaybeForget sends ctrl_kill for childID and, on a clean exit
// (exitCode 0, no signal, not escalated) and when noForget is false, follows
// up with ctrl_forget. Returns the kill result and whether forget ran.
//
// Shared between `fundi kill` and `fundi create`/`fundi attach`'s post-detach
// terminate prompt so both code paths apply the same auto-forget policy.
func killAndMaybeForget(ctx context.Context, c *client.Client, childID string, st, kt time.Duration, noForget bool) (killAndForgetResult, error) {
	var res killAndForgetResult

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

	resp, err := c.Request(ctx, req)
	if err != nil {
		return res, err
	}
	if !resp.Success {
		return res, fmt.Errorf("ctrl_kill: %s", client.FormatError(resp))
	}

	if err := json.Unmarshal(resp.Data, &res.Kill); err != nil {
		// Couldn't decode kill response — surface the raw payload and skip forget logic.
		return res, fmt.Errorf("decode kill response: %w (raw=%s)", err, string(resp.Data))
	}

	cleanExit := res.Kill.ExitCode != nil && *res.Kill.ExitCode == 0 &&
		res.Kill.Signal == "" && !res.Kill.Escalated
	if cleanExit && !noForget {
		fresp, ferr := c.Request(ctx, protocol.ForgetRequest{
			Type:    protocol.TypeCtrlForget,
			ChildID: childID,
		})
		switch {
		case ferr != nil:
			res.ForgetErr = ferr
		case !fresp.Success:
			res.ForgetErr = fmt.Errorf("%s", client.FormatError(fresp))
		default:
			res.DidForget = true
		}
	}

	return res, nil
}
