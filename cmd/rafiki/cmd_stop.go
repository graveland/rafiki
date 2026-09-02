package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"time"

	"github.com/jedib0t/go-pretty/v6/table"
	"github.com/spf13/cobra"

	"go.graveland.dev/rafiki/pkg/client"
	"go.graveland.dev/rafiki/pkg/protocol"
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
finalize a child in one step.`,
		Args: cobra.MinimumNArgs(1),
		RunE: runStop,
	}
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
	c := mustDial(cmd)
	defer c.Close()

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

	results := make([]stopTargetResult, 0, len(args))
	var failures int
	for _, arg := range args {
		childID, kr, err := stopOne(ctx, c, arg, st, kt)
		results = append(results, stopTargetResult{Arg: arg, ChildID: childID, Kill: kr, Err: err})
		if err != nil {
			failures++
		}
	}
	// Children changed state even on a mixed run, so the cache is stale either way.
	dropChildCompletionCache(cmd)

	mode, useColor := outputOpts(cmd)
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
	Kill    protocol.KillResponseData
	Err     error
}

// stopOne resolves arg and sends ctrl_kill for it. It does not close —
// that composition lives in `rafiki close` now.
func stopOne(ctx context.Context, c *client.Client, arg string, st, kt time.Duration) (string, protocol.KillResponseData, error) {
	childID, err := c.Resolve(ctx, arg)
	if err != nil {
		return "", protocol.KillResponseData{}, err
	}

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
		return childID, protocol.KillResponseData{}, err
	}
	if !resp.Success {
		return childID, protocol.KillResponseData{}, fmt.Errorf("ctrl_kill: %s", client.FormatError(resp))
	}

	var kr protocol.KillResponseData
	if err := json.Unmarshal(resp.Data, &kr); err != nil {
		return childID, protocol.KillResponseData{}, fmt.Errorf("decode kill response: %w (raw=%s)", err, string(resp.Data))
	}
	return childID, kr, nil
}

// stopResultJSON is the JSON shape of one stopTargetResult. A distinct type
// from stopTargetResult because error is not directly marshalable and ID
// falls back to the typed arg when resolution never produced a childID.
type stopResultJSON struct {
	ID         string `json:"id"`
	ExitCode   *int   `json:"exitCode"`
	Signal     string `json:"signal,omitempty"`
	DurationMs int64  `json:"durationMs"`
	Escalated  bool   `json:"escalated"`
	Abandoned  bool   `json:"abandoned,omitempty"`
	Error      string `json:"error,omitempty"`
}

// renderStopResults writes the outcome of a `rafiki stop` run either as JSON
// (one document, not one object per line — the bug this replaced) or as a
// table matching renderList's styling.
func renderStopResults(w io.Writer, results []stopTargetResult, mode outputMode, useColor bool) error {
	if mode == outputJSON {
		out := make([]stopResultJSON, len(results))
		for i, r := range results {
			id := r.ChildID
			if id == "" {
				id = r.Arg
			}
			rj := stopResultJSON{
				ID:         id,
				ExitCode:   r.Kill.ExitCode,
				Signal:     r.Kill.Signal,
				DurationMs: r.Kill.DurationMs,
				Escalated:  r.Kill.Escalated,
				Abandoned:  r.Kill.Abandoned,
			}
			if r.Err != nil {
				rj.Error = r.Err.Error()
			}
			out[i] = rj
		}
		return writeJSON(w, map[string]any{"results": out})
	}

	tw := table.NewWriter()
	tw.SetOutputMirror(w)
	st := table.StyleLight
	st.Color = table.ColorOptions{}
	tw.SetStyle(st)

	colNames := []string{"ID", "EXIT", "SIGNAL", "DURATION", "ESCALATED", "ERROR"}
	headerRow := make(table.Row, len(colNames))
	for i, name := range colNames {
		if useColor {
			headerRow[i] = dim(name)
		} else {
			headerRow[i] = name
		}
	}
	tw.AppendHeader(headerRow)

	for _, r := range results {
		id := r.ChildID
		if id == "" {
			id = r.Arg
		}
		exit := "-"
		if r.Kill.ExitCode != nil {
			exit = strconv.Itoa(*r.Kill.ExitCode)
		}
		errCell := ""
		if r.Err != nil {
			errCell = r.Err.Error()
			if useColor {
				errCell = red(errCell)
			}
		}
		tw.AppendRow(table.Row{
			id,
			exit,
			defaultDash(r.Kill.Signal),
			time.Duration(r.Kill.DurationMs) * time.Millisecond,
			r.Kill.Escalated,
			errCell,
		})
	}

	tw.Render()
	return nil
}
