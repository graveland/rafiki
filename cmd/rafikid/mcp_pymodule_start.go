// SPDX-License-Identifier: Apache-2.0

package main

import (
	"go.graveland.dev/rafiki/pkg/fundi/tools"
)

// mcp_pymodule_start.go is the MCP face's pymodule_start: the script-child
// sibling of mcp_pymodule_run. Both are face-local blueprints (fundi never
// sees them; the fundi registry materializes pymodule_start through Agents,
// like agent_spawn), and both are gated the same way -- only a
// ProvenanceChildToken caller whose own childstore row carries a live
// executor binding gets them (see mcp_face.go's getServer and
// ToolOpts.PyModuleStarter). The interactive human spawns script children
// with `rafiki create --kind script`; the child callers these tools serve are
// exactly the ones whose daemon can route the spawn onto their own executor
// world.
//
// The description is face-local (mcpPyModuleRunBlueprint's pattern): a face
// caller has no guaranteed notification channel, so the registry text's
// settlement promise is replaced with the deliberate-check wording, and the
// input schema is shared verbatim with the registry blueprint so the two
// surfaces cannot drift apart on what a call carries.

// mcpPymoduleStartDescription rewords pymodule_start for a face caller: no
// settlement promise, and the delegate-run/start pairing the pymodule family
// states on this surface.
const mcpPymoduleStartDescription = "Start a saved Python script as a rafiki SCRIPT child — a " +
	"daemon-managed process that runs to completion on its own, with no " +
	"model, no API key and no turn loop. Returns immediately with the " +
	"child's id — it does NOT wait for the script to finish. Choose this " +
	"over pymodule_run when the work should run on its own: a workflow " +
	"driver that orchestrates subagents, a long-running poller, a batch job " +
	"too long to hold your turn. Choose pymodule_run instead when you want " +
	"the output back in this conversation: it blocks until the script exits " +
	"and returns its stdout.\n\n" +
	"Exit 0 settles the child done, anything else failed. A settlement " +
	"notification may be pushed to your client as a best-effort log message " +
	"and many clients drop those unless they have set a logging level, so " +
	"when you need to know whether a script child finished, check " +
	"agent_list or task_list deliberately.\n\n" +
	"`repo` picks the source: \"local\" for your owner's saved pymodules " +
	"(pymodule_put — the spawning owner's bucket is what the child " +
	"materializes from), or a git source's name as reported by discovery. " +
	"`script` is the module name exactly as saved with pymodule_put -- not " +
	"a path, and not inline code. `modules` names further saved pymodules " +
	"the script imports, resolved within the same repo. The child gets the " +
	"ordinary child furniture: `labels` (key=value pairs for `rafiki list` " +
	"filters; the rafiki/ prefix and the owner key are daemon-reserved), " +
	"`max_cost` (USD budget for the child and everything IT spawns — set " +
	"one when the script will spawn subagents), `max_children`, and " +
	"`executor` (a label selector or a bare machine name, like " +
	"agent_spawn's). The child's name defaults to the script's name."

// mcpPyModuleStartBlueprint is the MCP face's pymodule_start. It reuses the
// registry blueprint's input schema and Execute — one decoder, one SpawnSpec
// builder, two surfaces — and differs only in description (face-local, above)
// and Materialize (the pymodule_run gate, via ToolOpts.PyModuleStarter).
type mcpPyModuleStartBlueprint struct {
	tools.PyModuleStartBlueprint
}

func (mcpPyModuleStartBlueprint) Description() string { return mcpPymoduleStartDescription }

func (mcpPyModuleStartBlueprint) Materialize(opts tools.ToolOpts) (tools.Tool, error) {
	if opts.PyModuleStarter == nil {
		return nil, nil
	}
	// The spawner rides PyModuleStarter explicitly, not Agents: the gate and
	// the binding must be one field, or a future opts change could
	// materialize a tool bound to a caller its gate never admitted.
	return tools.PyModuleStartBlueprint{}.Materialize(tools.ToolOpts{
		Agents: opts.PyModuleStarter,
		Cwd:    opts.Cwd,
	})
}
