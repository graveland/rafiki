// Package paths resolves the on-disk locations rafiki owns, following the XDG
// Base Directory specification.
//
// These deliberately do NOT live under ~/.pi. That directory belongs to pi
// itself, whose config rafiki reads but does not own, and a daemon that writes
// its own runtime state into another program's config directory makes both
// harder to reason about, back up, and remove.
//
// (Historically this also prevented a socket collision with pi-controller,
// which rafiki replaced. That collision is gone; the separation is still
// correct for the reason above.)
//
// Genuine pi integration points (~/.pi/agent/models.json, pi's extensions
// directory) are pi's contract and stay where they are — they are not resolved
// here.
package paths

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// appName is the per-application leaf every base directory gets.
const appName = "rafiki"

// homeDirWarnOnce guards the warning below against spam. base() is called on
// every path resolution — once per incoming request in a long-lived rafikid —
// so logging unconditionally would flood the log with the same fact on every
// call. A single fired warning already says "every path from here on is
// wrong, relative to the wrong directory": os.UserHomeDir() only reads $HOME
// (no getpwuid fallback on Unix), and nothing in this process calls
// os.Setenv("HOME", ...), so the failure is a static property of how the
// process was launched, not a transient condition that could later clear or
// recur differently. If that assumption ever stops holding, this needs a
// rate-limited warn instead of a one-shot.
var homeDirWarnOnce sync.Once

// base returns $envVar/rafiki when envVar is set, else $HOME/<fallback>/rafiki.
// The spec says a relative or empty value must be ignored in favour of the
// default, which is why only an absolute path is honoured.
func base(envVar string, fallback ...string) string {
	if v := os.Getenv(envVar); filepath.IsAbs(v) {
		return filepath.Join(v, appName)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		// Nothing sensible to fall back to; a relative path at least keeps the
		// process running rather than panicking at init. But a relative path
		// resolves against whatever the current working directory happens to
		// be at each call site — the daemon's cwd for SocketPath(), a child's
		// project cwd for InstructionsFile() — silently landing state in the
		// wrong place. That must not pass without a trace in the log.
		homeDirWarnOnce.Do(func() {
			slog.Warn("cannot determine home directory; falling back to a path relative to the current working directory",
				"error", err, "envVar", envVar)
		})
		return filepath.Join(append(fallback, appName)...)
	}
	return filepath.Join(append([]string{home}, append(fallback, appName)...)...)
}

// ConfigDir is user configuration: $XDG_CONFIG_HOME/rafiki, else ~/.config/rafiki.
func ConfigDir() string { return base("XDG_CONFIG_HOME", ".config") }

// DataDir is persistent application data: $XDG_DATA_HOME/rafiki, else
// ~/.local/share/rafiki. Session records live here — they must survive a reboot.
func DataDir() string { return base("XDG_DATA_HOME", ".local", "share") }

// StateDir is state that persists but is not precious: $XDG_STATE_HOME/rafiki,
// else ~/.local/state/rafiki. Logs live here.
func StateDir() string { return base("XDG_STATE_HOME", ".local", "state") }

// RuntimeDir is for sockets and other runtime files: $XDG_RUNTIME_DIR/rafiki.
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

// ExecutorSocketPath is where the daemon accepts executor connections from the
// same machine.
//
// A second socket rather than a second protocol on the control socket: the
// control socket speaks raw framed JSON straight into handleConn with no HTTP
// at all, and the executor link is an HTTP/1.1 Upgrade. Distinguishing them on
// one socket means sniffing the first bytes, which is precisely the
// demultiplexer the executor design rejected — and the cost of a second socket
// in the same directory is one file.
//
// It lives beside the control socket in RuntimeDir, so a machine with no
// XDG_RUNTIME_DIR gets both under StateDir together rather than split across
// two lifetimes.
func ExecutorSocketPath() string { return filepath.Join(RuntimeDir(), "executor.sock") }

// ConnectSocketPath is where the daemon serves the Connect control plane to
// local clients.
//
// A third socket, for the same reason ExecutorSocketPath is a second one: the
// control socket speaks raw framed JSON straight into handleConn with no HTTP
// at all, and the Connect plane is HTTP/2. Serving both on one socket means
// sniffing the first bytes, which is the demultiplexer this repo has already
// rejected twice. The cost of another socket in the same directory is one file.
//
// It carries no token: like the control socket, the trust boundary is the 0600
// socket inside the 0700 directory.
func ConnectSocketPath() string { return filepath.Join(RuntimeDir(), "connect.sock") }

// RecordsDir holds persisted session records.
func RecordsDir() string { return filepath.Join(DataDir(), "state") }

// LogsDir holds per-child log files.
func LogsDir() string { return filepath.Join(StateDir(), "logs") }

// ServiceLogPath is where the launchd/systemd unit sends the daemon's own
// stdout and stderr.
func ServiceLogPath() string { return filepath.Join(StateDir(), "controller.log") }

// ExecutorServiceLogPath is where the EXECUTOR's service unit sends its output.
// Separate from the daemon's: a machine can run both, and interleaving two
// services into one file makes each one's log harder to read than either would
// be alone.
func ExecutorServiceLogPath() string { return filepath.Join(StateDir(), "executor.log") }

// ActiveFile records the child id `fundi` treats as the current one. Runtime
// state, so it sits beside the socket rather than with the persisted records.
func ActiveFile() string { return filepath.Join(RuntimeDir(), "active") }

// CacheDir is disposable, regenerable data: $XDG_CACHE_HOME/rafiki, else
// ~/.cache/rafiki.
func CacheDir() string { return base("XDG_CACHE_HOME", ".cache") }

// SpillDir is where a standalone `rafikid fundi` writes clipped tool output. Cache
// rather than data: it is large, disposable, and reconstructible from the
// conversation. No "fundi-" prefix on the leaf — the directory is already
// namespaced, unlike the os.TempDir() location this replaced, where the prefix
// was the only thing keeping it out of another tool's way.
//
// A daemon-spawned child does not use this: the controller pins --spill-dir
// under its own state directory so Forget can find it deterministically.
func SpillDir(ref string) string { return filepath.Join(CacheDir(), "spill", ref) }

// InstructionsFile is the user-global instruction file: $RAFIKI_INSTRUCTIONS,
// else <ConfigDir>/instructions.md. Deliberately not ~/.claude/CLAUDE.md —
// that directory belongs to Claude Code, and rafiki reads its own configuration
// from its own directory. Point the variable at a Claude profile to use one.
func InstructionsFile() string {
	if v := Get(Instructions); v != "" {
		return v
	}
	return filepath.Join(ConfigDir(), "instructions.md")
}

// ProvidersFile is the LLM provider registry: $RAFIKI_PROVIDERS, else
// <ConfigDir>/providers.toml. Its contents are by definition the contents of
// the future rafikid.toml [llm] section, so the eventual merge is a lift with
// no key renamed.
func ProvidersFile() string {
	if v := Get(Providers); v != "" {
		return v
	}
	return filepath.Join(ConfigDir(), "providers.toml")
}

// SkillsDirs is the ordered skill search path: $RAFIKI_SKILLS_DIRS split on the
// OS path-list separator, else [<ConfigDir>/skills]. Order is
// lowest-to-highest precedence, matching skills.DiscoverSkills. Empty segments
// are dropped so a leading, trailing, or doubled separator is not read as the
// current directory.
//
// Deliberately not ~/.claude/skills — that directory belongs to Claude Code,
// and rafiki reads its own configuration from its own directory. Point
// $RAFIKI_SKILLS_DIRS at a Claude skills tree (or a plugin cache) to use one.
// This only covers the user-global dir: a per-project <cwd>/.claude/skills is
// still searched by cmd/rafikid/agent.go's assembleSkillDirs, alongside
// rafiki's own <cwd>/.rafiki/skills, so existing per-repo skills keep working.
func SkillsDirs() []string {
	v := Get(SkillsDirsEnv)
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
// ~/.pi/agent/rafiki-presets.json — rafiki's own file inside pi's directory.
func PresetsFile() string { return filepath.Join(ConfigDir(), "presets.json") }

// GlobalMCPConfig is the machine-wide .mcp.json: $RAFIKI_MCP_CONFIG, else
// <ConfigDir>/mcp.json. The per-cwd .mcp.json remains the primary source and
// takes precedence; this is the fallback for servers you want everywhere.
func GlobalMCPConfig() string {
	if v := Get(MCPConfig); v != "" {
		return v
	}
	return filepath.Join(ConfigDir(), "mcp.json")
}

// GlobalLSPConfig is the machine-wide lsp.json: $RAFIKI_LSP_CONFIG, else
// <ConfigDir>/lsp.json. The per-cwd .lsp.json remains the primary source and
// takes precedence; this is the fallback for servers you want everywhere.
func GlobalLSPConfig() string {
	if v := Get(LSPConfig); v != "" {
		return v
	}
	return filepath.Join(ConfigDir(), "lsp.json")
}

// TokenFile is the client's bearer token: <ConfigDir>/token, mode 0600.
// Written by `rafiki user create`, re-read on every dial so `user rm` +
// `user create` rotation works without a restart.
func TokenFile() string { return filepath.Join(ConfigDir(), "token") }

// TokenFromEnv returns the client's token: RAFIKI_TOKEN, else the trimmed
// contents of TokenFile(), else "". One credential for both surfaces — the
// control plane's ctrl_auth frame and the face's Authorization header.
func TokenFromEnv() string {
	if t := os.Getenv(Token); t != "" {
		return t
	}
	b, err := os.ReadFile(TokenFile())
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}
