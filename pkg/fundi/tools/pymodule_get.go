// SPDX-License-Identifier: Apache-2.0

package tools

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.graveland.dev/rafiki/pkg/pymodules"
)

const pymoduleGetDescription = "Fetch one saved pymodule by name: its full " +
	"source plus description, version and creation time. Use it to review " +
	"what a module does before running it with pymodule_run, and to " +
	"read-modify-write: fetch, edit, then save the result as a new version " +
	"with pymodule_put. Errors if no live module has that name."

func init() { DefaultBlueprint.Register(&PyModuleGetBlueprint{}) }

type PyModuleGetBlueprint struct{}

func (PyModuleGetBlueprint) Name() string        { return "pymodule_get" }
func (PyModuleGetBlueprint) Description() string { return pymoduleGetDescription }
func (PyModuleGetBlueprint) InputSchema() Schema {
	return Schema{
		Type: "object",
		Properties: []SchemaProperty{
			{Name: "repo", Type: "string", Description: "Must be \"local\" — you can only save to and read from your own pymodule store this way. A git-sourced pymodule is read-only through pymodule_run and pymodule_get; this operation cannot target one."},
			{Name: "name", Type: "string", Description: "Module name, exactly as saved with pymodule_put."},
		},
		Required: []string{"repo", "name"},
	}
}
func (PyModuleGetBlueprint) Execute(context.Context, ToolInput) (ToolResult, error) {
	panic("blueprint: call Materialize first")
}
func (PyModuleGetBlueprint) Materialize(opts ToolOpts) (Tool, error) {
	if opts.PyModules == nil {
		return nil, nil
	}
	return &pymoduleGetTool{PyModuleGetBlueprint: PyModuleGetBlueprint{}, store: opts.PyModules}, nil
}

type pymoduleGetTool struct {
	PyModuleGetBlueprint
	store PyModuleStore
}

type pymoduleGetInput struct {
	Repo string `json:"repo"`
	Name string `json:"name"`
}

func (gt *pymoduleGetTool) Execute(ctx context.Context, input ToolInput) (ToolResult, error) {
	var in pymoduleGetInput
	if err := input.Unmarshal(&in); err != nil {
		return ToolResult{}, fmt.Errorf("pymodule_get: invalid input: %w", err)
	}
	// The repo check comes first, before every other validation: a caller
	// aiming at a git source must be told this operation cannot target one
	// (serving a git-sourced get needs an executor round-trip pymodule_run
	// already has and get does not), whatever else is wrong with its input.
	if in.Repo != pymodules.LocalRepo {
		return ToolResult{}, fmt.Errorf("pymodule_get: repo must be %q — a git-sourced pymodule is managed through its own repo's pull requests, not through rafiki", pymodules.LocalRepo)
	}
	if err := pymodules.ValidName(in.Name); err != nil {
		return ToolResult{}, fmt.Errorf("pymodule_get: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return ToolResult{}, err
	}
	rec, err := gt.store.Get(ctx, in.Repo, in.Name)
	if err != nil {
		if errors.Is(err, pymodules.ErrNotFound) {
			return ToolResult{}, fmt.Errorf("pymodule_get: no module named %q in your store", in.Name)
		}
		return ToolResult{}, fmt.Errorf("pymodule_get: %w", err)
	}
	header := fmt.Sprintf("pymodule %q version %d saved %s", rec.Name, rec.ID, rec.CreatedAt.UTC().Format(time.RFC3339))
	if rec.Description != "" {
		header += "\ndescription: " + rec.Description
	}
	return NewTextResult(header + "\n\n" + rec.Code), nil
}
