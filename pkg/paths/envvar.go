package paths

import (
	"log/slog"
	"os"
)

// This file names the environment variables fundi owns, and reads them with a
// deprecation fallback to their pi-controller-era spellings.
//
// The old names were inherited from the fork and misnamed the product. They are
// still honoured — a shell that exports PI_CONTROLLER_SOCKET today keeps working
// — but the new name wins when both are set. Nothing here is pi's contract:
// fundi both sets and reads all four, so renaming them breaks no pi process.

// The variables fundi owns, and the pre-rename spelling each still accepts.
const (
	// Socket overrides the controller socket path for the daemon's clients and
	// for any child it spawns.
	Socket    = "FUNDI_SOCKET"
	socketOld = "PI_CONTROLLER_SOCKET"

	// ChildID is set by the daemon on each child so the child knows its own id;
	// `fundid agent` uses it as the default --ref.
	ChildID    = "FUNDI_CHILD_ID"
	childIDOld = "PI_CONTROLLER_CHILD_ID"

	// GraceHours overrides how long an exited child's record is retained.
	GraceHours    = "FUNDI_GRACE_HOURS"
	graceHoursOld = "PI_CONTROLLER_GRACE_HOURS"

	// PiBinary overrides the path to the pi binary the daemon spawns for `pi`
	// kind children. Namespaced because it is fundi's knob, not pi's own.
	PiBinary    = "FUNDI_PI_BINARY"
	piBinaryOld = "PI_BINARY"

	// NoAutoInstallHelpers, when non-empty, stops `fundi create` from installing
	// or updating the bundled fundi-helpers pi extension.
	NoAutoInstallHelpers    = "FUNDI_NO_AUTO_INSTALL_HELPERS"
	noAutoInstallHelpersOld = "PIC_NO_AUTO_INSTALL_HELPERS"

	// DefaultModel supplies the model when `fundi create` gets no --model.
	DefaultModel    = "FUNDI_DEFAULT_MODEL"
	defaultModelOld = "PIC_DEFAULT_MODEL"

	// DefaultPreset supplies the preset name when --preset is not given.
	DefaultPreset    = "FUNDI_DEFAULT_PRESET"
	defaultPresetOld = "PIC_DEFAULT_PRESET"

	// DefaultLabels is a comma-separated "k=v,k2=v2" set of label defaults,
	// merged before any --label flags.
	DefaultLabels    = "FUNDI_DEFAULT_LABELS"
	defaultLabelsOld = "PIC_DEFAULT_LABELS"

	// AttachTail bounds the scrollback fundi-attach replays into the TUI. Set by
	// `fundi attach --tail` for the TUI process; read on the TypeScript side.
	AttachTail    = "FUNDI_ATTACH_TAIL"
	attachTailOld = "PIC_ATTACH_TAIL"

	// AgentDB was always fundi-named; listed here so every owned variable has
	// one home.
	AgentDB = "FUNDI_AGENT_DB"

	// ProxyURL points pi and claude children at a rafiki proxy, so their turns
	// are captured and their models resolved by the same path the agent kind
	// already uses in-process.
	//
	// Empty DISABLES the mechanism entirely: children talk to providers
	// directly, exactly as before. That default matters — enabling this cannot
	// break an install that has not opted in.
	ProxyURL = "FUNDI_PROXY_URL"

	// ProxyToken authenticates to the proxy. claude sends it as a bearer, pi as
	// X-Api-Key; rafiki's face accepts either.
	//
	// A credential, so it belongs in the environment file rather than the
	// service unit (see ServiceEnvFile), and is deliberately absent from
	// cmd/fundi's daemonEnvVars for that reason.
	ProxyToken = "FUNDI_PROXY_TOKEN"

	// ProxyKinds limits which child kinds are routed, comma-separated. Default
	// "pi,claude"; the agent kind is never listed because it reaches rafiki
	// in-process and has no proxy to point at.
	//
	// An escape hatch: it makes "is this regression the proxy?" answerable by
	// restarting the daemon rather than rebuilding it.
	ProxyKinds = "FUNDI_PROXY_KINDS"

	// Instructions is the user-global instruction file. fundi's own config, so
	// it is NOT ~/.claude/CLAUDE.md — see this package's doc comment for why
	// fundi does not read its configuration out of another tool's directory.
	// Point it at a Claude profile explicitly if that is what you want.
	Instructions = "FUNDI_INSTRUCTIONS"

	// SkillsDirsEnv is an OS-path-list of skill directories ($PATH convention:
	// ":" on unix). Ordered lowest-to-highest precedence, matching
	// skills.DiscoverSkills, so a later entry overrides an earlier one on name
	// collision. Non-existent entries are skipped, not errors.
	//
	// The Env suffix breaks a collision with SkillsDirs(), the resolver in this
	// same package that reads it. It is the only one of these names that clashes.
	SkillsDirsEnv = "FUNDI_SKILLS_DIRS"

	// MCPConfig is the path to a global .mcp.json, merged under the per-cwd one.
	MCPConfig = "FUNDI_MCP_CONFIG"
)

// Variables consumed only by the TypeScript side (fundi-attach and the
// fundi-helpers pi extension) are named here so this file stays the one
// inventory of what fundi reads from the environment, even though Go does not
// read them. Their TS readers accept the old spelling the same way Get does.
//
//	FUNDI_ATTACH_TUI       (was PIC_ATTACH_TUI)       set by fundi-attach, read by fundi-helpers
//	FUNDI_ATTACH_CHILD_ID  (was PIC_ATTACH_CHILD_ID)  set by fundi-attach, read by fundi-helpers
//	FUNDI_ATTACH_DEBUG     (was PIC_ATTACH_DEBUG)     user-set, read by fundi-attach
//	FUNDI_KILL_ON_EXIT     (was PIC_KILL_ON_EXIT)     user-set, read by fundi-attach

// deprecated maps each current name to the spelling it replaced.
var deprecated = map[string]string{
	Socket:               socketOld,
	ChildID:              childIDOld,
	GraceHours:           graceHoursOld,
	PiBinary:             piBinaryOld,
	NoAutoInstallHelpers: noAutoInstallHelpersOld,
	DefaultModel:         defaultModelOld,
	DefaultPreset:        defaultPresetOld,
	DefaultLabels:        defaultLabelsOld,
	AttachTail:           attachTailOld,
}

// Get returns the value of name, falling back to its deprecated spelling. A
// value found only under the old name is logged once per read at warn level —
// silently accepting it would let a stale export outlive every rename.
func Get(name string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	old, ok := deprecated[name]
	if !ok {
		return ""
	}
	v := os.Getenv(old)
	if v != "" {
		slog.Warn("using deprecated environment variable; rename it",
			"deprecated", old, "use", name)
	}
	return v
}
