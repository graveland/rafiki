# Configuration

Where rafiki keeps its files, how a client picks a daemon, and the environment variables the daemon reads. [`.env.example`](../../.env.example) documents every variable in full.

## Paths

rafiki follows the XDG base directories:

| | Default | Override |
|---|---|---|
| socket | `~/.local/state/rafiki/controller.sock` | `$XDG_RUNTIME_DIR` |
| records | `~/.local/share/rafiki/state` | `$XDG_DATA_HOME` |
| logs | `~/.local/state/rafiki/logs` | `$XDG_STATE_HOME` |
| config | `~/.config/rafiki` (instructions, skills, `mcp.json`, `lsp.json`, `profiles.toml`, `rafikid.toml`) | `$XDG_CONFIG_HOME` |

This is where the **daemon** binds and reads from — a client reaches a
different daemon by naming it in a profile, not by overriding these paths
(see [Profiles](#profiles)).

Project instructions (`CLAUDE.md`/`AGENTS.md`) belong to the *workspace*, so
when the workspace lives on an executor the daemon fetches them over the
`ProjectContext` RPC rather than reading a local path. The user-global
instructions file (`$RAFIKI_INSTRUCTIONS`) belongs to whoever runs the agent
loop, so the daemon always reads that from its own disk.

launchd/systemd service identity: `dev.graveland.rafiki` / `rafiki`.

## Profiles

The `rafiki` client resolves exactly one **profile** per invocation — a name
bound to the daemon it should talk to and the credential that daemon needs.
There is no client-side `--socket`/`RAFIKI_URL` any more; only profiles name
a daemon.

```toml
# ~/.config/rafiki/profiles.toml
[profile.work]
socket = "/run/user/1000/rafiki/controller.sock"  # a local daemon...
# url    = "https://rafiki.example.net"           # ...or a remote one (needs a token)
proxy  = "http://localhost:8035"                  # rafiki claude's proxy face
kind   = "fundi"                                   # rafiki create's defaults
model  = "anthropic/claude-sonnet-4-5"
preset = "quick"
labels = { team = "infra" }
```

Exactly one of `socket`/`url` is required. Each profile owns its own token
file (`~/.config/rafiki/profiles/<name>/token`, 0600), and each daemon its own
presets, so two daemons' model universes and credentials need not overlap. A
profile's `preset` names one of that daemon's presets (`rafiki preset list`).
On a local profile the token is optional but not decorative: it resolves the
connection's identity (the daemon's optional-credential rule on the local
socket), so owner-scoped state — a preset saved with `rafiki preset put` —
lands in the right bucket and is visible to `rafiki create --preset` on the
same profile.

**Resolution order:** `-P`/`--profile` for one command → `$RAFIKI_PROFILE`
for one shell → the `current-profile` pointer (`rafiki profile use <name>`)
→ bootstrap (a machine with no `profiles.toml` gets a `default` profile
pointing at the local XDG socket, written automatically on first use).

```bash
rafiki profile add work --socket ~/.local/state/rafiki/controller.sock --proxy http://localhost:8035
rafiki profile add prod --url https://rafiki.example.net --token "$TOKEN"
rafiki profile use work
rafiki -P prod status              # override for one command
```

`rafiki profile show -o json` (or `-j`) prints the resolved profile as one
machine-readable record — name, socket, url, connect_socket, token, kind,
model, preset, labels. Its `token` field carries the RESOLVED token value
(the human rendering deliberately shows only whether a token exists), which
is what machine consumers like [the Python SDK's](../../sdk/python/README.md)
`Client.from_profile` build their connection from. `connect_socket` equals
`socket` — the one control socket — and the key is kept because rafiki-py
reads it.

`RAFIKI_URL`, `RAFIKI_TOKEN`, `RAFIKI_SOCKET`, `RAFIKI_DEFAULT_MODEL`,
`RAFIKI_DEFAULT_PRESET`, `RAFIKI_DEFAULT_LABELS` are hard client-side errors
now (naming the offending variable) rather than silently-ignored or
silently-outranking globals. They keep their old meaning for `rafikid`
itself, and for the two headless, profile-exempt commands `rafiki executor
serve` / `rafiki executor service install`.

## MCP servers

`mcp.json` (the standard `.mcp.json` shape, read from `$RAFIKI_MCP_CONFIG`, then
`<cwd>/.mcp.json`, then `<config dir>/mcp.json`) names the MCP servers a fundi
child connects to. A server is either local to the daemon:

```json
{ "mcpServers": { "local": { "command": "hesabu", "args": ["mcp"], "env": {"TOKEN": "..."} } } }
```

…or it lives on an **executor**, for a server only that machine can reach (see
[executors](executors.md#mcp-servers-on-an-executor)):

```json
{
  "mcpServers": {
    "hesabu":   { "via_executor": { "selector": "name=greyshift", "command": "hesabu" } },
    "codescan": { "via_executor": { "selector": "name=greyshift", "proxy":   "codescan" } }
  }
}
```

A `via_executor` entry is validated when the file is LOADED, and refused by
server name when it:

- also carries its own top-level `command`/`args`/`env`/`url` — the argv and the
  environment live on the executor, which is the machine holding the
  credentials;
- sets both `command` and `proxy`, or neither;
- has an empty `selector`.

Each server is then resolved against the live executors ONCE, at spawn: a
selector that matches nothing, or a machine that does not declare the name, is a
spawn error rather than a child that quietly has no tools. A WELL-FORMED
`via_executor` server a child's `--mcp-servers` allowlist excludes is skipped and
never refuses its spawn — the load-time refusals above are a property of the
file, not of this child, so a malformed entry refuses every child that reads it
whatever their allowlist says.

## Environment

rafiki's variables are `RAFIKI_`-prefixed; `pkg/paths.Get` reads exactly the
current name (older `FUNDI_*`/`PIC_*` spellings are retired and silently
ignored). `.env.example` documents each one in full.

| | |
|---|---|
| `RAFIKI_PROFILE` | client-side: selects a profile for one shell (see [Profiles](#profiles)) |
| `RAFIKI_INSTRUCTIONS` | user-global instruction file (default `~/.config/rafiki/instructions.md`) |
| `RAFIKI_SKILLS_DIRS` | on-disk skill directories, path-list separated (opt-in — default is the daemon's database store). May be symlinks |
| `RAFIKI_MCP_CONFIG` | global `.mcp.json` (default `~/.config/rafiki/mcp.json`) |
| `RAFIKI_LSP_CONFIG` | global `lsp.json` for language server config (default `~/.config/rafiki/lsp.json`) |
| `RAFIKI_CONFIG` | daemon-side: path to `rafikid.toml` (default `~/.config/rafiki/rafikid.toml`). Its `[telemetry]` section configures OTLP export; `OTEL_EXPORTER_OTLP_*` env vars override it |
| `RAFIKI_PROXY_LISTEN` | bind address for the proxy face (default `:8035`) |
| `RAFIKI_DB` | postgres URL for conversation persistence; **required** — a DB-less daemon has no history, cost accounting, task ledger, user identity, executor plane or conversation leases, and `rafikid` refuses to start without it |
| `RAFIKI_DAEMON_ID` | stable identity for this daemon — what a conversation lease records as its holder. Optional on a laptop (generated on first run); **required in Kubernetes**, where the pod filesystem is ephemeral |
| `RAFIKI_HEARTBEAT_INTERVAL` | how long a continuously-working child runs before its parent gets a coalesced check-in (Go duration, default 4m; `0` disables). A deadline on the scheduler, not a tick — the first check-in lands exactly one interval into a working spell |
| `RAFIKI_CONTROL_LISTEN` | TCP address for the remote control plane. Unset = UDS only. Requires `RAFIKI_DB` |
| `RAFIKI_CONTROL_TLS_CERT` / `RAFIKI_CONTROL_TLS_KEY` | PEM cert/key for the control plane TCP listener; mandatory when `RAFIKI_CONTROL_LISTEN` is set |
| `RAFIKI_URL` / `RAFIKI_TOKEN` | **daemon-side only.** Points a `rafikid`-spawned `claude` child at an external rafiki instance instead of the embedded proxy face, plus its bearer token. Client-side both names are hard errors (a profile names the daemon instead) except for `rafiki executor serve`/`service install`, which still derive `--connect` from `RAFIKI_URL` |
| `RAFIKI_DEFAULT_MODEL` | **daemon-side only** — the model the proxy face falls back to when a request names none |
| `RAFIKI_TOOLS_WEB` | `1` enables the fundi `webfetch`/`websearch` tools (default off) |
| `RAFIKI_BRAVE_API_KEY` | optional: use the Brave Search API for `websearch` instead of scraping DuckDuckGo Lite |
| `RAFIKI_BASH_RTK` | route fundi's `bash` output through [rtk](https://github.com/rtk-ai/rtk): `auto` (default), `on`, `off` |
| `RAFIKI_LOG_LEVEL` | daemon log verbosity: `debug`, `info` (default), `warn`, `error`. The daemon's `--log-level` flag overrides it. `debug` exposes per-request housekeeping lines |
| `RAFIKI_EXECUTOR_SELECTOR` | client-side default label selector for `rafiki create --executor-selector` |
| `RAFIKI_EXECUTORS_ENABLED` | daemon-side: `0`/`false` refuses executors outright. Defaults ON when `RAFIKI_CONTROL_LISTEN` is unset (UDS-only trust boundary), OFF once it's set |

**OpenTelemetry export** is configured in `rafikid.toml` (or the standard
`OTEL_EXPORTER_OTLP_*` env vars, which win over the file):

```toml
[telemetry]
endpoint = "http://127.0.0.1:4318"   # an OTLP collector
protocol = "http/protobuf"           # or "grpc"
content  = "off"                     # "all"/"rules" export prompt & reply text
```

Tracing turns on as soon as an endpoint is configured; unset means a no-op
provider. Postgres query spans start only inside an existing span, so the
indexer and background sweeps never produce root traces, and query parameters
are never recorded. `content` gates prompt, reply and tool-argument text;
errors are always exported.

**Web access (webfetch/websearch)** is opt-in (`RAFIKI_TOOLS_WEB=1`, daemon
side) since a fundi child may run unattended without egress. When disabled
the tools never appear in `tools[]`. `webfetch` resolves and checks the
**resolved IP** (blocking loopback, link-local/metadata, RFC 1918, IPv6 ULA —
a DNS name pointing at a private address is blocked too) and caps body reads
at 100 KB while reading, not after. `websearch` uses DuckDuckGo Lite by
default (no key) or Brave if `RAFIKI_BRAVE_API_KEY` is set.

Once `RAFIKI_DB` is set, `rafiki conversations stats|search|export|query`
queries persisted history through the daemon socket (no separate DB
credentials needed on the client machine) — the same queries as `rafikid
agent stats|search|export|query`, sibling renderers, transport-only
difference. Output is table on a TTY and a pipe alike by default; `-o
json`/`-j` for pretty JSON, `-J`/`-o jsonl` for one record per line. See
`docs/agent-cli.md` and `docs/reference/control-protocol.md` §2.3.

`rafiki conversations review <id|name>...` asks the daemon to run the same
LLM-driven skill-gap detector `rafikid agent analyze` uses, but over Connect
— no DSN needed on the client, and it works against a remote daemon.
`--stage rank` also persists findings; `rafiki conversations findings` lists
them. `rafiki close --review` runs it automatically on every conversation a
close touches (best-effort — a failed review is reported but never fails the
close itself).

## Telemetry

`rafikid` reads its own configuration — the `[telemetry]` section today, more sections over time — from `rafikid.toml`, at `$RAFIKI_CONFIG` else `<ConfigDir>/rafikid.toml` (a missing file means defaults, and an unknown key fails startup). Setting an OTLP endpoint there, or in the standard `OTEL_EXPORTER_OTLP_*` environment variables that override it, exports OpenTelemetry traces: one trace per agent turn, `execute_tool` and `chat` spans nested under it, cross-agent causality as span links, and postgres spans only under an active parent. Prompt, reply and tool-argument text is opt-in via content rules; errors are always exported. Metrics are independent of tracing and served at `/metrics`. See [`docs/reference/telemetry.md`](../reference/telemetry.md) for the configuration, span and attribute vocabulary, content policy, and a TraceQL query for one agent tree.
