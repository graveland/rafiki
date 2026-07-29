// Package paths resolves the on-disk locations fundi owns, following the XDG
// Base Directory specification.
//
// These deliberately do NOT live under ~/.pi. That directory belongs to pi
// itself, and while fundi is a pi-controller successor it is a distinct daemon:
// sharing ~/.pi/run meant both processes claimed the same controller socket, so
// running fundi while pi-controller was up failed with "socket in use by a live
// process". Genuine pi integration points (~/.pi/agent/models.json, pi's
// extensions directory) are pi's contract and stay where they are — they are not
// resolved here.
package paths

import (
	"os"
	"path/filepath"
	"strings"

	"git.graveland.dev/brent/fundi/internal/envvar"
)

// appName is the per-application leaf every base directory gets.
const appName = "fundi"

// base returns $envVar/fundi when envVar is set, else $HOME/<fallback>/fundi.
// The spec says a relative or empty value must be ignored in favour of the
// default, which is why only an absolute path is honoured.
func base(envVar string, fallback ...string) string {
	if v := os.Getenv(envVar); filepath.IsAbs(v) {
		return filepath.Join(v, appName)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		// Nothing sensible to fall back to; a relative path at least keeps the
		// process running rather than panicking at init.
		return filepath.Join(append(fallback, appName)...)
	}
	return filepath.Join(append([]string{home}, append(fallback, appName)...)...)
}

// ConfigDir is user configuration: $XDG_CONFIG_HOME/fundi, else ~/.config/fundi.
func ConfigDir() string { return base("XDG_CONFIG_HOME", ".config") }

// DataDir is persistent application data: $XDG_DATA_HOME/fundi, else
// ~/.local/share/fundi. Session records live here — they must survive a reboot.
func DataDir() string { return base("XDG_DATA_HOME", ".local", "share") }

// StateDir is state that persists but is not precious: $XDG_STATE_HOME/fundi,
// else ~/.local/state/fundi. Logs live here.
func StateDir() string { return base("XDG_STATE_HOME", ".local", "state") }

// RuntimeDir is for sockets and other runtime files: $XDG_RUNTIME_DIR/fundi.
// That variable is normally unset on macOS (it is a Linux/systemd convention),
// so it falls back to StateDir rather than to a path outside the spec — keeping
// the socket short enough for sun_path either way.
func RuntimeDir() string {
	if v := os.Getenv("XDG_RUNTIME_DIR"); filepath.IsAbs(v) {
		return filepath.Join(v, appName)
	}
	return StateDir()
}

// SocketPath is the controller's unix socket.
func SocketPath() string { return filepath.Join(RuntimeDir(), "controller.sock") }

// RecordsDir holds persisted session records.
func RecordsDir() string { return filepath.Join(DataDir(), "state") }

// LogsDir holds per-child log files.
func LogsDir() string { return filepath.Join(StateDir(), "logs") }

// ServiceLogPath is where the launchd/systemd unit sends the daemon's own
// stdout and stderr.
func ServiceLogPath() string { return filepath.Join(StateDir(), "controller.log") }

// ActiveFile records the child id `fundi` treats as the current one. Runtime
// state, so it sits beside the socket rather than with the persisted records.
func ActiveFile() string { return filepath.Join(RuntimeDir(), "active") }

// CacheDir is disposable, regenerable data: $XDG_CACHE_HOME/fundi, else
// ~/.cache/fundi.
func CacheDir() string { return base("XDG_CACHE_HOME", ".cache") }

// SpillDir is where a standalone `fundid agent` writes clipped tool output. Cache
// rather than data: it is large, disposable, and reconstructible from the
// conversation. No "fundi-" prefix on the leaf — the directory is already
// namespaced, unlike the os.TempDir() location this replaced, where the prefix
// was the only thing keeping it out of another tool's way.
//
// A daemon-spawned child does not use this: the controller pins --spill-dir
// under its own state directory so Forget can find it deterministically.
func SpillDir(ref string) string { return filepath.Join(CacheDir(), "spill", ref) }

// InstructionsFile is the user-global instruction file: $FUNDI_INSTRUCTIONS,
// else <ConfigDir>/instructions.md. Deliberately not ~/.claude/CLAUDE.md —
// that directory belongs to Claude Code, and fundi reads its own configuration
// from its own directory. Point the variable at a Claude profile to use one.
func InstructionsFile() string {
	if v := envvar.Get(envvar.Instructions); v != "" {
		return v
	}
	return filepath.Join(ConfigDir(), "instructions.md")
}

// SkillsDirs is the ordered skill search path: $FUNDI_SKILLS_DIRS split on the
// OS path-list separator, else [<ConfigDir>/skills]. Order is
// lowest-to-highest precedence, matching agent.DiscoverSkills. Empty segments
// are dropped so a leading, trailing, or doubled separator is not read as the
// current directory.
func SkillsDirs() []string {
	v := envvar.Get(envvar.SkillsDirs)
	if v == "" {
		return []string{filepath.Join(ConfigDir(), "skills")}
	}
	var out []string
	for _, d := range strings.Split(v, string(os.PathListSeparator)) {
		if d != "" {
			out = append(out, d)
		}
	}
	if len(out) == 0 {
		return []string{filepath.Join(ConfigDir(), "skills")}
	}
	return out
}

// PresetsFile is the presets file: <ConfigDir>/presets.json. It used to live at
// ~/.pi/agent/fundi-presets.json — fundi's own file inside pi's directory.
func PresetsFile() string { return filepath.Join(ConfigDir(), "presets.json") }

// GlobalMCPConfig is the machine-wide .mcp.json: $FUNDI_MCP_CONFIG, else
// <ConfigDir>/mcp.json. The per-cwd .mcp.json remains the primary source and
// takes precedence; this is the fallback for servers you want everywhere.
func GlobalMCPConfig() string {
	if v := envvar.Get(envvar.MCPConfig); v != "" {
		return v
	}
	return filepath.Join(ConfigDir(), "mcp.json")
}
