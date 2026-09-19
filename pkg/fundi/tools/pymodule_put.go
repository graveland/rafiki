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
	// Put returns the new row's id and an advisory notice string --
	// possibly multi-line, possibly empty -- describing anything worth the
	// calling agent's attention that did NOT block the save: a lint
	// finding, or a dependency-install failure on one or more of the
	// owner's executors. An empty notice means nothing to report.
	Put(ctx context.Context, name, code, description string) (id int64, notice string, err error)

	// Get returns one saved module's full record: code, description, version
	// (the row id) and creation time. Returns an error wrapping
	// pymodules.ErrNotFound when no live module has that name. Bound to the
	// same single owner as Put/Delete -- no method takes an identity.
	Get(ctx context.Context, name string) (pymodules.Record, error)

	// Delete soft-deletes the named module: every version of it leaves the
	// inventory and is pruned from executors on the next sync; a later Put
	// under the same name restores it. Returns an error wrapping
	// pymodules.ErrNotFound when no live module has that name. The returned
	// notice is advisory, the same rule as Put's: anything the post-delete
	// sync found worth reporting that did not block the delete.
	Delete(ctx context.Context, name string) (notice string, err error)
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
	id, notice, err := pt.store.Put(ctx, in.Name, in.Code, in.Description)
	if err != nil {
		return ToolResult{}, fmt.Errorf("pymodule_put: %w", err)
	}
	msg := fmt.Sprintf("saved %q as version %d", in.Name, id)
	if notice != "" {
		msg += "\n\n" + notice
	}
	return NewTextResult(msg), nil
}
