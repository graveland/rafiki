// SPDX-License-Identifier: Apache-2.0

package tools

import (
	"context"
	"fmt"

	"go.graveland.dev/rafiki/pkg/pymodules"
)

// PyModuleStore lets an agent save a reusable Python snippet. Bound to one
// owner at construction; no method takes a caller-supplied identity.
type PyModuleStore interface {
	Put(ctx context.Context, name, code, description string) (id int64, err error)

	// Delete soft-deletes the named module: every version of it leaves the
	// inventory and is pruned from executors on the next sync; a later Put
	// under the same name restores it. Returns an error wrapping pymodules.ErrNotFound
	// when no live module has that name.
	Delete(ctx context.Context, name string) error
}

const pymodulePutDescription = "Save a reusable Python snippet (a class, a " +
	"helper function) to your own pymodule store. Each save is a new " +
	"version under `name`; nothing already saved is ever overwritten. " +
	"Anything you save becomes importable by name with pymodule_run. Only " +
	"you can see or run what you save here."

func init() { DefaultBlueprint.Register(&PyModulePutBlueprint{}) }

type PyModulePutBlueprint struct{}

func (PyModulePutBlueprint) Name() string        { return "pymodule_put" }
func (PyModulePutBlueprint) Description() string { return pymodulePutDescription }
func (PyModulePutBlueprint) InputSchema() Schema {
	return Schema{
		Type: "object",
		Properties: []SchemaProperty{
			{Name: "name", Type: "string", Description: "Module name, later importable as `import <name>`. Must be a bare Python identifier: letters, digits, underscore, not starting with a digit."},
			{Name: "code", Type: "string", Description: "Full Python source of the module."},
			{Name: "description", Type: "string", Description: "One-line description shown in your pymodule inventory."},
		},
		Required: []string{"name", "code"},
	}
}
func (PyModulePutBlueprint) Execute(context.Context, ToolInput) (ToolResult, error) {
	panic("blueprint: call Materialize first")
}
func (PyModulePutBlueprint) Materialize(opts ToolOpts) (Tool, error) {
	if opts.PyModules == nil {
		return nil, nil
	}
	return &pymodulePutTool{PyModulePutBlueprint: PyModulePutBlueprint{}, store: opts.PyModules}, nil
}

type pymodulePutTool struct {
	PyModulePutBlueprint
	store PyModuleStore
}

type pymodulePutInput struct {
	Name        string `json:"name"`
	Code        string `json:"code"`
	Description string `json:"description"`
}

func (pt *pymodulePutTool) Execute(ctx context.Context, input ToolInput) (ToolResult, error) {
	var in pymodulePutInput
	if err := input.Unmarshal(&in); err != nil {
		return ToolResult{}, fmt.Errorf("pymodule_put: invalid input: %w", err)
	}
	if err := pymodules.ValidName(in.Name); err != nil {
		return ToolResult{}, fmt.Errorf("pymodule_put: %w", err)
	}
	if in.Code == "" {
		return ToolResult{}, fmt.Errorf("pymodule_put: code is required")
	}
	if err := ctx.Err(); err != nil {
		return ToolResult{}, err
	}
	id, err := pt.store.Put(ctx, in.Name, in.Code, in.Description)
	if err != nil {
		return ToolResult{}, fmt.Errorf("pymodule_put: %w", err)
	}
	return NewTextResult(fmt.Sprintf("saved %q as version %d", in.Name, id)), nil
}
