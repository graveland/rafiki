package paths

import "os"

// This file names the environment variables rafiki owns.
//
// The pre-rename spellings (FUNDI_*, and before that PIC_*/PI_CONTROLLER_*)
// are retired: Get reads exactly the current name, nothing else. A shell
// still exporting one of those old names is silently ignored — see
// docs/MIGRATING.md for how to move a `.env`.

// The variables rafiki owns.
const (
	// Socket overrides the controller socket path for the daemon's clients and
	// for any child it spawns.
	Socket = "RAFIKI_SOCKET"

	// ChildID is set by the daemon on each child so the child knows its own id;
	// `rafikid agent` uses it as the default --ref.
	ChildID = "RAFIKI_CHILD_ID"

	// GraceHours overrides how long an exited child's record is retained.
	GraceHours = "RAFIKI_GRACE_HOURS"

	// PiBinary overrides the path to the pi binary the daemon spawns for `pi`
	// kind children. Namespaced because it is rafiki's knob, not pi's own.
	PiBinary = "RAFIKI_PI_BINARY"

	// NoAutoInstallHelpers, when non-empty, stops `rafiki create` from installing
	// or updating the bundled rafiki-helpers pi extension.
	NoAutoInstallHelpers = "RAFIKI_NO_AUTO_INSTALL_HELPERS"

	// DefaultModel supplies the model when `rafiki create` gets no --model.
	DefaultModel = "RAFIKI_DEFAULT_MODEL"

	// DefaultPreset supplies the preset name when --preset is not given.
	DefaultPreset = "RAFIKI_DEFAULT_PRESET"

	// DefaultLabels is a comma-separated "k=v,k2=v2" set of label defaults,
	// merged before any --label flags.
	DefaultLabels = "RAFIKI_DEFAULT_LABELS"

	// AttachTail bounds the scrollback rafiki-attach replays into the TUI. Set by
	// `rafiki attach --tail` for the TUI process; read on the TypeScript side.
	AttachTail = "RAFIKI_ATTACH_TAIL"

	// DB is the conversations database. One DSN: the daemon opens a single
	// pool and hands it to both the agent runtime and the proxy face, so
	// FUNDI_AGENT_DB and RAFIKI_DB were always the same database.
	DB = "RAFIKI_DB"

	// URL is the base URL of a rafiki proxy this process should route LLM
	// calls through. Client-side. Absorbs FUNDI_PROXY_URL and
	// RAFIKI_PROXY_URL, which meant the same thing to different callers.
	//
	// Empty DISABLES the mechanism: children talk to providers directly.
	URL = "RAFIKI_URL"

	// Token is the bearer token this process PRESENTS to a proxy.
	// Client-side, always. Absorbs RAFIKI_PROXY_TOKEN and the client half of
	// FUNDI_PROXY_TOKEN.
	Token = "RAFIKI_TOKEN"

	// ServeToken is one additional token rafikid's own face ACCEPTS, on top
	// of the per-boot child secret. Server-side, always. This is the server
	// half of the old FUNDI_PROXY_TOKEN, split out so neither survivor means
	// two opposite things depending on what else is set.
	ServeToken = "RAFIKI_SERVE_TOKEN"

	// ProxyKinds limits which child kinds are routed, comma-separated. Default
	// "pi,claude"; the agent kind is never listed because it reaches rafiki
	// in-process and has no proxy to point at.
	//
	// An escape hatch: it makes "is this regression the proxy?" answerable by
	// restarting the daemon rather than rebuilding it.
	ProxyKinds = "RAFIKI_PROXY_KINDS"

	// ProxyListen is the address rafikid's proxy face binds. Defaults to
	// :8035. An address, not a credential — it belongs in the service unit,
	// where the binding is visible to anyone reading the service definition.
	ProxyListen = "RAFIKI_PROXY_LISTEN"

	// Instructions is the user-global instruction file. rafiki's own config, so
	// it is NOT ~/.claude/CLAUDE.md — see this package's doc comment for why
	// rafiki does not read its configuration out of another tool's directory.
	// Point it at a Claude profile explicitly if that is what you want.
	Instructions = "RAFIKI_INSTRUCTIONS"

	// SkillsDirsEnv is an OS-path-list of skill directories ($PATH convention:
	// ":" on unix). Ordered lowest-to-highest precedence, matching
	// skills.DiscoverSkills, so a later entry overrides an earlier one on name
	// collision. Non-existent entries are skipped, not errors.
	//
	// The Env suffix breaks a collision with SkillsDirs(), the resolver in this
	// same package that reads it. It is the only one of these names that clashes.
	SkillsDirsEnv = "RAFIKI_SKILLS_DIRS"

	// MCPConfig is the path to a global .mcp.json, merged under the per-cwd one.
	MCPConfig = "RAFIKI_MCP_CONFIG"

	// LSPConfig is the path to a global lsp.json, merged under the per-cwd one.
	LSPConfig = "RAFIKI_LSP_CONFIG"

	// LSPDisable, when "1", suppresses LSP entirely — no config files are read
	// and the PATH auto-detection step is skipped. Equivalent to --no-lsp.
	LSPDisable = "RAFIKI_LSP_DISABLE"

	// ControlListen is the address the daemon's TCP control plane binds
	// (e.g. "tcp:8036"). Unset = UDS only.
	ControlListen = "RAFIKI_CONTROL_LISTEN"

	// ControlToken is the shared secret for control-plane auth.
	// Daemon checks it; clients present it as the first frame on TCP.
	ControlToken = "RAFIKI_CONTROL_TOKEN"

	// ControlTLSCert is the PEM TLS certificate path for the control
	// plane's TCP listener. Mandatory when ControlListen is set.
	ControlTLSCert = "RAFIKI_CONTROL_TLS_CERT"

	// ControlTLSKey is the PEM TLS private key path for the control
	// plane's TCP listener. Mandatory when ControlListen is set.
	ControlTLSKey = "RAFIKI_CONTROL_TLS_KEY"

	// BroadcastListen, when set, binds a dedicated HTTP listener for
	// OpenRouter's OTLP broadcast webhook. Empty = disabled.
	BroadcastListen = "RAFIKI_BROADCAST_LISTEN"

	// ControlURL is the remote rafikid URL a client dials
	// (e.g. "tls://rafiki.graveland.dev:443"). Wins over RAFIKI_SOCKET.
	ControlURL = "RAFIKI_CONTROL_URL"

	// RecordRequests, when "1", enables raw HTTP request/response capture to
	// the conversations.raw_http_request hypertable. Debug-only; off by default.
	RecordRequests = "RAFIKI_RECORD_REQUESTS"

	// BashRTK controls whether the fundi bash tool routes commands through
	// rtk for output compression: "auto" (default, use it when installed),
	// "on" (require it), or "off".
	BashRTK = "RAFIKI_BASH_RTK"

	// ToolsWeb, when "1", enables the fundi webfetch and websearch tools.
	// Default off: fundi runs unattended and may have no egress.
	ToolsWeb = "RAFIKI_TOOLS_WEB"

	// BraveAPIKey selects the Brave Search API for websearch. Empty falls
	// back to the keyless DuckDuckGo scraper, so the tool works with no
	// setup — but a scrape can break on a layout change or be served a
	// bot-detection page, which is indistinguishable from "no results" to
	// a model. Set this when search reliability matters.
	BraveAPIKey = "RAFIKI_BRAVE_API_KEY"

	// MaxDepth caps a child's ABSOLUTE position in the spawn tree, regardless
	// of what any parent granted. Default 3; "0" disables spawning entirely.
	MaxDepth = "RAFIKI_MAX_DEPTH"

	// ExecutorSocket is the default unix socket of a rafiki-executor to run
	// agent children's filesystem and shell tools in. Used by `rafiki create
	// --executor-socket` when the flag is not given.
	ExecutorSocket = "RAFIKI_EXECUTOR_SOCKET"
)

// Variables consumed only by the TypeScript side (rafiki-attach and the
// rafiki-helpers pi extension) are named here so this file stays the one
// inventory of what rafiki reads from the environment, even though Go does
// not read them.
//
//	RAFIKI_ATTACH_TUI       set by rafiki-attach, read by rafiki-helpers
//	RAFIKI_ATTACH_CHILD_ID  set by rafiki-attach, read by rafiki-helpers
//	RAFIKI_ATTACH_DEBUG     user-set, read by rafiki-attach
//	RAFIKI_KILL_ON_EXIT     user-set, read by rafiki-attach

// Get returns the value of name. The deprecation fallback this used to carry
// is gone: the FUNDI_* and PIC_* spellings are retired, and a chain three
// renames deep is exactly the drift the rafiki consolidation removed.
func Get(name string) string {
	return os.Getenv(name)
}
