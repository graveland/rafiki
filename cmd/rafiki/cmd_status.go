package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	"go.graveland.dev/rafiki/pkg/client"
	"go.graveland.dev/rafiki/pkg/clientstate"
	"go.graveland.dev/rafiki/pkg/costfmt"
	"go.graveland.dev/rafiki/pkg/protocol"
)

func newStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "status",
		Aliases: []string{"st"},
		Short:   "Show daemon status",
		Args:    cobra.NoArgs,
		RunE:    runStatus,
	}
}

func runStatus(cmd *cobra.Command, _ []string) error {
	c := mustDial(cmd)
	defer c.Close()

	resp, err := c.Request(cmdCtx(cmd), protocol.StatusRequest{
		Type: protocol.TypeCtrlStatus,
	})
	if err != nil {
		return err
	}
	if !resp.Success {
		return fmt.Errorf("ctrl_status: %s", client.FormatError(resp))
	}

	mode, useColor, err := outputOpts(cmd)
	if err != nil {
		return err
	}
	return emitStatus(os.Stdout, resp.Data, mode, useColor)
}

// emitStatus writes ctrl_status's payload in the resolved mode: pretty JSON
// passthrough in JSON mode (today's shape, unchanged), one compact line in
// JSONL, and a key/value block in table mode.
func emitStatus(w io.Writer, data []byte, mode outputMode, useColor bool) error {
	switch mode {
	case outputJSON:
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(json.RawMessage(data))
	case outputJSONL:
		enc := json.NewEncoder(w)
		return enc.Encode(json.RawMessage(data))
	default:
		for _, kv := range statusKeyValues(data, useColor) {
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

// statusKeyValues decodes a status payload into ordered key/value pairs, one
// per populated field. The payload is the daemon's own status summary
// (protocol.StatusResponseData); a child summary (the get payload) is also
// accepted and rendered with the same block, so the key/value shape serves
// either. Unknown payloads yield no lines rather than a guess.
func statusKeyValues(data []byte, useColor bool) []statusKV {
	var child protocol.ChildSummary
	if err := json.Unmarshal(data, &child); err == nil && child.ChildID != "" {
		return childStatusKeyValues(child, useColor)
	}

	var st protocol.StatusResponseData
	if err := json.Unmarshal(data, &st); err != nil {
		return nil
	}
	var out []statusKV
	if st.Version != "" {
		out = append(out, statusKV{"version", st.Version})
	}
	if st.StartedAt > 0 {
		out = append(out, statusKV{"started", formatUnixMilli(st.StartedAt)})
	}
	if st.Children.Live > 0 || st.Children.Exited > 0 {
		out = append(out, statusKV{"children",
			fmt.Sprintf("%d live, %d exited", st.Children.Live, st.Children.Exited)})
	}
	if st.MemoryBytes > 0 {
		out = append(out, statusKV{"memory", humanBytes(st.MemoryBytes)})
	}
	if st.Socket != "" {
		out = append(out, statusKV{"socket", st.Socket})
	}
	if st.LogsDir != "" {
		out = append(out, statusKV{"logs", st.LogsDir})
	}
	return out
}

// childStatusKeyValues renders one child's summary as the key/value block:
// one line per populated field, in the brief's field order.
func childStatusKeyValues(ch protocol.ChildSummary, useColor bool) []statusKV {
	cur := clientstate.LoadScoped(clientstate.Scope{}).Currency
	out := []statusKV{
		{"id", ch.ChildID},
	}
	if ch.Name != "" {
		out = append(out, statusKV{"name", ch.Name})
	}
	out = append(out, statusKV{"kind", kindOrDefault(ch.Kind)})
	out = append(out, statusKV{"status", defaultDash(formatStatus(ch.Status, ch.ExitCode, ch.ExitSignal, useColor))})
	if ch.Model != "" {
		out = append(out, statusKV{"model", ch.Model})
	}
	if ch.CostUSD != nil {
		out = append(out, statusKV{"cost", costfmt.Format(*ch.CostUSD, cur)})
	}
	if ch.Cwd != "" {
		out = append(out, statusKV{"cwd", shortenCwd(ch.Cwd)})
	}
	out = append(out, statusKV{"started", formatUnixMilli(ch.StartedAt)})
	out = append(out, statusKV{"labels", formatLabels(ch.Labels, 40, false)})
	return out
}
