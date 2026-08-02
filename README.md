# rafiki

An LLM proxy and conversation-capture store, an agent library built on it, and
a daemon that hosts coding-agent children. One module, two surfaces:

- **the proxy / library** — Anthropic- and OpenAI-protocol faces, routing with
  failover, and a DB-backed conversation store that captures every turn.
- **the daemon** — `rafikid` runs coding-agent children, multiplexes
  their event streams to concurrent clients, and exposes a control plane over a
  Unix socket. Its native `fundi` child kind drives the Anthropic API through
  this repo's own library rather than shelling out to Claude Code.

They were separate repos until they weren't: the daemon (then called fundi)
consumed rafiki as a pinned module, and the pin drifted silently for a whole
phase because a `go.work` file resolved it off disk instead. One module
removes the failure mode entirely, and a later identity consolidation folded
the daemon's own name into rafiki too — `fundi` now survives only as the
native agent runtime's child kind (`--kind fundi`).

## Layout

```
pkg/llm/        typed builder + Conversation — the library front door
pkg/routing/    breaker, OpenRouter catalog, model resolution, ClassifyFailure,
                prefix_hash, SSE capture parsing, turn store
pkg/agentloop/  ToolSet, Run/Resume, Events — tool-use loop primitives
pkg/store/      conversations schema migrations (source of truth), message
                persistence, recovery queries
pkg/server/     HTTP faces (/v1/messages, /v1/chat/completions), Authenticator
                seam, static token auth, Prometheus metrics
pkg/insights/   read-only queries over the captured corpus
pkg/analyze/    LLM-driven skill-gap detection and finding triage
pkg/agentcli/   the `rafikid agent` verb implementations

pkg/fundi/      the native agent runtime: turn engine, tools, context and skills
pkg/child/      child process lifecycle and the per-backend providers
pkg/inproc/     the child.Runner that runs a fundi child as a goroutine
                over a pair of OS pipes
pkg/control/    the daemon's control plane: dispatch and socket server
pkg/childstore/ child session records and snapshots
pkg/protocol/   typed wire shapes for every ctrl_* command, response and event
pkg/client/     Go client for the daemon's socket
pkg/bus/        event fan-out to concurrent subscribers
pkg/ring/       bounded ring buffer for child output
pkg/persist/    on-disk records and log dumps
pkg/paths/      XDG path resolution and the RAFIKI_* environment inventory
pkg/skills/     skill discovery and loading
pkg/models/     LLM model catalog enumeration
pkg/version/    build-derived version string

cmd/rafikid/    the rafiki daemon: proxy face, standalone `rafikid fundi` stdio
                mode, the DSN-backed `rafikid agent` insights CLI, and `migrate`
cmd/rafiki/     the rafiki CLI client, plus `rafiki claude` (the launcher)
attach/         rafiki-attach, the TUI (TypeScript, built with bun)
```

Everything importable lives under `pkg/`; `cmd/` is binaries. There is no
`internal/` — if you want to use something, import it. Private helpers are
unexported identifiers inside the package that uses them.

---

# The proxy / library

**Built (phases 1–4):** the routing core (per-upstream breaker, OpenRouter
catalog, model resolution, prefix_hash, SSE capture parsing); the typed
builder API + DB-backed `Conversation` (trim policy, cache breakpoints,
write-ahead persistence); agent-loop primitives (`agentloop.Run`/`Resume`
with fabricated-error crash recovery); both proxy faces — Anthropic
`/v1/messages` and OpenAI `/v1/chat/completions` — behind an `Authenticator`
seam; static bearer-token auth; Prometheus metrics; OTLP tracing; and `--dev`
mode. The standalone `rafiki serve`/`rafiki migrate` binary is gone — the
`rafikid` daemon now serves the proxy face itself, and `rafikid migrate` applies
the schema. sc imports the module in-process and mounts the same faces; sc's
diagnose loop, Slack agent and core-dump analyzer run on the library.

## Model selection

Requests on the `/v1/messages` face (and `llm.Conversation`s) take:

- a concrete Anthropic id (`claude-opus-4-8`) — sent to the Anthropic
  primary, with breaker-gated OpenRouter failover when configured;
- a `<family>-latest` alias (`opus-latest`) — resolved live from the
  OpenRouter catalog to the family's newest Anthropic model;
- a **short model alias** (`kimi-k3`, `deepseek-v4-pro`, `deepseek-v4-flash`,
  `glm-5.2`) — resolved live from the catalog to the newest release of that
  model line (the id itself or a stamped point release like `-0905`, never a
  variant fork or a new line), yielding an OpenRouter slash id;
- any OpenRouter slash id (`moonshotai/kimi-k3`) — routed directly to
  OpenRouter with no failover: the caller asked for that specific model.

Some model lines carry a **provider pin** (`routing.ProviderPrefsFor`):
open-weight models are served by many OpenRouter providers of varying
quantization and data-retention policy, so pinned lines get an OpenRouter
`provider` routing object injected restricting them to vetted hosts
(`glm-5.2` → Fireworks). A caller-supplied `provider` field always wins
over the pin.

No concrete model ids are hardcoded: aliases name families/lines and the
catalog is the source of truth, so an unresolvable alias errors instead of
falling back to a stale id. Slash ids and model aliases require
`OPENROUTER_API_KEY`. The `/v1/chat/completions` face does no resolution —
it takes raw ids and routes by configured prefix.

## `rafikid agent`

`rafikid agent <stats|search|export|analyze|findings>` is a DSN-backed CLI
over the captured `conversations` schema: read-only insights, the
LLM-driven skill-gap detector, and finding triage. See
[`docs/agent-cli.md`](docs/agent-cli.md) for every verb, flag, and the dev
loop.

```bash
make install
rafikid agent analyze --corpus DIR --compact --out DIR   # no DSN, no credentials
```

Without installing, run it as `go run ./cmd/rafikid agent`.

## Schema ownership

This repo owns the `conversations` schema. `store.Migrate` brings a database to
the head of the embedded chain, creating the tracking table
(`public.rafiki_schema_migrations`) on first run and applying whatever it does
not already record. It is idempotent, and concurrent callers are serialized by
an advisory lock so two servers booting together apply the chain exactly once.

## Model effort adaptation

Some OpenRouter models reject an `output_config.effort` value Claude Code sends
(e.g. `gpt-5-codex` accepts only `medium`). The proxy learns each model's
allowed set at runtime: on a rejection that enumerates supported values, it
records the constraint in an in-memory cache, clamps the effort, and retries the
request once, so the client gets a working response instead of a dead turn.
Subsequent requests to that model clamp proactively. The cache is per-process
(never persisted) and starts empty, so it always reflects the current provider
behavior. See `pkg/routing/effortmap.go` (`EffortCache`) and `pkg/server/proxy.go`
(`effortRetry`).

---

# rafiki — the agent daemon

rafiki began as a fork of pi-controller and speaks the same JSONL wire protocol,
so pi-controller's `pic` client and the pi TUI work against either. What it adds
is a **native agent runtime**: the `fundi` child kind drives the Anthropic API
through `pkg/llm` and `pkg/agentloop` directly rather than shelling out. That is
what makes in-band abort possible — abort arrives as a protocol frame and the
process stays resident.

| Child kind | Backend |
|---|---|
| `fundi` (default) | native loop over `pkg/agentloop` — in-band abort, per-turn token and cost accounting |
| `pi` | a pi process in `--mode rpc` |
| `claude` | Claude Code |

The kinds have **different model universes**, and `--model` completion is scoped
to the one you picked. A `fundi` child routes through this module, so it takes
concrete Anthropic ids, `<family>-latest` aliases and any OpenRouter slash id. A
`pi` child resolves the id against pi's *own* providers in
`~/.pi/agent/models.json`, so an OpenRouter slash id means nothing to it — pick
one and the child spawns, attaches, and then never answers.

`fundi` needs `ANTHROPIC_API_KEY` in the **daemon-visible** environment
(unconditionally — the client always builds an Anthropic sender), plus
`OPENROUTER_API_KEY` for any non-`anthropic/` model. Both reach a child from the
caller's shell via `rafiki create --forward-env`, on by default. A missing key
fails fast at spawn rather than on the first turn.

## Binaries

The usual daemon/client split, as with `dockerd`/`docker`:

| Binary | Role |
|---|---|
| `rafikid` | the daemon. It runs `fundi`-kind children as goroutines inside itself; `pi` and `claude` children remain subprocesses. `rafikid fundi` still exists as a standalone one-child-on-stdio mode, but the daemon no longer re-execs itself to spawn one |
| `rafiki` | the CLI client — the one you type |
| `rafiki-attach` | the TUI, spawned by `rafiki create` / `rafiki attach` |

Note that a `pic` on your `$PATH` is *pi-controller's* client, not rafiki's.

## Paths

rafiki follows the XDG base directories, so it coexists with a standalone
pi-controller install instead of competing for its `~/.pi/run` socket:

| | Default | Override |
|---|---|---|
| socket | `~/.local/state/rafiki/controller.sock` | `$XDG_RUNTIME_DIR`, or `$RAFIKI_SOCKET` |
| records | `~/.local/share/rafiki/state` | `$XDG_DATA_HOME` |
| logs | `~/.local/state/rafiki/logs` | `$XDG_STATE_HOME` |
| config | `~/.config/rafiki` (instructions, skills, `mcp.json`, `presets.json`) | `$XDG_CONFIG_HOME` |

Its launchd/systemd service identity is `dev.graveland.rafiki` / `rafiki`, again
distinct from pi-controller's.

The one thing rafiki writes outside its own directories is the `rafiki-helpers`
pi extension, into `~/.pi/agent/extensions/` — that is pi's contract, and how
pi discovers extensions.

## Environment

rafiki's variables are `RAFIKI_`-prefixed. Older spellings (`FUNDI_*`, and
before that `PIC_*`/`PI_CONTROLLER_*`) are retired: `pkg/paths.Get` reads
exactly the current name and nothing else, so a shell still exporting an old
name is silently ignored. `pkg/paths` is the single source of truth for what
rafiki reads from the environment; `.env.example` documents each one in full.

| | |
|---|---|
| `RAFIKI_SOCKET` | override the controller socket path |
| `RAFIKI_DEFAULT_MODEL` | model used when `rafiki create` gets no `--model` |
| `RAFIKI_DEFAULT_PRESET` | preset used when `--preset` is not given |
| `RAFIKI_DEFAULT_LABELS` | comma-separated `k=v` label defaults |
| `RAFIKI_NO_AUTO_INSTALL_HELPERS` | skip the `rafiki-helpers` auto-install |
| `RAFIKI_ATTACH_TAIL` | scrollback the TUI replays (`-1` all, `0` none) |
| `RAFIKI_ATTACH_DEBUG` | `1` logs every event the TUI receives to stderr |
| `RAFIKI_KILL_ON_EXIT` | `1` terminates the child when a directly-invoked TUI quits |
| `RAFIKI_INSTRUCTIONS` | user-global instruction file (default `~/.config/rafiki/instructions.md`) |
| `RAFIKI_SKILLS_DIRS` | skill directories, path-list separated (default `~/.config/rafiki/skills`) |
| `RAFIKI_MCP_CONFIG` | global `.mcp.json` (default `~/.config/rafiki/mcp.json`) |
| `RAFIKI_DB` | postgres URL for conversation persistence; **required for cost accounting** |

These must reach the **daemon's** environment, not your shell's — see
`.env.example`, which documents why and how to verify it.

Once `RAFIKI_DB` is set, `rafiki conversations stats|search|export` queries
that persisted history through the daemon socket — no separate DB credentials
needed on the machine running `rafiki`. See `docs/agent-cli.md` for the
DSN-direct equivalent (`rafikid agent stats|search|export`), or
`docs/reference/pi-controller-protocol.md` §6.17-6.19 for the wire commands.

`rafikid -h` and `rafikid fundi -h` document the two daemon process modes;
`rafiki --help` covers the client. See `docs/reference/pi-controller-protocol.md` for the
wire protocol spec and `docs/plans/2026-07-20-fundi-design.md` for the
architecture.

---

# Development

```bash
make check    # vet + golangci-lint + unit tests (-race) — the full local gate
make test     # tests only
make build    # all three Go binaries into bin/
make install  # copy them to ~/.local/bin (override with DESTDIR=)
make help     # every target
```

There is **no CI on this repo**, so `make check` is the only gate. `make
build-linux` cross-compiles for linux/amd64 — nothing else exercises `GOOS=linux`,
and the daemon silently bitrotted there for an entire phase before that target
existed.

Integration tests (migrator, capture store) need TimescaleDB >= 2.22 on
PostgreSQL 18:

```bash
docker run -d --name rafiki-test-db -p 5433:5432 \
  -e POSTGRES_PASSWORD=postgres timescale/timescaledb:2.28.2-pg18
RAFIKI_TEST_DSN='postgres://postgres:postgres@localhost:5433/postgres?sslmode=disable' \
  make test
```

`make test` sources a gitignored `.env` and warns loudly when `RAFIKI_TEST_DSN`
is unset — without it every DB-backed test *skips* while the run still reports
success, which is indistinguishable from a clean pass.

## Running locally

```bash
cp .env.example .env   # fill in ANTHROPIC_API_KEY (+ OPENROUTER_API_KEY)
make run               # rafikid in the foreground, proxy face on :8035
```

**`rafikid` serves the proxy itself.** It mounts the same `/v1/messages` and
`/v1/chat/completions` handlers the old standalone `rafiki serve` used to, on
the same port, so anything already pointed at a local rafiki keeps working —
and pi and claude children get capture, failover and model resolution without
a second process anyone has to remember to start. The fundi kind never uses
it: it reaches the library in-process.

The face binds **all interfaces** by default (`RAFIKI_PROXY_LISTEN`, default
`:8035`), so other hosts on your network can use one capture store and one set
of breakers. Auth is always required. The daemon mints a per-boot token for its
own children; `RAFIKI_SERVE_TOKEN` names an additional token for everything
else, which is what `make run` sets to `dev` so `make claude` (which sends
`RAFIKI_TOKEN`) works. A busy port is a hard error rather than a fallback —
the address is a contract, and silently landing elsewhere would mean talking
to whatever *did* claim it.

Because `make run` is now the daemon, it and the installed `rafiki service`
cannot both hold the port. Use one or the other.

`make run` does **not** migrate, where the old standalone `rafiki serve --dev`
did — run `go run ./cmd/rafikid migrate` once against a fresh database. Then,
in another shell:

```bash
make claude                                   # Claude Code through the local proxy
make claude ARGS='--model glm-5.2'            # …on any model the proxy can route
make claude ARGS='-- --permission-mode plan'  # …passing claude its own flags
psql 'postgres://postgres:postgres@localhost:5433/rafiki_live' \
  -c 'select model, status, upstream from conversations.conversation_turn'
```

`make claude` is a thin wrapper over `rafiki claude`, which preflights
`/healthz` and fails with a hint if the server isn't up. `RAFIKI_URL` /
`RAFIKI_TOKEN` / `RAFIKI_MODEL` retarget it, and everything after `--` is passed
to claude verbatim.

`--model` accepts anything the proxy can resolve: a concrete id, a
`<family>-latest` alias, or an OpenRouter slash id. That works because the
launcher registers the id as a *custom model option* rather than setting
`ANTHROPIC_MODEL` — Claude Code validates the latter against a client-side
allowlist of Anthropic ids and rejects everything else before a request ever
leaves, which would otherwise make the whole routing story above unreachable
from this launcher. It also pins `CLAUDE_CODE_AUTO_COMPACT_WINDOW` to the
model's real context window (Claude Code assumes 200K for a proxied model it
cannot verify, so it compacts at the wrong point) and strips inherited
`ANTHROPIC_*` variables, so launching a session from inside one does not land
its turns on the outer session's captured conversation.

Any other Anthropic-protocol client works the same way via
`ANTHROPIC_BASE_URL` + `ANTHROPIC_AUTH_TOKEN`.

## The rafiki TUI

`rafiki-attach` is TypeScript, bundled with bun, and links against the `pi`
git submodule:

```bash
make bootstrap      # fresh clone: init the submodule, build and install everything
make build-attach   # just the TUI (needs bun + an initialised submodule)
```

`make build` deliberately does **not** build the TUI: it needs bun and the
submodule, and the three Go binaries should build from a bare clone with
nothing but a Go toolchain.

> **Note:** the `pi` submodule points at a private host, so
> `git clone --recursive` does not work for outside contributors yet.
> Replacing it with the published `@earendil-works/pi-*` packages is tracked
> work.
