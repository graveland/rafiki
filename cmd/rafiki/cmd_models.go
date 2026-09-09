package main

import (
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"go.graveland.dev/rafiki/pkg/modelquery"
	"go.graveland.dev/rafiki/pkg/table"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
)

func newModelsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "models",
		Aliases: []string{"model"},
		Short:   "List the models the daemon offers",
		Long: `List LLM models the DAEMON offers, through its Connect control plane, filtered
and ordered by the same engine the cockpit picker uses.

Sources reported per row:
  user-config   your configured providers (providers.toml)
  builtin       curated static list of common provider/model IDs
  alias         providers.toml [providers.<name>.models.<alias>] entries
  openrouter    the daemon's OpenRouter catalog snapshot
  local         the daemon's live probe of a configured local provider

An empty CTX or price cell means the daemon has no catalog entry for that
id — every locally-served model — which is not the same as a zero. The V
column says ? there for the same reason, and a bound (--min-ctx, --max-in,
…) ADMITS such a row: unknown is never read as "no". The one exception is
--scored, which asks for a benchmark score and therefore rejects unscored
rows; likewise --paid-only rejects only a KNOWN price of zero.

Query flags:
  --sort F[:asc|:desc]   repeatable, first key wins; F is ctx, in, out, cache,
                         max out, age, intel, code or agentic
                         (default: agentic:desc, intel:desc)
  --min-*/--max-*        bounds on ctx, in, out, intel, code, agentic; prices
                         are per million tokens, ctx takes k/m suffixes (200k, 1m)
  --paid-only            a known price strictly above zero
  --scored               carries an intelligence score
  --tools --vision       drop rows the catalog definitively says lack the
                         capability (unknowns are kept)
  --q TEXT               case-insensitive substring on model id or name
  --verbose              add the sparse columns ID, CACHE, MAX OUT, INTEL,
                         TOOLS, CREATED, CUTOFF, EXPIRES

The command always asks the daemon and then rewrites the completion cache, so
it is the documented escape hatch for a stale completion: run it after adding
a provider or starting a new local model, instead of waiting out the cache.`,
		Args: cobra.NoArgs,
		RunE: runModels,
	}
	cmd.Flags().String("provider", "", "Filter by provider name (e.g. anthropic, openai, ollama)")
	cmd.Flags().String("source", "", "Filter by source: user-config|builtin|alias|openrouter|local")
	cmd.Flags().StringArray("sort", nil, "Order rows: field[:asc|:desc], repeatable, first key wins")
	cmd.Flags().String("min-ctx", "", "Minimum context window in tokens (k/m suffixes: 200k, 1m)")
	cmd.Flags().String("max-ctx", "", "Maximum context window in tokens (k/m suffixes)")
	cmd.Flags().String("min-in", "", "Minimum prompt price, per million tokens")
	cmd.Flags().String("max-in", "", "Maximum prompt price, per million tokens")
	cmd.Flags().String("min-out", "", "Minimum completion price, per million tokens")
	cmd.Flags().String("max-out", "", "Maximum completion price, per million tokens")
	cmd.Flags().String("min-intel", "", "Minimum intelligence score")
	cmd.Flags().String("max-intel", "", "Maximum intelligence score")
	cmd.Flags().String("min-code", "", "Minimum coding score")
	cmd.Flags().String("max-code", "", "Maximum coding score")
	cmd.Flags().String("min-agentic", "", "Minimum agentic score")
	cmd.Flags().String("max-agentic", "", "Maximum agentic score")
	cmd.Flags().Bool("paid-only", false, "Keep only models with a known, non-zero price")
	cmd.Flags().Bool("scored", false, "Keep only models carrying a benchmark (intelligence) score")
	cmd.Flags().Bool("tools", false, "Keep only models the catalog says can tool-call (unknowns kept)")
	cmd.Flags().Bool("vision", false, "Keep only models the catalog says accept images (unknowns kept)")
	cmd.Flags().String("q", "", "Case-insensitive substring match on model id or name")
	cmd.Flags().Bool("verbose", false, "Show the sparse columns: ID, CACHE, MAX OUT, INTEL, TOOLS, CREATED, CUTOFF, EXPIRES")
	_ = cmd.RegisterFlagCompletionFunc("provider", cobra.FixedCompletions(
		[]string{"anthropic", "openai", "google", "xai", "ollama", "lmstudio"},
		cobra.ShellCompDirectiveNoFileComp,
	))
	_ = cmd.RegisterFlagCompletionFunc("source", cobra.FixedCompletions(
		[]string{"user-config", "builtin", "alias", "openrouter", "local"},
		cobra.ShellCompDirectiveNoFileComp,
	))
	_ = cmd.RegisterFlagCompletionFunc("sort", cobra.FixedCompletions(
		[]string{"ctx", "in", "out", "cache", "max out", "age", "intel", "code", "agentic"},
		cobra.ShellCompDirectiveNoFileComp,
	))
	return cmd
}

func runModels(cmd *cobra.Command, _ []string) error {
	// Parse the query BEFORE the round trip: a typo'd --sort or bound is a
	// user-input error and must fail fast, not after a two-second fetch.
	q, err := modelsQueryFromFlags(cmd)
	if err != nil {
		return err
	}
	provider, _ := cmd.Flags().GetString("provider")
	rows, err := fetchModelRows(cmd, provider, "")
	if err != nil {
		return err
	}
	return reportModels(cmd, rows, q)
}

// reportModels is runModels' second half — everything after the daemon round
// trip — split out so a test can pin the cache contract without a live
// daemon. The cache rewrite sees the FULL row set; every display filter lives
// downstream of it, in renderModelRows.
func reportModels(cmd *cobra.Command, rows []*rafikiv1.ModelRow, q modelsQuery) error {
	// rafiki models is the escape hatch for a stale completion cache: it always
	// asks the daemon, so it also rewrites what completion reads next. This is
	// the documented way to pick up a newly-configured provider or a fresh
	// ollama model without waiting out modelCacheTTL. Written from the FULL
	// row set — --source is a display filter, not a cache-shaping one.
	ids := make([]string, 0, len(rows))
	for _, r := range rows {
		ids = append(ids, r.GetId())
	}
	cacheWrite("models-fundi", completionEndpointKey(cmd), ids)

	mode, useColor, err := outputOpts(cmd)
	if err != nil {
		return err
	}
	return renderModelRows(os.Stdout, rows, q, mode, useColor)
}

// modelsQuery is the models command's parsed display flags: what to filter,
// how to order, and whether to widen the table. runModels builds it once so
// renderModelRows stays testable without cobra, and so a bad flag value fails
// before the daemon round trip.
type modelsQuery struct {
	source  string
	q       string
	bounds  map[modelquery.Field]modelquery.Bound
	sort    []modelquery.SortKey
	tools   bool
	vision  bool
	verbose bool
}

// modelsBoundFlags pairs each --min-/--max- flag with the field it
// constrains. Price bounds are per-million tokens, the unit
// modelquery.Field.Value reports; context bounds are raw tokens.
var modelsBoundFlags = []struct {
	minFlag, maxFlag string
	field            modelquery.Field
}{
	{"min-ctx", "max-ctx", modelquery.FieldContext},
	{"min-in", "max-in", modelquery.FieldPromptUSD},
	{"min-out", "max-out", modelquery.FieldCompletionUSD},
	{"min-intel", "max-intel", modelquery.FieldIntelligence},
	{"min-code", "max-code", modelquery.FieldCoding},
	{"min-agentic", "max-agentic", modelquery.FieldAgentic},
}

// modelsQueryFromFlags reads the query flags off a models command and builds
// the filter/sort description renderModelRows applies.
func modelsQueryFromFlags(cmd *cobra.Command) (modelsQuery, error) {
	q := modelsQuery{}
	q.source, _ = cmd.Flags().GetString("source")
	q.q, _ = cmd.Flags().GetString("q")
	q.tools, _ = cmd.Flags().GetBool("tools")
	q.vision, _ = cmd.Flags().GetBool("vision")
	q.verbose, _ = cmd.Flags().GetBool("verbose")

	q.bounds = make(map[modelquery.Field]modelquery.Bound, len(modelsBoundFlags)+2)
	for _, bf := range modelsBoundFlags {
		if err := addModelBound(cmd, q.bounds, bf.field, bf.minFlag, bf.maxFlag); err != nil {
			return modelsQuery{}, err
		}
	}

	// Two predicates that are not thresholds, each on one field: --paid-only
	// on the prompt price (the picker's ">free" stop lives there too), and
	// --scored on intelligence — the ONE deliberate exception to the
	// admit-unknowns rule, and the only way to ask for benchmarked models
	// only, because "intel >= 40" admits unscored rows by that rule.
	if paid, _ := cmd.Flags().GetBool("paid-only"); paid {
		b := q.bounds[modelquery.FieldPromptUSD]
		b.PaidOnly = true
		q.bounds[modelquery.FieldPromptUSD] = b
	}
	if scored, _ := cmd.Flags().GetBool("scored"); scored {
		b := q.bounds[modelquery.FieldIntelligence]
		b.RequirePresent = true
		q.bounds[modelquery.FieldIntelligence] = b
	}

	sorts, _ := cmd.Flags().GetStringArray("sort")
	for _, s := range sorts {
		k, err := parseModelSortKey(s)
		if err != nil {
			return modelsQuery{}, err
		}
		q.sort = append(q.sort, k)
	}
	return q, nil
}

// addModelBound folds one flag pair into the bound set. Only a pair that
// constrains something enters the map — an unset bound must never reach
// AdmitsAll and look like a constraint.
func addModelBound(cmd *cobra.Command, bounds map[modelquery.Field]modelquery.Bound, field modelquery.Field, minFlag, maxFlag string) error {
	minStr, _ := cmd.Flags().GetString(minFlag)
	maxStr, _ := cmd.Flags().GetString(maxFlag)
	if minStr == "" && maxStr == "" {
		return nil
	}
	b := bounds[field]
	if minStr != "" {
		v, err := parseModelBoundValue(minStr, field)
		if err != nil {
			return fmt.Errorf("invalid --%s value %q: %w", minFlag, minStr, err)
		}
		b.Min = &v
	}
	if maxStr != "" {
		v, err := parseModelBoundValue(maxStr, field)
		if err != nil {
			return fmt.Errorf("invalid --%s value %q: %w", maxFlag, maxStr, err)
		}
		b.Max = &v
	}
	bounds[field] = b
	return nil
}

// parseModelBoundValue parses one bound value in the field's own unit: a
// token count with k/m suffixes for context, a plain float for everything
// else (per-million prices, benchmark scores).
func parseModelBoundValue(s string, field modelquery.Field) (float64, error) {
	if field == modelquery.FieldContext {
		return parseTokenCount(s)
	}
	v, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	return v, err
}

// parseTokenCount parses a token count with an optional k/m suffix: 200k →
// 200000, 1m → 1000000, 128000 → 128000.
func parseTokenCount(s string) (float64, error) {
	t := strings.ToLower(strings.TrimSpace(s))
	mult := 1.0
	switch {
	case strings.HasSuffix(t, "k"):
		mult, t = 1e3, strings.TrimSuffix(t, "k")
	case strings.HasSuffix(t, "m"):
		mult, t = 1e6, strings.TrimSuffix(t, "m")
	}
	n, err := strconv.ParseFloat(t, 64)
	if err != nil {
		return 0, err
	}
	return n * mult, nil
}

// parseModelSortKey parses one --sort value: field[:asc|:desc]. The direction
// suffix splits on the LAST colon, and the field resolves through
// modelquery.ParseField, so the accepted spellings stay shared with the
// picker and the agent tool.
func parseModelSortKey(s string) (modelquery.SortKey, error) {
	field, dir := s, ""
	if i := strings.LastIndex(s, ":"); i >= 0 {
		field, dir = s[:i], s[i+1:]
	}
	desc := false
	switch dir {
	case "", "asc":
	case "desc":
		desc = true
	default:
		return modelquery.SortKey{}, fmt.Errorf("unknown sort direction %q (try :asc or :desc)", dir)
	}
	f, ok := modelquery.ParseField(field)
	if !ok {
		return modelquery.SortKey{}, fmt.Errorf(
			"unknown sort field %q (try ctx, in, out, cache, max out, age, intel, code, agentic)", field)
	}
	return modelquery.SortKey{Field: f, Desc: desc}, nil
}

// defaultModelSort is what --sort falls back to: agentic first, intelligence
// as tiebreak, both descending — the useful order for picking an agent model.
// Under the presence rule the unscored rows land LAST despite the descending
// keys (an absent value is no answer, never "the largest"), which visibly
// parks the local fleet at the bottom.
func defaultModelSort() []modelquery.SortKey {
	return []modelquery.SortKey{
		{Field: modelquery.FieldAgentic, Desc: true},
		{Field: modelquery.FieldIntelligence, Desc: true},
	}
}

// renderModelRows renders the daemon's model rows in the resolved output mode:
// table by default, JSON/JSONL on request — the same resolveOutputMode every
// other list-shaped verb answers to. Filters run first (--q, --source, then
// the bounds), then modelquery.Sort orders what survives. The completion
// cache has already been written by the time this runs — from the full row
// set, before any of this.
func renderModelRows(w io.Writer, rows []*rafikiv1.ModelRow, q modelsQuery, mode outputMode, useColor bool) error {
	rows = filterModelRows(rows, q)
	keys := q.sort
	if len(keys) == 0 {
		keys = defaultModelSort()
	}
	modelquery.Sort(rows, keys)

	switch mode {
	case outputJSON:
		// encoding/json over the generated structs: their json tags spell the
		// fields snake_case, and omitempty drops an unset OPTIONAL pointer —
		// "the daemon has no catalog entry" stays absent rather than reading
		// as a zero, while a real zero (a pointer TO 0) still prints as 0.
		return writeJSON(w, map[string]any{"models": rows})
	case outputJSONL:
		// One ModelRow per line, unwrapped — no envelope, so the output is
		// consumable line by line (jq -s, grep, tail -f).
		anyRows := make([]any, len(rows))
		for i, r := range rows {
			anyRows[i] = r
		}
		return writeJSONL(w, anyRows)
	}
	return renderModelRowsWidth(w, rows, q, useColor, modelsDisplayWidth())
}

// filterModelRows applies every display filter in one pass and returns a new
// slice, so the caller's row set — already in the completion cache — is
// never reordered or reshaped by a display choice.
//
// The capability flags follow the absence rule: unknown support is KEPT,
// never read as "no", so only a definitive catalog claim filters a row out.
// The numeric bounds inherit modelquery.AdmitsAll's rule that a row the
// catalog cannot answer for is admitted.
func filterModelRows(rows []*rafikiv1.ModelRow, q modelsQuery) []*rafikiv1.ModelRow {
	out := make([]*rafikiv1.ModelRow, 0, len(rows))
	for _, r := range rows {
		if !qMatchesModel(q.q, r) {
			continue
		}
		if q.source != "" && r.GetSource() != q.source {
			continue
		}
		if len(q.bounds) > 0 && !modelquery.AdmitsAll(q.bounds, r) {
			continue
		}
		if q.tools && modelquery.Tools(r) == modelquery.SupportNo {
			continue
		}
		if q.vision && modelquery.Vision(r) == modelquery.SupportNo {
			continue
		}
		out = append(out, r)
	}
	return out
}

// qMatchesModel is the one filter modelquery lacks: a case-insensitive
// substring over the id and the display name.
func qMatchesModel(q string, r *rafikiv1.ModelRow) bool {
	if q == "" {
		return true
	}
	needle := strings.ToLower(q)
	return strings.Contains(strings.ToLower(r.GetId()), needle) ||
		strings.Contains(strings.ToLower(r.GetName()), needle)
}

// renderModelRowsWidth renders the table at an explicit width — the seam that
// lets a test drive the drop order without a TTY.
//
// Column indices, and therefore the Drop order, are fixed: 0 MODEL, 1 SOURCE,
// 2 CTX, 3 IN $, 4 OUT $, 5 CODE, 6 AGENTIC, 7 V — then, with --verbose,
// 8 ID, 9 CACHE, 10 MAX OUT, 11 INTEL, 12 TOOLS, 13 CREATED, 14 CUTOFF,
// 15 EXPIRES. The sparsest columns drop first (the verbose ones, which most
// rows leave empty), then CODE, AGENTIC, SOURCE, V, CTX and the prices;
// MODEL is not in Drop, so it never drops.
func renderModelRowsWidth(w io.Writer, rows []*rafikiv1.ModelRow, q modelsQuery, useColor bool, width int) error {
	headers := []string{"MODEL", "SOURCE", "CTX", "IN $", "OUT $", "CODE", "AGENTIC", "V"}
	drop := []int{5, 6, 1, 7, 2, 3, 4}
	if q.verbose {
		headers = append(headers, "ID", "CACHE", "MAX OUT", "INTEL", "TOOLS", "CREATED", "CUTOFF", "EXPIRES")
		drop = append([]int{13, 14, 15, 12, 11, 10, 9, 8}, drop...)
	}

	tb := table.New(w, table.Options{Color: useColor, Width: width, Drop: drop})
	tb.Header(headers...)
	for _, r := range rows {
		cells := []string{
			modelDisplayCell(r), r.GetSource(), optInt(r.ContextWindow),
			perMillion(r.PromptUsd), perMillion(r.CompletionUsd),
			optFloat(r.CodingIndex), optFloat(r.AgenticIndex),
			visionCell(r.GetInputModalities()),
		}
		if q.verbose {
			cells = append(cells,
				r.GetId(), perMillion(r.CacheReadUsd), optInt(r.MaxCompletionTokens),
				optFloat(r.IntelligenceIndex), supportCell(modelquery.Tools(r)),
				createdCell(r.Created), dashWhen(r.KnowledgeCutoff), dashWhen(r.ExpiresAt),
			)
		}
		tb.Row(cells...)
	}
	return tb.Render()
}

// modelsDisplayWidth is the width the table fits: the terminal width on a
// TTY, so columns drop in declared order rather than wrapping mid-cell; 0 on
// a pipe, where the rule is every column, no ANSI, no data dropped.
func modelsDisplayWidth() int {
	if !isStdoutTTY() {
		return 0
	}
	return tailDisplayWidth()
}

// modelDisplayCell shows the bare model part — openrouter/anthropic/claude-x
// renders as anthropic/claude-x, killing the triple-openrouter row labels;
// SOURCE carries the provider identity and --verbose the full id. An alias
// can leave Model empty, so the full id is the fallback.
func modelDisplayCell(r *rafikiv1.ModelRow) string {
	if m := r.GetModel(); m != "" {
		return m
	}
	return r.GetId()
}

// supportCell renders the tri-state capability claim the way the V column
// already does: ? for no catalog entry, never "no".
func supportCell(s modelquery.Support) string {
	switch s {
	case modelquery.SupportYes:
		return "yes"
	case modelquery.SupportNo:
		return "no"
	}
	return "?"
}

// createdCell renders OpenRouter's listing date (unix seconds). Absent — nil
// or a literal 0 — is an em dash, matching every other sparse cell.
func createdCell(v *int64) string {
	if v == nil || *v <= 0 {
		return "—"
	}
	return time.Unix(*v, 0).UTC().Format("2006-01-02")
}

// dashWhen renders an empty string as an em dash, matching optInt/optFloat's
// absent-value rendering.
func dashWhen(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

// optFloat renders a third-party index (intelligence, coding, agentic) with
// the same absent rule as optInt: absent is UNSCORED, never zero — 62% of the
// catalog carries no benchmark at all, and a 0.0 would read as the worst
// model rather than as no answer.
func optFloat(v *float64) string {
	if v == nil {
		return "—"
	}
	return fmt.Sprintf("%.1f", *v)
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
