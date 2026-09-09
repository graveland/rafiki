package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	"go.graveland.dev/rafiki/pkg/client"
	"go.graveland.dev/rafiki/pkg/protocol"
)

func newGetCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "get [id|name...]",
		Short:   "Show details for one or more children",
		Aliases: []string{"show", "info"},
		Args:    cobra.MinimumNArgs(1),
		RunE:    runGet,
	}
	cmd.ValidArgsFunction = func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		return completeChildren(cmd, toComplete), cobra.ShellCompDirectiveNoFileComp
	}
	return cmd
}

func runGet(cmd *cobra.Command, args []string) error {
	c := mustDial(cmd)
	defer c.Close()

	ctx := cmdCtx(cmd)

	mode, useColor, err := outputOpts(cmd)
	if err != nil {
		return err
	}

	var children []protocol.ChildSummary
	var failures int
	for _, arg := range args {
		childID, err := c.Resolve(ctx, arg)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: resolve %q: %v\n", arg, err)
			failures++
			continue
		}
		resp, err := c.Request(ctx, protocol.GetRequest{
			Type:    protocol.TypeCtrlGet,
			ChildID: childID,
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: get %q: %v\n", arg, err)
			failures++
			continue
		}
		if !resp.Success {
			fmt.Fprintf(os.Stderr, "error: get %q: %s\n", arg, client.FormatError(resp))
			failures++
			continue
		}
		var child protocol.ChildSummary
		if err := json.Unmarshal(resp.Data, &child); err != nil {
			fmt.Fprintf(os.Stderr, "error: decode %q: %v\n", arg, err)
			failures++
			continue
		}
		children = append(children, child)
	}

	if err := emitGet(os.Stdout, args, children, failures, mode, useColor); err != nil {
		return err
	}

	if failures > 0 {
		return fmt.Errorf("%d target(s) failed", failures)
	}
	return nil
}

// emitGet writes get's output in the resolved mode. Every per-target failure
// has already gone to stderr by the time this runs, so all three modes emit
// the successful children only — data is never withheld because a sibling
// target failed.
//
// The JSON shapes are get's backward-compatibility contract and are preserved
// exactly: a single successful target emits a bare ChildSummary object,
// multiple targets (or any failures) wrap in {"children":[...]}. JSONL emits
// one compact ChildSummary per line, unwrapped. Table mode renders the same
// table `list` renders, flat.
func emitGet(w io.Writer, args []string, children []protocol.ChildSummary, failures int, mode outputMode, useColor bool) error {
	switch mode {
	case outputJSON:
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		// Single successful target: plain object for backward compatibility.
		// Multiple targets (or any failures): wrap in {"children":[...]}.
		if len(args) == 1 && failures == 0 && len(children) == 1 {
			return enc.Encode(children[0])
		}
		return enc.Encode(map[string]any{"children": children})
	case outputJSONL:
		return writeJSONL(w, childRows(children))
	default:
		return renderList(w, children, outputTable, useColor, true)
	}
}
