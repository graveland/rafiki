package tools

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.graveland.dev/rafiki/pkg/protocol"
)

func init() {
	DefaultBlueprint.Register(&AgentListBlueprint{})
}

// AgentInfo is one descendant, as the daemon sees it. Every field is
// daemon-stamped: nothing here is asserted by the child it describes.
type AgentInfo struct {
	ChildID string
	Name    string
	Model   string
	Status  string // spawning|idle|streaming|running|tool_running|compacting|blocked_ui|shutting_down|exited
	// Kind is the child protocol ("claude"); empty means the fundi runtime
	// default. See docs/plans/2026-09-05-daraja-proxy-identity-design.md,
	// Piece 4.
	Kind string
	Cwd  string
	// Depth is the number of hops from the agent that asked, so a
	// coordinator can tell a worker from a worker's reviewer.
	Depth int
	// Task is the handle of the ledger row assigned to this child at spawn,
	// or "" when it was spawned without one.
	Task string
	// InSubtree is set only on an OWNER-scope list a child caller made: true
	// for a row inside its own subtree, false for another of its owner's
	// agents, which it may read but not steer. Nil everywhere else — a spawn
	// result, the settle digest, and every list a caller with no position in
	// the tree makes — because "not in your subtree" would be a claim those
	// callers cannot make.
	InSubtree *bool
}

// SpawnSpec is a request to create a child. It carries NO parent field: the
// implementation supplies the caller's own id, which is why an agent cannot
// spawn into somebody else's subtree.
type SpawnSpec struct {
	Name   string
	Model  string
	Cwd    string
	Prompt string
	// Task is a handle in the CALLER's ledger to assign to the new child.
	// Assignment happens only after the controller admits the spawn, so a
	// refused spawn leaves no row pointing at a child that never started.
	Task string
	// Kind selects the child runtime ("fundi", "claude", "script"). Empty
	// means fundi.
	Kind string
	// Script, for Kind "script", names the pymodule that IS the child's
	// brain — a saved Python module run as the child's process. Nil for
	// every other kind; the controller refuses a script kind without one.
	Script *protocol.ScriptSpec
	// Labels are user-supplied labels on the new child. The rafiki/ and
	// fundi/ prefixes and the "owner" key are daemon-reserved; the
	// controller validates and refuses.
	Labels map[string]string

	// MaxDepth / MaxCost / MaxChildren are GRANTS, not claims. The controller
	// treats them as a request and enforces its own stored limits regardless
	// — a nil here means "use the default", not "no limit".
	MaxDepth    *int
	MaxCost     *float64
	MaxChildren *int

	// ExecutorSelector is a label selector over EXECUTOR labels. The
	// controller intersects it with the spawner's own effective set, so this
	// can only ever narrow — a child cannot reach an executor its parent
	// could not, whatever this says.
	ExecutorSelector string
	// WorkspaceMode is "ephemeral" or "pinned". It decides whether the child
	// can be rescheduled when its machine goes away, which is the only reason
	// the model is asked about it at all.
	WorkspaceMode string

	// Preset names a preset the controller resolves before admission; the
	// fields below and Model/MaxCost/... then override or narrow it.
	Preset   string
	Thinking string
	// AppendSystemPrompt is appended AFTER the preset's own append.
	AppendSystemPrompt string
	// Tools/Skills/MCPServers are TRI-STATE requests: nil = no request,
	// non-nil empty = none, a list = exactly those (a subset of the preset's).
	Tools      *[]string
	Skills     *[]string
	MCPServers *[]string
	// ContextFiles: nil = no request; false = skip context files.
	ContextFiles *bool

	// Prefill lists files (or globs) the child reads through its own Read
	// tool before its first turn, recorded as real tool_use/tool_result
	// history. Nil means no pre-fill; an empty non-nil list is refused.
	Prefill []protocol.PrefillRead

	// Sandbox, when set, is the spawn block's sandbox: the new child's executor
	// is provisioned into a container described by it, and that container's
	// executor is offered to the child (ScopeSelf) or its subtree
	// (ScopeSubtree). Nil means the child runs on the ordinary executor set.
	// It carries no Name and no TTL — both are named-sandbox-only fields the
	// daemon refuses on a spawn block.
	Sandbox *protocol.SandboxSpec

	// ForkFrom, when set, starts the new child from a point in another child's
	// history instead of from nothing: it inherits that conversation up to the
	// cursor, by reference. Authority is the daemon's — a child may fork itself
	// or a child it could already read — and the fork is resolved ONCE, so a
	// later tag move never reaches it. Exclusive with FromTag.
	ForkFrom *ForkFromSpec
	// FromTag names a tag (see agent_tags) whose node the new child starts
	// from, resolved nearest scope first and once. Exclusive with ForkFrom.
	FromTag string
}

// ForkFromSpec is where a fork starts: the source child and, optionally, a
// cursor into its conversation. An empty Cursor means the source's last
// completed turn.
type ForkFromSpec struct {
	ChildID string
	Cursor  string
}

// TagSpec is one agent_tag call. TagID empty creates a tag; TagID and
// ExpectNodeID both set move it, and Name, Scope and Replace must then be
// unset. ChildID is the child whose conversation the tag points into, and is
// ignored by a move that names no child (the tag keeps its own).
type TagSpec struct {
	Name         string
	Scope        string
	ChildID      string
	Cursor       string
	Note         string
	TagID        string
	ExpectNodeID string
	// Replace takes over a live name in the same scope, which only a user
	// credential may do.
	Replace bool
}

// TagQuery is one agent_tags read: the caller's live tags, or one tag's whole
// history when TagID is set.
type TagQuery struct {
	NamePrefix string
	Limit      int
	TagID      string
}

// TagInfo is one tag as the daemon reports it. Every field is daemon-stamped:
// the creator and the node are read from stored state, never from the caller.
type TagInfo struct {
	TagID string
	Name  string
	// Scope is the stored spelling: "lineage", "tree" or "owner".
	Scope     string
	ScopeRoot string
	// ChildID is the child whose conversation the tag's node belongs to.
	ChildID string
	NodeID  string
	// Ordinal is the node's insertion ordinal in its conversation.
	Ordinal int
	// Creator is the child that created the tag; empty means a user did.
	Creator   string
	Note      string
	CreatedAt time.Time
	// Current reports whether this row is the live version. A history listing
	// carries the superseded and tombstoned versions too, with Current false.
	Current bool
}

// SendSpec is one agent_send: the message and the steps the daemon runs at
// send time, their output appended to the message.
type SendSpec struct {
	ChildID string
	Message string
	Steps   []protocol.SendStep
}

// ViewSpec is one agent_view read. Before, After, Entry and Find are the
// opaque cursor strings a previous agent_view marker printed (Find is the
// search text); at most one is set, except Find with After. Validate
// enforces that.
type ViewSpec struct {
	ChildID string
	Limit   int
	Before  string
	After   string
	Entry   string
	Find    string
}

// Validate refuses a combination of paging arguments the view cannot honour.
// Conflicting arguments are refused rather than resolved by precedence: the
// only combination that means anything is find with after, which continues a
// search.
func (s ViewSpec) Validate() error {
	switch {
	case s.Before != "" && s.After != "":
		return conflicting("before", "after")
	case s.Before != "" && s.Entry != "":
		return conflicting("before", "entry")
	case s.Before != "" && s.Find != "":
		return conflicting("before", "find")
	case s.After != "" && s.Entry != "":
		return conflicting("after", "entry")
	case s.Entry != "" && s.Find != "":
		return conflicting("entry", "find")
	case s.Entry != "" && s.Limit != 0:
		return errors.New("limit does not apply to entry")
	}
	return nil
}

func conflicting(a, b string) error {
	return fmt.Errorf("%s and %s cannot be combined; only find and after go together", a, b)
}

// SendResult reports each step to the sender (never its output, except
// an echo the step asked for).
type SendResult struct {
	Steps []protocol.StepSummary
}

// ListScope selects which agents a list reports.
//
// The EMPTY string means ListScopeSubtree, and that is deliberate: the narrow
// answer is the default, so a caller that names no scope can never be handed
// its owner's whole agent set by an ordinary zero value.
//
// The two scopes differ only in who is READING. Steering stays on the spawn
// tree either way: an agent outside the caller's subtree appears on an owner
// list marked read-only.
type ListScope string

const (
	// ListScopeSubtree lists the agents the caller spawned, and anything they
	// spawned in turn.
	ListScopeSubtree ListScope = "subtree"
	// ListScopeOwner adds every other agent the caller's owner owns: readable,
	// not steerable.
	ListScopeOwner ListScope = "owner"
)

// ListOpts is one list request. The zero value asks for ListScopeSubtree.
type ListOpts struct {
	Scope ListScope
}

// AgentSpawner is the daemon-side capability behind the agent_* tools.
//
// Every method is scoped to the ONE binding this value was constructed for,
// and takes no caller identity in any method: the child-bound implementation
// scopes to its child's subtree, the user-bound one to the whole daemon.
// Implementations must reject a childID outside their binding, reading
// stored state rather than any argument. The fundi tools package cannot
// import cmd/rafikid, so the daemon provides the implementation — the same
// seam as LSPClient and ExecutorClient.
type AgentSpawner interface {
	// List returns every live and exited agent opts.Scope selects, nearest
	// first: the caller's descendants for ListScopeSubtree, plus every other
	// agent its owner owns for ListScopeOwner, those marked InSubtree false.
	// Authority to LIST is the binding's, exactly as it is for every other
	// method here.
	List(ctx context.Context, opts ListOpts) ([]AgentInfo, error)
	// Models enumerates the models a child may be spawned on, narrowed by q.
	// A zero ModelQuery asks for everything the child's kind can resolve; the
	// caller renders a summary rather than several hundred rows.
	Models(ctx context.Context, q ModelQuery) ([]ModelInfo, error)
	// Spawn creates a descendant and returns it once it is registered.
	Spawn(ctx context.Context, spec SpawnSpec) (AgentInfo, error)
	// View renders a descendant's transcript as plain text: the newest page
	// by default, or the page/entry/search spec names. spec.Limit caps
	// entries (find hits, for a search); 0 means the default.
	View(ctx context.Context, spec ViewSpec) (string, error)
	// Send delivers a prompt to a descendant, running spec.Steps first.
	Send(ctx context.Context, spec SendSpec) (SendResult, error)
	// Kill shuts a descendant down and waits for the exit to be recorded.
	Kill(ctx context.Context, childID string) error
	// SetBudget changes a child's MaxCost, in USD — the same units
	// SpawnSpec.MaxCost already uses. 0 means unlimited. What may be
	// re-budgeted is a property of the binding: a child-bound spawner
	// accepts only its DIRECT child (not merely any descendant, unlike
	// every other method on this interface); a user-bound spawner accepts
	// only top-level children. In both cases the answer is read from stored
	// lineage, never trusted from the argument.
	SetBudget(ctx context.Context, childID string, maxCost float64) error
	// SetRouting merges a bracket-free routing-spec delta over childID's
	// stored routing and returns the new canonical spec. Authority is a
	// property of the binding, exactly as SetBudget's is: the child-bound
	// implementation steers its own subtree with child authority (subtree
	// membership and the child-provenance only= refusal are enforced by the
	// daemon method it calls, never re-implemented here), and the user-bound
	// implementation steers with operator authority. As with every method on
	// this interface the caller identity is the binding's own, not an
	// argument.
	SetRouting(ctx context.Context, childID, spec string) (string, error)
	// Tag creates the tag spec names, or moves it when spec.TagID is set.
	// The caller's identity — and, for a child-bound spawner, its own child
	// id, which a create defaults the source child to — is the binding's, so
	// a tag's creator and scope root can never be asserted by the caller.
	Tag(ctx context.Context, spec TagSpec) (TagInfo, error)
	// Untag tombstones a tag. Only its creator, an ancestor of the creator,
	// or a user credential may; the daemon decides that from stored lineage.
	Untag(ctx context.Context, tagID string) error
	// Tags lists the tags the caller may fork from, nearest scope first, or
	// one tag's history when q.TagID is set.
	Tags(ctx context.Context, q TagQuery) ([]TagInfo, error)
}

const (
	agentListDescription = "List the agents you have spawned (your subagents and " +
		"anything they spawned in turn), with each one's id, name, model, current " +
		"status, working directory and assigned task handle. Pass scope=\"owner\" to " +
		"add every other agent your owner owns: those are marked read-only — you " +
		"can read them (agent_view, conversation_export) but not steer them. Use " +
		"this before agent_send or agent_kill to find the id you mean, or to look " +
		"something up. Calling it in a loop to detect completion is unnecessary — " +
		"you are notified when a subagent settles, and pinged periodically while " +
		"one is still working."

	// maxOwnerListRows caps one agent_list page. An owner-scoped list can name
	// every agent a user owns, and an owner running several coordinators can
	// hold hundreds; the cap applies to both scopes because an unusually large
	// subtree benefits from the same bound, and it applies to EVERY caller —
	// including a user credential's whole-daemon list, which the MCP face serves
	// from userSpawner.List. The name says "owner" because that is the scope it
	// was introduced for; the bound itself is unconditional. The Connect
	// ListChildren verb stays uncapped. The tool says how many it hid, so the
	// model never mistakes a page for the whole set.
	maxOwnerListRows = 50
)

// --- agent_list ---

type AgentListBlueprint struct{}

func (AgentListBlueprint) Name() string        { return "agent_list" }
func (AgentListBlueprint) Description() string { return agentListDescription }
func (AgentListBlueprint) InputSchema() Schema {
	return Schema{Type: "object", Properties: []SchemaProperty{
		{
			Name: "scope", Type: "string",
			Enum: []string{string(ListScopeSubtree), string(ListScopeOwner)},
			Description: "subtree (default) lists the agents you spawned; owner also lists " +
				"every other agent your owner owns, which you can read (agent_view, " +
				"conversation_export) but not steer",
		},
		{
			Name: "status", Type: "string",
			Description: "only agents with exactly this status",
		},
	}}
}

func (AgentListBlueprint) Execute(context.Context, ToolInput) (ToolResult, error) {
	panic("blueprint: call Materialize first")
}

func (AgentListBlueprint) Materialize(opts ToolOpts) (Tool, error) {
	if opts.Agents == nil {
		return nil, nil
	}
	return &agentListTool{agents: opts.Agents}, nil
}

type agentListTool struct {
	AgentListBlueprint
	agents AgentSpawner
}

func (t *agentListTool) Execute(ctx context.Context, input ToolInput) (ToolResult, error) {
	var params struct {
		Scope  string `json:"scope,omitempty"`
		Status string `json:"status,omitempty"`
	}
	if err := input.Unmarshal(&params); err != nil {
		return ToolResult{}, fmt.Errorf("agent_list: invalid input: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return ToolResult{}, err
	}
	// The empty scope is the SUBTREE default, spelled out here rather than
	// passed through, so the spawner only ever sees the two documented values.
	scope := ListScope(params.Scope)
	switch scope {
	case "", ListScopeSubtree:
		scope = ListScopeSubtree
	case ListScopeOwner:
	default:
		return ToolResult{}, errors.New(`agent_list: scope must be "subtree" or "owner"`)
	}
	kids, err := t.agents.List(ctx, ListOpts{Scope: scope})
	if err != nil {
		return ToolResult{}, fmt.Errorf("agent_list: %w", err)
	}
	if params.Status != "" {
		// A fresh slice, not an in-place filter: the spawner owns the slice it
		// returned, and compacting it would rewrite that caller's rows.
		kept := make([]AgentInfo, 0, len(kids))
		for _, k := range kids {
			if k.Status == params.Status {
				kept = append(kept, k)
			}
		}
		kids = kept
	}
	total := len(kids)
	if total > maxOwnerListRows {
		kids = kids[:maxOwnerListRows]
	}
	text := renderAgents(kids, total)
	if hidden := total - len(kids); hidden > 0 {
		text += fmt.Sprintf("\n... %d more agents not shown; narrow with status=<state>\n", hidden)
	}
	return NewTextResult(text), nil
}

// RenderAgents formats a descendant list for the model. Exported so the
// daemon-side settle digest renders identically to what agent_list shows.
func RenderAgents(kids []AgentInfo) string {
	return renderAgents(kids, len(kids))
}

// renderAgents renders kids under a header naming total, the number of rows
// the caller had before the cap: a truncated page heads "50 of 83 agent(s)"
// so the model can tell it is not seeing everything.
func renderAgents(kids []AgentInfo, total int) string {
	var sb strings.Builder
	if total != len(kids) {
		fmt.Fprintf(&sb, "%d of %d agent(s)\n", len(kids), total)
	} else {
		fmt.Fprintf(&sb, "%d agent(s)\n", len(kids))
	}
	for _, k := range kids {
		indent := strings.Repeat("  ", k.Depth)
		fmt.Fprintf(&sb, "%s%s  %s  [%s]  %s  %s", indent, k.ChildID, orDash(k.Name), k.Status, kindOrDefault(k.Kind), orDash(k.Model))
		if k.Task != "" {
			fmt.Fprintf(&sb, "  task=%s", k.Task)
		}
		if k.Cwd != "" {
			fmt.Fprintf(&sb, "  cwd=%s", k.Cwd)
		}
		// Only a pointer-to-FALSE marks a row: a caller that may read but not
		// steer this agent gets told so, and a nil (every caller with no
		// position in the tree) says nothing at all.
		if k.InSubtree != nil && !*k.InSubtree {
			sb.WriteString("  read-only")
		}
		sb.WriteString("\n")
	}
	return sb.String()
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// kindOrDefault names a child's kind for display, defaulting an empty Kind
// (the fundi runtime default) to "fundi" rather than the usual "-" dash — an
// empty Kind is a real, meaningful value, not absent data. This is a
// deliberate separate copy of cmd/rafiki/output.go's identical helper: this
// package cannot import cmd/rafiki, and the two callers' formatting contexts
// (a table cell vs. an inline text field) are independent enough not to
// share one.
func kindOrDefault(kind string) string {
	if kind == "" {
		return "fundi"
	}
	return kind
}
