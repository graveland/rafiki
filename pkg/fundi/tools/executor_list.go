package tools

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"go.graveland.dev/rafiki/pkg/protocol"
)

func init() { DefaultBlueprint.Register(&ExecutorListBlueprint{}) }

// ExecutorLister is the daemon-side capability behind executor_list: the
// executors ONE caller may inspect, with the pool's live health view.
//
// Bound to one caller at construction, like ExecutorInstaller and
// SandboxManager: no method takes a caller identity, because a tool argument is
// produced by an LLM that can be prompt-injected into naming somebody else.
//
// It is a SEPARATE interface from ExecutorInstaller on purpose. The install
// verbs need the launch RECIPE table; a listing needs only the executor store
// plus the pool, so a daemon that can answer one may not be able to answer the
// other, and each declines on its own nil rather than tying a readable status to
// an ability to install.
type ExecutorLister interface {
	// List returns the executors the bound caller may see, sorted by name then
	// id so a repeated call renders the same way.
	List(ctx context.Context) ([]protocol.ExecutorStatus, error)
}

// executorListDescription is the tool's description VERBATIM; its wording is
// the contract a model reads, and the docs restate it.
const executorListDescription = "List the executors you can see, with whether each is connected right now. " +
	"An installed one also shows the launcher and ssh destination it came from, so you can " +
	"tell a machine you provisioned from one somebody else did. A down executor is not " +
	"necessarily broken: an installed one restarts on its next use, as long as its launcher " +
	"is reachable, while a disabled one never connects. Annotations are hints another agent " +
	"left about that machine and may be stale. Takes no arguments."

// ExecutorListBlueprint is the static metadata for executor_list.
type ExecutorListBlueprint struct{}

func (ExecutorListBlueprint) Name() string        { return "executor_list" }
func (ExecutorListBlueprint) Description() string { return executorListDescription }
func (ExecutorListBlueprint) InputSchema() Schema { return Schema{Type: "object"} }

func (ExecutorListBlueprint) Execute(context.Context, ToolInput) (ToolResult, error) {
	panic("blueprint: call Materialize first")
}

func (b ExecutorListBlueprint) Materialize(opts ToolOpts) (Tool, error) {
	list := opts.Executors
	if list == nil {
		return nil, nil // decline
	}
	return &executorListTool{list: list}, nil
}

type executorListTool struct {
	list ExecutorLister
}

func (t *executorListTool) Name() string        { return "executor_list" }
func (t *executorListTool) Description() string { return executorListDescription }
func (t *executorListTool) InputSchema() Schema { return ExecutorListBlueprint{}.InputSchema() }

func (t *executorListTool) Execute(ctx context.Context, _ ToolInput) (ToolResult, error) {
	if err := ctx.Err(); err != nil {
		return ToolResult{}, err
	}
	rows, err := t.list.List(ctx)
	if err != nil {
		return ToolResult{}, fmt.Errorf("executor_list: %w", err)
	}
	if len(rows) == 0 {
		return NewTextResult("no executors"), nil
	}
	return NewTextResult(renderExecutors(rows)), nil
}

// renderExecutors renders one block per executor: a name-and-id heading, then
// the facts a caller acts on, one per line, and only the ones that are set — an
// unset annotation or root is absent, never rendered as an empty string a model
// would read as "this machine has no roots".
//
// A down executor says WHY it may be down as far as the daemon can know
// (disabled, or merely not connected), because "down" alone invites the reader
// to conclude "broken" about a machine that restarts on next use.
func renderExecutors(rows []protocol.ExecutorStatus) string {
	var b strings.Builder
	for i, e := range rows {
		if i > 0 {
			b.WriteString("\n")
		}
		name := e.Name
		if name == "" {
			name = "-"
		}
		fmt.Fprintf(&b, "%s (%s)\n", name, e.ID)
		fmt.Fprintf(&b, "  status: %s\n", executorStatusLine(e))

		var caps []string
		caps = append(caps, "workspace "+workspaceModeOrPinned(e.WorkspaceMode))
		if e.Isolation != "" {
			caps = append(caps, "isolation "+e.Isolation)
		}
		if len(e.LaunchKinds) > 0 {
			caps = append(caps, "launches "+strings.Join(e.LaunchKinds, ", "))
		}
		fmt.Fprintf(&b, "  %s\n", strings.Join(caps, ", "))
		if len(e.Roots) > 0 {
			fmt.Fprintf(&b, "  roots: %s\n", strings.Join(e.Roots, ", "))
		}
		if labels := sortedLabels(e.Labels); labels != "" {
			fmt.Fprintf(&b, "  labels: %s\n", labels)
		}
		if annotations := sortedLabels(e.Annotations); annotations != "" {
			fmt.Fprintf(&b, "  annotations: %s\n", annotations)
		}
		if e.InstalledVia != "" || e.InstalledTarget != "" {
			target := e.InstalledTarget
			if target == "" {
				target = "an unnamed target"
			}
			if e.InstalledVia != "" {
				fmt.Fprintf(&b, "  installed: on %s via %s\n", target, e.InstalledVia)
			} else {
				fmt.Fprintf(&b, "  installed: on %s (launcher no longer connected)\n", target)
			}
		}
	}
	return b.String()
}

// executorStatusLine spells out the connection state, and for a down executor
// whether it may still come up.
func executorStatusLine(e protocol.ExecutorStatus) string {
	var b strings.Builder
	if e.Connected {
		b.WriteString("live")
		if e.ConnectedAt != nil {
			fmt.Fprintf(&b, ", connected since %s", e.ConnectedAt.Format(time.RFC3339))
		}
	} else if !e.Enabled {
		b.WriteString("disabled")
	} else {
		b.WriteString("down")
	}
	if e.LastSeen != nil {
		fmt.Fprintf(&b, ", last seen %s", e.LastSeen.Format(time.RFC3339))
	} else if !e.Connected {
		b.WriteString(", never seen")
	}
	return b.String()
}

// workspaceModeOrPinned mirrors the daemon's own default: an executor that
// declared no workspace mode is pinned, and a listing must not render that as
// an empty field a reader could mistake for "unknown".
func workspaceModeOrPinned(mode string) string {
	if mode == "" {
		return defaultWorkspaceMode
	}
	return mode
}

// defaultWorkspaceMode is what an executor that declared no workspace mode
// gets — the same "pinned" the executor store writes (pkg/executorsdb).
const defaultWorkspaceMode = "pinned"

// sortedLabels renders a label map as "k=v, k=v" in key order, so a repeated
// listing is byte-identical and a test can assert on it.
func sortedLabels(m map[string]string) string {
	if len(m) == 0 {
		return ""
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+m[k])
	}
	return strings.Join(parts, ", ")
}
