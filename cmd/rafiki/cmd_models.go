package main

import (
	"fmt"
	"os"
	"strconv"

	"github.com/jedib0t/go-pretty/v6/table"
	"github.com/spf13/cobra"
)

func newModelsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "models",
		Aliases: []string{"model"},
		Short:   "List the models the daemon offers",
		Long: `List LLM models the DAEMON offers, through its Connect control plane.

This is the daemon's view — the same list --model completion reads — not what
the CLI could discover on its own. Rows carry what the daemon knows about each
id: its source, and, when it has a catalog entry, the context window, per-million
prices and whether the model accepts images.

Sources reported per row:
  user-config   your configured providers (providers.toml)
  builtin       curated static list of common provider/model IDs
  alias         providers.toml [providers.<name>.models.<alias>] entries
  openrouter    the daemon's OpenRouter catalog snapshot
  local         the daemon's live probe of a configured local provider

An empty CONTEXT or price cell means the daemon has no catalog entry for that
id — every locally-served model — which is not the same as a zero. The VISION
column says ? there for the same reason.

The command always asks the daemon and then rewrites the completion cache, so
it is the documented escape hatch for a stale completion: run it after adding
a provider or starting a new local model, instead of waiting out the cache.`,
		Args: cobra.NoArgs,
		RunE: runModels,
	}
	cmd.Flags().String("provider", "", "Filter by provider name (e.g. anthropic, openai, ollama)")
	cmd.Flags().String("source", "", "Filter by source: user-config|builtin|alias|openrouter|local")
	_ = cmd.RegisterFlagCompletionFunc("provider", cobra.FixedCompletions(
		[]string{"anthropic", "openai", "google", "xai", "ollama", "lmstudio"},
		cobra.ShellCompDirectiveNoFileComp,
	))
	_ = cmd.RegisterFlagCompletionFunc("source", cobra.FixedCompletions(
		[]string{"user-config", "builtin", "alias", "openrouter", "local"},
		cobra.ShellCompDirectiveNoFileComp,
	))
	return cmd
}

func runModels(cmd *cobra.Command, _ []string) error {
	provider, _ := cmd.Flags().GetString("provider")
	source, _ := cmd.Flags().GetString("source")

	rows, err := fetchModelRows(cmd, provider, "")
	if err != nil {
		return err
	}

	// rafiki models is the escape hatch for a stale completion cache: it always
	// asks the daemon, so it also rewrites what completion reads next. This is
	// the documented way to pick up a newly-configured provider or a fresh
	// ollama model without waiting out modelCacheTTL.
	ids := make([]string, 0, len(rows))
	for _, r := range rows {
		ids = append(ids, r.GetId())
	}
	cacheWrite("models-fundi", completionEndpointKey(), ids)

	t := table.NewWriter()
	t.SetOutputMirror(os.Stdout)
	t.AppendHeader(table.Row{"ID", "PROVIDER", "SOURCE", "CONTEXT", "IN $/M", "OUT $/M", "VISION"})
	for _, r := range rows {
		if source != "" && r.GetSource() != source {
			continue
		}
		t.AppendRow(table.Row{
			r.GetId(), r.GetProvider(), r.GetSource(),
			optInt(r.ContextWindow), perMillion(r.PromptUsd), perMillion(r.CompletionUsd),
			visionCell(r.GetInputModalities()),
		})
	}
	t.Render()
	return nil
}

// optInt renders an absent optional as an em dash. A zero is a REAL value and
// prints as 0 — the whole reason these fields are optional.
func optInt(v *int32) string {
	if v == nil {
		return "—"
	}
	return strconv.Itoa(int(*v))
}

// perMillion renders a per-token price the way humans quote them. Absent stays
// absent: an unpriced model must not read as free.
func perMillion(v *float64) string {
	if v == nil {
		return "—"
	}
	return fmt.Sprintf("%.2f", *v*1e6)
}

// visionCell answers from the catalog's claim, and says so when there is none.
// Empty modalities means the daemon has NO catalog entry — every locally-served
// model — which is not the same as "no vision", and rendering it as "no" would
// hide the whole local fleet from anyone filtering on this column.
func visionCell(mods []string) string {
	if len(mods) == 0 {
		return "?"
	}
	for _, m := range mods {
		if m == "image" {
			return "yes"
		}
	}
	return "no"
}
