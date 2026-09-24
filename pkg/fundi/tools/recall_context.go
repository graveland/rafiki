// SPDX-License-Identifier: Apache-2.0

package tools

import (
	"context"
	"errors"
	"fmt"

	"go.graveland.dev/rafiki/pkg/recall"
)

const recallContextDescription = "Expand one recall hit: a window (w:) returns the " +
	"surrounding messages with tool results collapsed to their size; a summary (s:) " +
	"returns the full summary and its conversation id; a memory (m:) returns the " +
	"full memory."

// recallContextDefaultSpan is how many messages recall_context includes on
// each side of a window's span when the model does not ask for a width.
const recallContextDefaultSpan = 3

func init() { DefaultBlueprint.Register(&RecallContextBlueprint{}) }

type RecallContextBlueprint struct{}

func (RecallContextBlueprint) Name() string        { return "recall_context" }
func (RecallContextBlueprint) Description() string { return recallContextDescription }
func (RecallContextBlueprint) InputSchema() Schema {
	return Schema{
		Type: "object",
		Properties: []SchemaProperty{
			{Name: "id", Type: "string",
				Description: "The hit id from recall: m:…, s:… or w:…."},
			{Name: "before", Type: "integer",
				Description: "How many messages before the window's span to include (default 3; windows only)."},
			{Name: "after", Type: "integer",
				Description: "How many messages after the window's span to include (default 3; windows only)."},
			{Name: "max_chars", Type: "integer",
				Description: "Maximum output characters (default 8000)."},
		},
		Required: []string{"id"},
	}
}
func (RecallContextBlueprint) Execute(context.Context, ToolInput) (ToolResult, error) {
	panic("blueprint: call Materialize first")
}
func (RecallContextBlueprint) Materialize(opts ToolOpts) (Tool, error) {
	if opts.Recall == nil {
		return nil, nil
	}
	return &recallContextTool{RecallContextBlueprint: RecallContextBlueprint{}, recall: opts.Recall}, nil
}

type recallContextTool struct {
	RecallContextBlueprint
	recall RecallBinding
}

type recallContextInput struct {
	ID       string `json:"id"`
	Before   int    `json:"before"`
	After    int    `json:"after"`
	MaxChars int    `json:"max_chars"`
}

func (t *recallContextTool) Execute(ctx context.Context, input ToolInput) (ToolResult, error) {
	var in recallContextInput
	if err := input.Unmarshal(&in); err != nil {
		return ToolResult{}, fmt.Errorf("recall_context: invalid input: %w", err)
	}
	if in.ID == "" {
		return ToolResult{}, errors.New("recall_context: id is required")
	}
	if err := ctx.Err(); err != nil {
		return ToolResult{}, err
	}
	before, after := in.Before, in.After
	if before <= 0 {
		before = recallContextDefaultSpan
	}
	if after <= 0 {
		after = recallContextDefaultSpan
	}
	maxChars := in.MaxChars
	if maxChars <= 0 {
		maxChars = recall.ContextDefaultMaxChars
	}
	text, err := t.recall.Context(ctx, in.ID, before, after, maxChars)
	if err != nil {
		return ToolResult{}, fmt.Errorf("recall_context: %w", err)
	}
	return NewTextResult(text), nil
}
