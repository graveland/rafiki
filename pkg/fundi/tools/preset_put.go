// SPDX-License-Identifier: Apache-2.0

package tools

import (
	"context"
	"errors"
	"fmt"

	"go.graveland.dev/rafiki/pkg/presets"
)

const presetPutDescription = "Create a preset, or save a new version of an " +
	"existing one: each save is a new version and nothing already saved is " +
	"ever overwritten. The spec's fields fix what agent_spawn's `preset` " +
	"gives a spawned worker." +
	" " + presetConvention

func init() { DefaultBlueprint.Register(&PresetPutBlueprint{}) }

type PresetPutBlueprint struct{}

func (PresetPutBlueprint) Name() string        { return "preset_put" }
func (PresetPutBlueprint) Description() string { return presetPutDescription }
func (PresetPutBlueprint) InputSchema() Schema {
	return Schema{
		Type: "object",
		Properties: []SchemaProperty{
			{Name: "name", Type: "string",
				Description: "Name of the preset: `<group>:<role>` (e.g. default:implementer) or a bare name. Required."},
			{Name: "description", Type: "string",
				Description: "One-line description shown by preset_list."},
			{Name: "kind", Type: "string",
				Description: "Agent runtime this preset applies to: \"fundi\" (default) or \"claude\". Most fields only apply to fundi children."},
			{Name: "provider", Type: "string",
				Description: "Optional provider hint; usually leave unset and fix model only."},
			{Name: "model", Type: "string",
				Description: "Model id children spawned with this preset run on."},
			{Name: "thinking", Type: "string",
				Description: "off|low|medium|high|xhigh."},
			{Name: "executor", Type: "string",
				Description: "Executor label selector children spawned with this preset are confined to."},
			{Name: "labels", Type: "object",
				Description: "Arbitrary key/value labels attached to the preset for selection."},
			{Name: "tools", Type: "array", Items: &Schema{Type: "string"},
				Description: "Tool names the preset's children may use. Omit for the kind's default (everything); [] for none."},
			{Name: "skills", Type: "array", Items: &Schema{Type: "string"},
				Description: "Skill names the preset's children may load. Omit for the kind's default (everything); [] for none."},
			{Name: "mcp_servers", Type: "array", Items: &Schema{Type: "string"},
				Description: "MCP server names the preset's children may call. Omit for the kind's default (everything); [] for none."},
			{Name: "context_files", Type: "boolean",
				Description: "false to have children skip CLAUDE.md/AGENTS.md context files. Omit for the default (on)."},
			{Name: "system_prompt", Type: "string",
				Description: "Full system prompt replacing the runtime default."},
			{Name: "append_system_prompt", Type: "string",
				Description: "Text appended after the child's system prompt."},
			{Name: "max_cost", Type: "number",
				Description: "USD budget for children spawned with this preset. 0 means unlimited."},
			{Name: "max_depth", Type: "integer",
				Description: "How many further levels children spawned with this preset may spawn."},
			{Name: "max_children", Type: "integer",
				Description: "How many agents may be alive beneath children spawned with this preset."},
		},
		Required: []string{"name"},
	}
}
func (PresetPutBlueprint) Execute(context.Context, ToolInput) (ToolResult, error) {
	panic("blueprint: call Materialize first")
}
func (PresetPutBlueprint) Materialize(opts ToolOpts) (Tool, error) {
	if opts.Presets == nil {
		return nil, nil
	}
	return &presetPutTool{PresetPutBlueprint: PresetPutBlueprint{}, store: opts.Presets}, nil
}

type presetPutTool struct {
	PresetPutBlueprint
	store PresetStore
}

func (t *presetPutTool) Execute(ctx context.Context, input ToolInput) (ToolResult, error) {
	// Decode straight into the domain Spec: it is the wire shape by
	// construction, and encoding/json turns a JSON [] into a non-nil pointer
	// to an empty slice, keeping the tri-state (absent = unset, [] = none)
	// intact through to the store.
	var spec presets.Spec
	if err := input.Unmarshal(&spec); err != nil {
		return ToolResult{}, fmt.Errorf("preset_put: invalid input: %w", err)
	}
	if spec.Name == "" {
		return ToolResult{}, errors.New("preset_put: name is required")
	}
	if err := ctx.Err(); err != nil {
		return ToolResult{}, err
	}
	rec, err := t.store.Put(ctx, spec)
	if err != nil {
		return ToolResult{}, fmt.Errorf("preset_put: %w", err)
	}
	return NewTextResult(fmt.Sprintf("saved %q as version %d", spec.Name, rec.ID)), nil
}
