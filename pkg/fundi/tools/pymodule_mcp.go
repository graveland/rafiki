// SPDX-License-Identifier: Apache-2.0

package tools

import (
	"context"
	"encoding/json"
)

// PyModuleInfo is one row of an owner's pymodule inventory, rendered by the
// pymodule_list MCP tool. Mirrors the fields pymoduleInventory's dynamic
// skill body already renders for fundi.
type PyModuleInfo struct {
	Name        string
	Description string
}

// PyModuleLister lets an MCP caller list its own saved pymodules. Bound to
// one owner at construction, same rule as PyModuleStore -- no method takes a
// caller-supplied identity.
//
// Fundi never sets ToolOpts.PyModuleList: it renders the same inventory as
// the dynamic "rafiki:python-modules" skill instead (see
// ToolOpts.PyModulesInventory). This interface exists only for the MCP face,
// which has no skill mechanism.
type PyModuleLister interface {
	List(ctx context.Context) ([]PyModuleInfo, error)
}

// PyModuleExecutor runs a pymodule_run call on the calling child's own bound
// executor. input is the same {script, modules, cwd, args} JSON
// PyModuleRunBlueprint's InputSchema describes. Bound to one child's
// resolved executor binding at construction.
//
// The daemon-side implementation owns resolving the input's `cwd` against the
// CALLING CHILD's own working directory before the bytes leave it — absent
// and relative both resolve there, absolute and ~-prefixed pass through —
// because the executor it lands on serves one root registry to every caller
// without a workspace, and a relative path left to the executor's own
// resolveToolPath would resolve against that root, not the caller's tree.
//
// Fundi never sets ToolOpts.PyModuleExecutor: its own pymodule_run
// (pymodule_run.go) executes via fundi's tiered tool-routing, not through
// this interface. This exists only for the MCP face's own pymodule_run tool.
type PyModuleExecutor interface {
	Run(ctx context.Context, input json.RawMessage) (string, error)
}
