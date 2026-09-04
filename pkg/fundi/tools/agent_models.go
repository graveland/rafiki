package tools

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

func init() {
	DefaultBlueprint.Register(&AgentModelsBlueprint{})
}

// ModelInfo is one model an agent may spawn a child on.
//
// Every catalog-sourced field is a POINTER because absent and zero differ all
// the way down: a reported price of 0 (a free model) and a model the catalog
// has never heard of (every locally-served one) are different facts that rank
// differently. A > 0 guard anywhere here destroys the distinction.
type ModelInfo struct {
	ID       string
	Provider string
	Name     string

	ContextWindow       *int
	MaxCompletionTokens *int
	// Prices are USD PER TOKEN, exactly as the catalog reports them. The
	// helpers below convert to the per-million unit everything is rendered in.
	PromptUSD     *float64
	CompletionUSD *float64
	CacheReadUSD  *float64

	IntelligenceIndex *float64
	CodingIndex       *float64
	AgenticIndex      *float64

	// Tools and Vision are tri-states spelled "yes", "no" or "unknown".
	// Unknown means the catalog has no entry, NOT that the capability is
	// missing -- treating it as "no" hides the entire local fleet.
	Tools  string
	Vision string
}

func perMillion(v *float64) (float64, bool) {
	if v == nil {
		return 0, false
	}
	return *v * 1e6, true
}

// PromptUSDPerMillion returns the input price per million tokens. ok is false
// when the catalog has no answer -- never zero.
func (m ModelInfo) PromptUSDPerMillion() (float64, bool) { return perMillion(m.PromptUSD) }

// CompletionUSDPerMillion returns the output price per million tokens.
func (m ModelInfo) CompletionUSDPerMillion() (float64, bool) { return perMillion(m.CompletionUSD) }

// ModelQuery narrows what agent_models returns.
//
// A zero ModelQuery is the DISCOVERY call: it asks for a summary of what
// exists rather than for rows. Constrained fields are pointers so "not asked
// for" stays distinct from a bound of zero.
type ModelQuery struct {
	// Kind scopes the answer to what that runtime can actually resolve. A
	// claude child cannot run an OpenRouter id, and offering one produces a
	// child that spawns, attaches and never answers.
	Kind string `json:"kind"`

	MaxInUSD   *float64 `json:"max_in_usd"`  // per million input tokens
	MaxOutUSD  *float64 `json:"max_out_usd"` // per million output tokens
	MinContext *int     `json:"min_context"` // tokens

	// Needs holds capability requirements: "tools", "vision", "reasoning".
	// A model the catalog cannot answer for is KEPT.
	Needs []string `json:"needs"`

	Sort  string `json:"sort"`
	Limit int    `json:"limit"`
}

// constrained reports whether the caller narrowed anything. An unconstrained
// query gets the summary rather than several hundred rows.
func (q ModelQuery) constrained() bool {
	return q.MaxInUSD != nil || q.MaxOutUSD != nil || q.MinContext != nil ||
		len(q.Needs) > 0 || q.Sort != "" || q.Limit > 0
}

const (
	modelsDefaultLimit = 20
	modelsMaxLimit     = 50
)

// modelSortKeys are the orderings agent_models accepts. Every one of these
// must resolve through modelquery.ParseField on the daemon side; a test in
// cmd/rafikid pins that, because a key accepted here and unknown there would
// silently order on nothing.
var modelSortKeys = []string{
	"in", "out", "ctx", "agentic", "intel", "code", "cache", "max out", "newest", "model",
}

// modelNeeds are the capability requirements agent_models accepts.
var modelNeeds = []string{"tools", "vision", "reasoning"}

const agentModelsDescription = "List the models you may spawn an agent on, with " +
	"their cost, context window and capability scores. Use this before agent_spawn " +
	"when you want to put a worker on a cheaper or a stronger model than your own, " +
	"rather than guessing at a model id.\n\n" +
	"Call it with NO arguments first: there are several hundred models, so a bare " +
	"call answers with a summary — how many there are and how price, context and " +
	"capability are distributed — not a list. Use that to aim a narrowed query, " +
	"then call again with filters. Filter with max_in_usd / max_out_usd (USD per " +
	"MILLION tokens), min_context, and needs; order with sort; cap with limit.\n\n" +
	"A model the catalog knows nothing about — every locally-served one — has no " +
	"price, no context window and no score. Those are KEPT by a filter rather than " +
	"dropped, and shown as \"-\": absent means unknown, never zero or \"cannot\"."

type AgentModelsBlueprint struct{}

func (AgentModelsBlueprint) Name() string        { return "agent_models" }
func (AgentModelsBlueprint) Description() string { return agentModelsDescription }
func (AgentModelsBlueprint) InputSchema() Schema {
	return Schema{
		Type: "object",
		Properties: []SchemaProperty{
			{Name: "kind", Type: "string",
				Description: "Agent runtime the model must be runnable by: \"fundi\" (default) or \"claude\". A claude child resolves only Anthropic ids."},
			{Name: "max_in_usd", Type: "number",
				Description: "Maximum input price in USD per MILLION tokens (e.g. 1.0). Models with no published price are kept."},
			{Name: "max_out_usd", Type: "number",
				Description: "Maximum output price in USD per MILLION tokens. Models with no published price are kept."},
			{Name: "min_context", Type: "integer",
				Description: "Minimum context window in tokens (e.g. 200000). Models with no published window are kept."},
			{Name: "needs", Type: "array",
				Description: "Capabilities the model must claim: \"tools\", \"vision\", \"reasoning\". A model the catalog cannot answer for is kept. Spawning an agent on a model that cannot call tools produces a worker that does nothing.",
				Items:       &Schema{Type: "string"}},
			{Name: "sort", Type: "string",
				Description: "Order the results: \"in\" or \"out\" (cheapest first), \"ctx\", \"agentic\", \"intel\", \"code\" (best first), \"newest\", \"model\". Unscored models sort last either way."},
			{Name: "limit", Type: "integer",
				Description: "How many rows to return. Default 20, maximum 50. The reply always says how many matched before the cap."},
		},
	}
}

func (AgentModelsBlueprint) Execute(context.Context, ToolInput) (ToolResult, error) {
	panic("blueprint: call Materialize first")
}

func (AgentModelsBlueprint) Materialize(opts ToolOpts) (Tool, error) {
	if opts.Agents == nil {
		return nil, nil
	}
	return &agentModelsTool{agents: opts.Agents}, nil
}

type agentModelsTool struct {
	AgentModelsBlueprint
	agents AgentSpawner
}

func (t *agentModelsTool) Execute(ctx context.Context, in ToolInput) (ToolResult, error) {
	if err := ctx.Err(); err != nil {
		return ToolResult{}, err
	}
	var q ModelQuery
	if err := in.Unmarshal(&q); err != nil {
		return ToolResult{}, fmt.Errorf("agent_models: %w", err)
	}
	if err := validateModelQuery(q); err != nil {
		return ToolResult{}, err
	}

	rows, err := t.agents.Models(ctx, q)
	if err != nil {
		return ToolResult{}, fmt.Errorf("agent_models: %w", err)
	}

	// The summary and the zero-match diagnostic both describe the WHOLE
	// universe for this kind, not the filtered slice -- describing the empty
	// slice would tell the agent nothing it could re-aim with.
	if !q.constrained() || len(rows) == 0 {
		all := rows
		if q.constrained() {
			all, err = t.agents.Models(ctx, ModelQuery{Kind: q.Kind})
			if err != nil {
				return ToolResult{}, fmt.Errorf("agent_models: %w", err)
			}
		}
		return NewTextResult(renderModelSummary(all, q)), nil
	}
	return NewTextResult(renderModelRows(rows, q)), nil
}

func validateModelQuery(q ModelQuery) error {
	if q.Sort != "" && !containsFold(modelSortKeys, q.Sort) {
		return fmt.Errorf("agent_models: unknown sort %q; valid keys are %s",
			q.Sort, strings.Join(modelSortKeys, ", "))
	}
	for _, n := range q.Needs {
		if !containsFold(modelNeeds, n) {
			return fmt.Errorf("agent_models: unknown capability %q in needs; valid values are %s",
				n, strings.Join(modelNeeds, ", "))
		}
	}
	return nil
}

func containsFold(list []string, want string) bool {
	for _, v := range list {
		if strings.EqualFold(v, want) {
			return true
		}
	}
	return false
}

// --- rendering ---

// renderModelRows prints the window the agent asked for. Columns are fixed so
// the same fact sits in the same place on every row; an absent value is "-",
// never a zero.
func renderModelRows(rows []ModelInfo, q ModelQuery) string {
	limit := q.Limit
	if limit <= 0 {
		limit = modelsDefaultLimit
	}
	if limit > modelsMaxLimit {
		limit = modelsMaxLimit
	}
	matched := len(rows)
	shown := rows
	if len(shown) > limit {
		shown = shown[:limit]
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "%d matched, showing %d", matched, len(shown))
	if q.Sort != "" {
		fmt.Fprintf(&sb, " (sorted by %s, %s first)", q.Sort, SortDirectionWord(q.Sort))
	}
	sb.WriteString("\nprices are USD per million tokens; \"-\" means the catalog has no answer\n\n")

	idW := len("MODEL")
	for _, r := range shown {
		if len(r.ID) > idW {
			idW = len(r.ID)
		}
	}
	extra, extraHdr := extraModelColumn(q.Sort)

	fmt.Fprintf(&sb, "%-*s  %8s  %8s  %8s  %7s", idW, "MODEL", "IN$/M", "OUT$/M", "CTX", "TOOLS")
	if extraHdr != "" {
		fmt.Fprintf(&sb, "  %8s", extraHdr)
	}
	sb.WriteString("\n")

	for _, r := range shown {
		in, _ := r.PromptUSDPerMillion()
		out, _ := r.CompletionUSDPerMillion()
		fmt.Fprintf(&sb, "%-*s  %8s  %8s  %8s  %7s", idW, r.ID,
			priceCell(r.PromptUSD, in), priceCell(r.CompletionUSD, out),
			tokenCell(r.ContextWindow), orUnknown(r.Tools))
		if extra != nil {
			fmt.Fprintf(&sb, "  %8s", extra(r))
		}
		sb.WriteString("\n")
	}
	if matched > len(shown) {
		fmt.Fprintf(&sb, "\n%d more matched. Narrow further or raise limit (max %d).\n",
			matched-len(shown), modelsMaxLimit)
	}
	return sb.String()
}

// extraModelColumn adds the column the sort makes worth showing. Sorting by
// something invisible is a list that reorders for no visible reason.
func extraModelColumn(sortKey string) (func(ModelInfo) string, string) {
	switch strings.ToLower(sortKey) {
	case "agentic":
		return func(m ModelInfo) string { return scoreCell(m.AgenticIndex) }, "AGENTIC"
	case "intel":
		return func(m ModelInfo) string { return scoreCell(m.IntelligenceIndex) }, "INTEL"
	case "code":
		return func(m ModelInfo) string { return scoreCell(m.CodingIndex) }, "CODE"
	case "cache":
		return func(m ModelInfo) string {
			v, ok := perMillion(m.CacheReadUSD)
			if !ok {
				return "-"
			}
			return fmt.Sprintf("%.2f", v)
		}, "CACHE$/M"
	case "max out":
		return func(m ModelInfo) string { return tokenCell(m.MaxCompletionTokens) }, "MAXOUT"
	}
	return nil, ""
}

func priceCell(raw *float64, perM float64) string {
	if raw == nil {
		return "-"
	}
	if perM == 0 {
		return "free"
	}
	return fmt.Sprintf("%.2f", perM)
}

func tokenCell(v *int) string {
	if v == nil {
		return "-"
	}
	switch n := *v; {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1e6)
	case n >= 1_000:
		return fmt.Sprintf("%dk", n/1000)
	default:
		return fmt.Sprintf("%d", n)
	}
}

func scoreCell(v *float64) string {
	if v == nil {
		return "-"
	}
	return fmt.Sprintf("%.1f", *v)
}

func orUnknown(s string) string {
	if s == "" {
		return "?"
	}
	return s
}

// renderModelSummary answers the discovery call and the zero-match case.
//
// It reports the DISTRIBUTION, not just a count: a bare count tells the agent
// to filter but not where to aim, so it guesses a bound, matches three models
// and spends another turn. Which of the two cases this is comes from q --
// a constrained query that reached here matched nothing.
//
// all is always the WHOLE universe for the query's kind, never the filtered
// slice: describing an empty slice would tell the agent nothing it could
// re-aim with, which is the entire point of the zero-match branch.
func renderModelSummary(all []ModelInfo, q ModelQuery) string {
	var sb strings.Builder

	kind := q.Kind
	if kind == "" {
		kind = "fundi"
	}
	if q.constrained() {
		fmt.Fprintf(&sb, "0 of %d models matched.\n", len(all))
		if why := explainEmpty(all, q); why != "" {
			sb.WriteString(why)
		}
		sb.WriteString("\n")
	} else {
		fmt.Fprintf(&sb, "%d models available to a %s agent.\n\n", len(all), kind)
	}

	sb.WriteString("Narrow with: max_in_usd, max_out_usd (USD per million tokens), " +
		"min_context, needs, sort, limit.\n\n")

	sb.WriteString("What is available:\n")
	writeNumericLine(&sb, "  in$/M ", all, func(m ModelInfo) (float64, bool) { return m.PromptUSDPerMillion() })
	writeNumericLine(&sb, "  out$/M", all, func(m ModelInfo) (float64, bool) { return m.CompletionUSDPerMillion() })
	writeNumericLine(&sb, "  ctx   ", all, func(m ModelInfo) (float64, bool) {
		if m.ContextWindow == nil {
			return 0, false
		}
		return float64(*m.ContextWindow), true
	})

	free, unpriced := 0, 0
	for _, m := range all {
		v, ok := m.PromptUSDPerMillion()
		switch {
		case !ok:
			unpriced++
		case v == 0:
			free++
		}
	}
	fmt.Fprintf(&sb, "  price   %d free, %d unpriced (unpriced = no catalog entry, not free)\n",
		free, unpriced)

	writeSupportLine(&sb, "  tools ", all, func(m ModelInfo) string { return m.Tools })
	writeSupportLine(&sb, "  vision", all, func(m ModelInfo) string { return m.Vision })

	scored := 0
	for _, m := range all {
		if m.AgenticIndex != nil || m.IntelligenceIndex != nil || m.CodingIndex != nil {
			scored++
		}
	}
	fmt.Fprintf(&sb, "  scores  %d of %d carry intel/code/agentic; the rest sort last, never as zero\n",
		scored, len(all))

	sb.WriteString("\nExample: agent_models with max_in_usd=1.0, sort=\"agentic\", limit=10\n")
	return sb.String()
}

// explainEmpty names the bound that excluded everything, with the nearest
// value that would not have. One retry instead of five.
func explainEmpty(all []ModelInfo, q ModelQuery) string {
	var sb strings.Builder
	if q.MaxInUSD != nil {
		if lo, ok := cheapestPaid(all, func(m ModelInfo) (float64, bool) { return m.PromptUSDPerMillion() }); ok && lo > *q.MaxInUSD {
			fmt.Fprintf(&sb, "  max_in_usd %.4f excludes everything; the cheapest paid model is %.2f/M.\n", *q.MaxInUSD, lo)
		}
	}
	if q.MaxOutUSD != nil {
		if lo, ok := cheapestPaid(all, func(m ModelInfo) (float64, bool) { return m.CompletionUSDPerMillion() }); ok && lo > *q.MaxOutUSD {
			fmt.Fprintf(&sb, "  max_out_usd %.4f excludes everything; the cheapest paid model is %.2f/M.\n", *q.MaxOutUSD, lo)
		}
	}
	if q.MinContext != nil {
		best := 0
		for _, m := range all {
			if m.ContextWindow != nil && *m.ContextWindow > best {
				best = *m.ContextWindow
			}
		}
		if best > 0 && best < *q.MinContext {
			fmt.Fprintf(&sb, "  min_context %d excludes everything; the largest published window is %d.\n", *q.MinContext, best)
		}
	}
	return sb.String()
}

func cheapestPaid(all []ModelInfo, get func(ModelInfo) (float64, bool)) (float64, bool) {
	lo, found := 0.0, false
	for _, m := range all {
		v, ok := get(m)
		if !ok || v <= 0 {
			continue
		}
		if !found || v < lo {
			lo, found = v, true
		}
	}
	return lo, found
}

func writeNumericLine(sb *strings.Builder, label string, all []ModelInfo, get func(ModelInfo) (float64, bool)) {
	vals := make([]float64, 0, len(all))
	for _, m := range all {
		if v, ok := get(m); ok {
			vals = append(vals, v)
		}
	}
	if len(vals) == 0 {
		fmt.Fprintf(sb, "%s  no published values\n", label)
		return
	}
	sort.Float64s(vals)
	med := vals[len(vals)/2]
	isTokens := strings.Contains(label, "ctx")
	format := func(v float64) string {
		if isTokens {
			n := int(v)
			return tokenCell(&n)
		}
		return fmt.Sprintf("%.2f", v)
	}
	span := format(vals[0]) + " - " + format(vals[len(vals)-1])
	fmt.Fprintf(sb, "%s  %-16s median %-8s (%d of %d published)\n",
		label, span, format(med), len(vals), len(all))
}

func writeSupportLine(sb *strings.Builder, label string, all []ModelInfo, get func(ModelInfo) string) {
	yes, no, unknown := 0, 0, 0
	for _, m := range all {
		switch strings.ToLower(get(m)) {
		case "yes":
			yes++
		case "no":
			no++
		default:
			unknown++
		}
	}
	fmt.Fprintf(sb, "%s  %d yes, %d no, %d unknown (unknown is kept by a filter, not dropped)\n",
		label, yes, no, unknown)
}

// ModelSortKeys returns the sort keys agent_models accepts. Exported so the
// daemon can assert every one of them resolves on its side: a key accepted
// here and unknown there would order on nothing, silently.
func ModelSortKeys() []string {
	return append([]string(nil), modelSortKeys...)
}

// SortDirectionWord says which end of a sort key comes first, so the header
// line states it rather than leaving the caller to infer it from the rows.
// It mirrors modelquery.BiggerIsBetter; this package deliberately does not
// import that one, which would drag protobuf into every tool binary.
func SortDirectionWord(key string) string {
	switch strings.ToLower(key) {
	case "ctx", "agentic", "intel", "code", "max out", "newest":
		return "highest"
	}
	return "lowest"
}
