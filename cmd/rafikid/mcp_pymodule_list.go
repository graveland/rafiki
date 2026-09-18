// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"strings"

	"go.graveland.dev/rafiki/pkg/fundi/tools"
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

func (l *mcpPyModuleLister) List(ctx context.Context) ([]tools.PyModuleInfo, error) {
	recs, err := l.ctrl.pymoduleStore.List(ctx, l.ownerUserID)
	if err != nil {
		return nil, err
	}
	out := make([]tools.PyModuleInfo, len(recs))
	for i, r := range recs {
		out[i] = tools.PyModuleInfo{Name: r.Name, Description: r.Description}
	}
	return out, nil
}

const mcpPymoduleListDescription = "List the pymodules you've saved with pymodule_put. " +
	"Each entry is a name and its one-line description. Use pymodule_put to save a new " +
	"one or a new version, pymodule_delete to remove one. Takes no arguments."

// mcpPyModuleListBlueprint implements tools.Tool + tools.Materializer
// directly. It is MCP-face-only -- fundi renders this same inventory as the
// dynamic "rafiki:python-modules" skill instead -- and is never registered
// on tools.DefaultBlueprint, so it never reaches a fundi child's tool set;
// it is added by hand to cmd/rafikid/mcp_face.go's mcpBlueprints instead.
type mcpPyModuleListBlueprint struct{}

func (mcpPyModuleListBlueprint) Name() string        { return "pymodule_list" }
func (mcpPyModuleListBlueprint) Description() string { return mcpPymoduleListDescription }
func (mcpPyModuleListBlueprint) InputSchema() tools.Schema {
	return tools.Schema{Type: "object"}
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

func (t *mcpPyModuleListTool) Execute(ctx context.Context, _ tools.ToolInput) (tools.ToolResult, error) {
	infos, err := t.lister.List(ctx)
	if err != nil {
		return tools.ToolResult{}, fmt.Errorf("pymodule_list: %w", err)
	}
	if len(infos) == 0 {
		return tools.NewTextResult("No pymodules saved yet. Use pymodule_put to save one."), nil
	}
	var b strings.Builder
	for _, i := range infos {
		fmt.Fprintf(&b, "%s — %s\n", i.Name, i.Description)
	}
	return tools.NewTextResult(b.String()), nil
}
