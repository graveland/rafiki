// SPDX-License-Identifier: Apache-2.0

package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"go.graveland.dev/rafiki/pkg/recall"
)

// RecallBinding is recall + memories for one caller. Conversation-derived
// results use the caller's conversation scope; memories are always the
// caller's own.
type RecallBinding interface {
	Recall(ctx context.Context, q RecallQuery) (string, error) // formatted one-line hits
	Context(ctx context.Context, hitID string, before, after, maxChars int) (string, error)
	MemoryPut(ctx context.Context, path, name, body string, meta json.RawMessage) (recall.Memory, error)
	MemoryGet(ctx context.Context, path, name string) (recall.Memory, error)
	MemoryTree(ctx context.Context, path string, depth int) (string, error) // formatted, budgeted
	MemoryDelete(ctx context.Context, path, name string) error
}

// RecallQuery is one recall search. The daemon maps it onto recall.SearchQuery:
// the conversation sources read under the caller's scope, memories under the
// caller's own owner id. Sources are source names, not recall.Source values.
type RecallQuery struct {
	Query                string
	Sources              []string
	Under                string
	Repo                 string
	SinceUnix, UntilUnix int64
	Limit                int
}

const recallDescription = "Search your past conversations and saved memories by keyword " +
	"and meaning. Returns one line per hit — never full text — each with an id (m:/s:/w:). " +
	"Summaries (s:) describe a whole past conversation; windows (w:) are an exact passage; " +
	"memories (m:) are facts saved with memory_put. Expand a hit with recall_context. " +
	"Conversation results cover what your credential can see; memories are always your own."

func init() { DefaultBlueprint.Register(&RecallBlueprint{}) }

type RecallBlueprint struct{}

func (RecallBlueprint) Name() string        { return "recall" }
func (RecallBlueprint) Description() string { return recallDescription }
func (RecallBlueprint) InputSchema() Schema {
	return Schema{
		Type: "object",
		Properties: []SchemaProperty{
			{Name: "query", Type: "string",
				Description: "What to look for: keywords or a phrase; the search matches both exactly and by meaning."},
			{Name: "sources", Type: "array", Items: &Schema{Type: "string"},
				Description: "Restrict the search to some of: memory, summary, window. Omit for all three."},
			{Name: "under", Type: "string",
				Description: "Memory path prefix (e.g. projects.rafiki); only memory hits under it."},
			{Name: "repo", Type: "string",
				Description: "Repo directory basename; only conversation hits from conversations in that repo."},
			{Name: "since_unix", Type: "integer",
				Description: "Unix seconds; only hits with activity at or after this time."},
			{Name: "until_unix", Type: "integer",
				Description: "Unix seconds; only hits with activity before this time."},
			{Name: "limit", Type: "integer",
				Description: "Max hits per source (default 10, max 50)."},
		},
		Required: []string{"query"},
	}
}
func (RecallBlueprint) Execute(context.Context, ToolInput) (ToolResult, error) {
	panic("blueprint: call Materialize first")
}
func (RecallBlueprint) Materialize(opts ToolOpts) (Tool, error) {
	if opts.Recall == nil {
		return nil, nil
	}
	return &recallTool{RecallBlueprint: RecallBlueprint{}, recall: opts.Recall}, nil
}

type recallTool struct {
	RecallBlueprint
	recall RecallBinding
}

type recallInput struct {
	Query     string   `json:"query"`
	Sources   []string `json:"sources"`
	Under     string   `json:"under"`
	Repo      string   `json:"repo"`
	SinceUnix int64    `json:"since_unix"`
	UntilUnix int64    `json:"until_unix"`
	Limit     int      `json:"limit"`
}

func (t *recallTool) Execute(ctx context.Context, input ToolInput) (ToolResult, error) {
	var in recallInput
	if err := input.Unmarshal(&in); err != nil {
		return ToolResult{}, fmt.Errorf("recall: invalid input: %w", err)
	}
	if in.Query == "" {
		return ToolResult{}, errors.New("recall: query is required")
	}
	if err := ctx.Err(); err != nil {
		return ToolResult{}, err
	}
	limit := in.Limit
	switch {
	case limit <= 0:
		limit = recall.RecallDefaultLimit
	case limit > recall.RecallMaxLimit:
		limit = recall.RecallMaxLimit
	}
	text, err := t.recall.Recall(ctx, RecallQuery{
		Query:     in.Query,
		Sources:   in.Sources,
		Under:     in.Under,
		Repo:      in.Repo,
		SinceUnix: in.SinceUnix,
		UntilUnix: in.UntilUnix,
		Limit:     limit,
	})
	if err != nil {
		return ToolResult{}, fmt.Errorf("recall: %w", err)
	}
	return NewTextResult(text), nil
}
