package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

func init() {
	DefaultBlueprint.Register(&ConversationSearchBlueprint{})
	DefaultBlueprint.Register(&ConversationExportBlueprint{})
	DefaultBlueprint.Register(&ConversationQueryBlueprint{})
}

// ConversationSummaryRow is one conversation, as the daemon's insights layer
// reports it. Package-local mirror of insights.ConversationSummary -- see
// ModelRow/RateLimitStatus in pkg/connectapi for the established pattern.
type ConversationSummaryRow struct {
	ID, Name, Owner, Persona, Source, Model, Status, DrivenBy string
	CreatedAtUnix                                             int64
	Turns                                                     int
	InputTokens, OutputTokens, CacheReadTokens                int64
	CacheHitRatio, TotalCostUSD                               float64
	FirstMessage                                              string
}

// ConversationQuery mirrors insights.SearchFilter.
type ConversationQuery struct {
	SinceUnix, UntilUnix                        int64
	Owner, Persona, Source, Model, Status, Path string
	MinTokens                                   int64
	Text                                        string
	Limit                                       int
}

// ConversationTranscriptTurn mirrors insights.TranscriptTurn.
type ConversationTranscriptTurn struct {
	Ordinal                                    int
	Role                                       string
	Content                                    json.RawMessage
	Skills                                     []string
	InputTokens, OutputTokens, CacheReadTokens int64
	LatencyMS                                  int
	Model, PrefixHash                          string
}

// ConversationTranscript mirrors insights.Transcript.
type ConversationTranscript struct {
	ConversationID, Owner, Persona, Source, DrivenBy string
	Turns                                            []ConversationTranscriptTurn
	AvailableSkills                                  []string
}

// ConversationReader answers scoped conversation reads, bound to one caller
// at construction -- the same reasoning as QuotaReader and AgentSpawner: no
// method takes a caller-supplied id, so the constructor is the only binding.
type ConversationReader interface {
	ConversationSearch(ctx context.Context, q ConversationQuery) ([]ConversationSummaryRow, error)
	ConversationExport(ctx context.Context, conversationID string) (*ConversationTranscript, error)
	RunQuery(ctx context.Context, name string, f CatalogueFilter) (CatalogueResult, error)
}

// CatalogueFilter mirrors insights.StatsFilter, for the conversation_query
// tool. Distinct from ConversationQuery above (that type mirrors
// insights.SearchFilter, for conversation_search) -- same package, two
// different filter shapes for two different tools.
type CatalogueFilter struct {
	SinceUnix, UntilUnix                int64
	Owner, Persona, Source, Model, Path string
}

// CatalogueColumn mirrors insights.Column.
type CatalogueColumn struct {
	Name, Kind, Format string
}

// CatalogueEntry mirrors insights.Entry as a plain union rather than an
// interface -- this type crosses exactly one boundary (ConversationReader to
// the tool's text rendering) and is never read by a query implementation
// the way insights.Entry is.
type CatalogueEntry struct {
	Str     string
	Int     int64
	Float   float64
	IsInt   bool
	IsFloat bool
}

// CatalogueResult mirrors insights.QueryResult.
type CatalogueResult struct {
	Columns []CatalogueColumn
	Rows    [][]CatalogueEntry
}

const conversationSearchDescription = "Search your own past conversations by time, model, " +
	"source, status, or a substring of the first message. Returns summaries (turn counts, " +
	"tokens, cost) -- use conversation_export to read a specific conversation's full transcript. " +
	"Results are scoped to conversations you own; there is no way to search another user's " +
	"conversations with this tool."

type ConversationSearchBlueprint struct{}

func (ConversationSearchBlueprint) Name() string        { return "conversation_search" }
func (ConversationSearchBlueprint) Description() string { return conversationSearchDescription }
func (ConversationSearchBlueprint) InputSchema() Schema {
	return Schema{
		Type: "object",
		Properties: []SchemaProperty{
			{Name: "since_unix", Type: "integer", Description: "Unix seconds; only conversations with turn activity at or after this time."},
			{Name: "until_unix", Type: "integer", Description: "Unix seconds; only conversations with turn activity before this time."},
			{Name: "model", Type: "string", Description: "Filter by served model id."},
			{Name: "source", Type: "string", Description: "Filter by capture source (e.g. \"agent\", \"claude\", \"rafiki-claude\")."},
			{Name: "status", Type: "string", Description: "Filter by conversation status."},
			{Name: "path", Type: "string", Description: "\"proxy\" or \"direct\"; empty means either."},
			{Name: "min_tokens", Type: "integer", Description: "Minimum total (input+output) tokens across the conversation."},
			{Name: "text", Type: "string", Description: "Substring match against the first user message."},
			{Name: "limit", Type: "integer", Description: "Max results (default 50)."},
		},
	}
}
func (ConversationSearchBlueprint) Execute(context.Context, ToolInput) (ToolResult, error) {
	panic("blueprint: call Materialize first")
}
func (ConversationSearchBlueprint) Materialize(opts ToolOpts) (Tool, error) {
	if opts.Conversations == nil {
		return nil, nil
	}
	return &conversationSearchTool{reader: opts.Conversations}, nil
}

type conversationSearchTool struct {
	ConversationSearchBlueprint
	reader ConversationReader
}

func (t *conversationSearchTool) Execute(ctx context.Context, in ToolInput) (ToolResult, error) {
	if err := ctx.Err(); err != nil {
		return ToolResult{}, err
	}
	var q ConversationQuery
	if err := in.Unmarshal(&q); err != nil {
		return ToolResult{}, fmt.Errorf("conversation_search: %w", err)
	}
	rows, err := t.reader.ConversationSearch(ctx, q)
	if err != nil {
		return ToolResult{}, fmt.Errorf("conversation_search: %w", err)
	}
	if len(rows) == 0 {
		return NewTextResult("no conversations matched"), nil
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "%d matched\n\n", len(rows))
	for _, r := range rows {
		fmt.Fprintf(&sb, "%s  %-10s  %4d turns  $%.4f  %s\n", r.ID, r.Model, r.Turns, r.TotalCostUSD, r.FirstMessage)
	}
	return NewTextResult(sb.String()), nil
}

const conversationExportDescription = "Read one of your own conversations' full transcript " +
	"(every message, with per-turn token/cost metrics and skills invoked). Use conversation_search " +
	"first to find the id. Refuses -- as not-found, not a permission error -- a conversation you " +
	"do not own."

type ConversationExportBlueprint struct{}

func (ConversationExportBlueprint) Name() string        { return "conversation_export" }
func (ConversationExportBlueprint) Description() string { return conversationExportDescription }
func (ConversationExportBlueprint) InputSchema() Schema {
	return Schema{
		Type: "object",
		Properties: []SchemaProperty{
			{Name: "conversation_id", Type: "string", Description: "The conversation id, from conversation_search."},
		},
		Required: []string{"conversation_id"},
	}
}
func (ConversationExportBlueprint) Execute(context.Context, ToolInput) (ToolResult, error) {
	panic("blueprint: call Materialize first")
}
func (ConversationExportBlueprint) Materialize(opts ToolOpts) (Tool, error) {
	if opts.Conversations == nil {
		return nil, nil
	}
	return &conversationExportTool{reader: opts.Conversations}, nil
}

type conversationExportTool struct {
	ConversationExportBlueprint
	reader ConversationReader
}

func (t *conversationExportTool) Execute(ctx context.Context, in ToolInput) (ToolResult, error) {
	if err := ctx.Err(); err != nil {
		return ToolResult{}, err
	}
	var req struct {
		ConversationID string `json:"conversation_id"`
	}
	if err := in.Unmarshal(&req); err != nil {
		return ToolResult{}, fmt.Errorf("conversation_export: %w", err)
	}
	if req.ConversationID == "" {
		return ToolResult{}, fmt.Errorf("conversation_export: conversation_id is required")
	}
	tr, err := t.reader.ConversationExport(ctx, req.ConversationID)
	if err != nil {
		return ToolResult{}, fmt.Errorf("conversation_export: %w", err)
	}
	b, err := json.MarshalIndent(tr, "", "  ")
	if err != nil {
		return ToolResult{}, fmt.Errorf("conversation_export: marshal: %w", err)
	}
	return NewTextResult(string(b)), nil
}

const conversationQueryDescription = "Run a named catalogue query over your own conversation history: " +
	"\"tools\" (tool_use counts by tool, case-insensitively merged under the " +
	"most common spelling, sorted by calls descending), \"skills\" (skill invocations, namespace-normalized), " +
	"\"classes\" (behavioral breakdown: coordinator/brainstorming/planning/worker), " +
	"\"models\" (served-model distribution), \"sizes\" (turn-count histogram per class), " +
	"\"coverage\" (child-row instrumentation coverage by week, agent-kind conversations only). " +
	"Time filters are per query: tools and skills filter message time, models filters turn " +
	"time, classes and sizes filter turn activity, coverage filters conversation creation week. " +
	"Results are scoped to conversations you own, same as conversation_search."

type ConversationQueryBlueprint struct{}

func (ConversationQueryBlueprint) Name() string        { return "conversation_query" }
func (ConversationQueryBlueprint) Description() string { return conversationQueryDescription }
func (ConversationQueryBlueprint) InputSchema() Schema {
	return Schema{
		Type: "object",
		Properties: []SchemaProperty{
			{Name: "name", Type: "string", Description: "One of: tools, skills, classes, models, sizes, coverage."},
			{Name: "since_unix", Type: "integer", Description: "Unix seconds lower bound (meaning varies by query -- see its description)."},
			{Name: "until_unix", Type: "integer", Description: "Unix seconds upper bound."},
			{Name: "model", Type: "string", Description: "Filter by served model id (ignored by queries with no per-turn model)."},
			{Name: "source", Type: "string", Description: "Filter by capture source."},
			{Name: "path", Type: "string", Description: "\"proxy\" or \"direct\"; empty means either."},
		},
		Required: []string{"name"},
	}
}
func (ConversationQueryBlueprint) Execute(context.Context, ToolInput) (ToolResult, error) {
	panic("blueprint: call Materialize first")
}
func (ConversationQueryBlueprint) Materialize(opts ToolOpts) (Tool, error) {
	if opts.Conversations == nil {
		return nil, nil
	}
	return &conversationQueryTool{reader: opts.Conversations}, nil
}

type conversationQueryTool struct {
	ConversationQueryBlueprint
	reader ConversationReader
}

func (t *conversationQueryTool) Execute(ctx context.Context, in ToolInput) (ToolResult, error) {
	if err := ctx.Err(); err != nil {
		return ToolResult{}, err
	}
	var req struct {
		Name                string `json:"name"`
		SinceUnix           int64  `json:"since_unix"`
		UntilUnix           int64  `json:"until_unix"`
		Model, Source, Path string
	}
	if err := in.Unmarshal(&req); err != nil {
		return ToolResult{}, fmt.Errorf("conversation_query: %w", err)
	}
	if req.Name == "" {
		return ToolResult{}, fmt.Errorf("conversation_query: name is required")
	}
	res, err := t.reader.RunQuery(ctx, req.Name, CatalogueFilter{
		SinceUnix: req.SinceUnix, UntilUnix: req.UntilUnix,
		Model: req.Model, Source: req.Source, Path: req.Path,
	})
	if err != nil {
		return ToolResult{}, fmt.Errorf("conversation_query: %w", err)
	}
	if len(res.Rows) == 0 {
		return NewTextResult("no rows"), nil
	}
	var sb strings.Builder
	for i, c := range res.Columns {
		if i > 0 {
			sb.WriteString("\t")
		}
		sb.WriteString(c.Name)
	}
	sb.WriteString("\n")
	for _, row := range res.Rows {
		for i, cell := range row {
			if i > 0 {
				sb.WriteString("\t")
			}
			switch {
			case cell.IsInt:
				fmt.Fprintf(&sb, "%d", cell.Int)
			case cell.IsFloat:
				fmt.Fprintf(&sb, "%g", cell.Float)
			default:
				sb.WriteString(cell.Str)
			}
		}
		sb.WriteString("\n")
	}
	return NewTextResult(sb.String()), nil
}
