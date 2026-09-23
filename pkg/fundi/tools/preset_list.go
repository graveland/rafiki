// SPDX-License-Identifier: Apache-2.0

package tools

import (
	"context"
	"fmt"
	"strings"
)

const presetListDescription = "List the presets you can spawn agents with: " +
	"one row per preset with its name, kind, model and description. Pass " +
	"prefix to list a single group (e.g. \"default:\"); omit it for all of " +
	"them. Use preset_get to read one preset's full spec." +
	" " + presetConvention

func init() { DefaultBlueprint.Register(&PresetListBlueprint{}) }

type PresetListBlueprint struct{}

func (PresetListBlueprint) Name() string        { return "preset_list" }
func (PresetListBlueprint) Description() string { return presetListDescription }
func (PresetListBlueprint) InputSchema() Schema {
	return Schema{
		Type: "object",
		Properties: []SchemaProperty{
			{Name: "prefix", Type: "string",
				Description: "Only presets whose name starts with this prefix, e.g. \"default:\" for one group. Omit to list every preset."},
		},
	}
}
func (PresetListBlueprint) Execute(context.Context, ToolInput) (ToolResult, error) {
	panic("blueprint: call Materialize first")
}
func (PresetListBlueprint) Materialize(opts ToolOpts) (Tool, error) {
	if opts.Presets == nil {
		return nil, nil
	}
	return &presetListTool{PresetListBlueprint: PresetListBlueprint{}, store: opts.Presets}, nil
}

type presetListTool struct {
	PresetListBlueprint
	store PresetStore
}

type presetListInput struct {
	Prefix string `json:"prefix"`
}

func (t *presetListTool) Execute(ctx context.Context, input ToolInput) (ToolResult, error) {
	var in presetListInput
	if err := input.Unmarshal(&in); err != nil {
		return ToolResult{}, fmt.Errorf("preset_list: invalid input: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return ToolResult{}, err
	}
	recs, err := t.store.List(ctx, in.Prefix)
	if err != nil {
		return ToolResult{}, fmt.Errorf("preset_list: %w", err)
	}
	if len(recs) == 0 {
		return NewTextResult("No presets."), nil
	}
	lines := make([]string, 0, len(recs))
	for _, r := range recs {
		model := r.Model
		if model == "" {
			model = "(default model)"
		}
		lines = append(lines, fmt.Sprintf("%s\t%s\t%s\t%s", r.Name, r.Kind, model, r.Description))
	}
	return NewTextResult(strings.Join(lines, "\n")), nil
}
