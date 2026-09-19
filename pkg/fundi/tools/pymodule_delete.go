// SPDX-License-Identifier: Apache-2.0

package tools

import (
	"context"
	"errors"
	"fmt"

	"go.graveland.dev/rafiki/pkg/pymodules"
)

const pymoduleDeleteDescription = "Soft-delete one of your saved pymodules by " +
	"name. Deletion marks every saved version deleted: the module leaves your " +
	"inventory, and history is kept -- saving a new version under the same " +
	"name restores it. Errors if no live module has that name."

func init() { DefaultBlueprint.Register(&PyModuleDeleteBlueprint{}) }

type PyModuleDeleteBlueprint struct{}

func (PyModuleDeleteBlueprint) Name() string        { return "pymodule_delete" }
func (PyModuleDeleteBlueprint) Description() string { return pymoduleDeleteDescription }
func (PyModuleDeleteBlueprint) InputSchema() Schema {
	return Schema{
		Type: "object",
		Properties: []SchemaProperty{
			{Name: "repo", Type: "string", Description: "Must be \"local\" — you can only save to and read from your own pymodule store this way. A git-sourced pymodule is read-only through pymodule_run and pymodule_get; this operation cannot target one."},
			{Name: "name", Type: "string", Description: "Name of the module to delete, exactly as saved with pymodule_put."},
		},
		Required: []string{"repo", "name"},
	}
}
func (PyModuleDeleteBlueprint) Execute(context.Context, ToolInput) (ToolResult, error) {
	panic("blueprint: call Materialize first")
}
func (PyModuleDeleteBlueprint) Materialize(opts ToolOpts) (Tool, error) {
	if opts.PyModules == nil {
		return nil, nil
	}
	return &pymoduleDeleteTool{PyModuleDeleteBlueprint: PyModuleDeleteBlueprint{}, store: opts.PyModules}, nil
}

type pymoduleDeleteTool struct {
	PyModuleDeleteBlueprint
	store PyModuleStore
}

type pymoduleDeleteInput struct {
	Repo string `json:"repo"`
	Name string `json:"name"`
}

func (dt *pymoduleDeleteTool) Execute(ctx context.Context, input ToolInput) (ToolResult, error) {
	var in pymoduleDeleteInput
	if err := input.Unmarshal(&in); err != nil {
		return ToolResult{}, fmt.Errorf("pymodule_delete: invalid input: %w", err)
	}
	// The repo check comes first, before every other validation: a caller
	// aiming at a git source must be told this operation cannot target one,
	// whatever else is wrong with its input.
	if in.Repo != pymodules.LocalRepo {
		return ToolResult{}, fmt.Errorf("pymodule_delete: repo must be %q — a git-sourced pymodule is managed through its own repo's pull requests, not through rafiki", pymodules.LocalRepo)
	}
	if err := pymodules.ValidName(in.Name); err != nil {
		return ToolResult{}, fmt.Errorf("pymodule_delete: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return ToolResult{}, err
	}
	notice, err := dt.store.Delete(ctx, in.Repo, in.Name)
	if err != nil {
		if errors.Is(err, pymodules.ErrNotFound) {
			return ToolResult{}, fmt.Errorf("pymodule_delete: no module named %q in your store", in.Name)
		}
		return ToolResult{}, fmt.Errorf("pymodule_delete: %w", err)
	}
	msg := fmt.Sprintf("deleted %q -- it leaves your inventory and is pruned from your executors on the next sync; save a new version under the same name to restore it", in.Name)
	if notice != "" {
		msg += "\n\n" + notice
	}
	return NewTextResult(msg), nil
}
