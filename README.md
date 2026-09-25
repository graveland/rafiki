# rafiki

An LLM proxy and conversation-capture store, an agent library built on it, and
a daemon (`rafikid`) that hosts coding-agent children — driven from a CLI/TUI
client (`rafiki`).

## Naming

Three Swahili words, three roles:

- **rafiki** ("friend") — the project as a whole: the client/TUI binary
  (`rafiki`), the daemon (`rafikid`), and the library underneath both.
- **fundi** ("craftsman") — the native agent runtime, one of three child
  kinds (`--kind fundi`, the default). It's the one that does the work
  in-process, through this repo's own `pkg/llm`/`pkg/agentloop` rather than
  shelling out to another agent binary.
- **daraja** ("bridge") — the mechanism that lets a `claude`-kind child run
  on a remote executor while still looking, to the daemon, like a normal
  managed child: the executor launches Claude Code and reverse-dials back
  into the daemon's pool, bridging capture, cost accounting, and routing
  visibility onto a process the daemon didn't start directly. See
  [Daraja](#daraja) below.

## Highlights

- **Native agent runtime (`fundi`)** — drives the Anthropic, OpenRouter, or any
  OpenAI-compatible provider directly through this repo's own library instead
  of shelling out, with breaker-gated failover. In-band abort: the process
  stays resident and abort arrives as a protocol frame.
- **Subagent trees, either kind.** `fundi` and `--kind claude` agents get the
  *same* rafiki agent-control surface — spawn, steer, budget, and watch real
  subagents, not just Claude Code's own opaque `Task` tool. Kinds mix freely:
  a `fundi` coordinator can spawn `claude` children and a `claude` session can
  spawn `fundi` ones. Depth/cost/concurrency limits are enforced by the
  daemon against stored state, never the caller's own claim, and settlement is
  push-based — a coalesced digest lands in the parent's next turn instead of
  it polling.
- **Cost-tiered fleets** — because kinds and models mix freely under one
  budgeted tree, a coordinator can plan with an expensive model, fan out
  execution across a swarm of cheap OpenRouter subagents, and review with a
  third — plan with Opus, execute with a fleet of $0.10/Mtok workers while
  Sonnet coordinates, review with Fable — all watchable live in one cockpit.
- **A searchable model catalog, not a static list.** `agent_models` queries
  the live OpenRouter catalog (300+ entries) plus every locally-configured
  provider — price, context window, tool/vision support, benchmark scores
  where known — so a coordinator can find "cheapest tool-capable model under
  $0.20/Mtok" itself instead of being handed a flat id list or guessing.
- **Ships its own coordination skills, used like Superpowers.** A curated
  skill set — brainstorming an idea into a design, writing a wave-based plan,
  executing it by dispatching concurrent subagents, and the `agent_spawn`/
  `task_*`/budget mechanics underneath — is synced both to a developer's
  local Claude Code and into the daemon's own skill store, so any `fundi` or
  `claude` agent rafiki spawns can pick up the same brainstorm → plan →
  dispatch-a-fleet workflow using rafiki's own feature set, not just a human
  working on this repo.
- **Claude Code's own subagents are captured too.** Claude Code's native
  `Task`-tool subagents get split into their own conversations (and their own
  rail rows in the cockpit) rather than disappearing into one opaque parent
  turn — so a `rafiki claude` session's built-in delegation is just as
  inspectable as a rafiki-spawned one.
- **Executor plane** — a subagent's filesystem/shell/LSP tools run on
  whatever executor it's bound to, not the daemon's own process, and can
  only touch what exists there: install one inside a VM or container and an
  agent using it is confined to that VM/container's filesystem, nothing on
  the host. `rafikid` itself can run in Kubernetes with no local filesystem
  of interest at all — you connect from the cockpit on your laptop and start
  agents against executors elsewhere, isolated per agent if you want it.
  Background jobs survive a 600s `bash` ceiling and notify on completion;
  workspaces can be pinned to a machine or rescheduled across an
  interchangeable pool.
- **A Python library that follows the agent around.** `pymodule_put` saves a
  reusable snippet — a class, a helper, or a runnable script — to the agent's
  own store: versioned, soft-deletable, persisted in the daemon database,
  visible only to its owner. The store syncs to the executors that owner's
  agents land on, and `pymodule_run` executes a saved script straight out of
  that synced cache by name — never a workspace copy, while the script runs
  in the agent's working directory or a `cwd` the call names — with further
  saved modules importable for the run. Pools beyond the personal store:
  `rafiki python repo` registers a git-backed pymodule source that every
  executor clones, discovers (`scripts/*.py` plus top-level packages) and
  builds one shared venv for, so `pymodule_run` can run a repo's scripts
  against its own dependencies. Code written once in one
  conversation is still runnable in the next one, on another machine.
- **Watch or drive the same conversation from anywhere.** A running agent
  isn't tied to one viewer: the cockpit (`rafiki attach`), `rafiki watch`,
  another agent's MCP tools (`agent_view`/`agent_send`), and a script hitting
  the Connect API directly can all observe or steer the same conversation at
  once — the event log fans out to every subscriber, and any number of them
  can replay a child's history from an ordinal.
- **Conversation review** — an LLM-driven pass over your own captured history
  that detects skill gaps and triages findings, on demand
  (`rafiki conversations review`) or automatically on close (`rafiki close
  --review`) — no direct DB access needed on the client.
- **Provider cache guard** — OpenRouter can silently move an unpinned model to
  a colder, more expensive provider mid-conversation. `routing.ProviderGuard`
  watches for the miss pattern and ejects the bad provider automatically.
- **Model aliasing** — short names for long local/custom model ids, with a
  declared context window so Claude Code doesn't assume 200K against a
  16K-context local model and blow past it.
- **Claude Code integration** — `rafiki claude` launches Claude Code through
  the proxy with full capture, routing, and cost accounting, plus the injected
  agent-control surface above — and can bill your own Claude subscription
  instead of the daemon's API key.
- **The `rafiki` cockpit** — a bubbletea TUI built into the client binary for
  watching and driving a tree of agents live, no separate build step.
- **Multi-daemon profiles** — one client resolves distinct daemons (local
  socket or remote TLS) by name, each with its own token and model defaults;
  agent presets (named seats: model, tools, prompt, budget) live in each
  daemon's database.

---

# The proxy / library

Routing core (per-upstream breaker, OpenRouter catalog, model resolution,
`prefix_hash`, SSE capture parsing); a typed builder API and DB-backed
`Conversation` (trim policy, cache breakpoints, write-ahead persistence);
agent-loop primitives (`agentloop.Run`/`Resume` with crash recovery); both
proxy faces — Anthropic `/v1/messages` and OpenAI `/v1/chat/completions` —
behind an `Authenticator` seam; static bearer-token auth; Prometheus metrics;
OTLP tracing. `rafikid` serves the proxy face itself and applies the schema
(`rafikid migrate`) — there is no separate `rafiki serve` binary.

## Model selection

Requests on the `/v1/messages` face (and `llm.Conversation`s) take:

- a concrete Anthropic id (`claude-opus-4-8`) — sent to the Anthropic
  primary, with breaker-gated OpenRouter failover when configured;
- a `<family>-latest` alias (`opus-latest`) — resolved live from the
  OpenRouter catalog to the family's newest Anthropic model;
- a **short model alias** (`kimi-k3`, `deepseek-v4-pro`, `glm-5.2`) —
  resolved live from the catalog to the newest release of that model line,
  yielding an OpenRouter slash id;
- any OpenRouter slash id (`moonshotai/kimi-k3`) — routed directly to
  OpenRouter with no failover.

Some model lines carry a **provider pin** (`routing.ProviderPrefsFor`):
open-weight models are served by many OpenRouter providers of varying
quantization and data-retention policy, so pinned lines get an OpenRouter
`provider` routing object restricting them to vetted hosts (`glm-5.2` →
Fireworks). A caller-supplied `provider` field always wins over the pin.

No concrete model ids are hardcoded: aliases name families/lines and the
catalog is the source of truth, so an unresolvable alias errors instead of
falling back to a stale id. Slash ids and model aliases require
`OPENROUTER_API_KEY`. The `/v1/chat/completions` face does no resolution —
it takes raw ids and routes by configured prefix.

### The provider cache guard

OpenRouter picks which provider serves an unpinned model, and that choice can
change without warning — silently, at up to 15× the input-token cost, because
the new provider doesn't cache the way the old one did. `routing.ProviderGuard`
watches completed OpenRouter turns for the tell (same conversation, same
`prefix_hash` and provider as the previous turn, a prompt over 4096 tokens,
and a cache miss). Five consecutive misses eject the provider for 24 hours,
capped at 3 ejected providers per model line so the guard can never blacklist
a line into unroutability. It needs capture enabled — it judges misses using
`prefix_hash` and conversation id, both capture-only — so with capture off it
is inert by design, and it applies **even when the caller supplied its own
`provider` object**. Set `RAFIKI_PROVIDER_GUARD=off` to disable it.

Ejections are logged append-only and reseed the in-memory list at startup:

```sql
SELECT created_at, provider, model_line, reason, expires_at, evidence
  FROM openrouter.provider_ejection
 ORDER BY created_at DESC LIMIT 20;
```

OpenRouter's catalog can't answer this for you — its `supports_implicit_caching`
flag doesn't correlate with actual cache behavior — which is why the guard is
observational rather than a lookup.

## `rafikid agent`

`rafikid agent <stats|search|export|query|analyze|findings>` is a DSN-backed
CLI over the captured `conversations` schema: read-only insights, the
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
One migration detail sets the supported floor at PostgreSQL >= 15: `NULLS
NOT DISTINCT`, first used by migration 0014's tasks-ordinal index and again by
0034's unique index on `conversations.pymodule_git_sources (owner_user_id,
name)` — there it keeps the unattributed bucket (a NULL owner) one identity
rather than NULLs-as-distinct rows. The dev container's pg18 image already
satisfies this.

## Model effort adaptation

Some OpenRouter models reject an `output_config.effort` value Claude Code sends
(e.g. `gpt-5-codex` accepts only `medium`). The proxy learns each model's
allowed set at runtime: on a rejection that enumerates supported values, it
records the constraint in an in-memory (per-process, never persisted) cache,
clamps the effort, and retries once. Subsequent requests to that model clamp
proactively. See `pkg/routing/effortmap.go` (`EffortCache`) and
`pkg/server/proxy.go` (`effortRetry`).

---

# rafiki — the agent daemon

The control plane is newline-delimited JSON frames over a Unix socket (a
legacy of rafiki's pi-controller fork), plus a Connect/protobuf plane for the
TUI and remote access. What the daemon adds beyond hosting a process is a
**native agent runtime**: the `fundi` child kind drives the Anthropic API
through `pkg/llm`/`pkg/agentloop` directly.

| Child kind | Backend |
|---|---|
| `fundi` (default) | native loop over `pkg/agentloop` — in-band abort, per-turn token and cost accounting |
| `pi` | a pi process in `--mode rpc` |
| `claude` | Claude Code |

The kinds have **different model universes**, and `--model` completion is
scoped to the one you picked: `fundi` takes concrete Anthropic ids,
`<family>-latest` aliases, and OpenRouter slash ids; `pi` resolves against
its own `~/.pi/agent/models.json`, so an OpenRouter id means nothing to it —
pick one and the child spawns, attaches, and never answers. `fundi` needs
`ANTHROPIC_API_KEY` in the **daemon-visible** environment (unconditionally),
plus `OPENROUTER_API_KEY` for any non-`anthropic/` model — both reach a
spawned child from the caller's shell via `rafiki create --forward-env`, on
by default.

## The agent inbox

The daemon runs two durable stores for the same traffic with opposite
guarantees. `conversations.event_log` is **at-least-once-to-a-cursor**,
fanned out to every subscriber — any attached client can replay a child's
history from an ordinal. `conversations.agent_inbox` is
**consume-once-into-a-turn** for exactly one consumer, the agent itself:
every message destined for it — a prompt, an abort, a coalesced subagent
digest — is persisted before it's acknowledged, and retired only once the
agent confirms it entered a turn. A daemon restart between accepting and
delivering a message replays it rather than losing it.

## Binaries

The usual daemon/client split, as with `dockerd`/`docker`:

| Binary | Role |
|---|---|
| `rafikid` | the daemon. Runs `fundi`-kind children as goroutines inside itself; `claude` children route through daraja on an executor when one is configured for it, else run as local subprocesses. `rafikid fundi` is a standalone one-child-on-stdio mode |
| `rafiki` | the CLI client — the one you type. Also the executor, via `rafiki executor serve` |

## Recall and memory

The daemon indexes what it captures into a searchable recall index: every
captured conversation contributes **windows** — ~3200-char slices of dialogue
with tool-call arguments compacted in — and, when a summaries model is
configured, rolling **summaries**. Tool results are never indexed (they
appear only as size markers in context expansion); neither are the daemon's
own LLM conversations (`recall.ExcludedEntrypoints`). On top of the derived
index sits the **memory tree**: dot-separated paths of small notes each user
(or their agents) save explicitly, always private to the saver.

Agents use six tools: `recall` (hybrid BM25 + vector search, RRF-fused over
memory/summary/window), `recall_context` (expand one hit id), and the memory
CRUD four — `memory_put`, `memory_get`, `memory_tree`, `memory_delete`. The
same surface is on the CLI:

```bash
rafiki recall "payment retry logic" --repo rafiki --limit 20
rafiki recall context w:9b1deb4d-3b7d-4bad-9bdd-2b0d7b3dcb6d --before 5
rafiki memory tree projects
rafiki memory put projects.rafiki auth "tokens ride the transport"
rafiki memory get projects.rafiki auth
rafiki memory delete projects.rafiki auth
rafiki memory status
```

Conversation-derived hits follow the credential — an admin's `rafiki recall`
sees every user's conversations, a user's only their own — while memories
are always the caller's own, admin included.

Two optional tables in `providers.toml` configure the LLM-backed halves:

```toml
[embeddings]
url = "https://api.openai.com/v1/embeddings"
api_key_env = "OPENAI_API_KEY"
model = "text-embedding-3-small"
dimensions = 1536

[summaries]
model = "openrouter/<model>"
# max_segment_tokens = 200000
```

Without `[embeddings]` search is keyword-only (BM25); without `[summaries]`
there are no conversation summaries. Historical conversations are never
summarized automatically — the summarizer only touches conversations active
after it first ran; use `rafiki memory backfill --since DURATION|RFC3339
--max-cost USD` (admin credential) to arm a budgeted backfill of older ones.
The database needs three extensions created by a superuser — `ltree`,
`vector`, `pg_textsearch` — with `pg_textsearch` also listed in
`shared_preload_libraries`.

## Daraja

An ordinary `rafiki create --kind claude` routes through daraja automatically
whenever the daemon has an executor pool configured and one of its executors
declares `claude` in `--launch` — no operator action needed. A daraja-routed
claude child is proxied through the daemon (capture, cost accounting, routing
visibility) while still billing the user's own Claude subscription by default
(`--passthrough-auth auto|on|off`, mirroring `rafiki claude --passthrough-auth`).

`rafiki daraja launch` is the manual entry point — useful for debugging the
launch path without a full spawn. It resolves an executor matching the
selector and supporting claude, mints a one-shot ticket, and waits for the
daraja to reverse-dial back before returning the child id:

```
rafiki daraja launch --cwd <dir> --model <provider/model> [--executor <selector>] [--resume <session-id>]
```

See `docs/reference/control-protocol.md` for the three Connect RPCs behind
this (`DarajaLaunch`, `DarajaSend`, `DarajaWatch`).

## Executor

**`rafiki create` defaults to a workspace that can serve the kind you asked
for.** For `fundi` that's your own machine: the client starts an in-process
executor and points the spawn at it, so `read`/`write`/`bash` run where your
files are. A kind that must be *launched* (`claude`) can't use that
throwaway executor, so with nothing explicit on the command line it's
resolved across every live, admitted executor that declares the kind —
asking you when ambiguous. `--no-local-executor` turns the local offer off;
`--executor <machine-or-id>` / `--executor-selector <labels>` target one.

There are two kinds of executor:

- **Durable** — enrolled with `rafiki executor enroll` or created outright
  with `rafiki executor create`. Has a database row with operator-written
  labels, admission, isolation, and workspace mode; lives until deleted.
  Naming one is two steps on two machines — name the machine where it will
  run, then mint the enrollment token wherever the operator is:

  ```sh
  rafiki executor name laptop                 # on the machine the executor runs on
  rafiki executor enroll --name laptop         # wherever the operator is; carry the token over
  ```
- **Transient** — started automatically by `rafiki create`/`rafiki attach`.
  No database row, authenticated by a one-shot ticket over the already
  authenticated control connection; dies when that connection closes.

If a durable executor already covers this machine and user, the client uses
that instead of starting its own, so an agent keeps working after you detach.

**Without an executor, an agent has no workspace tools at all** —
`read`/`write`/`edit`/`glob`/`grep`/`ls`/`bash`/`lsp_*` simply aren't
registered, never silently running against the daemon's own filesystem. What
remains is the daemon tier: MCP, web fetch/search, the task ledger, skills,
and the agent-control verbs.

```bash
go build -o bin/rafiki ./cmd/rafiki
rafiki executor serve --connect-socket "$XDG_RUNTIME_DIR/rafiki/executor.sock" \
  --enroll-token <token> --root "$PWD"
```

**Flags:** `--connect host:port` (remote reverse-dial) / `--connect-socket`
(local reverse-dial) — one is required; `--root` (working directory root,
default cwd); `--concurrency` (default 6); `--proxy name=base_url`
(repeatable — forward an LLM endpoint only this machine can reach, see "The
executor relay" below); `--launch kind` (repeatable — opt this machine into
hosting a launched child kind, e.g. `claude`); `--lsp-config` / `--no-lsp`
(language servers auto-detect from `PATH` by default; a configured-but-absent
server just drops its `lsp_*` tools rather than advertising broken ones).

No certificate is involved for a local socket. **Socket permissions are the
only access control**: the socket is `0600` from creation (`0177` umask), and
the daemon refuses a second executor on a path already served by a live one.
Anyone who can open the socket gets arbitrary `bash` and filesystem access
inside `--root`.

**The executor's environment.** launchd/systemd --user don't inherit a login
shell, so `executor service install` captures the installing shell's
environment into `<config dir>/executor.env` (0600) at install time — three
classes are skipped (rafiki's own `RAFIKI_*`/provider keys, `PATH`, and
stale session/GUI residue like `PWD`/`TERM*`/`DIRENV_*`). A second,
hand-maintained file, `<config dir>/executor-overrides.env`, sets variables
**unconditionally** — needed for anything the service manager itself seeds
wrong, like launchd's per-session `SSH_AUTH_SOCK`. See `.env.example` and
`RAFIKI_EXECUTOR_ENV_FILE`/`RAFIKI_EXECUTOR_OVERRIDES_FILE`.

**Background execution** is the immediate win over plain `bash`, which is
synchronous with a 600s ceiling in-process:

| Tool | Purpose |
|---|---|
| `bash_start` | start a command in the background, return a handle immediately |
| `bash_output` | read everything the job has printed; reports running/exited and exit code |
| `bash_kill` | stop the job and its whole process group |

Completion is **pushed, not polled** — the daemon watches every job a child
starts and injects a fragment into that child's next turn when it exits,
through the same coalescing path subagent settlements use. Output is written
to a file on the executor with a per-workspace byte budget
(`--job-output-budget-mb`, default 256 MB, drop-oldest-finished-first) and
**no time-based expiry** — a turn can end and resume hours later, so a job's
output lives until its workspace is released, not until a wall-clock window
elapses. Watches are in-memory and not re-armed across a daemon restart. See
`docs/reference/executor-protocol.md` for the full wire protocol.

### The executor relay

A provider in `providers.toml` can be reached through an executor's own
localhost instead of the daemon dialing it directly — for a local inference
server (vmlx, Ollama, …) the daemon usually can't reach:

```toml
[providers.vmlx]
kind = "anthropic"
base_url = "http://localhost:8005"

[providers.vmlx.via_executor]
selector = "role=workstation"   # which executor(s)
proxy = "vmlx"                  # matches a --proxy name on that executor
```

The executor side is the allowlist (`rafiki executor serve --proxy
vmlx=http://localhost:8005` declares exactly what it forwards to); a request
naming an undeclared name never reaches the network. A `via_executor`
provider with no matching executor is a hard failure on every spawn, never a
silent direct dial (which would have the daemon reach its own localhost
instead). The relay carries request headers over the executor's connection,
so a keyed provider's credential transits whatever machine relays it — treat
`via_executor` like any other tool call an executor already runs on your
behalf.

### Model aliases and declared context windows

A local model's real id is often long, and its context window is never in
the OpenRouter catalog, so Claude Code falls back to assuming 200K — which
overflows a small local model with an opaque `prompt_too_long` from the
inference server itself, outside rafiki's view. A provider can declare short
aliases, each optionally carrying its real context window:

```toml
[providers.vmlx.models.qwen]
id                   = "models/Qwen3.8-27B-Abliterated-MLX-4bit"
context_window       = 16384
context_files_tokens = 12288      # optional; default 20% of context_window, clamped [1024, 30000]
skills               = ""         # "" = none; "*"/omitted = all; "a,b,c" = only those
mcp_servers          = "codescan" # same tri-state convention

[providers.openrouter.models."glm-flash@together"]
id   = "z-ai/glm-5.3-flash"
only = ["together"]              # openrouter only: provider slugs allowed to serve this alias
```

`rafiki claude --model vmlx/qwen` sends the real id upstream and pins
`CLAUDE_CODE_AUTO_COMPACT_WINDOW` accordingly; the alias shows up in `rafiki
models` and `--model` completion (`source: alias`) once a reachable daemon
knows about it. `context_files_tokens`/`skills`/`mcp_servers` bound what gets
built into a **fundi** child's system prompt and tool inventory for that
model — a `rafiki claude` child assembles those itself and only
`context_window` reaches it. An explicit `--skills`/`--mcp-servers` flag on
the spawn always overrides the model's declared default.

`only` is an OpenRouter-only pin on an `anthropic-openrouter` provider's
alias: requests made through the alias are sent with
`"provider": {"only": [...]}` so only those OpenRouter provider slugs may
serve them, and it replaces any built-in routing pin for that model line for
that request (the cache guard's ignore list still merges in). Because the
pin lives on the alias, two aliases can route the SAME model id to different
providers — an eval compares `openrouter/glm-flash@together` against
`openrouter/glm-flash@fireworks`, both sending `z-ai/glm-5.3-flash` with
different `only` lists. On any other provider kind, `only` is a config error.

### Container executors

An executor serves whatever filesystem it can see; whether that's a
container is decided by how the operator starts it, not by a flag:

```
docker run -d --name rafiki-executor -v /home/user/worktrees:/work \
  rafiki-executor:latest \
  rafiki executor serve --connect daemon.example.com:443 --enroll-token <token>
```

`--root` sets the working directory — it is **not** a sandbox; the
container's mounts (or the host user's permissions, for a native executor)
are the boundary. The executor asserts nothing about itself: `isolation`,
`workspace_mode`, `roots`, `labels` all live on the database row set at
token-mint time, so the row is where you declare a container:

```
rafiki executor create --isolation container --workspace-mode ephemeral \
  --root /work --label env=ci
```

There is deliberately no path vocabulary narrower than a whole executor:
`docker run -v` expresses the ro/rw model for containers, and for native
executors a userspace path check on the file tools would be fake since
`bash` could still escape it. Native access is gated by **admission**
(label-selection), not by paths.

### Workspace lifecycle

Each child gets a workspace provisioned before it starts and released when it
exits, at the child's cwd.

- **ephemeral** — the operator declares these machines interchangeable; if
  the executor is lost, the child reschedules onto another one.
- **pinned** — the executor exposes an existing tree; if it's lost, the child
  parks until it returns or a timeout expires.

This says nothing about filesystem isolation, which comes from the
operator's composition (containers) or from git worktrees passed as cwd.

## Subagents

An agent can spawn and steer its own descendants, regardless of its own
kind — `fundi` and `--kind claude` both get this surface (a `claude` agent
reaches it through the injected MCP agent-control tools described under
[Running locally](#running-locally) below), and `kind` on a spawn picks the
child's runtime independently of the parent's, so the two mix freely in one
tree:

| Tool | Purpose |
|---|---|
| `agent_spawn` | start a subagent (`kind: fundi\|claude`); returns a handle immediately, does not block |
| `agent_list` | your subtree — id, name, model, status, assigned task |
| `agent_view` | the tail of a descendant's transcript |
| `agent_send` | steer a descendant mid-flight, or give it more work |
| `agent_kill` | stop a descendant and everything below it |
| `agent_models` | the models you may spawn on |

Every verb naming another agent is checked against stored lineage (an agent
may only see/steer/kill its own descendants) — read from `childstore`, never
from a tool argument, since arguments come from a model that can be
prompt-injected. Completion is a **signal**: `agent_spawn` returns as soon as
the child is registered, and settlement lands as one coalesced digest per
parent turn regardless of how many descendants finished together. Background
jobs (`bash_start`) settle the same way.

### Claude Code's own subagents

A `--kind claude` agent still has its native `Task` tool available, and
rafiki's tool descriptions and injected coordination prompt steer it toward
`agent_spawn` instead — but when it uses `Task` anyway (or when a plain
`rafiki claude` session does), rafiki still makes the result observable: each
native subagent thread is detected from the underlying Claude Code protocol
traffic and captured as its own conversation, with its own rail row in the
cockpit and its own cost attribution, rather than disappearing into one
opaque parent turn. A helper Claude Code spawns internally for a single
tool call (its WebFetch/WebSearch summarizers) is captured too but not shown
as an agent row — its cost still rolls up into its parent's total.

### Where a subagent runs

`agent_spawn` takes `executor` (a label selector, e.g. `env=work,os=linux`)
and `workspace` (`ephemeral`/`pinned`, as above) — the entire placement
grant. Neither creates an isolated checkout; every workspace on an executor
shares its root, and isolation is `cwd` — a coordinator wanting an isolated
worker creates a git worktree and passes it. **A selector can only narrow**:
the daemon intersects the parent's effective executor set with the child's
selector, so a child can never reach an executor its parent couldn't, by
construction. The worker is told where it landed (machine, isolation,
workspace mode, roots) in its own system prompt.

Two things these grants don't defend against, worth knowing: **MCP bypasses
them entirely** (any agent may use any MCP tool, regardless of executor
confinement), and **a native executor grants everything its user can
reach** — the mitigation is that admission is rare, not that scope is
narrow. Grants defend against the model, not a compromised executor host.

### Limits

Three independent ceilings, enforced by the daemon against stored state —
never against a value in the request asking for them.

| Limit | Set with | Default | Bounded by |
|---|---|---|---|
| **depth** | `--max-depth`, `agent_spawn(max_depth=…)` | `1` | `RAFIKI_MAX_DEPTH` (default `3`) |
| **cost** | `--max-cost`, `agent_spawn(max_cost=…)` | unlimited | the parent's remaining budget |
| **concurrency** | `--max-children`, `agent_spawn(max_children=…)` | `4` | — |

Depth is granted locally per hop (a coordinator granting `1` means its
workers grant `0`) but bounded absolutely by `RAFIKI_MAX_DEPTH` regardless of
what any parent granted. Cost decrements across the whole subtree; a child
may be granted at most its parent's remainder, and unset means unlimited —
fine for a top-level interactive agent, wrong for a coordinator. A budget hit
mid-flight is not a kill: unfinished tasks go `blocked`, live agents are
steered once, and raising the budget resumes the work. Budget checks fail
closed — if a budgeted agent's spend can't be read, the spawn is refused.

### Presets

A preset is a named seat — model, tools, system prompt and budget — that an
agent spawn starts from. By convention a preset is named `<group>:<role>`
(`default:implementer`, `default:reviewer`); every preset in one group is a
seat in the same fleet. `rafiki preset list|get|put|delete` manages them;
`rafiki preset put NAME -f spec.json` saves the spec (append-only — each save
is a new version, a delete stamps every live version, `get NAME --history`
reads them back). The JSON fields: `description, kind, provider, model,
thinking, executor, labels, tools, skills, mcp_servers, context_files,
system_prompt, append_system_prompt, max_cost, max_depth, max_children`.
`tools`/`skills`/`mcp_servers` are tri-state: omitted = the kind's default
(everything), `[]` = none, a list = exactly those.

Agents spawn by `agent_spawn`'s `preset` argument. Spawn-time fields may
override `model`/`thinking`/budgets, append to the system prompt, and only
narrow `tools`/`skills`/`mcp_servers`/`context_files` — a preset's allowlist
never widens, and its system prompt is never replaced. A claude-kind preset
accepts only `model`, `provider`, `append_system_prompt`, `executor`,
`labels` and budgets.

## Paths

rafiki follows the XDG base directories:

| | Default | Override |
|---|---|---|
| socket | `~/.local/state/rafiki/controller.sock` | `$XDG_RUNTIME_DIR` |
| records | `~/.local/share/rafiki/state` | `$XDG_DATA_HOME` |
| logs | `~/.local/state/rafiki/logs` | `$XDG_STATE_HOME` |
| config | `~/.config/rafiki` (instructions, skills, `mcp.json`, `lsp.json`, `profiles.toml`) | `$XDG_CONFIG_HOME` |

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

`RAFIKI_URL`, `RAFIKI_TOKEN`, `RAFIKI_SOCKET`, `RAFIKI_DEFAULT_MODEL`,
`RAFIKI_DEFAULT_PRESET`, `RAFIKI_DEFAULT_LABELS` are hard client-side errors
now (naming the offending variable) rather than silently-ignored or
silently-outranking globals. They keep their old meaning for `rafikid`
itself, and for the two headless, profile-exempt commands `rafiki executor
serve` / `rafiki executor service install`.

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
| `RAFIKI_PROXY_LISTEN` | bind address for the proxy face (default `:8035`) |
| `RAFIKI_DB` | postgres URL for conversation persistence; **required** — a DB-less daemon has no history, cost accounting, task ledger, user identity, executor plane or conversation leases, and `rafikid` refuses to start without it |
| `RAFIKI_DAEMON_ID` | stable identity for this daemon — what a conversation lease records as its holder. Optional on a laptop (generated on first run); **required in Kubernetes**, where the pod filesystem is ephemeral |
| `RAFIKI_HEARTBEAT_INTERVAL` | how often a continuously-working child's parent gets a coalesced check-in (Go duration, default 5m; `0` disables) |
| `RAFIKI_CONTROL_LISTEN` | TCP address for the remote control plane. Unset = UDS only. Requires `RAFIKI_DB` |
| `RAFIKI_CONTROL_TLS_CERT` / `RAFIKI_CONTROL_TLS_KEY` | PEM cert/key for the control plane TCP listener; mandatory when `RAFIKI_CONTROL_LISTEN` is set |
| `RAFIKI_URL` / `RAFIKI_TOKEN` | **daemon-side only.** Points a `rafikid`-spawned `claude`/`pi` child at an external rafiki instance instead of the embedded proxy face, plus its bearer token. Client-side both names are hard errors (a profile names the daemon instead) except for `rafiki executor serve`/`service install`, which still derive `--connect` from `RAFIKI_URL` |
| `RAFIKI_DEFAULT_MODEL` | **daemon-side only** — the model the proxy face falls back to when a request names none |
| `RAFIKI_TOOLS_WEB` | `1` enables the fundi `webfetch`/`websearch` tools (default off) |
| `RAFIKI_BRAVE_API_KEY` | optional: use the Brave Search API for `websearch` instead of scraping DuckDuckGo Lite |
| `RAFIKI_BASH_RTK` | route fundi's `bash` output through [rtk](https://github.com/rtk-ai/rtk): `auto` (default), `on`, `off` |
| `RAFIKI_EXECUTOR_SELECTOR` | client-side default label selector for `rafiki create --executor-selector` |
| `RAFIKI_EXECUTORS_ENABLED` | daemon-side: `0`/`false` refuses executors outright. Defaults ON when `RAFIKI_CONTROL_LISTEN` is unset (UDS-only trust boundary), OFF once it's set |

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
`docs/agent-cli.md` and `docs/reference/control-protocol.md` §6.17-6.19.

`rafiki conversations review <id|name>...` asks the daemon to run the same
LLM-driven skill-gap detector `rafikid agent analyze` uses, but over Connect
— no DSN needed on the client, and it works against a remote daemon.
`--stage rank` also persists findings; `rafiki conversations findings` lists
them. `rafiki close --review` runs it automatically on every conversation a
close touches (best-effort — a failed review is reported but never fails the
close itself).

### First user: claiming a fresh daemon

A daemon with no users at all starts in **bootstrap mode**: every listener
(UDS, and TCP/TLS if configured) admits a connection with no `ctrl_auth`
frame and lets it run exactly one command, `ctrl_user_create` — whoever's
lands first becomes the only user. Deliberate (a freshly-started pod has no
operator shell to hand a token to), but it's a real if narrow race, logged as
a WARN once a minute while the window stays open. Close it before exposing
the daemon: run a local `rafikid` against the same `RAFIKI_DB` and create the
user before the real daemon's socket is reachable, or `kubectl port-forward`
to the pod and create it before any Service/Ingress exposes the control
plane. Deleting the last active user (`rafiki user rm`) reopens the window.

---

# Development

## Prerequisites

**[ripgrep](https://github.com/BurntSushi/ripgrep) (`rg`) must be on `PATH`.**
Not optional: the fundi runtime's file-discovery tools are built directly on
it, and `BuildRuntime` refuses to start without it (`apt-get install
ripgrep` / `brew install ripgrep`).

[rtk](https://github.com/rtk-ai/rtk) is genuinely optional (`RAFIKI_BASH_RTK`
defaults to `auto`).

```bash
make check    # vet + golangci-lint + unit tests (-race) — the full local gate, no CI on this repo
make test     # tests only
make build    # all Go binaries into bin/
make install  # copy them to ~/.local/bin (override with DESTDIR=)
make help     # every target
```

Integration tests (migrator, capture store) need TimescaleDB >= 2.22 on
PostgreSQL 18:

```bash
docker run -d --name rafiki-test-db -p 5433:5432 \
  -e POSTGRES_PASSWORD=postgres timescale/timescaledb:2.28.2-pg18
RAFIKI_TEST_DSN='postgres://postgres:postgres@localhost:5433/postgres?sslmode=disable' \
  make test
```

`make test` sources a gitignored `.env` and warns loudly when
`RAFIKI_TEST_DSN` is unset — without it every DB-backed test *skips* while
the run still reports success.

## Running locally

```bash
cp .env.example .env   # fill in ANTHROPIC_API_KEY (+ OPENROUTER_API_KEY), and RAFIKI_DB
set -a; . ./.env; set +a
go run ./cmd/rafikid migrate   # once, against a fresh database
make run                       # rafikid in the foreground, proxy face on :8035
```

`rafikid` serves the proxy itself — `/v1/messages`, `/v1/chat/completions`,
`/v1/messages/count_tokens`, and `HEAD /api/hello` — so pi and claude
children get capture, failover and model resolution with no second process.
The fundi kind never uses the face; it reaches the library in-process.

The face binds all interfaces by default (`RAFIKI_PROXY_LISTEN`, default
`:8035`). Auth is always required — a fresh daemon starts in bootstrap mode
(above), so create a user once while `make run` is up:

```bash
go run ./cmd/rafiki user create dev
```

That mints a token into the resolved profile's token file; `rafiki claude`
picks it up with nothing further to export.

```bash
make claude                                   # Claude Code through the local proxy
make claude ARGS='--model glm-5.2'            # …on any model the proxy can route
make claude ARGS='-- --permission-mode plan'  # …passing claude its own flags
```

`--model` accepts a concrete id, a `<family>-latest` alias, or an OpenRouter
slash id — the launcher registers it as a *custom model option* rather than
setting `ANTHROPIC_MODEL`, which Claude Code would otherwise validate against
a client-side allowlist and reject. It also pins
`CLAUDE_CODE_AUTO_COMPACT_WINDOW` to the model's real context window and
strips inherited `ANTHROPIC_*` variables so a nested launch can't land on the
outer session's captured conversation.

Every proxied session gets rafiki's MCP agent-control surface injected
automatically (a `--mcp-config` pointing at the proxy's `/mcp` mount, with
the credential carried as a `RAFIKI_MCP_TOKEN` env var Claude Code expands at
connect time — never in argv, which is world-readable via `ps`). Daemon-
spawned `--kind claude` children also get a short coordination prompt merged
into `--append-system-prompt`, steering them toward `agent_spawn` over the
built-in `Task` tool. Any other Anthropic-protocol client gets the same
routing via `ANTHROPIC_BASE_URL` + `ANTHROPIC_AUTH_TOKEN`.

### Billing your own subscription

`--passthrough-auth` (or `RAFIKI_CLAUDE_PASSTHROUGH`) is a three-way switch —
`auto` (default), `on`, `off` — for who gets billed. `on` bills **your**
Claude subscription instead of the daemon's `ANTHROPIC_API_KEY` (by omitting
`ANTHROPIC_AUTH_TOKEN`, the only thing that makes Claude Code prefer
API-key auth over OAuth); `off` always bills the daemon's key; `auto` picks
`on` whenever `--model` resolves to an Anthropic id. It's Anthropic-only
(rejected outright against a non-Anthropic model, with OpenRouter failover
disabled for these requests too) and fails closed on every ambiguous case —
no rafiki token, no forwardable credential, a typoed flag value — never
silently falling back to the daemon's key. Only `rafiki claude` supports it;
daemon-spawned `--kind claude` children can't unset the daemon's own key.

Every passthrough response carries Anthropic's `anthropic-ratelimit-unified-*`
headers (your subscription's 5h/7d utilization), captured best-effort into a
latest-only per-user snapshot. `rafiki claude --limits` prints it; the
cockpit's status line shows a live-polled summary; a `quota_status` tool lets
a coordinator check its own headroom before deciding to fail over.

One client-side gotcha when pointing Claude Code at any proxy by hand: its
SSE-ping stream-idle watchdog only activates when the base URL host is
exactly `api.anthropic.com`, so a thinking phase with >300s between content
events on a custom host dies with "Response stalled mid-stream" even though
bytes flowed the whole time. Set `_CLAUDE_CODE_ASSUME_FIRST_PARTY_BASE_URL=1`
to restore direct-connection behavior (`rafiki claude` and fundi-spawned
claude children get it automatically) — but that variable also re-enables
deferred tool search, which 400s on non-Anthropic models, so the launcher
also sets `ENABLE_TOOL_SEARCH=false` whenever `--model` isn't an Anthropic id.

## The rafiki cockpit

A bubbletea program built into the `rafiki` binary itself, speaking Connect —
no separate build step. It reaches the same daemon every other `rafiki` verb
does (the resolved profile):

```bash
rafiki profile add prod --url https://rafiki.example.net --token "$TOKEN"
rafiki -P prod attach
```

### Entry points

| Command | Opens | Rail subscribes to |
|---|---|---|
| `rafiki create …` | the new child, full width | its subtree, plus itself |
| `rafiki attach <id\|name>` | that child, full width | its subtree, plus itself |
| `rafiki attach`, or bare `rafiki` | the rail, nothing focused | everything you can see |

A session follows its own delegation — spawn implementers and they appear in
the rail because they're in the subtree. The rail is hidden until a second
agent exists; `^R`/`⇥` peek it, and `n` (a rail key) opens the spawn form.
With no children at all there is nothing to peek, so a bare attach, and `^R`
against an empty rail, open the spawn form directly instead. Attaching loads
the full conversation from the database first (`GetHistory`), then follows
the live event stream.

### Watching without the cockpit

`rafiki watch [id|name]` subscribes to the same lifecycle events the rail is
built from and prints them to stdout, one line per event:

```
14:32:01  spawn   c_01ABC  impl-auth  parent=c_root
14:32:02  status  c_01ABC  impl-auth  spawning → streaming
14:33:14  turn    c_01ABC  impl-auth  cost=$0.0142 stop=END_TURN (1m12s)
14:33:14  status  c_01ABC  impl-auth  streaming → idle (1m12s)
14:40:02  exit    c_01ABC  impl-auth  code=0 (lifetime 8m01s)
```

Not a TUI, replays nothing — live events only, from everything you can see or
one child's subtree when named. `-J` emits one raw event per line.

### Keys

Two focus targets — the input box and the agent rail — toggled with `⇥`.

**Global**

| Key | Does |
|---|---|
| `⇥` / `⇧⇥` | Toggle between input box and agent rail; reveals it if hidden |
| `⌥N` / `^PgDn` | Hop to the next agent that needs you |
| `⌥P` / `^PgUp` | Hop to the previous agent that needs you |
| `^↑` / `^↓` | Hop up/down the rail without changing pane |
| `esc` / `^X` | Abort the running turn |
| `^R` / `^B` | Collapse/restore the agent rail; with one agent, peek it |
| `^G` | Toggle the help overlay |
| `^C`/`^D` | Quit — press the same one twice within two seconds. Children keep running; reattach any time |

**Input pane**

| Key | Does |
|---|---|
| `⏎` | Send a prompt — queues work for the agent |
| `⇧⏎` / `^J` | Insert a newline (`⇧⏎` needs a Kitty-keyboard-protocol terminal — Ghostty, Kitty, WezTerm, recent iTerm2/Alacritty; `^J` works everywhere) |
| paste | Over six lines or 800 characters folds to `[pasted #1: 40 lines]`; paste again to insert in full |
| `^U` | Clear the whole input, folded pastes included |
| `⌥⏎` / `^S` | Steer — inject into the turn already running |
| `PgUp`/`PgDn`, `home`/`end` | Scroll the transcript without leaving the input box |
| `↑` / `↓` | Move the cursor; with nowhere to move, scroll the transcript |

Prompt and steer are separate keys rather than one that guesses from agent
state, since only you know whether you mean "queue this for later" or
"interrupt now."

**Rail pane**

| Key | Does |
|---|---|
| `↑` / `↓` | Move the cursor — the conversation pane previews the highlighted agent, live: once the cursor rests, that agent's own event stream takes over (the previously open one resumes when you come back) |
| `⏎` | Open the selected agent and return to the input box |
| `esc` | Back to the input box, leaving focus where it was |

### Reading the transcript

Follows new output at the bottom, holds position when scrolled back; sending
returns you to the bottom. A bottom-right readout (`↓ 1840/2272 81%`) shows
position. Three weights make it scannable: solid `▌` gutter for the agent's
own prose, dotted `┊` for thinking, none for tool calls — which show their
argument (`⚒ bash go test ./...`) and truncate output from the *end* (where
the error usually is). No mouse support, deliberately — capturing it would
take away your terminal's own select-and-copy.

### Activity and attention

```
◌ spawning   ○ idle        ◐ streaming   ⚒ running a tool
⊛ compacting ‼ needs you   ◇ stopping    ⟳ retrying    ✓/✗ exited
```

The badge counts only events worth a human (a finished turn, going idle,
blocking on you, an error, a child exiting) — an agent merely working shows a
glyph and no badge. Retries aren't badged (automatic retry means a
recoverable stream error isn't your problem), but the `⟳` glyph keeps one
from looking identical to progress.
