// SPDX-License-Identifier: Apache-2.0

package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"go.graveland.dev/rafiki/pkg/presets"
)

const presetGetDescription = "Read one preset: its version stamp and full " +
	"spec (kind, model, tools, prompts, budget) as JSON. Use it to see what " +
	"a preset fixes before spawning with it, and pass history to read every " +
	"past version, deleted ones included. Errors if no preset has that name." +
	" " + presetConvention

func init() { DefaultBlueprint.Register(&PresetGetBlueprint{}) }

type PresetGetBlueprint struct{}

func (PresetGetBlueprint) Name() string        { return "preset_get" }
func (PresetGetBlueprint) Description() string { return presetGetDescription }
func (PresetGetBlueprint) InputSchema() Schema {
	return Schema{
		Type: "object",
		Properties: []SchemaProperty{
			{Name: "name", Type: "string",
				Description: "Name of the preset to read, exactly as saved with preset_put (e.g. \"default:implementer\")."},
			{Name: "history", Type: "boolean",
				Description: "true to return every version of the preset, deleted ones included, newest first. Default false: the latest live version only."},
		},
		Required: []string{"name"},
	}
}
func (PresetGetBlueprint) Execute(context.Context, ToolInput) (ToolResult, error) {
	panic("blueprint: call Materialize first")
}
func (PresetGetBlueprint) Materialize(opts ToolOpts) (Tool, error) {
	if opts.Presets == nil {
		return nil, nil
	}
	return &presetGetTool{PresetGetBlueprint: PresetGetBlueprint{}, store: opts.Presets}, nil
}

type presetGetTool struct {
	PresetGetBlueprint
	store PresetStore
}

type presetGetInput struct {
	Name    string `json:"name"`
	History bool   `json:"history"`
}

// presetNotFoundError reports a missing preset with the tool-facing sentence
// while wrapping presets.ErrNotFound, so a caller can errors.Is against the
// domain sentinel without the sentinel's text leaking into the message.
type presetNotFoundError struct {
	tool string
	name string
}

func (e *presetNotFoundError) Error() string { return fmt.Sprintf("%s: no preset %q", e.tool, e.name) }
func (e *presetNotFoundError) Unwrap() error { return presets.ErrNotFound }

// presetVersionJSON is one version of a preset as preset_get returns it.
// written_by_child is omitted when empty (written by the operator);
// deleted_at is set only on a history row that has been deleted.
type presetVersionJSON struct {
	Version        int64        `json:"version"`
	CreatedAt      string       `json:"created_at"`
	WrittenByChild string       `json:"written_by_child,omitempty"`
	DeletedAt      string       `json:"deleted_at,omitempty"`
	Spec           presets.Spec `json:"spec"`
}

func presetVersionJSONOf(r presets.Record) presetVersionJSON {
	v := presetVersionJSON{
		Version:        r.ID,
		CreatedAt:      r.CreatedAt.UTC().Format(time.RFC3339),
		WrittenByChild: r.WrittenByChild,
		Spec:           presets.SpecOf(r),
	}
	if r.DeletedAt != nil {
		v.DeletedAt = r.DeletedAt.UTC().Format(time.RFC3339)
	}
	return v
}

func (t *presetGetTool) Execute(ctx context.Context, input ToolInput) (ToolResult, error) {
	var in presetGetInput
	if err := input.Unmarshal(&in); err != nil {
		return ToolResult{}, fmt.Errorf("preset_get: invalid input: %w", err)
	}
	if in.Name == "" {
		return ToolResult{}, errors.New("preset_get: name is required")
	}
	if err := ctx.Err(); err != nil {
		return ToolResult{}, err
	}
	var out any
	if in.History {
		recs, err := t.store.History(ctx, in.Name)
		if err != nil {
			if errors.Is(err, presets.ErrNotFound) {
				return ToolResult{}, &presetNotFoundError{tool: "preset_get", name: in.Name}
			}
			return ToolResult{}, fmt.Errorf("preset_get: %w", err)
		}
		versions := make([]presetVersionJSON, 0, len(recs))
		for _, r := range recs {
			versions = append(versions, presetVersionJSONOf(r))
		}
		out = versions
	} else {
		rec, err := t.store.Get(ctx, in.Name)
		if err != nil {
			if errors.Is(err, presets.ErrNotFound) {
				return ToolResult{}, &presetNotFoundError{tool: "preset_get", name: in.Name}
			}
			return ToolResult{}, fmt.Errorf("preset_get: %w", err)
		}
		out = presetVersionJSONOf(rec)
	}
	b, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return ToolResult{}, fmt.Errorf("preset_get: %w", err)
	}
	return NewTextResult(string(b)), nil
}
