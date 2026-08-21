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

### The provider cache guard

OpenRouter picks which provider serves an unpinned model, and that choice can
change without warning. On 2026-08-12 it moved `deepseek/deepseek-v4-pro` from
Novita to CoreWeave; Novita had been serving 98–99% of input tokens from prompt
cache, CoreWeave served 5.9%. Nothing errored — requests succeeded, quality was
unaffected, and the cost per input token went up 15.6×. Sticky routing then kept
every subsequent turn of the conversation on the bad provider.

`routing.ProviderGuard` watches completed OpenRouter turns for exactly that. A
turn counts as evidence against its provider only when it *should* have hit
cache: same conversation, same `prefix_hash` as the previous turn, same provider
as the previous turn, and a prompt over 4096 tokens. Five consecutive such
misses eject the provider — its slug goes into the outbound request's
`provider.ignore` for that model line, which is also what breaks OpenRouter's
sticky routing and lets the next request land somewhere else.

- **Threshold: 5.** Measured, not guessed. Across ~1200 turns of healthy Novita
  traffic the worst consecutive miss streak was 1; the broken CoreWeave produced
  streaks of 15–28. Both sequences are checked into
  `pkg/routing/testdata/` and replayed as tests.
- **TTL: 24 hours**, then the provider is eligible again — long enough that a
  bad provider cannot re-burn the budget the same day, short enough that a
  repaired endpoint returns unattended.
- **Cap: 3 providers per model line**, oldest evicted first, so the guard can
  never blacklist a model into unroutability.
- The guard's ignore list is merged in **even when the caller supplied its own
  `provider` object** — unlike the pin, which the caller overrides. A budget
  guard any caller can switch off is not a guard.
- It needs capture enabled: it judges misses using `prefix_hash` and the
  conversation id, so with capture off nothing qualifies as evidence and the
  guard is inert by design.

Set `RAFIKI_PROVIDER_GUARD=off` to disable it entirely.

Ejections are logged append-only, so the table is breakage history rather than
just state, and unexpired rows reseed the in-memory list at startup:

```sql
SELECT created_at, provider, model_line, reason, expires_at, evidence
  FROM openrouter.provider_ejection
 ORDER BY created_at DESC LIMIT 20;
```

Note that OpenRouter's endpoints API cannot answer this question for you: its
`supports_implicit_caching` flag is `false` for Novita, which cached 99% of the
time, and `true` only for the first-party DeepSeek endpoint. Every third-party
endpoint also publishes an `input_cache_read` price whether or not it delivers
one. Observed behaviour is the only signal, which is why the guard is
observational rather than a catalog lookup.

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

rafiki began as a fork of pi-controller, which is why the control plane is
newline-delimited JSON frames over a unix socket rather than something more
modern. What it adds
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
| `rafiki` | the CLI client — the one you type. Also the executor, via `rafiki executor serve` |
| `rafiki-attach` | the TUI, spawned by `rafiki create` / `rafiki attach` |

## Executor

**By default, `rafiki create` makes your own machine the workspace.** The client asks the
daemon for an executor row, starts an executor in-process, and points the spawn at it — so
`read`, `write`, `bash` and the rest run where your files are, whether the daemon is on
this machine or in a cluster. `--no-local-executor` turns it off; `--executor-selector`
sends the child somewhere else instead.

If a durable executor already covers this machine and user — one installed with
`rafiki executor service install` — the client uses that instead of starting its own. That
one outlives your terminal, so an agent keeps working after you detach.

**Without an executor, an agent has no workspace tools at all.** `read`, `write`, `edit`,
`glob`, `grep`, `ls`, `bash` and the `lsp_*` verbs are not registered — not registered and
failing, and above all not silently running against the daemon's own filesystem. What
remains is the daemon tier: MCP, web fetch and search, the task ledger, skills, and the
agent verbs. That is a useful agent, and it is the right one for a caller that only wants
reasoning over tools the daemon legitimately owns.

**Project skills come from the workspace's machine.** A skill found in
`<cwd>/.rafiki/skills` or `<cwd>/.claude/skills` is a workspace skill — and the
workspace lives on the executor. The daemon fetches the project-tier inventory at
spawn, merges it with the operator's own skills (project shadows user on a name
collision), and fetches bodies on the turn the model asks for one rather than
eagerly at spawn.

`rafiki create` gives you an executor automatically, so this is not something you normally
arrange.

`rafiki executor serve` moves the filesystem and shell tools (`read`, `write`,
`edit`, `glob`, `grep`, `ls`, `bash`) and the language-server tools (`lsp_*`)
behind a Connect RPC surface. The daemon becomes
the RPC client: when an executor is configured, tool calls are dispatched to it;
when absent, no workspace tool is registered at all.

It is a subcommand of `rafiki` rather than its own binary, so there are two
artifacts to build and ship — one client, one server — not three. The
administrative verbs alongside it (`enroll`, `list`, `label`, `disable`,
`enable`) act on the daemon's control socket, which an executor host does not
have, so their presence on such a host grants nothing.

**Background execution** is the immediate win: `bash` is synchronous with a
600s ceiling in-process, so dev servers, log tails, and any test suite slower
than ten minutes are unavailable. The executor's `Attach` stream survives a
dropped connection — a laptop sleeping mid-build does not lose the build.

```bash
go build -o bin/rafiki ./cmd/rafiki

# On the daemon's own machine, reverse-dial the executor socket:
rafiki executor serve --connect-socket "$XDG_RUNTIME_DIR/rafiki/executor.sock" \
  --enroll-token <token> --root "$PWD"
```

No certificate is involved: a single-machine install should not need one.

**Flags:**
- `--connect` — reverse-dial a daemon at host:port (for remote executors)
- `--connect-socket` — reverse-dial a rafikid on this machine over its executor unix socket
- `--root` — working directory root (defaults to current directory)
- `--concurrency` — maximum concurrent tool calls (default 6)
- `--proxy name=base_url` — declare an LLM endpoint this executor will forward
  to (repeatable). See "The executor relay" below.

**Language servers run on the executor**, because that is where the files are. With no
`--lsp-config`, the executor auto-detects what is installed on its own `PATH`; a config
naming a server that is not installed leaves the `lsp_*` tools out of the agent's tool list
rather than advertising eight tools that can only answer "executable file not found".
`--no-lsp` disables them entirely.

New flags: `--lsp-config` (path to an `lsp.json`), `--no-lsp`.

**Socket permissions are the only access control in this phase.** The socket
is created under a `0177` umask, so it is `0600` from the moment it exists —
no window in which another local user can connect. The daemon refuses to
accept a second executor on a path already served by a live one, rather than
silently stealing its future connections. Anyone who *can* open the socket
gets arbitrary `bash` and filesystem access inside `--root`; there is no
authentication beyond the filesystem.

**Background jobs.** With an executor configured, agents gain three tools
that plain `bash` cannot offer, because plain `bash` is synchronous with a
600s ceiling:

| Tool | Purpose |
|---|---|
| `bash_start` | start a command in the background, return a handle immediately |
| `bash_output` | read everything the job has printed; reports running/exited and the exit code |
| `bash_kill` | stop the job and its whole process group |

They are parent-side tools implemented as RPCs, and they **do not exist**
when no executor is configured — a tool that can only answer "not configured"
costs a turn to learn nothing. Output is written to a file on the executor,
retained with drop-oldest at 8 MB, and a poll returns at most 100 KB of it from
the end — when it clips, the reply names the file so the agent can `read` or
`grep` the rest on the same machine.

A finished job's output has **no expiry**. It lives until the agent's workspace
is released, because a wall-clock window cannot know when an async agent will
come back for it: a turn can end and resume hours later. What bounds it is a
per-workspace byte budget (`--job-output-budget-mb`, 256 MB), which evicts the
oldest finished job first and never evicts a running one.

See `docs/reference/executor-protocol.md` for the full wire protocol.

### The executor relay

A provider in `providers.toml` can be reached through an executor's own
localhost instead of the daemon dialing it directly — the case for a local
inference server (vmlx, Ollama, …) that only listens on one machine's
loopback, which the daemon usually is not on:

```toml
[providers.vmlx]
kind = "anthropic"
base_url = "http://localhost:8005"

[providers.vmlx.via_executor]
selector = "role=workstation"   # which executor(s)
proxy = "vmlx"                  # matches a --proxy name on that executor
```

The executor side is the allowlist: `rafiki executor serve --proxy
vmlx=http://localhost:8005` declares the one name/base_url pair that machine
is willing to forward to, and it is enforced there — a proxy request naming
an undeclared name, or a path that would escape the declared base, never
reaches the network. `base_url` still lives on the provider regardless of
`via_executor`: it is the request's target URL either way, only the
transport (direct dial vs. relayed through the executor's existing
connection) changes.

**No matching executor is a hard failure, on every spawn, for as long as the
provider carries a `via_executor` table** — never a silent fall-through to a
direct dial. A direct dial of a `via_executor` provider's `base_url` would
have the daemon reach its OWN localhost, which either refuses outright or,
worse, reaches something unrelated that happens to be listening on the same
port — indistinguishable from success until the response comes back wrong.
The relay is resolved once, when a child's `llm.Client` is built
(`relayTransport`, `cmd/rafikid/provider_relay.go`); an executor that shows up
afterward is not picked up until the next spawn.

**A keyed provider's credential transits the executor process.** The relay
carries the request (headers included) over the executor's own connection to
rafikid, so an `api_key_env` credential attached to a `via_executor` provider
is visible to whatever runs on that executor's machine for the length of the
request. This is no different from any other tool call an executor already
runs on the operator's behalf, but it is worth stating: `via_executor` is for
endpoints the operator trusts with a credential, same as they already trust
with `bash`.

The relay is **not** subject to executor confinement — that machinery exists
because tools run code on a machine, and the relay runs none. See the design
doc's "Non-goals" section for the reasoning.

### Container executors

An executor serves the filesystem it can see. Whether that view is a container
is determined by how the operator starts it, not by a flag. To run a container
executor, put `rafiki executor serve` inside a container the operator starts:

```
docker run -d --name rafiki-executor \
  -v /home/user/worktrees:/work \
  rafiki-executor:latest \
  rafiki executor serve --connect daemon.example.com:443 --enroll-token <token>
```

The `--root` flag sets the working directory; it is NOT a sandbox. The
process's filesystem view — the container's mounts, chosen in `docker run`, or
the host user's permissions — is the boundary. The executor declares nothing
about itself: `isolation`, `workspace_mode`, `roots` and `labels` live only on
the database row set at token-mint time.

**Which means the row is where you say it is a container**, when you mint the
credential:

```
rafiki executor create --isolation container --workspace-mode ephemeral \
  --root /work --label env=ci
```

Nothing detects this and nothing ever will — an executor sniffing
`/proc/1/cgroup` would be asserting a fact that gates it. The row is what
selects the machine (`--workspace-mode` narrows placement), what decides whether
losing it fails a child or moves it, and what tells the child it is sandboxed at
all. Leave `--isolation` at its `none` default on a machine that really is a
container and the worker is never told, so its first denied path reads as a
broken repository rather than as the sandbox working.

**No path vocabulary.** There is no way to restrict a worker to a subtree of
its worktree, by design. For container executors, `docker run -v` expresses the
ro/rw model and the kernel enforces it. For native executors, path scoping would
be fake in the only place it matters: the file tools could enforce structured
path arguments in userspace, but `bash` could not. Native access is gated by
**admission** (label-selection), not by paths.

**The macOS caveat:** docker on macOS is a Linux VM, so a containerised
executor means the agent works on Linux — different toolchain, different
caches, bind-mount I/O that is not native-fast. Fine for rafiki (pure Go,
cross-compiles); likely wrong for a Rust/Zig/ESP toolchain.

### Workspace lifecycle

Each child gets a workspace provisioned before it starts and released when it
exits:

- **ephemeral**: the executor constructs the workspace per child. It is
  reconstructible, so the child can be rescheduled to another executor if
  the first one is lost.
- **pinned**: the executor exposes an existing tree. If the executor is lost,
  the child is parked until it returns or the timeout expires.

This distinction is what the park-vs-fail decision consults when an executor
goes away.

## Subagents

A fundi agent can spawn and steer its own descendants through six tools:

| Tool | Purpose |
|---|---|
| `agent_spawn` | start a subagent; returns a handle immediately, does not block |
| `agent_list` | your subtree — id, name, model, status, assigned task |
| `agent_view` | the tail of a descendant's transcript |
| `agent_send` | steer a descendant mid-flight, or give it more work |
| `agent_kill` | stop a descendant and everything below it |
| `agent_models` | the models you may spawn on |

**Every verb that names another agent is checked against stored lineage.** An
agent may only see, steer or kill its own descendants; a sibling's child, its
own parent, and another conversation's agent are all refused with an error
naming the id. The check reads the parent chain in `childstore`, never a tool
argument — tool arguments are produced by a model that can be prompt-injected.

**Completion is a signal, not a return value.** `agent_spawn` returns as soon
as the child is registered. When a descendant settles, one coalesced digest is
injected into its parent's next turn: five workers finishing together cost one
turn, not five. The digest names who finished; *what they did* is read from the
task ledger with `task_list(assignee=…)`, which is one indexed query rather
than a transcript replay.

**Unresolved work is caught, not prompted for.** An agent that settles holding
non-terminal tasks is told once, naming the handles. A second settle with the
same residue escalates to its coordinator instead of nudging again.

### Where a subagent runs

`agent_spawn` takes two more parameters, and they are the entire grant:

| Parameter | Meaning |
|---|---|
| `executor` | a label selector over machines — `env=work,os=linux` |
| `workspace` | `ephemeral` (fresh, rebuildable, reschedulable) or `pinned` (an existing tree on one machine) |

Nothing path-shaped is model-facing. Mounts are derived by the daemon from the
child's worktree; a coordinator choosing labels and a workspace mode cannot make
the mistakes a coordinator composing path allowlists would, which is what makes
these grants safe to author without human review.

**A selector can only narrow.** The daemon computes the parent's effective
executor set, evaluates the child's selector independently, and intersects. A
child can never reach an executor its parent could not — by construction, not by
a rule that has to be checked. A spawn matching nothing fails immediately and
names the excluding predicate per candidate; it does not queue.

**The worker is told where it landed** — machine, isolation, workspace mode,
roots, and which of them are read-only — in its system prompt, at the moment the
assignment is made. A sandboxed worker that does not know it is sandboxed
misreads its first denial as a broken repository.

#### What these grants do and do not defend against

- **MCP bypasses the grant.** Any agent may use any MCP tool, so a worker
  sandboxed to its worktree can still reach whatever the MCP surface reaches.
  Correct today — an MCP server's containment is its operator's job — but adding
  a filesystem- or kubectl-shaped MCP server silently widens every worker in the
  fleet. Gating MCP at server granularity is a later change the vocabulary
  leaves room for.
- **A native executor grants everything its user can reach** — `~/.ssh`, every
  repository on the machine. The mitigation is that admission is rare, not that
  scope is narrow.
- **Grants defend against the model, not against a compromised executor host.**
  A remote executor self-applies the grant it is handed; a malicious one could
  ignore it. Executors are infrastructure you deployed. mTLS would answer "is
  this my executor", never "is my executor honest" — do not later mistake the
  grant for a defence against a hostile host.
- **Annotations are unverified claims across conversation boundaries.** Two
  unrelated agents coordinate through shared mutable state; that is the value,
  but a label write is a cross-conversation side effect rather than something
  scoped to one child's lifetime. The same caveat applies to task metadata.

### Limits

Three independent ceilings, all enforced by the daemon against stored state —
never against a value in the request that asks for them.

| Limit | Set with | Default | Bounded by |
|---|---|---|---|
| **depth** | `--max-depth`, `agent_spawn(max_depth=…)` | `1` | `RAFIKI_MAX_DEPTH` (default `3`) |
| **cost** | `--max-cost`, `agent_spawn(max_cost=…)` | unlimited | the parent's remaining budget |
| **concurrency** | `--max-children`, `agent_spawn(max_children=…)` | `4` | — |

**Depth is granted locally and bounded absolutely.** A parent grants what its
child needs without reference to its own allowance: a coordinator making one
hop grants each worker `1`, and those workers grant reviewers `0`. Nobody
computes the tree's total depth — `RAFIKI_MAX_DEPTH` caps the child's absolute
position, computed from stored lineage, and refuses regardless of what any
parent granted.

**Cost decrements across the subtree.** Spend is summed over every conversation
in the tree, reached by conversation id (in-process agents) and by
`external_ref` (proxied ones). A child may be granted at most its parent's
remainder. Unset means unlimited — right for a top-level interactive agent,
wrong for a coordinator, which should always set one.

A budget hit **mid-flight is not a kill**: the subtree's unfinished tasks go
`blocked` (not `orphaned` — the agents are alive), every live agent is steered
once, and raising the budget resumes the work.

Budget checks **fail closed**: if a budgeted agent's spend cannot be read, the
spawn is refused. An agent with no budget is unaffected, so a daemon without a
database keeps working.

## Paths

rafiki follows the XDG base directories:

| | Default | Override |
|---|---|---|
| socket | `~/.local/state/rafiki/controller.sock` | `$XDG_RUNTIME_DIR`, or `$RAFIKI_SOCKET` |
| records | `~/.local/share/rafiki/state` | `$XDG_DATA_HOME` |
| logs | `~/.local/state/rafiki/logs` | `$XDG_STATE_HOME` |
| config | `~/.config/rafiki` (instructions, skills, `mcp.json`, `lsp.json`, `presets.json`) | `$XDG_CONFIG_HOME` |

**Instruction files come from two machines.** The user-global instructions file
(`$RAFIKI_INSTRUCTIONS`, else `~/.config/rafiki/instructions.md`) belongs to
whoever runs the agent loop, so the daemon reads it from its own disk. Project
instructions — `CLAUDE.md` and `AGENTS.md` at the git root and at the working
directory — belong to the *workspace*, so when the workspace lives on an
executor the daemon asks that executor for them over the `ProjectContext` RPC
rather than reading a path that does not exist there.

Its launchd/systemd service identity is `dev.graveland.rafiki` / `rafiki`.

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
| `RAFIKI_SKILLS_DIRS` | skill directories, path-list separated (default `~/.config/rafiki/skills`). Entries may be symlinks (e.g. into `~/.claude/skills` or a plugin cache); discovery follows them |
| `RAFIKI_MCP_CONFIG` | global `.mcp.json` (default `~/.config/rafiki/mcp.json`) |
| `RAFIKI_LSP_CONFIG` | global `lsp.json` for language server config (default `~/.config/rafiki/lsp.json`) |
| `RAFIKI_PROXY_LISTEN` | bind address for the proxy face (default `:8035`) |
| `RAFIKI_DB` | postgres URL for conversation persistence; **required for cost accounting**. `rafiki service install` writes it to `~/.config/rafiki/service.env` (0600), never the unit file — it carries a password |
| `RAFIKI_CONTROL_LISTEN` | TCP address for the remote control plane (e.g. `tcp:8036`). Unset = UDS only. **Requires `RAFIKI_DB`** — control-plane identity is row-backed, and a listener without a user table is fatal at startup |
| `RAFIKI_CONTROL_TLS_CERT` | PEM cert for the control plane TCP listener; mandatory when `RAFIKI_CONTROL_LISTEN` is set |
| `RAFIKI_CONTROL_TLS_KEY` | PEM key for the control plane TCP listener; mandatory when `RAFIKI_CONTROL_LISTEN` is set |
| `RAFIKI_URL` | client-side: the one dial target, `https://host[:port]` (port defaults to 443 when omitted). Reaches the LLM proxy and, over TLS, the control plane's `/control` upgrade (`client.IsRemoteURL` — `https://` only). Unset or `http://` means local: the loopback proxy face and, for control-plane commands, the UDS. Wins over `RAFIKI_SOCKET` when it names a remote control plane. `tls://` is retired — always spell a remote daemon as `https://`. `rafiki create`/`rafiki resume --pi-session` require an explicit `--cwd` against a remote `RAFIKI_URL`: the default (this process's own working directory) names a path on the CLIENT, which almost certainly doesn't exist on the remote daemon's filesystem — `Controller.Spawn` stats `cwd` server-side. |
| `RAFIKI_TOKEN` | client-side: the one bearer credential, presented as the proxy's `Authorization`/`X-Rafiki-Token` and the control plane's `ctrl_auth` token. Unset falls back to `~/.config/rafiki/token` (0600), written by `rafiki user create` |
| `RAFIKI_TOOLS_WEB` | `1` enables the fundi webfetch and websearch tools. Default off |
| `RAFIKI_BRAVE_API_KEY` | optional: use the [Brave Search API](https://api.search.brave.com/) for `websearch` instead of scraping DuckDuckGo Lite. Unset falls back to the keyless scraper, which needs no setup but can break on a markup change |
| `RAFIKI_BASH_RTK` | route fundi's `bash` output through [rtk](https://github.com/rtk-ai/rtk) for compression: `auto` (default, use it when installed), `on`, `off`. Overridden by `--bash-rtk` |
| `RAFIKI_EXECUTOR_SELECTOR` | client-side: default label selector for `rafiki create --executor-selector`, choosing an executor from the daemon's enrolled pool (e.g. `owner=brent`). Unset means `rafiki create` makes the client's own machine the workspace by default: it starts a session executor and points the spawn at it. Set it to send children somewhere else |
| `RAFIKI_EXECUTORS_ENABLED` | daemon-side: `0`/`false` refuses executors outright, any other value forces them on (needs `RAFIKI_DB`). Unset defaults ON when `RAFIKI_CONTROL_LISTEN` is unset (the only path in is the local unix socket, same trust boundary as the control socket) and OFF once it's set (that path becomes reachable over the TCP listener) |

**Web access (webfetch / websearch).** The fundi runtime includes two opt-in web
tools: `webfetch` fetches a URL and returns its text, and `websearch` queries
DuckDuckGo and returns the top results. Both are **off by default** because a
fundi child may run unattended without egress; set `RAFIKI_TOOLS_WEB=1` in the
**daemon's** environment to enable them. When disabled the tools never appear in
`tools[]` — they decline materialization rather than advertising an operation
the model cannot use.

**Security posture:**
- `webfetch` resolves the host and checks the **resolved IP**, not the hostname
  string, before making a request. Blocked: loopback (`127.0.0.0/8`, `::1`),
  link-local (`169.254.0.0/16`, notably the cloud metadata endpoint at
  `169.254.169.254`), RFC 1918 private ranges (`10.0.0.0/8`, `172.16.0.0/12`,
  `192.168.0.0/16`), and IPv6 unique local (`fc00::/7`). A DNS name pointing
  at a private address is blocked.
- Body reads are capped at 100 KB via `io.LimitReader` — the cap is enforced
  **while reading**, not after, so a hostile server cannot exhaust memory.
- `websearch` uses DuckDuckGo Lite, the anonymous HTML-only endpoint — no API
  key, no credential plumbing. Results are limited to 20 (default 10).

These must reach the **daemon's** environment, not your shell's — see
`.env.example`, which documents why and how to verify it.

Once `RAFIKI_DB` is set, `rafiki conversations stats|search|export` queries
that persisted history through the daemon socket — no separate DB credentials
needed on the machine running `rafiki`. It renders the same tables as the
DSN-direct `rafikid agent stats|search|export` (same queries, same renderers,
only the transport differs); `--output` picks the format, defaulting to tables
at a terminal and JSON when piped. See `docs/agent-cli.md` for the DSN-direct
equivalent, or `docs/reference/control-protocol.md` §6.17-6.19 for the wire
commands.

Note that the two read whatever DSN each was given: `rafiki conversations` uses
the **daemon's** `RAFIKI_DB` (written to `service.env` at `service install`
time), while `rafikid agent --db` defaults to your shell's `RAFIKI_DB`, then
`RAFIKI_TEST_DSN`. If their numbers disagree, check that first.

`rafikid -h` and `rafikid fundi -h` document the two daemon process modes;
`rafiki --help` covers the client. See `docs/reference/control-protocol.md` for the
wire protocol spec and `docs/plans/2026-07-20-fundi-design.md` for the
architecture.

### First user: claiming a fresh daemon

Identity is row-backed (`conversations.users`), so a daemon that has never had
`rafiki user create` run against it has no users at all — and **while that is
true, both faces are wide open**: the proxy face accepts only its per-boot
child secret (unknown outside the process tree, so effectively closed to
anyone else), but every listener the control plane serves — UDS and, if
configured, the TCP/TLS one — admits a connection with no `ctrl_auth` frame
and lets it run exactly one command, `ctrl_user_create`. **Whoever's
`ctrl_user_create` lands first becomes the daemon's first (and, until they add
more, only) user.** This is deliberate — a freshly-started pod has no operator
shell to hand it a token through — but it means the window between "the
listener is reachable" and "the first user exists" is a real, if narrow,
race, bounded only by how quickly the operator closes it. The daemon logs a
WARN once a minute for as long as it stays open, and logs the peer address of
whoever's `ctrl_user_create` claims it.

Three sequences close the window before anything untrusted can reach the
daemon:

- **Local daemon, central database.** Run a `rafikid` on your own machine
  pointed at the same `RAFIKI_DB` the real deployment will use, and
  `rafiki user create` against that — the row lands in the shared database
  before the real daemon ever serves a socket.
- **Port-forward before ingress.** In Kubernetes, `kubectl port-forward` to
  the pod and create the first user through the tunnel before any Service or
  Ingress makes the control plane reachable from outside the cluster.
- **Before upstream credentials exist.** Create the first user before
  `ANTHROPIC_API_KEY`/`OPENROUTER_API_KEY` are configured, so a daemon claimed
  during the window has nothing to spend even if someone else's
  `ctrl_user_create` wins the race.

Once a user exists, every other command — on any listener — requires a valid
`ctrl_auth`/bearer token; deleting the last active user (`rafiki user rm`)
returns the daemon to bootstrap mode, so the same window reopens.

---

# Development

## Prerequisites

**[ripgrep](https://github.com/BurntSushi/ripgrep) (`rg`) must be on `PATH`.** It
is not optional and not a fallback: the fundi runtime's file-discovery tools are
built directly on it, so `BuildRuntime` probes for it at startup and refuses to
start without it. A daemon on a host with no `rg` fails every `fundi` child with
`ripgrep (rg) is required but was not found on PATH` rather than degrading. The
container image installs it (see `Dockerfile`); a local checkout needs
`apt-get install ripgrep` or `brew install ripgrep`.

[rtk](https://github.com/rtk-ai/rtk) is genuinely optional — `RAFIKI_BASH_RTK`
defaults to `auto`, which uses it when present and runs plain bash when it is
not. Only `RAFIKI_BASH_RTK=on` promotes a missing `rtk` to a startup error, which
is the entire difference between `on` and `auto`.

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
cp .env.example .env   # fill in ANTHROPIC_API_KEY (+ OPENROUTER_API_KEY), and RAFIKI_DB
set -a; . ./.env; set +a
go run ./cmd/rafikid migrate   # once, against a fresh database
make run                       # rafikid in the foreground, proxy face on :8035
```

**`rafikid` serves the proxy itself.** It mounts the same `/v1/messages` and
`/v1/chat/completions` handlers the old standalone `rafiki serve` used to, on
the same port, so anything already pointed at a local rafiki keeps working —
and pi and claude children get capture, failover and model resolution without
a second process anyone has to remember to start. It also proxies the two
Anthropic-protocol endpoints Claude Code calls beyond `/v1/messages`:
`POST /v1/messages/count_tokens` (token counting, with the same model
resolution and upstream routing) and `HEAD /api/hello` (a connectivity
preflight probe). Both are recorded in `conversations.raw_http_request` when
`RAFIKI_RECORD_REQUESTS=1`, but neither opens a capture turn — a token count
and a probe are metadata, not turns. The fundi kind never uses the face: it
reaches the library in-process.

The face binds **all interfaces** by default (`RAFIKI_PROXY_LISTEN`, default
`:8035`), so other hosts on your network can use one capture store and one set
of breakers. Auth is always required: the daemon mints a per-boot token for its
own spawned children, but that secret is unknown to any human or tool
connecting from outside, so `make claude` needs a real user. A fresh daemon has
none — it starts in bootstrap mode (see
[First user](#first-user-claiming-a-fresh-daemon) below) — so create one once,
in another shell, while `make run` is still up:

```bash
go run ./cmd/rafiki user create dev
```

That mints a token and writes it to `~/.config/rafiki/token` (0600); `make
claude`'s default `RAFIKI_TOKEN` falls back to that file with nothing further
to export. A busy port is a hard error rather than a fallback — the address is
a contract, and silently landing elsewhere would mean talking to whatever
*did* claim it.

Because `make run` is now the daemon, it and the installed `rafiki service`
cannot both hold the port. Use one or the other.

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

### Billing your own subscription

`rafiki claude --passthrough-auth` (or `RAFIKI_CLAUDE_PASSTHROUGH=1`) captures
the conversation as usual but bills **your** Claude subscription rather than the
daemon's `ANTHROPIC_API_KEY`. It works by *not* setting `ANTHROPIC_AUTH_TOKEN`:
that variable is the only thing that makes Claude Code prefer API-key auth over
its OAuth subscription, so omitting it lets the subscription credential through.
rafiki's own token moves to an `X-Rafiki-Token` header, leaving `Authorization`
free to carry yours, which the proxy forwards upstream untouched.

Consequences worth knowing:

- **Anthropic models only.** The launcher refuses a non-Anthropic `--model` up
  front, and the proxy rejects one with a 400 if it gets that far — a
  subscription credential cannot buy an OpenRouter model, and failing over
  would bill the key you just opted out of. OpenRouter failover is off for
  these requests for the same reason: an upstream error reaches you verbatim.
- **It fails closed, never quietly.** Every way this can go wrong ends in an
  error rather than a surprise bill: no rafiki token, no credential to forward
  (you are logged out of Claude Code, or `CLAUDE_CODE_USE_BEDROCK`/`VERTEX` is
  set), an `Authorization` that turns out to be rafiki's own token, or the
  request landing on the OpenAI-compatible face, which cannot honour
  passthrough. None of these fall back to the daemon's key.
- **`rafiki claude` only.** Daemon-spawned `--kind claude` children cannot use
  it; they receive environment *additions* appended to the daemon's own
  environment, which cannot un-set the daemon's `ANTHROPIC_API_KEY`.
- **`RAFIKI_CLAUDE_PASSTHROUGH` must be exactly `1`.** Any other value, `0` and
  `false` included, leaves it off.

One client-side caveat when pointing Claude Code at any proxy by hand: it
attaches its byte watchdog — the mechanism that lets SSE keep-alive pings feed
the 300s stream idle watchdog — only when the base URL host is exactly
`api.anthropic.com`. On a custom base URL, pings stop counting as activity and
a thinking phase with more than 300s between content events dies with
"Response stalled mid-stream" even though bytes flowed the whole time. Set
`_CLAUDE_CODE_ASSUME_FIRST_PARTY_BASE_URL=1` to restore direct-connection
behaviour (`rafiki claude` and fundi-spawned claude children get it
automatically).

That variable has a second, less obvious effect, because Claude Code gates two
features on the same "is this a first-party host" predicate: it also re-enables
*deferred tools* (tool search), which omit most tools from `tools[]` and send
`tool_reference` blocks instead. Only Anthropic models can call a tool that
was omitted, so a session on an OpenRouter-routed model dies on its first turn
with `400 Deferred custom tools are only supported on Anthropic models`. The
launcher and the daemon therefore set `ENABLE_TOOL_SEARCH=false` whenever
`--model` is not an Anthropic id; hand-configured clients pointing at a
non-Anthropic model need it in their own env. An explicit `ENABLE_TOOL_SEARCH`
in the environment always wins, so `true` / `auto` / `auto:N` remain available
if a future upstream does support them.

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
