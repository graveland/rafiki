package paths

import (
	"os"
	"strings"
)

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

	// URL is the single dial target for both surfaces a client reaches: an
	// LLM proxy this process routes calls through, and (when the scheme is
	// https) the remote daemon's control plane. Client-side. One name now
	// covers what a separate, retired control-URL variable used to carry —
	// both always meant "where do I send this", just to different callers.
	//
	// Empty DISABLES the proxy mechanism: children talk to providers
	// directly. client.IsRemoteURL decides whether a given value also
	// names a control-plane host (https:// only — an http:// value is the
	// local loopback face, which has no control listener).
	URL = "RAFIKI_URL"

	// Token is the bearer token this process PRESENTS — to a proxy's
	// Authorization header, and to a remote daemon's ctrl_auth frame. One
	// name now covers what a separate, retired control-token variable used
	// to carry. Client-side, always. See paths.TokenFromEnv, which also
	// falls back to paths.TokenFile().
	Token = "RAFIKI_TOKEN"

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

	// Providers overrides the providers.toml location. The RAFIKI_* names in
	// this file are duplicated by hand in attach/src/client.ts and
	// cmd/rafiki/helpersembed/rafiki-helpers/index.ts — nothing enforces
	// agreement, so grep for a name before changing it.
	Providers = "RAFIKI_PROVIDERS"

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

	// ProviderGuard, when "off"/"false"/"0"/"no", disables the provider cache
	// guard: OpenRouter providers that stop serving prompt-cache hits are no
	// longer ejected from routing. Any other value (including unset) leaves it
	// on — see routing.ProviderGuard.
	ProviderGuard = "RAFIKI_PROVIDER_GUARD"

	// ControlListen is the address the daemon's TCP control plane binds
	// (e.g. "tcp:8036"). Unset = UDS only.
	ControlListen = "RAFIKI_CONTROL_LISTEN"

	// ControlTLSCert is the PEM TLS certificate path for the control
	// plane's TCP listener. Mandatory when ControlListen is set.
	ControlTLSCert = "RAFIKI_CONTROL_TLS_CERT"

	// ControlTLSKey is the PEM TLS private key path for the control
	// plane's TCP listener. Mandatory when ControlListen is set.
	ControlTLSKey = "RAFIKI_CONTROL_TLS_KEY"

	// BroadcastListen, when set, binds a dedicated HTTP listener for
	// OpenRouter's OTLP broadcast webhook. Empty = disabled.
	BroadcastListen = "RAFIKI_BROADCAST_LISTEN"

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

	// ExecutorSelector is the default label selector for picking an executor
	// from the daemon's enrolled pool. Used by `rafiki create
	// --executor-selector` when the flag is not given.
	//
	// It exists because an interactive operator has one answer to "which
	// executor" for every session — their own machine — and typing it on every
	// spawn is the kind of friction that ends with nobody using executors at
	// all.
	ExecutorSelector = "RAFIKI_EXECUTOR_SELECTOR"

	// ExecutorName names the machine this box's executor represents. It is the
	// operator's own string, not a derived id: it is written into the executor
	// row as the `machine` trust label at mint time, and read back here by an
	// interactive client asking "is there a durable executor that shares my
	// filesystem?". The env var exists for containers and pods, where the
	// natural source is the downward API and the data dir is not durable.
	ExecutorName = "RAFIKI_EXECUTOR_NAME"

	// DaemonID identifies this daemon across restarts. It is what a
	// conversation lease records as its holder, and it is what lets a
	// restarted daemon reclaim its own leases without waiting out the TTL.
	//
	// It MUST be unique per running daemon. Two daemons sharing one value
	// reclaim each other's leases on every acquire, which reproduces exactly
	// the split-brain the lease exists to prevent.
	DaemonIDVar = "RAFIKI_DAEMON_ID"

	// ExecutorsEnabled turns on the executor endpoint. "0" or "false" refuses
	// executors outright; any other non-empty value forces them on; unset
	// takes the default, which depends on RAFIKI_CONTROL_LISTEN (see
	// cmd/rafikid/main.go's executorsEnabled).
	//
	// It is a flag rather than an address because the endpoint no longer has one
	// of its own: executors are reached at a PATH on the control listener
	// (RAFIKI_CONTROL_LISTEN), upgraded out of HTTP/1.1, so one port and one
	// certificate serve both. With no control listener, the only executor path
	// is the local UDS socket — the same trust boundary as the daemon's own
	// control socket — so an unset value there defaults ON. Once
	// RAFIKI_CONTROL_LISTEN is set, a remote machine could reach that same
	// path, so an unset value there defaults OFF: turning on a TCP control
	// plane must not silently also open executor enrollment to it.
	ExecutorsEnabled = "RAFIKI_EXECUTORS_ENABLED"
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

// IsReservedEnvKey reports whether an environment variable belongs to rafiki
// itself and must not be handed to a process outside the daemon's trust.
//
// Two callers, different reasons, same answer:
//
//   - collectCallerEnv (cmd/rafiki) strips these from a CALLER's environment
//     before they reach a spawned child, so a stale export cannot override what
//     the daemon injects per-child.
//   - mcpServerEnv (pkg/fundi/tools) strips them from the DAEMON's environment
//     before exec'ing a third-party MCP server, which would otherwise receive
//     RAFIKI_DB — a connection string with credentials — plus RAFIKI_TOKEN and
//     both provider API keys.
//
// Three prefixes: the current RAFIKI_ spelling and the two retired ones. Get no
// longer falls back to either, so this is not load-bearing for resolution, but a
// stale export under an old name has no business travelling either.
//
// It is a BLACKLIST of what rafiki owns, deliberately, and NOT a general secret
// filter. A daemon environment holding AWS_SECRET_ACCESS_KEY or GITHUB_TOKEN
// still passes those on. An allowlist would break every MCP server needing PATH,
// HOME or a proxy variable, and deciding which of an operator's own variables
// are secret is not something this can do correctly.
func IsReservedEnvKey(k string) bool {
	if strings.HasPrefix(k, "RAFIKI_") ||
		strings.HasPrefix(k, "FUNDI_") ||
		strings.HasPrefix(k, "PI_CONTROLLER_") {
		return true
	}
	// The daemon owns its own provider credentials.
	return k == "ANTHROPIC_API_KEY" || k == "OPENROUTER_API_KEY"
}
