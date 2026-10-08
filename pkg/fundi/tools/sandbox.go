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
	DefaultBlueprint.Register(&SandboxCreateBlueprint{})
	DefaultBlueprint.Register(&SandboxListBlueprint{})
	DefaultBlueprint.Register(&SandboxRemoveBlueprint{})
}

// SandboxManager is the daemon-side capability behind the sandbox_* tools.
//
// Every method is scoped to the ONE binding this value was constructed for and
// takes no caller identity in any method: the daemon binds a child-bound
// implementation (the caller's child id and its owner's NON-admin identity
// closed over at construction) or an operator-bound one. That is deliberate and
// the same rule as AgentSpawner — an id passed as an argument is a tool
// argument, and a tool argument is produced by an LLM that can be
// prompt-injected into naming somebody else. The fundi tools package cannot
// import cmd/rafikid, so the daemon provides the implementation; the same seam
// as AgentSpawner, LSPClient and ExecutorClient.
type SandboxManager interface {
	// Create provisions a named sandbox and returns it once its executor has
	// joined the pool.
	Create(ctx context.Context, spec protocol.SandboxSpec) (protocol.SandboxInfo, error)
	// List returns the sandboxes the bound caller may see.
	List(ctx context.Context) ([]protocol.SandboxInfo, error)
	// Remove tears down one sandbox by name or row id.
	Remove(ctx context.Context, ref string) error
	// Sync copies a file or a directory between two executors the bound caller
	// may reach, brokered by the daemon. MaxBytes is the caller's cap; nil means
	// no caller cap and a present value must be > 0 (the daemon refuses a
	// present non-positive value).
	Sync(ctx context.Context, req protocol.SyncPathRequest) (protocol.SyncPathResult, error)
	// SyncRepo fetches one git branch from a repository on one executor into a
	// repository on another as a bundle. Only committed state travels.
	SyncRepo(ctx context.Context, req protocol.SyncRepoRequest) (protocol.SyncRepoResult, error)
}

// --- shared input parsing --------------------------------------------------

// sandboxMountParam is the flat, typed mount input both sandbox_create and the
// agent_spawn sandbox block accept. It is deliberately flat: a model that has
// to nest JSON mis-nests it, and a mount is the one part of a sandbox a caller
// must get exactly right (a ro mount is the difference between reading a
// checkout and corrupting it).
type sandboxMountParam struct {
	Target   string `json:"target"`
	Kind     string `json:"kind"`
	HostPath string `json:"host_path,omitempty"`
	Volume   string `json:"volume,omitempty"`
}

// parseSandboxMounts maps the flat mount inputs onto protocol mounts.
//
// An OMITTED kind is an error, never a silent "rw": the brief's rule is that
// nothing about a mount has an unset-means-default surprise. The remaining
// source rules (ro requires exactly one of host_path/volume, ephemeral forbids
// a source, host_path must sit under the launcher's roots) live in
// pkg/sandbox.Validate on the daemon and surface here as the daemon's error.
func parseSandboxMounts(in []sandboxMountParam) ([]protocol.SandboxMount, error) {
	if in == nil {
		return nil, nil
	}
	out := make([]protocol.SandboxMount, 0, len(in))
	for i, m := range in {
		kind := protocol.MountKind(m.Kind)
		switch kind {
		case protocol.MountRO, protocol.MountRW, protocol.MountEphemeral:
		case "":
			return nil, fmt.Errorf("mounts[%d].kind is required (ro, rw or ephemeral); an omitted kind is not a default", i)
		default:
			return nil, fmt.Errorf("mounts[%d].kind must be ro, rw or ephemeral, got %q", i, m.Kind)
		}
		if m.Target == "" {
			return nil, fmt.Errorf("mounts[%d].target is required", i)
		}
		out = append(out, protocol.SandboxMount{
			Target:   m.Target,
			Kind:     kind,
			HostPath: m.HostPath,
			Volume:   m.Volume,
		})
	}
	return out, nil
}

// parseNetworkMode maps the network input. Empty is the daemon's configured
// default (resolved and persisted by the daemon); anything else must be one of
// the two declared modes.
func parseNetworkMode(s string) (protocol.NetworkMode, error) {
	switch protocol.NetworkMode(s) {
	case "", protocol.NetworkEgress, protocol.NetworkNone:
		return protocol.NetworkMode(s), nil
	default:
		return "", fmt.Errorf("network must be %q or %q, got %q", protocol.NetworkEgress, protocol.NetworkNone, s)
	}
}

// parseSandboxTTL parses the duration string form ("72h", "30m"). Empty means
// the daemon's configured default TTL; a bad value is an error naming ttl.
func parseSandboxTTL(s string) (time.Duration, error) {
	if s == "" {
		return 0, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("ttl: %q is not a duration like \"72h\" or \"30m\"", s)
	}
	return d, nil
}

// spawnSandboxParam is the agent_spawn `sandbox` block. It carries the same flat
// typed fields sandbox_create accepts, MINUS name and ttl (both named-sandbox
// only, refused by the daemon on a spawn block), PLUS scope (required: the
// spawn block is the only place scope is meaningful).
type spawnSandboxParam struct {
	Scope          string              `json:"scope"`
	Image          string              `json:"image,omitempty"`
	Launcher       string              `json:"launcher,omitempty"`
	Workdir        string              `json:"workdir,omitempty"`
	Mounts         []sandboxMountParam `json:"mounts,omitempty"`
	Network        string              `json:"network,omitempty"`
	ReadOnlyRootfs *bool               `json:"read_only_rootfs,omitempty"`
	Env            map[string]string   `json:"env,omitempty"`
	User           string              `json:"user,omitempty"`
	Labels         map[string]string   `json:"labels,omitempty"`
	MemoryBytes    int64               `json:"memory_bytes,omitempty"`
	CPUs           float64             `json:"cpus,omitempty"`
	PidsLimit      int64               `json:"pids_limit,omitempty"`
}

// buildSpawnSandbox converts the agent_spawn `sandbox` block into a spawn-block
// SandboxSpec. A nil block is no sandbox (nil, nil). scope is required and is
// checked here — an omitted scope is an error, never a default — the same
// no-unset-means-default rule the mount kind follows; the daemon re-checks it
// in pkg/sandbox.Validate.
func buildSpawnSandbox(p *spawnSandboxParam) (*protocol.SandboxSpec, error) {
	if p == nil {
		return nil, nil
	}
	scope := protocol.SandboxScope(p.Scope)
	switch scope {
	case protocol.ScopeSelf, protocol.ScopeSubtree:
	case "":
		return nil, errors.New("sandbox.scope is required (\"self\" or \"subtree\"): self offers the sandbox to the new agent only, subtree also to its descendants")
	default:
		return nil, fmt.Errorf("sandbox.scope must be %q or %q, got %q", protocol.ScopeSelf, protocol.ScopeSubtree, p.Scope)
	}
	mounts, err := parseSandboxMounts(p.Mounts)
	if err != nil {
		return nil, fmt.Errorf("sandbox.%w", err)
	}
	network, err := parseNetworkMode(p.Network)
	if err != nil {
		return nil, fmt.Errorf("sandbox.%w", err)
	}
	return &protocol.SandboxSpec{
		Image:          p.Image,
		Launcher:       p.Launcher,
		Workdir:        p.Workdir,
		Mounts:         mounts,
		Network:        network,
		ReadOnlyRootfs: p.ReadOnlyRootfs != nil && *p.ReadOnlyRootfs,
		Env:            p.Env,
		User:           p.User,
		MemoryBytes:    p.MemoryBytes,
		CPUs:           p.CPUs,
		PidsLimit:      p.PidsLimit,
		Labels:         p.Labels,
		Scope:          scope,
	}, nil
}

// sandboxMountSchema is the JSON schema for one mount, shared by sandbox_create
// and the agent_spawn sandbox block so the two cannot drift.
func sandboxMountSchema() *Schema {
	return &Schema{
		Type: "object",
		Properties: []SchemaProperty{
			{Name: "target", Type: "string",
				Description: "Absolute path inside the container where the mount appears (e.g. \"/work\")."},
			{Name: "kind", Type: "string", Enum: []string{"ro", "rw", "ephemeral"},
				Description: "\"ro\": read-only — the container cannot write it, not even as root. \"rw\": read-write. \"ephemeral\": a tmpfs, gone with the container. Required: an omitted kind is an error, not a default."},
			{Name: "host_path", Type: "string",
				Description: "Host directory to bind for a ro/rw mount. It must sit under a directory the launcher's operator allowed (its --sandbox-mount-root); a path outside is refused."},
			{Name: "volume", Type: "string",
				Description: "Named volume to bind for a ro/rw mount, instead of host_path. Exactly one of host_path/volume for a ro mount; a rw mount with neither is an anonymous volume."},
		},
		Required: []string{"target", "kind"},
	}
}

// sandboxResourceProperties are the limit fields shared by sandbox_create and
// the agent_spawn sandbox block.
func sandboxResourceProperties() []SchemaProperty {
	return []SchemaProperty{
		{Name: "memory_bytes", Type: "integer",
			Description: "Memory limit in bytes (e.g. 2147483648 for 2 GiB). Omit for no explicit limit."},
		{Name: "cpus", Type: "number",
			Description: "CPU limit as a count of cores (e.g. 2 or 0.5). Omit for no explicit limit."},
		{Name: "pids_limit", Type: "integer",
			Description: "Maximum number of processes in the container. Omit for no explicit limit."},
	}
}

// --- sandbox_create --------------------------------------------------------

const sandboxCreateDescription = "Create a sandbox: an isolated container that runs its own " +
	"rafiki executor, with its own mounts, network and resource limits, reachable from " +
	"agent_spawn. Use it to give a worker a clean, disposable machine — a checkout it " +
	"cannot corrupt, or a run whose CONTAINER has no network — instead of running it on " +
	"your own filesystem.\n\n" +
	"`mounts` is a list of {target, kind, host_path, volume}. `kind` is required and is " +
	"one of \"ro\", \"rw\" or \"ephemeral\": a \"ro\" mount cannot be written even by root " +
	"inside the container, so it is how you hand in a checkout to read; \"rw\" is " +
	"read-write; \"ephemeral\" is a tmpfs removed with the container. A `host_path` source " +
	"must sit under a directory the launcher's operator allowed (its --sandbox-mount-root) " +
	"— a path outside is refused — and `volume` names a persistent named volume instead.\n\n" +
	"`network` is \"egress\" (reaches the network, the default) or \"none\" (severs the " +
	"container's own network, not even DNS). This is a DOCKER setting on the " +
	"container: an agent driving tools in it still runs in the daemon, so \"none\" " +
	"does not cut that agent's own egress. `ttl` is a duration like \"72h\" or \"30m\": the sandbox is removed " +
	"automatically when it elapses, so you do not have to remember to tear it down; remove " +
	"one early with sandbox_remove. `memory_bytes`, `cpus` and `pids_limit` bound the " +
	"container's resources. See sandbox_list for what you have."

type SandboxCreateBlueprint struct{}

func (SandboxCreateBlueprint) Name() string        { return "sandbox_create" }
func (SandboxCreateBlueprint) Description() string { return sandboxCreateDescription }
func (SandboxCreateBlueprint) InputSchema() Schema {
	props := []SchemaProperty{
		{Name: "name", Type: "string",
			Description: "Name for the sandbox, unique among your sandboxes (e.g. \"build-box\"). How you find it in sandbox_list and remove it with sandbox_remove."},
		{Name: "image", Type: "string",
			Description: "Container image to run. It must run `rafiki executor serve` so the sandbox can connect back. Omit to use the daemon's configured default image."},
		{Name: "launcher", Type: "string",
			Description: "Which docker launcher to create it on, by machine label or executor id. Omit when only one is in scope."},
		{Name: "workdir", Type: "string",
			Description: "Absolute working directory inside the container. Omit for the image's default."},
		{Name: "mounts", Type: "array", Items: sandboxMountSchema(),
			Description: "Directories to make visible inside the container. Each is {target, kind, host_path, volume}; kind (ro/rw/ephemeral) is required."},
		{Name: "network", Type: "string", Enum: []string{"egress", "none"},
			Description: "\"egress\" reaches the network (the default); \"none\" severs the CONTAINER's network — an agent driving tools in it still runs in the daemon, so this does not cut its own egress."},
		{Name: "read_only_rootfs", Type: "boolean",
			Description: "true to mount the container's own root filesystem read-only, so only the mounts are writable."},
		{Name: "env", Type: "object",
			Description: "Environment variables to set in the container, as a JSON object of name to value. Keys beginning RAFIKI_ are reserved."},
		{Name: "user", Type: "string",
			Description: "uid or uid:gid the container runs as. Omit for the image's default."},
		{Name: "labels", Type: "object",
			Description: "Labels to attach, as a JSON object of name to value. Keys under rafiki/ and fundi/ are reserved."},
	}
	props = append(props, sandboxResourceProperties()...)
	props = append(props, SchemaProperty{Name: "ttl", Type: "string",
		Description: "How long the sandbox lives before it is removed automatically, as a duration like \"72h\" or \"30m\". Omit for the daemon's default."})
	return Schema{Type: "object", Properties: props, Required: []string{"name"}}
}

func (SandboxCreateBlueprint) Execute(context.Context, ToolInput) (ToolResult, error) {
	panic("blueprint: call Materialize first")
}

func (SandboxCreateBlueprint) Materialize(opts ToolOpts) (Tool, error) {
	if opts.Sandboxes == nil {
		return nil, nil
	}
	return &sandboxCreateTool{sandboxes: opts.Sandboxes}, nil
}

type sandboxCreateTool struct {
	SandboxCreateBlueprint
	sandboxes SandboxManager
}

func (t *sandboxCreateTool) Execute(ctx context.Context, in ToolInput) (ToolResult, error) {
	if err := ctx.Err(); err != nil {
		return ToolResult{}, err
	}
	var params struct {
		Name           string              `json:"name"`
		Image          string              `json:"image,omitempty"`
		Launcher       string              `json:"launcher,omitempty"`
		Workdir        string              `json:"workdir,omitempty"`
		Mounts         []sandboxMountParam `json:"mounts,omitempty"`
		Network        string              `json:"network,omitempty"`
		ReadOnlyRootfs *bool               `json:"read_only_rootfs,omitempty"`
		Env            map[string]string   `json:"env,omitempty"`
		User           string              `json:"user,omitempty"`
		Labels         map[string]string   `json:"labels,omitempty"`
		MemoryBytes    int64               `json:"memory_bytes,omitempty"`
		CPUs           float64             `json:"cpus,omitempty"`
		PidsLimit      int64               `json:"pids_limit,omitempty"`
		TTL            string              `json:"ttl,omitempty"`
	}
	if err := in.Unmarshal(&params); err != nil {
		return ToolResult{}, fmt.Errorf("sandbox_create: invalid input: %w", err)
	}
	if params.Name == "" {
		return ToolResult{}, errors.New("sandbox_create: name is required — a named sandbox is how you find it in sandbox_list and remove it with sandbox_remove")
	}
	mounts, err := parseSandboxMounts(params.Mounts)
	if err != nil {
		return ToolResult{}, fmt.Errorf("sandbox_create: %w", err)
	}
	network, err := parseNetworkMode(params.Network)
	if err != nil {
		return ToolResult{}, fmt.Errorf("sandbox_create: %w", err)
	}
	ttl, err := parseSandboxTTL(params.TTL)
	if err != nil {
		return ToolResult{}, fmt.Errorf("sandbox_create: %w", err)
	}

	info, err := t.sandboxes.Create(ctx, protocol.SandboxSpec{
		Name:           params.Name,
		Image:          params.Image,
		Launcher:       params.Launcher,
		Workdir:        params.Workdir,
		Mounts:         mounts,
		Network:        network,
		ReadOnlyRootfs: params.ReadOnlyRootfs != nil && *params.ReadOnlyRootfs,
		Env:            params.Env,
		User:           params.User,
		MemoryBytes:    params.MemoryBytes,
		CPUs:           params.CPUs,
		PidsLimit:      params.PidsLimit,
		Labels:         params.Labels,
		TTL:            ttl,
	})
	if err != nil {
		// Return the ERROR, never its text as a successful result.
		return ToolResult{}, fmt.Errorf("sandbox_create: %w", err)
	}
	return NewTextResult(fmt.Sprintf("created sandbox %s (%s), state %s\n\n%s",
		info.Name, info.ID, info.State, renderSandboxes([]protocol.SandboxInfo{info}))), nil
}

// --- sandbox_list ----------------------------------------------------------

const sandboxListDescription = "List the sandboxes you can reach: each one's name, id, " +
	"state, network, image, launcher, expiry and whether its executor is currently " +
	"connected. A sandbox whose ttl has elapsed (or that was removed) is gone. Use the " +
	"name or id with sandbox_remove to tear one down early, or with agent_spawn to run a " +
	"worker inside it. Takes no arguments."

type SandboxListBlueprint struct{}

func (SandboxListBlueprint) Name() string        { return "sandbox_list" }
func (SandboxListBlueprint) Description() string { return sandboxListDescription }
func (SandboxListBlueprint) InputSchema() Schema { return Schema{Type: "object"} }

func (SandboxListBlueprint) Execute(context.Context, ToolInput) (ToolResult, error) {
	panic("blueprint: call Materialize first")
}

func (SandboxListBlueprint) Materialize(opts ToolOpts) (Tool, error) {
	if opts.Sandboxes == nil {
		return nil, nil
	}
	return &sandboxListTool{sandboxes: opts.Sandboxes}, nil
}

type sandboxListTool struct {
	SandboxListBlueprint
	sandboxes SandboxManager
}

func (t *sandboxListTool) Execute(ctx context.Context, _ ToolInput) (ToolResult, error) {
	if err := ctx.Err(); err != nil {
		return ToolResult{}, err
	}
	infos, err := t.sandboxes.List(ctx)
	if err != nil {
		return ToolResult{}, fmt.Errorf("sandbox_list: %w", err)
	}
	if len(infos) == 0 {
		return NewTextResult("no sandboxes"), nil
	}
	return NewTextResult(renderSandboxes(infos)), nil
}

// --- sandbox_remove --------------------------------------------------------

const sandboxRemoveDescription = "Remove a sandbox by name or id, stopping and deleting its " +
	"container and its executor. Named volumes are kept. A sandbox whose ttl has elapsed " +
	"is removed for you, so this is for tearing one down early. Use sandbox_list to find " +
	"the name or id."

type SandboxRemoveBlueprint struct{}

func (SandboxRemoveBlueprint) Name() string        { return "sandbox_remove" }
func (SandboxRemoveBlueprint) Description() string { return sandboxRemoveDescription }
func (SandboxRemoveBlueprint) InputSchema() Schema {
	return Schema{
		Type: "object",
		Properties: []SchemaProperty{
			{Name: "ref", Type: "string",
				Description: "Name or id of the sandbox to remove, from sandbox_list."},
		},
		Required: []string{"ref"},
	}
}

func (SandboxRemoveBlueprint) Execute(context.Context, ToolInput) (ToolResult, error) {
	panic("blueprint: call Materialize first")
}

func (SandboxRemoveBlueprint) Materialize(opts ToolOpts) (Tool, error) {
	if opts.Sandboxes == nil {
		return nil, nil
	}
	return &sandboxRemoveTool{sandboxes: opts.Sandboxes}, nil
}

type sandboxRemoveTool struct {
	SandboxRemoveBlueprint
	sandboxes SandboxManager
}

func (t *sandboxRemoveTool) Execute(ctx context.Context, in ToolInput) (ToolResult, error) {
	if err := ctx.Err(); err != nil {
		return ToolResult{}, err
	}
	var params struct {
		Ref string `json:"ref"`
	}
	if err := in.Unmarshal(&params); err != nil {
		return ToolResult{}, fmt.Errorf("sandbox_remove: invalid input: %w", err)
	}
	if params.Ref == "" {
		return ToolResult{}, errors.New("sandbox_remove: ref is required (a sandbox name or id from sandbox_list)")
	}
	if err := t.sandboxes.Remove(ctx, params.Ref); err != nil {
		return ToolResult{}, fmt.Errorf("sandbox_remove: %w", err)
	}
	return NewTextResult("removed sandbox " + params.Ref), nil
}

// --- rendering -------------------------------------------------------------

// renderSandboxes prints one line per sandbox, columns fixed so the same fact
// sits in the same place on every row. An absent value is "-", never a zero.
func renderSandboxes(infos []protocol.SandboxInfo) string {
	nameW := len("NAME")
	for _, s := range infos {
		if len(s.Name) > nameW {
			nameW = len(s.Name)
		}
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "%-*s  %-7s  %-7s  %-9s  %-20s  %s\n",
		nameW, "NAME", "STATE", "NETWORK", "CONNECTED", "EXPIRES", "ID")
	for _, s := range infos {
		name := s.Name
		if name == "" {
			name = "-"
		}
		expires := "-"
		if s.ExpiresAt != nil {
			expires = s.ExpiresAt.Format(time.RFC3339)
		}
		connected := "no"
		if s.Connected {
			connected = "yes"
		}
		fmt.Fprintf(&sb, "%-*s  %-7s  %-7s  %-9s  %-20s  %s\n",
			nameW, name, s.State, string(s.Network), connected, expires, s.ID)
	}
	return sb.String()
}
