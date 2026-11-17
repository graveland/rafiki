package tools

import (
	"context"
	"fmt"
	"strings"

	"go.graveland.dev/rafiki/pkg/protocol"
)

func init() {
	DefaultBlueprint.Register(&ExecutorInstallBlueprint{})
	DefaultBlueprint.Register(&ExecutorUninstallBlueprint{})
}

// ExecutorInstaller is the daemon-side capability behind executor_install and
// executor_uninstall. Bound to ONE caller at construction; no method takes a
// caller identity (the AgentSpawner/SandboxManager rule).
//
// The construction-time binding is what makes an install safe to expose: the
// caller's child id and its owner are closed over by the daemon, so a launcher
// named as a tool argument can only ever narrow which of the CALLER's machines
// runs the ssh. A child id passed as an argument would be an id an LLM can be
// prompt-injected into changing.
type ExecutorInstaller interface {
	// Install provisions and starts an executor on req.Target, through the
	// launcher req.Via names, and answers once it has connected.
	Install(ctx context.Context, req protocol.ExecutorInstallRequest) (protocol.ExecutorInstallResult, error)
	// Uninstall ends one installed executor this caller's lineage may remove.
	Uninstall(ctx context.Context, executor string) error
}

// executorInstallDescription is the tool's description VERBATIM: its wording is
// the contract a model reads, and the docs restate it.
const executorInstallDescription = "Install and start a rafiki executor on a machine reachable over ssh " +
	"from one of your executors (via). Afterwards name it with the executor " +
	"argument on any workspace tool. Reinstalling the same name reuses it. A down " +
	"installed executor restarts automatically on its next use. If ssh refuses " +
	"(host key, auth), the error carries ssh's own message: fix that on the via " +
	"machine and retry."

// executorInstallNoWorkspaceNote is appended to the description for a caller
// that cannot run workspace tools on the executor it installs. Its own tools
// belong to the host it runs on, not to an executor it may choose, so an
// install buys it nothing directly — only the agents it spawns (fundi children,
// which do address executors) can use the result.
const executorInstallNoWorkspaceNote = " You cannot run workspace tools on it yourself; agents you spawn can."

// executorInstallModeDescription is the `mode` parameter's description
// VERBATIM: its wording is the contract a model reads when it picks a
// transport. The rule itself lives in the daemon, so the tool passes `mode`
// through untouched.
const executorInstallModeDescription = "auto (default): probe the target for a route to the daemon " +
	"and install a self-dialling process if it has one, otherwise relay through the launcher's ssh " +
	"tunnel. direct: require the self-dialling form (refused if the target cannot reach the daemon). " +
	"tunnel: always relay through the launcher."

// executorInstallModeValues is the closed set the SCHEMA advertises — the
// model-facing vocabulary, not a validation rule. Which values are accepted,
// and what a value means for a given target, is the daemon's decision.
var executorInstallModeValues = []string{"auto", "direct", "tunnel"}

// ExecutorInstallBlueprint is the static metadata for executor_install.
type ExecutorInstallBlueprint struct{}

func (ExecutorInstallBlueprint) Name() string        { return "executor_install" }
func (ExecutorInstallBlueprint) Description() string { return executorInstallDescription }
func (ExecutorInstallBlueprint) InputSchema() Schema {
	return Schema{
		Type: "object",
		Properties: []SchemaProperty{
			{Name: "target", Type: "string", Description: "ssh destination of the machine to install on, \"host\" or \"user@host\". Resolved through the via machine's own ssh config, so ProxyJump/User/keys come from there."},
			{Name: "via", Type: "string", Description: "Which of your executors runs the ssh (its machine name or id). Omit for your default launcher."},
			{Name: "name", Type: "string", Description: "Machine name the new executor joins; omit for the destination's host part. Reinstalling the same name reuses that executor."},
			{Name: "root", Type: "string", Description: "Directory the new executor serves. Omit for its home directory."},
			{Name: "labels", Type: "object", Description: "Your own trust labels for the new executor, k=v. owner, machine and rafiki/ keys are written by the daemon and are refused here."},
			{Name: "admits", Type: "string", Description: "Executor-side admission selector over child labels."},
			{Name: "mode", Type: "string", Enum: executorInstallModeValues, Description: executorInstallModeDescription},
		},
		Required: []string{"target"},
	}
}
func (ExecutorInstallBlueprint) Execute(context.Context, ToolInput) (ToolResult, error) {
	panic("blueprint: call Materialize first")
}

func (b ExecutorInstallBlueprint) Materialize(opts ToolOpts) (Tool, error) {
	inst := opts.ExecutorInstaller
	if inst == nil {
		return nil, nil // decline
	}
	desc := b.Description()
	if opts.CannotUseInstalledExecutor {
		desc += executorInstallNoWorkspaceNote
	}
	return &executorInstallTool{inst: inst, desc: desc}, nil
}

type executorInstallTool struct {
	inst ExecutorInstaller
	desc string
}

func (t *executorInstallTool) Name() string        { return "executor_install" }
func (t *executorInstallTool) Description() string { return t.desc }
func (t *executorInstallTool) InputSchema() Schema { return ExecutorInstallBlueprint{}.InputSchema() }

func (t *executorInstallTool) Execute(ctx context.Context, in ToolInput) (ToolResult, error) {
	var raw struct {
		Target string            `json:"target"`
		Via    string            `json:"via"`
		Name   string            `json:"name"`
		Root   string            `json:"root"`
		Labels map[string]string `json:"labels"`
		Admits string            `json:"admits"`
		Mode   string            `json:"mode"`
	}
	if err := in.Unmarshal(&raw); err != nil {
		return ToolResult{}, fmt.Errorf("executor_install: %w", err)
	}
	if strings.TrimSpace(raw.Target) == "" {
		return ToolResult{}, fmt.Errorf(
			"executor_install: target is required — the ssh destination of the machine to install on, e.g. \"user@host\"")
	}
	res, err := t.inst.Install(ctx, protocol.ExecutorInstallRequest{
		Target: raw.Target,
		Via:    raw.Via,
		Name:   raw.Name,
		Root:   raw.Root,
		Labels: raw.Labels,
		Admits: raw.Admits,
		// Mode rides through unchanged: the daemon owns the vocabulary and the
		// refusal, so a copy of the rule here could only drift from it.
		Mode: raw.Mode,
	})
	if err != nil {
		return ToolResult{}, fmt.Errorf("executor_install: %w", err)
	}
	return ToolResult{Text: renderExecutorInstall(res)}, nil
}

// renderExecutorInstall renders one installed row for a model. The name falls
// back to the id so a row missing its machine label still names something to
// act on, and the connection state is spelled out rather than merely implied:
// an install that returns without a live connection is a fact the caller needs.
func renderExecutorInstall(res protocol.ExecutorInstallResult) string {
	name := res.Name
	if name == "" {
		name = res.ID
	}
	var b strings.Builder
	fmt.Fprintf(&b, "installed %s (%s)", name, res.ID)
	if res.Target != "" {
		fmt.Fprintf(&b, " on %s", res.Target)
	}
	if res.Via != "" {
		fmt.Fprintf(&b, " via %s", res.Via)
	}
	if res.Connected {
		b.WriteString("; connected")
	} else {
		b.WriteString("; not connected")
	}
	return b.String()
}

// executorUninstallDescription is the tool's description VERBATIM; see
// executorInstallDescription.
const executorUninstallDescription = "Remove an executor you installed over ssh. Its ssh session " +
	"ends and its executor row is deleted. The target keeps the shared binary cache, and " +
	"the install marker goes with the row, so installing there again is one call. Only " +
	"an install of your own can be removed."

// ExecutorUninstallBlueprint is the static metadata for executor_uninstall.
type ExecutorUninstallBlueprint struct{}

func (ExecutorUninstallBlueprint) Name() string        { return "executor_uninstall" }
func (ExecutorUninstallBlueprint) Description() string { return executorUninstallDescription }
func (ExecutorUninstallBlueprint) InputSchema() Schema {
	return Schema{
		Type: "object",
		Properties: []SchemaProperty{
			{Name: "executor", Type: "string", Description: "The installed executor to remove: its machine name or row id."},
		},
		Required: []string{"executor"},
	}
}
func (ExecutorUninstallBlueprint) Execute(context.Context, ToolInput) (ToolResult, error) {
	panic("blueprint: call Materialize first")
}

func (b ExecutorUninstallBlueprint) Materialize(opts ToolOpts) (Tool, error) {
	inst := opts.ExecutorInstaller
	if inst == nil {
		return nil, nil // decline
	}
	return &executorUninstallTool{inst: inst}, nil
}

type executorUninstallTool struct {
	inst ExecutorInstaller
}

func (t *executorUninstallTool) Name() string        { return "executor_uninstall" }
func (t *executorUninstallTool) Description() string { return executorUninstallDescription }
func (t *executorUninstallTool) InputSchema() Schema {
	return ExecutorUninstallBlueprint{}.InputSchema()
}

func (t *executorUninstallTool) Execute(ctx context.Context, in ToolInput) (ToolResult, error) {
	var raw struct {
		Executor string `json:"executor"`
	}
	if err := in.Unmarshal(&raw); err != nil {
		return ToolResult{}, fmt.Errorf("executor_uninstall: %w", err)
	}
	if strings.TrimSpace(raw.Executor) == "" {
		return ToolResult{}, fmt.Errorf(
			"executor_uninstall: executor is required — the installed executor's machine name or row id")
	}
	if err := t.inst.Uninstall(ctx, raw.Executor); err != nil {
		return ToolResult{}, fmt.Errorf("executor_uninstall: %w", err)
	}
	return ToolResult{Text: fmt.Sprintf("uninstalled %s", raw.Executor)}, nil
}
