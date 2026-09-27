// SPDX-License-Identifier: Apache-2.0

package main

import (
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/gen/rafiki/v1/rafikiv1connect"
	"go.graveland.dev/rafiki/pkg/paths"
)

func newLogsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "logs <id|name>",
		Aliases: []string{"log"},
		Short:   "Print a child's event history; -f follows the live stream after it",
		Long: `Print a child's event history, then optionally follow the live stream.

rafiki logs <id> prints the child's conversation history and exits. logs -f
prints it and then follows the live event stream from where the history ended.

The rendered view is one line per event, the same lines rafiki tail prints.
--types narrows the stream to named event types; --all-types widens it to
every type, including the ephemeral deltas; -r emits one protojson Event per
line instead of rendering.

--stdin, --stderr and --all dump the captured process streams, raw: snapshots,
not a follow. Live stderr is unavailable while the child runs — it is written
to disk on exit — and --path prints the log directory without reading anything.`,
		Args: cobra.MaximumNArgs(1),
		RunE: runLogs,
	}
	cmd.Flags().Bool("stdin", false, "Dump the raw stdin stream (snapshot, no follow)")
	cmd.Flags().Bool("stderr", false, "Dump the raw stderr stream (snapshot; live stderr unavailable — see logs after exit)")
	cmd.Flags().Bool("all", false, "Print the process streams with separator headers")
	cmd.Flags().Bool("path", false, "Print just the log directory path")
	cmd.Flags().IntP("tail", "n", -1, "Backfill the last N events (-1 = all, 0 = none)")
	cmd.Flags().BoolP("follow", "f", false, "Keep streaming new events after the history (≡ rafiki tail <id>)")
	cmd.Flags().StringSlice("types", nil, "Only these event types (comma-separated)")
	cmd.Flags().Bool("all-types", false, "Every event type, plus the ephemeral deltas (tier ALL)")
	cmd.Flags().BoolP("raw", "r", false, "One protojson Event per line instead of the rendered view")

	cmd.MarkFlagsMutuallyExclusive("stdin", "stderr", "all", "path")

	_ = cmd.RegisterFlagCompletionFunc("types", func(_ *cobra.Command, _ []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		return prefixCompletions(allNativeTypes(), toComplete), cobra.ShellCompDirectiveNoFileComp
	})

	cmd.ValidArgsFunction = func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		if len(args) > 0 {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		return completeChildren(cmd, toComplete), cobra.ShellCompDirectiveNoFileComp
	}

	return cmd
}

func runLogs(cmd *cobra.Command, args []string) error {
	mode, _, err := outputOpts(cmd)
	if err != nil {
		return err
	}
	ep, err := newConnectEndpoint(cmd)
	if err != nil {
		return err
	}
	client := ep.control()
	ctx := cmdCtx(cmd)

	target := ""
	if len(args) > 0 {
		target = args[0]
	}
	childID, err := resolveTargetConnect(ctx, client, mustProfile(cmd).Name, target, ep.describe)
	if err != nil {
		return err
	}

	wantPath, _ := cmd.Flags().GetBool("path")
	if wantPath {
		fmt.Println(filepath.Join(paths.LogsDir(), childID))
		return nil
	}

	wantStdin, _ := cmd.Flags().GetBool("stdin")
	wantStderr, _ := cmd.Flags().GetBool("stderr")
	wantAll, _ := cmd.Flags().GetBool("all")

	// in/err/all → raw process-stream dump (snapshot; no follow).
	which := ""
	switch {
	case wantStdin && !wantStderr && !wantAll:
		which = "in"
	case wantStderr && !wantStdin && !wantAll:
		which = "err"
	case wantAll && !wantStdin && !wantStderr:
		which = "all"
	}
	if which != "" {
		return dumpStreamsConnect(ctx, ep, client, childID, which)
	}

	tailN, _ := cmd.Flags().GetInt("tail")
	follow, _ := cmd.Flags().GetBool("follow")
	raw, _ := cmd.Flags().GetBool("raw")
	allTypes, _ := cmd.Flags().GetBool("all-types")
	explicit, _ := cmd.Flags().GetStringSlice("types")
	types, tier, err := resolveTypeFilter(explicit, allTypes, true)
	if err != nil {
		return err
	}

	return runEventQuery(ctx, ep, client, eventQuery{
		childID: childID,
		subject: childSubject(childID),
		types:   types,
		tier:    tier,
		tailN:   tailN,
		follow:  follow,
		raw:     raw,
		mode:    mode,
	}, cmd.OutOrStdout(), cmd.ErrOrStderr())
}

// dumpStreamsConnect prints the in/err process streams a live child's capture
// holds, via GetStreams. Live children are served from the daemon's in-memory
// capture; exited children (alive=false) fall back to the on-disk dump. Live
// stderr is never available (the daemon cannot read the stderr buffer without
// racing the reader goroutine), so for a live child we print a notice instead.
func dumpStreamsConnect(ctx context.Context, ep connectEndpoint, client rafikiv1connect.ControlClient, childID, which string) error {
	resp, err := client.GetStreams(ctx, connect.NewRequest(&rafikiv1.GetStreamsRequest{ChildId: childID, Which: which}))
	if err != nil {
		return diagnoseConnectError(err, ep.describe)
	}
	msg := resp.Msg
	wantIn := which == "in" || which == "all"
	wantErr := which == "err" || which == "all"
	if msg.GetAlive() {
		if wantIn {
			if which == "all" {
				fmt.Println("=== in ===")
			}
			for _, line := range msg.GetIn() {
				fmt.Fprintln(os.Stdout, string(line))
			}
		}
		if wantErr {
			if which == "all" {
				fmt.Println("=== err ===")
			}
			if len(msg.GetErr()) > 0 {
				_, _ = os.Stdout.Write(msg.GetErr())
			} else {
				fmt.Fprintln(os.Stderr, "note: stderr is not captured while the child is running; it is written to disk on exit (run `rafiki logs --stderr <child>` after it exits)")
			}
		}
		return nil
	}
	// Exited: fall back to the on-disk dump.
	return dumpDiskStreams(childID, wantIn, wantErr, which == "all")
}

func dumpDiskStreams(childID string, wantIn, wantErr, wantAll bool) error {
	logsDir := filepath.Join(paths.LogsDir(), childID)
	if _, err := os.Stat(logsDir); errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("no logs at %s (child alive but capture unavailable, or persistence mode is `never`)", logsDir)
	}
	if wantAll {
		for _, s := range []struct{ header, file string }{
			{"in", "in.jsonl.gz"},
			{"out", "out.jsonl.gz"},
			{"err", "err.log.gz"},
		} {
			fmt.Printf("=== %s ===\n", s.header)
			if err := zcatTo(os.Stdout, filepath.Join(logsDir, s.file)); err != nil {
				fmt.Fprintln(os.Stderr, "warning:", err)
			}
		}
		return nil
	}
	file := "out.jsonl.gz"
	if wantIn {
		file = "in.jsonl.gz"
	}
	if wantErr {
		file = "err.log.gz"
	}
	return zcatTo(os.Stdout, filepath.Join(logsDir, file))
}

func zcatTo(w io.Writer, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return err
	}
	defer gz.Close()
	_, err = io.Copy(w, gz)
	return err
}
