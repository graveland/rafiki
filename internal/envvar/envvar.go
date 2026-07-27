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

	// AgentDB was always fundi-named; listed here so every owned variable has
	// one home.
	AgentDB = "FUNDI_AGENT_DB"
)

// deprecated maps each current name to the spelling it replaced.
var deprecated = map[string]string{
	Socket:     socketOld,
	ChildID:    childIDOld,
	GraceHours: graceHoursOld,
	PiBinary:   piBinaryOld,
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
