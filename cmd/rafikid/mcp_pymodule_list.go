// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"strings"

	"go.graveland.dev/rafiki/pkg/fundi/tools"
	"go.graveland.dev/rafiki/pkg/pymodules"
	"go.graveland.dev/rafiki/pkg/users"
)

// mcpPyModuleLister adapts *Controller to tools.PyModuleLister for the MCP
// face's pymodule_list tool, bound to one caller's owner user id.
type mcpPyModuleLister struct {
	ctrl        *Controller
	ownerUserID string
}

func newMCPPyModuleLister(ctrl *Controller, owner users.Identity) *mcpPyModuleLister {
	return &mcpPyModuleLister{ctrl: ctrl, ownerUserID: owner.UserID}
}

// List spans the same scopes the fundi skill body renders: the caller's own
// store ("local") plus every git source's cached inventory. repo scopes it —
// empty means everything, "local" narrows to the blob store, any other value
// names one git source (unknown name: empty result, not an error). A nil
// gitpymodulePusher (no exec pool) contributes no git section, never an
// error.
func (l *mcpPyModuleLister) List(ctx context.Context, repo string) ([]tools.PyModuleInfo, error) {
	out := make([]tools.PyModuleInfo, 0)
	// "local" is in every span except one narrowed to another git source.
	if repo == "" || repo == pymodules.LocalRepo {
		recs, err := l.ctrl.pymoduleStore.List(ctx, l.ownerUserID)
		if err != nil {
			return nil, err
		}
		for _, r := range recs {
			out = append(out, tools.PyModuleInfo{Repo: pymodules.LocalRepo, Name: r.Name, Description: r.Description})
		}
	}
	if l.ctrl.gitpymodulePusher == nil {
		return out, nil
	}
	if repo == "" {
		out = append(out, gitPymoduleInfos(l.ctrl.gitpymodulePusher, l.ownerUserID)...)
		return out, nil
	}
	if repo != pymodules.LocalRepo {
		// One named git source; an unknown name yields an empty result, not
		// an error.
		if inv, ok := l.ctrl.gitpymodulePusher.inventoryFor(l.ownerUserID, repo); ok {
			out = append(out, gitSourcePymoduleInfos(repo, inv)...)
			sortPyModuleInfos(out)
		}
	}
	return out, nil
}

const mcpPymoduleListDescription = "List the pymodules available to you: what you've " +
	"saved with pymodule_put (repo \"local\") plus what your git sources provide, " +
	"each entry labeled with the repo it came from. Optional `repo` narrows to one " +
	"source (\"local\" or a git source name); omit it to see everything. Use " +
	"pymodule_put to save a new one or a new version, pymodule_delete to remove one."

// mcpPyModuleListBlueprint implements tools.Tool + tools.Materializer
// directly. It is MCP-face-only -- fundi renders this same inventory as the
// dynamic "rafiki:python-modules" skill instead -- and is never registered
// on tools.DefaultBlueprint, so it never reaches a fundi child's tool set;
// it is added by hand to cmd/rafikid/mcp_face.go's mcpBlueprints instead.
type mcpPyModuleListBlueprint struct{}

func (mcpPyModuleListBlueprint) Name() string        { return "pymodule_list" }
func (mcpPyModuleListBlueprint) Description() string { return mcpPymoduleListDescription }
func (mcpPyModuleListBlueprint) InputSchema() tools.Schema {
	return tools.Schema{
		Type: "object",
		Properties: []tools.SchemaProperty{
			{Name: "repo", Type: "string", Description: "Optional: narrow to one source (\"local\" or a git source name). Omit to see everything."},
		},
	}
}
func (mcpPyModuleListBlueprint) Execute(context.Context, tools.ToolInput) (tools.ToolResult, error) {
	panic("blueprint: call Materialize first")
}
func (mcpPyModuleListBlueprint) Materialize(opts tools.ToolOpts) (tools.Tool, error) {
	if opts.PyModuleList == nil {
		return nil, nil
	}
	return &mcpPyModuleListTool{lister: opts.PyModuleList}, nil
}

type mcpPyModuleListTool struct {
	mcpPyModuleListBlueprint
	lister tools.PyModuleLister
}

type mcpPyModuleListInput struct {
	Repo string `json:"repo"`
}

func (t *mcpPyModuleListTool) Execute(ctx context.Context, input tools.ToolInput) (tools.ToolResult, error) {
	var in mcpPyModuleListInput
	if len(input) > 0 {
		if err := input.Unmarshal(&in); err != nil {
			return tools.ToolResult{}, fmt.Errorf("pymodule_list: invalid input: %w", err)
		}
	}
	infos, err := t.lister.List(ctx, in.Repo)
	if err != nil {
		return tools.ToolResult{}, fmt.Errorf("pymodule_list: %w", err)
	}
	if len(infos) == 0 {
		return tools.NewTextResult("No pymodules saved yet. Use pymodule_put to save one."), nil
	}
	var b strings.Builder
	for _, i := range infos {
		// Same labeling the fundi skill body uses: a local row is the bare
		// name, a git-sourced row carries its source -- "reponame/name" --
		// so the caller can tell which repo value to pass back.
		label := i.Name
		if i.Repo != "" && i.Repo != pymodules.LocalRepo {
			label = i.Repo + "/" + i.Name
		}
		fmt.Fprintf(&b, "%s — %s\n", label, i.Description)
	}
	return tools.NewTextResult(b.String()), nil
}
