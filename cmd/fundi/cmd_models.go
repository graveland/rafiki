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

func newModelsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "models",
		Aliases: []string{"model"},
		Short:   "List available models from all sources",
		Long: `List LLM models discoverable by the controller.

Sources (in priority order):
  user-config   ~/.pi/agent/models.json (your configured providers)
  builtin       curated static list of common provider/model IDs
  ollama        live Ollama server (OLLAMA_HOST, default localhost:11434)
  lmstudio      live LM Studio server (LM_STUDIO_HOST, default localhost:1234)

Results are deduplicated by ID; user-config entries shadow builtin entries
with the same ID and carry the user's display name.`,
		Args: cobra.NoArgs,
		RunE: runModels,
	}
	cmd.Flags().String("provider", "", "Filter by provider name (e.g. anthropic, openai, ollama)")
	cmd.Flags().String("source", "", "Filter by source: builtin|user-config|ollama|lmstudio")
	_ = cmd.RegisterFlagCompletionFunc("provider", cobra.FixedCompletions(
		[]string{"anthropic", "openai", "google", "xai", "ollama", "lmstudio"},
		cobra.ShellCompDirectiveNoFileComp,
	))
	_ = cmd.RegisterFlagCompletionFunc("source", cobra.FixedCompletions(
		[]string{"builtin", "user-config", "ollama", "lmstudio"},
		cobra.ShellCompDirectiveNoFileComp,
	))
	return cmd
}

func runModels(cmd *cobra.Command, _ []string) error {
	c := mustDial(cmd)
	defer c.Close()

	provider, _ := cmd.Flags().GetString("provider")
	source, _ := cmd.Flags().GetString("source")

	resp, err := c.Request(cmdCtx(cmd), protocol.ListModelsRequest{
		Type:     protocol.TypeCtrlListModels,
		Provider: provider,
	})
	if err != nil {
		return err
	}
	if !resp.Success {
		return fmt.Errorf("ctrl_list_models: %s", client.FormatError(resp))
	}

	var data protocol.ListModelsResponseData
	if err := json.Unmarshal(resp.Data, &data); err != nil {
		return fmt.Errorf("decode models response: %w", err)
	}

	// Source filter is applied client-side; provider filter is server-side.
	infos := data.Models
	if source != "" {
		filtered := infos[:0]
		for _, m := range infos {
			if m.Source == source {
				filtered = append(filtered, m)
			}
		}
		infos = filtered
	}

	mode, useColor := outputOpts(cmd)
	return renderModels(os.Stdout, infos, mode, useColor)
}

func renderModels(w io.Writer, infos []protocol.ModelInfo, mode outputMode, useColor bool) error {
	if mode == outputJSON {
		if infos == nil {
			infos = []protocol.ModelInfo{}
		}
		return writeJSON(w, map[string]any{"models": infos})
	}

	tw := table.NewWriter()
	tw.SetOutputMirror(w)
	st := table.StyleLight
	st.Color = table.ColorOptions{}
	tw.SetStyle(st)

	colNames := []string{"ID", "NAME", "PROVIDER", "MODEL", "SOURCE"}
	headerRow := make(table.Row, len(colNames))
	for i, name := range colNames {
		if useColor {
			headerRow[i] = dim(name)
		} else {
			headerRow[i] = name
		}
	}
	tw.AppendHeader(headerRow)

	for _, m := range infos {
		tw.AppendRow(table.Row{
			m.ID,
			defaultDash(m.Name),
			defaultDash(m.Provider),
			defaultDash(m.Model),
			m.Source,
		})
	}

	tw.Render()
	return nil
}
