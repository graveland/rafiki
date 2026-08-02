package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/jedib0t/go-pretty/v6/table"
	"github.com/spf13/cobra"

	"go.graveland.dev/rafiki/pkg/client"
	"go.graveland.dev/rafiki/pkg/protocol"
)

func newPresetsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "presets",
		Aliases: []string{"preset"},
		Short:   "List presets from <config dir>/presets.json",
		Long: `List named presets from <config dir>/presets.json (default $XDG_CONFIG_HOME/rafiki/presets.json,
or ~/.config/rafiki/presets.json).

Presets bundle a model and label defaults that can be applied at spawn time
with --preset NAME or the RAFIKI_DEFAULT_PRESET environment variable.

Label filters narrow the output using the same AND-match semantics as
'fundi list --label': every --label k=v pair must appear in the preset's labels,
and every --has-label key must be present.`,
		Args: cobra.NoArgs,
		RunE: runPresets,
	}
	cmd.Flags().StringArray("label", nil, "AND-match label k=v (repeatable)")
	cmd.Flags().StringArray("has-label", nil, "Filter presets that have this label key (repeatable)")
	_ = cmd.RegisterFlagCompletionFunc("label", func(cmd *cobra.Command, _ []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		return completeLabelPairs(cmd, toComplete), cobra.ShellCompDirectiveNoFileComp
	})
	_ = cmd.RegisterFlagCompletionFunc("has-label", func(cmd *cobra.Command, _ []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		return completeLabelKeys(cmd, toComplete), cobra.ShellCompDirectiveNoFileComp
	})
	return cmd
}

func runPresets(cmd *cobra.Command, _ []string) error {
	c := mustDial(cmd)
	defer c.Close()

	var labels map[string]string
	var hasLabel []string

	if labelPairs, _ := cmd.Flags().GetStringArray("label"); len(labelPairs) > 0 {
		var err error
		labels, err = parseLabelFilterPairs(labelPairs)
		if err != nil {
			return fmt.Errorf("--label: %w", err)
		}
	}
	if hls, _ := cmd.Flags().GetStringArray("has-label"); len(hls) > 0 {
		var err error
		hasLabel, err = parseLabelFilterKeys(hls)
		if err != nil {
			return fmt.Errorf("--has-label: %w", err)
		}
	}

	resp, err := c.Request(cmdCtx(cmd), protocol.ListPresetsRequest{
		Type:     protocol.TypeCtrlListPresets,
		Labels:   labels,
		HasLabel: hasLabel,
	})
	if err != nil {
		return err
	}
	if !resp.Success {
		return fmt.Errorf("ctrl_list_presets: %s", client.FormatError(resp))
	}

	var data protocol.ListPresetsResponseData
	if err := json.Unmarshal(resp.Data, &data); err != nil {
		return fmt.Errorf("decode presets response: %w", err)
	}

	mode, useColor := outputOpts(cmd)
	return renderPresets(os.Stdout, data.Presets, mode, useColor)
}

func renderPresets(w io.Writer, presets []protocol.PresetInfo, mode outputMode, useColor bool) error {
	if mode == outputJSON {
		if presets == nil {
			presets = []protocol.PresetInfo{}
		}
		return writeJSON(w, map[string]any{"presets": presets})
	}

	tw := table.NewWriter()
	tw.SetOutputMirror(w)
	st := table.StyleLight
	st.Color = table.ColorOptions{}
	tw.SetStyle(st)

	colNames := []string{"NAME", "MODEL", "LABELS"}
	headerRow := make(table.Row, len(colNames))
	for i, name := range colNames {
		if useColor {
			headerRow[i] = dim(name)
		} else {
			headerRow[i] = name
		}
	}
	tw.AppendHeader(headerRow)

	for _, p := range presets {
		tw.AppendRow(table.Row{
			p.Name,
			defaultDash(p.Model),
			formatLabels(p.Labels, 60, true),
		})
	}

	tw.Render()
	return nil
}
