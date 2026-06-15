package main

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"git.graveland.dev/brent/pi-controller/client"
	"git.graveland.dev/brent/pi-controller/protocol"
	"github.com/spf13/cobra"
)

func newLogsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "logs <id|name>",
		Aliases: []string{"log"},
		Short:   "Show the captured logs for a child",
		Long: `logs == tail; -f follows. Rendered by default; --raw for verbatim bytes.

Filter the rendered out stream with --profile (a named preset) or
--include/--exclude (specific event types), same as pic tail. Note --profile
applies only to the live follow stream, not the backfill.

Only the out stream can be followed; --in/--err are snapshots (live stderr is
unavailable — captured to disk on exit) and ignore the filter flags.`,
		Args: cobra.MaximumNArgs(1),
		RunE: runLogs,
	}
	cmd.Flags().String("profile", "", "Subscription profile: firehose|results|coarse|lifecycle")
	cmd.Flags().StringSlice("include", nil, "Include only these event types (repeatable)")
	cmd.Flags().StringSlice("exclude", nil, "Exclude these event types (repeatable)")
	cmd.Flags().Bool("in", false, "Dump the raw stdin stream (snapshot, no follow)")
	cmd.Flags().Bool("err", false, "Dump the raw stderr stream (snapshot; live stderr unavailable — see logs after exit)")
	cmd.Flags().Bool("all", false, "Print all three streams with separator headers")
	cmd.Flags().Bool("path", false, "Print just the log directory path")
	cmd.Flags().IntP("tail", "n", -1, "Show the last N events (-1 = all, 0 = none)")
	cmd.Flags().BoolP("follow", "f", false, "Keep streaming new output after catching up (≡ pic tail)")
	cmd.Flags().Bool("raw", false, "Emit raw stream bytes/JSONL instead of the rendered view")
	cmd.Flags().Bool("no-deltas", true, "Suppress token-by-token message_update deltas in the rendered view (default true)")
	cmd.Flags().BoolP("verbose", "v", false, "Include internal RPC/lifecycle frames")

	cmd.MarkFlagsMutuallyExclusive("in", "err", "all", "path")

	_ = cmd.RegisterFlagCompletionFunc("profile", cobra.FixedCompletions(
		[]string{"firehose", "results", "coarse", "lifecycle"},
		cobra.ShellCompDirectiveNoFileComp,
	))
	_ = cmd.RegisterFlagCompletionFunc("include", func(_ *cobra.Command, _ []string, _ string) ([]string, cobra.ShellCompDirective) {
		return knownEventTypes, cobra.ShellCompDirectiveNoFileComp
	})
	_ = cmd.RegisterFlagCompletionFunc("exclude", func(_ *cobra.Command, _ []string, _ string) ([]string, cobra.ShellCompDirective) {
		return knownEventTypes, cobra.ShellCompDirectiveNoFileComp
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
	c := mustDial(cmd)
	defer c.Close()
	ctx := cmdCtx(cmd)

	target := ""
	if len(args) > 0 {
		target = args[0]
	}
	childID, err := resolveTarget(ctx, c, target)
	if err != nil {
		return err
	}

	wantPath, _ := cmd.Flags().GetBool("path")
	if wantPath {
		home, _ := os.UserHomeDir()
		fmt.Println(filepath.Join(home, ".pi", "run", "logs", childID))
		return nil
	}

	wantIn, _ := cmd.Flags().GetBool("in")
	wantErr, _ := cmd.Flags().GetBool("err")
	wantAll, _ := cmd.Flags().GetBool("all")
	tailN, _ := cmd.Flags().GetInt("tail")
	follow, _ := cmd.Flags().GetBool("follow")
	raw, _ := cmd.Flags().GetBool("raw")
	noDeltas, _ := cmd.Flags().GetBool("no-deltas")
	verbose, _ := cmd.Flags().GetBool("verbose")
	mode, useColor := outputOpts(cmd)

	// in/err/all → raw stream dump (snapshot; no follow).
	if wantIn || wantErr || wantAll {
		return dumpRawStreams(ctx, c, childID, wantIn, wantErr, wantAll)
	}

	profile, _ := cmd.Flags().GetString("profile")
	include, _ := cmd.Flags().GetStringSlice("include")
	exclude, _ := cmd.Flags().GetStringSlice("exclude")
	if noDeltas {
		exclude = append(exclude, "message_update")
	}

	return runHistoryOut(ctx, c, childID, historyOpts{
		follow:   follow,
		tailN:    tailN,
		raw:      raw,
		profile:  profile,
		include:  include,
		exclude:  exclude,
		verbose:  verbose,
		mode:     mode,
		useColor: useColor,
	})
}

// dumpRawStreams prints the in/err (and out, for --all) raw streams. Live
// children are served from the daemon's in-memory capture; exited children
// fall back to the on-disk gzip dump. Live stderr is never available (the
// daemon cannot read the stderr buffer without racing the reader goroutine),
// so for a live child we print a notice instead.
func dumpRawStreams(ctx context.Context, c *client.Client, childID string, wantIn, wantErr, wantAll bool) error {
	which := "all"
	switch {
	case wantIn && !wantErr && !wantAll:
		which = "in"
	case wantErr && !wantIn && !wantAll:
		which = "err"
	}

	resp, err := c.Request(ctx, protocol.GetStreamsRequest{
		Type:    protocol.TypeCtrlGetStreams,
		ChildID: childID,
		Which:   which,
	})
	if err != nil {
		return err
	}
	if resp.Success {
		var data protocol.GetStreamsResponseData
		if err := json.Unmarshal(resp.Data, &data); err != nil {
			return err
		}
		if data.Alive {
			if wantAll {
				fmt.Println("=== in ===")
			}
			if wantIn || wantAll {
				for _, line := range data.In {
					fmt.Fprintln(os.Stdout, string(line))
				}
			}
			if wantAll {
				fmt.Println("=== out ===")
				bf, err := fetchBackfill(ctx, c, childID, historyOpts{tailN: -1, raw: true})
				if err != nil {
					return err
				}
				for _, f := range bf {
					fmt.Fprintln(os.Stdout, string(f))
				}
				fmt.Println("=== err ===")
			}
			if wantErr || wantAll {
				if len(data.Err) > 0 {
					os.Stdout.Write(data.Err)
				} else {
					fmt.Fprintln(os.Stderr, "note: stderr is not captured while the child is running; it is written to disk on exit (run `pic logs --err <child>` after it exits)")
				}
			}
			return nil
		}
	}
	// Exited (or RPC unsupported): fall back to the on-disk dump.
	return dumpDiskStreams(childID, wantIn, wantErr, wantAll)
}

func dumpDiskStreams(childID string, wantIn, wantErr, wantAll bool) error {
	home, _ := os.UserHomeDir()
	logsDir := filepath.Join(home, ".pi", "run", "logs", childID)
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
