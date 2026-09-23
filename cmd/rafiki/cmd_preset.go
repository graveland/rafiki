// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/presets"
	"go.graveland.dev/rafiki/pkg/table"
)

func newPresetCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "preset",
		Short: "Manage agent presets: named seats fixing model, tools, prompt and budget",
	}
	cmd.AddCommand(newPresetListCmd(), newPresetGetCmd(), newPresetPutCmd(), newPresetDeleteCmd())
	return cmd
}

// presetView is a preset as the CLI prints it in JSON: the spec plus
// version metadata.
type presetView struct {
	Version        int64      `json:"version"`
	CreatedAt      time.Time  `json:"created_at"`
	DeletedAt      *time.Time `json:"deleted_at,omitempty"`
	WrittenByChild string     `json:"written_by_child,omitempty"`
	presets.Spec
}

// presetViewOf converts a wire row to the CLI's view. FromProto parses the
// RFC3339 timestamps; SpecOf folds the row back into the JSON shape a put
// would accept, so a get's JSON round-trips through put.
func presetViewOf(row *rafikiv1.PresetRow) presetView {
	rec := presets.FromProto(row)
	return presetView{
		Version:        row.GetVersion(),
		CreatedAt:      rec.CreatedAt,
		DeletedAt:      rec.DeletedAt,
		WrittenByChild: rec.WrittenByChild,
		Spec:           presets.SpecOf(rec),
	}
}

func newPresetListCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "list [PREFIX]",
		Short: "List presets, optionally narrowed to a name prefix",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			prefix := ""
			if len(args) > 0 {
				prefix = args[0]
			}
			ep, err := newConnectEndpoint(cmd)
			if err != nil {
				return err
			}
			resp, err := ep.control().ListPresets(cmdCtx(cmd),
				connect.NewRequest(&rafikiv1.ListPresetsRequest{Prefix: prefix}))
			if err != nil {
				return err
			}
			mode, useColor, err := outputOpts(cmd)
			if err != nil {
				return err
			}
			return emitPresetList(os.Stdout, resp.Msg.GetRows(), mode, useColor)
		},
	}
	return cmd
}

// emitPresetList writes list's output in the resolved mode. JSON/JSONL rows
// are presetView; the table is NAME, KIND, MODEL, DESCRIPTION.
func emitPresetList(w io.Writer, rows []*rafikiv1.PresetRow, mode outputMode, useColor bool) error {
	views := make([]presetView, 0, len(rows))
	for _, r := range rows {
		views = append(views, presetViewOf(r))
	}
	switch mode {
	case outputJSON:
		return writeJSON(w, map[string]any{"rows": views})
	case outputJSONL:
		anyRows := make([]any, len(views))
		for i, v := range views {
			anyRows[i] = v
		}
		return writeJSONL(w, anyRows)
	default:
		tb := table.New(w, table.Options{Color: useColor})
		tb.Header(dimHeader(useColor, "NAME", "KIND", "MODEL", "DESCRIPTION")...)
		for _, r := range rows {
			tb.Row(r.GetName(), r.GetKind(), defaultDash(r.GetModel()), defaultDash(r.GetDescription()))
		}
		return tb.Render()
	}
}

func newPresetGetCmd() *cobra.Command {
	var history bool
	cmd := &cobra.Command{
		Use:   "get <name>",
		Short: "Print one preset's spec (or its version history with --history)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ep, err := newConnectEndpoint(cmd)
			if err != nil {
				return err
			}
			resp, err := ep.control().GetPreset(cmdCtx(cmd),
				connect.NewRequest(&rafikiv1.GetPresetRequest{Name: args[0], History: history}))
			if err != nil {
				return err
			}
			mode, useColor, err := outputOpts(cmd)
			if err != nil {
				return err
			}
			if history {
				return emitPresetHistory(os.Stdout, resp.Msg.GetRows(), mode, useColor)
			}
			return emitPresetSpec(os.Stdout, resp.Msg.GetRows(), mode, useColor)
		},
	}
	cmd.Flags().BoolVar(&history, "history", false, "Print every version of the preset, live and deleted")
	cmd.ValidArgsFunction = completePresetNames
	return cmd
}

// emitPresetSpec writes get's output in the resolved mode: a two-column
// FIELD/VALUE table of the spec, or presetView JSON/JSONL.
func emitPresetSpec(w io.Writer, rows []*rafikiv1.PresetRow, mode outputMode, useColor bool) error {
	if len(rows) == 0 {
		return fmt.Errorf("preset not found")
	}
	view := presetViewOf(rows[0])
	switch mode {
	case outputJSON:
		return writeJSON(w, view)
	case outputJSONL:
		return writeJSONL(w, []any{view})
	default:
		tb := table.New(w, table.Options{Color: useColor})
		tb.Header(dimHeader(useColor, "FIELD", "VALUE")...)
		for _, cell := range presetSpecCells(view) {
			tb.Row(cell[0], cell[1])
		}
		return tb.Render()
	}
}

// presetSpecCells renders the get table: every spec field that is set, as a
// sorted FIELD/VALUE pair list. Extracted so the unit tests pin the rendering
// without a daemon.
func presetSpecCells(view presetView) [][2]string {
	s := view.Spec
	cells := [][2]string{
		{"kind", s.Kind},
		{"model", s.Model},
		{"provider", s.Provider},
		{"thinking", s.Thinking},
		{"executor", s.Executor},
		{"system_prompt", s.SystemPrompt},
		{"append_system_prompt", s.AppendSystemPrompt},
	}
	if s.Description != "" {
		cells = append(cells, [2]string{"description", s.Description})
	}
	cells = append(cells,
		[2]string{"tools", formatSpecList(s.Tools)},
		[2]string{"skills", formatSpecList(s.Skills)},
		[2]string{"mcp_servers", formatSpecList(s.MCPServers)},
		[2]string{"context_files", formatSpecBool(s.ContextFiles)},
		[2]string{"max_cost", formatSpecFloat(s.MaxCost)},
		[2]string{"max_depth", formatSpecInt(s.MaxDepth)},
		[2]string{"max_children", formatSpecInt(s.MaxChildren)},
	)
	for _, k := range sortedKeys(s.Labels) {
		cells = append(cells, [2]string{"labels." + k, s.Labels[k]})
	}
	out := cells[:0]
	for _, c := range cells {
		if c[1] != "" {
			out = append(out, c)
		}
	}
	return out
}

// emitPresetHistory writes --history's output in the resolved mode: a table
// with VERSION, CREATED, DELETED, WRITTEN BY and MODEL, or []presetView.
func emitPresetHistory(w io.Writer, rows []*rafikiv1.PresetRow, mode outputMode, useColor bool) error {
	views := make([]presetView, 0, len(rows))
	for _, r := range rows {
		views = append(views, presetViewOf(r))
	}
	switch mode {
	case outputJSON:
		return writeJSON(w, views)
	case outputJSONL:
		anyRows := make([]any, len(views))
		for i, v := range views {
			anyRows[i] = v
		}
		return writeJSONL(w, anyRows)
	default:
		tb := table.New(w, table.Options{Color: useColor})
		tb.Header(dimHeader(useColor, "VERSION", "CREATED", "DELETED", "WRITTEN BY", "MODEL")...)
		for _, r := range rows {
			deleted := "-"
			if r.GetDeletedAt() != "" {
				deleted = formatSavedAt(r.GetDeletedAt())
			}
			tb.Row(
				formatVersion(r.GetVersion()),
				formatSavedAt(r.GetCreatedAt()),
				deleted,
				defaultDash(r.GetWrittenByChild()),
				defaultDash(r.GetModel()),
			)
		}
		return tb.Render()
	}
}

func newPresetPutCmd() *cobra.Command {
	var file string
	cmd := &cobra.Command{
		Use:   "put <name> -f FILE",
		Short: "Save a new version of a preset from a file or stdin",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			if file == "" {
				return fmt.Errorf("-f is required (-f - reads stdin)")
			}
			var data []byte
			var err error
			if file == "-" {
				data, err = io.ReadAll(os.Stdin)
			} else {
				data, err = os.ReadFile(file)
			}
			if err != nil {
				return err
			}
			req, err := presetPutRequest(name, data)
			if err != nil {
				return err
			}
			ep, err := newConnectEndpoint(cmd)
			if err != nil {
				return err
			}
			resp, err := ep.control().PutPreset(cmdCtx(cmd), connect.NewRequest(req))
			if err != nil {
				return err
			}
			fmt.Printf("saved %s as version %d\n", name, resp.Msg.GetPreset().GetVersion())
			return nil
		},
	}
	cmd.Flags().StringVarP(&file, "file", "f", "", "path to the preset JSON (-f - reads stdin)")
	cmd.ValidArgsFunction = completePresetNames
	return cmd
}

// presetPutRequest turns a preset JSON file into a PutPresetRequest. Extracted
// so the tri-state parsing and the name check are testable without a daemon.
func presetPutRequest(name string, data []byte) (*rafikiv1.PutPresetRequest, error) {
	spec, err := presets.ParseSpec(data)
	if err != nil {
		return nil, err
	}
	if spec.Name != "" && spec.Name != name {
		return nil, fmt.Errorf("name in file %q does not match %q", spec.Name, name)
	}
	spec.Name = name
	rec := spec.Record()
	return &rafikiv1.PutPresetRequest{Preset: presets.ToProto(rec)}, nil
}

// formatSpecList renders an optional string list: nil = unset (""), a
// set-but-empty list = "(none)" — news worth printing, not blankness — and
// any other list joined with commas.
func formatSpecList(xs *[]string) string {
	if xs == nil {
		return ""
	}
	if len(*xs) == 0 {
		return "(none)"
	}
	return strings.Join(*xs, ",")
}

// formatSpecBool renders an optional bool: nil = unset, true/false otherwise.
func formatSpecBool(p *bool) string {
	if p == nil {
		return ""
	}
	return strconv.FormatBool(*p)
}

// formatSpecFloat renders an optional float: nil = unset, the number
// otherwise — including zero, which is a real choice.
func formatSpecFloat(p *float64) string {
	if p == nil {
		return ""
	}
	return strconv.FormatFloat(*p, 'g', -1, 64)
}

// formatSpecInt renders an optional int: nil = unset, the number otherwise.
func formatSpecInt(p *int) string {
	if p == nil {
		return ""
	}
	return strconv.Itoa(*p)
}

func newPresetDeleteCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "delete <name>",
		Short: "Delete a preset (every live version of it)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ep, err := newConnectEndpoint(cmd)
			if err != nil {
				return err
			}
			_, err = ep.control().DeletePreset(cmdCtx(cmd),
				connect.NewRequest(&rafikiv1.DeletePresetRequest{Name: args[0]}))
			if err != nil {
				return err
			}
			fmt.Printf("deleted %s\n", args[0])
			return nil
		},
	}
	cmd.ValidArgsFunction = completePresetNames
	return cmd
}

// completePresetNames completes the <name> argument shared by preset
// get/put/delete. It never exits, never prints, and cannot block past
// completionDeadline: every failure degrades to "no candidates" by design.
func completePresetNames(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	if len(args) > 0 {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	ep, err := newConnectEndpoint(cmd)
	if err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	ctx, cancel := context.WithTimeout(cmdCtx(cmd), completionDeadline)
	defer cancel()
	resp, err := ep.control().ListPresets(ctx, connect.NewRequest(&rafikiv1.ListPresetsRequest{}))
	if err != nil {
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
	names := make([]string, 0, len(resp.Msg.GetRows()))
	for _, r := range resp.Msg.GetRows() {
		names = append(names, r.GetName())
	}
	return names, cobra.ShellCompDirectiveNoFileComp
}
