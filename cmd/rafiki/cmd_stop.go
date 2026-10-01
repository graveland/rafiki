package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"strconv"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/gen/rafiki/v1/rafikiv1connect"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/table"
)

func newStopCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use: "stop [id|name...]",
		// `kill` is kept forever, not deprecated: it is in muscle memory and
		// in scripts, and an alias costs one line. `k` was kill's short alias
		// and survives the rename for the same reason.
		Aliases: []string{"kill", "k"},
		Short:   "Stop running children gracefully",
		Long: `Stop one or more running children gracefully, escalating to SIGKILL only if necessary.

stop only stops: it never closes or finalizes the child. A stopped child
stays in 'rafiki list' with status=exited so its record can still be
inspected (/tree navigation, disk artifacts). Use 'rafiki close' to stop AND
finalize a child in one step.

A target with running subagents is refused until you say what to do with them:
--include-subagents=true stops them too (deepest first, before the target),
--include-subagents=false stops only the target.`,
		Args: cobra.MinimumNArgs(1),
		RunE: runStop,
	}
	addIncludeSubagentsFlag(cmd, "stop")
	cmd.Flags().Duration("shutdown-timeout", 0, "Override shutdown timeout (e.g. 180s)")
	cmd.Flags().Duration("kill-timeout", 0, "Override kill timeout (e.g. 30s)")
	cmd.ValidArgsFunction = func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		return completeChildrenByState(cmd, toComplete, func(ch completionChild) bool {
			return ch.Status != string(protocol.StatusExited)
		}), cobra.ShellCompDirectiveNoFileComp
	}
	return cmd
}

func runStop(cmd *cobra.Command, args []string) error {
	ep, err := newConnectEndpoint(cmd)
	if err != nil {
		return err
	}
	ctrl := ep.control()

	ctx := cmdCtx(cmd)

	st, _ := cmd.Flags().GetDuration("shutdown-timeout")
	kt, _ := cmd.Flags().GetDuration("kill-timeout")

	// If custom timeouts push past the default RPC window, extend the context.
	total := st + kt
	if total > 30*time.Second {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, total+5*time.Second)
		defer cancel()
	}

	profileName := mustProfile(cmd).Name
	results := make([]stopTargetResult, 0, len(args))
	var failures int
	for _, arg := range args {
		childID, kr, err := stopOne(ctx, cmd, ctrl, profileName, arg, ep.describe, st, kt)
		results = append(results, stopTargetResult{Arg: arg, ChildID: childID, Kill: kr, Err: err})
		if err != nil {
			failures++
		}
	}
	// Children changed state even on a mixed run, so the cache is stale either way.
	dropChildCompletionCache(cmd)

	mode, useColor, err := outputOpts(cmd)
	if err != nil {
		return err
	}
	if err := renderStopResults(os.Stdout, results, mode, useColor); err != nil {
		return err
	}
	if failures > 0 {
		return fmt.Errorf("%d target(s) failed", failures)
	}
	return nil
}

// stopTargetResult is one target's outcome from `rafiki stop`. ChildID is
// empty when Resolve itself failed — the only case where there is no id to
// report.
type stopTargetResult struct {
	Arg     string
	ChildID string
	Kill    *rafikiv1.KillResponse
	Err     error
}

// stopOne resolves arg and sends Kill for it. It does not close —
// that composition lives in `rafiki close` now.
func stopOne(ctx context.Context, cmd *cobra.Command, ctrl rafikiv1connect.ControlClient, profileName, arg, describe string, st, kt time.Duration) (string, *rafikiv1.KillResponse, error) {
	childID, err := resolveTargetConnect(ctx, ctrl, profileName, arg, describe)
	if err != nil {
		return "", nil, err
	}

	include, err := includeSubagents(ctx, cmd, ctrl, childID, arg, false)
	if err != nil {
		return childID, nil, err
	}

	req := &rafikiv1.KillRequest{ChildId: childID, IncludeDescendants: include}
	if st > 0 {
		req.ShutdownTimeoutMs = st.Milliseconds()
	}
	if kt > 0 {
		req.KillTimeoutMs = kt.Milliseconds()
	}

	resp, err := ctrl.Kill(ctx, connect.NewRequest(req))
	if err != nil {
		return childID, nil, fmt.Errorf("kill: %s", formatConnectErr(err))
	}
	return childID, resp.Msg, nil
}

// stopResultJSON is the JSON shape of one stopTargetResult. A distinct type
// from stopTargetResult because error is not directly marshalable and ID
// falls back to the typed arg when resolution never produced a childID.
// These are the fields of the wire KillResponse the CLI renders; the framed
// plane's abandoned flag has no Connect counterpart and is gone.
type stopResultJSON struct {
	ID         string `json:"id"`
	ExitCode   *int32 `json:"exitCode"`
	Signal     string `json:"signal,omitempty"`
	DurationMs int64  `json:"durationMs"`
	Escalated  bool   `json:"escalated"`
	Error      string `json:"error,omitempty"`
}

// renderStopResults writes the outcome of a `rafiki stop` run either as JSON
// (one document, not one object per line — the bug this replaced) or as a
// table matching renderList's styling. The JSON is the CLI's own composite of
// one KillResponse per target (there is no single response message for a
// multi-target stop), so it stays encoding/json rather than protojson.
func renderStopResults(w io.Writer, results []stopTargetResult, mode outputMode, useColor bool) error {
	if mode == outputJSON {
		out := make([]stopResultJSON, len(results))
		for i, r := range results {
			id := r.ChildID
			if id == "" {
				id = r.Arg
			}
			rj := stopResultJSON{ID: id}
			if r.Kill != nil {
				rj.ExitCode = r.Kill.ExitCode
				rj.Signal = r.Kill.Signal
				rj.DurationMs = r.Kill.DurationMs
				rj.Escalated = r.Kill.Escalated
			}
			if r.Err != nil {
				rj.Error = r.Err.Error()
			}
			out[i] = rj
		}
		return writeJSON(w, map[string]any{"results": out})
	}

	tb := table.New(w, table.Options{Color: useColor})

	colNames := []string{"ID", "EXIT", "SIGNAL", "DURATION", "ESCALATED", "ERROR"}
	headerRow := make([]string, len(colNames))
	for i, name := range colNames {
		if useColor {
			headerRow[i] = dim(name)
		} else {
			headerRow[i] = name
		}
	}
	tb.Header(headerRow...)

	for _, r := range results {
		id := r.ChildID
		if id == "" {
			id = r.Arg
		}
		exit := "-"
		if r.Kill != nil && r.Kill.ExitCode != nil {
			exit = strconv.Itoa(int(*r.Kill.ExitCode))
		}
		errCell := ""
		if r.Err != nil {
			errCell = r.Err.Error()
			if useColor {
				errCell = red(errCell)
			}
		}
		tb.Row(
			id,
			exit,
			defaultDash(r.Kill.GetSignal()),
			(time.Duration(r.Kill.GetDurationMs()) * time.Millisecond).String(),
			strconv.FormatBool(r.Kill.GetEscalated()),
			errCell,
		)
	}

	return tb.Render()
}
