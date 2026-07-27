// Package envvar names the environment variables fundi owns, and reads them with
// a deprecation fallback to their pi-controller-era spellings.
//
// The old names were inherited from the fork and misnamed the product. They are
// still honoured — a shell that exports PI_CONTROLLER_SOCKET today keeps working
// — but the new name wins when both are set. Nothing here is pi's contract:
// fundi both sets and reads all four, so renaming them breaks no pi process.
package envvar

import (
	"log/slog"
	"os"
)

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
