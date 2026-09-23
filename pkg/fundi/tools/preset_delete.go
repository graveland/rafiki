// SPDX-License-Identifier: Apache-2.0

package tools

import (
	"context"
	"fmt"
)

const presetDeleteDescription = "Delete one of the presets by name. The " +
	"preset's history is kept: a later preset_put under the same name starts " +
	"a new version line. Errors if no live preset has that name." +
	" " + presetConvention

func init() { DefaultBlueprint.Register(&PresetDeleteBlueprint{}) }

type PresetDeleteBlueprint struct{}

func (PresetDeleteBlueprint) Name() string        { return "preset_delete" }
func (PresetDeleteBlueprint) Description() string { return presetDeleteDescription }
func (PresetDeleteBlueprint) InputSchema() Schema {
	return Schema{
		Type: "object",
		Properties: []SchemaProperty{
			{Name: "name", Type: "string",
				Description: "Name of the preset to delete, exactly as saved with preset_put."},
		},
		Required: []string{"name"},
	}
}
func (PresetDeleteBlueprint) Execute(context.Context, ToolInput) (ToolResult, error) {
	panic("blueprint: call Materialize first")
}
func (PresetDeleteBlueprint) Materialize(opts ToolOpts) (Tool, error) {
	if opts.Presets == nil {
		return nil, nil
	}
	return &presetDeleteTool{PresetDeleteBlueprint: PresetDeleteBlueprint{}, store: opts.Presets}, nil
}

type presetDeleteTool struct {
	PresetDeleteBlueprint
	store PresetStore
}

type presetDeleteInput struct {
	Name string `json:"name"`
}

func (t *presetDeleteTool) Execute(ctx context.Context, input ToolInput) (ToolResult, error) {
	var in presetDeleteInput
	if err := input.Unmarshal(&in); err != nil {
		return ToolResult{}, fmt.Errorf("preset_delete: invalid input: %w", err)
	}
	if in.Name == "" {
		return ToolResult{}, fmt.Errorf("preset_delete: name is required")
	}
	if err := ctx.Err(); err != nil {
		return ToolResult{}, err
	}
	if err := t.store.Delete(ctx, in.Name); err != nil {
		return ToolResult{}, fmt.Errorf("preset_delete: %w", err)
	}
	return NewTextResult(fmt.Sprintf("deleted %q", in.Name)), nil
}
