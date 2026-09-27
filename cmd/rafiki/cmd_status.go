package main

import (
	"fmt"
	"io"
	"os"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"
	"google.golang.org/protobuf/proto"

	"go.graveland.dev/rafiki/pkg/clientstate"
	"go.graveland.dev/rafiki/pkg/costfmt"
	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
)

func newStatusCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "status [id|name]",
		Aliases: []string{"st"},
		Short:   "Show daemon status, or one child's with an id",
		Args:    cobra.MaximumNArgs(1),
		RunE:    runStatus,
	}
	cmd.ValidArgsFunction = func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		// One optional target; past it there is nothing to offer.
		if len(args) > 0 {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		return completeChildren(cmd, toComplete), cobra.ShellCompDirectiveNoFileComp
	}
	return cmd
}

func runStatus(cmd *cobra.Command, args []string) error {
	// Resolve the mode before dialing: a malformed combination (-j -J) is a
	// user-input error and must not cost a connection (same rule runSearch
	// applies).
	mode, useColor, err := outputOpts(cmd)
	if err != nil {
		return err
	}

	ep, err := newConnectEndpoint(cmd)
	if err != nil {
		return err
	}
	ctrl := ep.control()

	ctx := cmdCtx(cmd)

	var payload proto.Message
	if len(args) == 1 {
		// An id|name target: this is the child's status, not the daemon's.
		// Same resolve/fetch pair `get` runs, but rendered through status's
		// key/value block instead of the list table.
		childID, err := resolveTargetConnect(ctx, ctrl, mustProfile(cmd).Name, args[0], ep.describe)
		if err != nil {
			return fmt.Errorf("resolve %q: %w", args[0], err)
		}
		resp, err := ctrl.GetChild(ctx, connect.NewRequest(&rafikiv1.GetChildRequest{ChildId: childID}))
		if err != nil {
			return diagnoseConnectError(err, ep.describe)
		}
		payload = resp.Msg.GetChild()
	} else {
		resp, err := ctrl.Status(ctx, connect.NewRequest(&rafikiv1.StatusRequest{}))
		if err != nil {
			return diagnoseConnectError(err, ep.describe)
		}
		payload = resp.Msg
	}
	return emitStatus(os.Stdout, payload, mode, useColor)
}

// emitStatus writes status's payload in the resolved mode: canonical protojson
// pretty in JSON mode, one compact line in JSONL, and a key/value block in
// table mode. The payload is Status's daemon summary, or — when the command
// was given an id — GetChild's child summary, which statusKeyValues renders
// with the same block.
func emitStatus(w io.Writer, payload proto.Message, mode outputMode, useColor bool) error {
	switch mode {
	case outputJSON, outputJSONL:
		return emitProto(w, payload, mode)
	default:
		for _, kv := range statusKeyValues(payload, useColor) {
			key := kv.Key + ":"
			if useColor {
				key = dim(key)
			}
			fmt.Fprintf(w, "%s %s\n", key, kv.Value)
		}
		return nil
	}
}

// statusKV is one ordered key/value line of the status block.
type statusKV struct {
	Key   string
	Value string
}

// statusKeyValues turns the payload into ordered key/value pairs, one per
// populated field. The payload is Status's daemon summary, or GetChild's
// child summary rendered with the same block. An unknown payload yields no
// lines rather than a guess.
func statusKeyValues(payload proto.Message, useColor bool) []statusKV {
	switch m := payload.(type) {
	case *rafikiv1.ChildSummary:
		return childStatusKeyValues(m, useColor)
	case *rafikiv1.StatusResponse:
		return daemonStatusKeyValues(m)
	default:
		return nil
	}
}

// daemonStatusKeyValues renders the daemon's vitals: one line per populated
// field.
func daemonStatusKeyValues(st *rafikiv1.StatusResponse) []statusKV {
	var out []statusKV
	if st.GetVersion() != "" {
		out = append(out, statusKV{"version", st.GetVersion()})
	}
	if st.GetStartedAt() > 0 {
		out = append(out, statusKV{"started", formatUnixMilli(st.GetStartedAt())})
	}
	if st.GetChildren().GetLive() > 0 || st.GetChildren().GetExited() > 0 {
		out = append(out, statusKV{"children",
			fmt.Sprintf("%d live, %d exited", st.GetChildren().GetLive(), st.GetChildren().GetExited())})
	}
	if st.GetMemoryBytes() > 0 {
		out = append(out, statusKV{"memory", humanBytes(st.GetMemoryBytes())})
	}
	if st.GetSocket() != "" {
		out = append(out, statusKV{"socket", st.GetSocket()})
	}
	if st.GetLogsDir() != "" {
		out = append(out, statusKV{"logs", st.GetLogsDir()})
	}
	return out
}

// childStatusKeyValues renders one child's summary as the key/value block:
// one line per populated field, in the brief's field order.
func childStatusKeyValues(ch *rafikiv1.ChildSummary, useColor bool) []statusKV {
	cur := clientstate.LoadScoped(clientstate.Scope{}).Currency
	out := []statusKV{
		{"id", ch.GetChildId()},
	}
	if ch.GetName() != "" {
		out = append(out, statusKV{"name", ch.GetName()})
	}
	out = append(out, statusKV{"kind", kindOrDefault(ch.GetKind())})
	out = append(out, statusKV{"status", defaultDash(formatStatus(ch.GetStatus(), ch.ExitCode, ch.GetExitSignal(), useColor))})
	if ch.GetModel() != "" {
		out = append(out, statusKV{"model", ch.GetModel()})
	}
	if ch.CostUsd != nil {
		out = append(out, statusKV{"cost", costfmt.Format(*ch.CostUsd, cur)})
	}
	if ch.GetCwd() != "" {
		out = append(out, statusKV{"cwd", shortenCwd(ch.GetCwd())})
	}
	out = append(out, statusKV{"started", formatUnixMilli(ch.GetStartedAt())})
	out = append(out, statusKV{"labels", formatLabels(ch.GetLabels(), 40, false)})
	return out
}
