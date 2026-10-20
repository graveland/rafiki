# Agents and subagents

The control plane is Connect (`proto/rafiki/v1/control.proto`): one unix
socket (`<RuntimeDir>/controller.sock`) locally, one optional TLS listener
remotely — the same RPC surface either way, and the one plane the CLI, the
cockpit, the Python SDK and spawned children all speak. A profile with a
token presents it as the bearer credential; owner-scoped state (presets
above all) resolves against the connection's owner. A token-less profile
stays anonymous on the local socket (local trust), and a token that no
longer resolves is refused (`unauthenticated`) with the same recovery:
delete the profile's token file, run `rafiki login` ([authentication](authentication.md), when the daemon
has SSO configured), or mint a replacement for the existing user with
`rafikid user token mint <name>` on the daemon host — it opens the database
directly, so it works when nothing can authenticate. What the daemon adds
beyond hosting a process is a
**native agent runtime**: the `fundi` child kind drives the Anthropic API
through `pkg/llm`/`pkg/agentloop` directly.

| Child kind | Backend |
|---|---|
| `fundi` (default) | native loop over `pkg/agentloop` — in-band abort, per-turn token and cost accounting |
| `claude` | Claude Code |
| `script` | a saved pymodule run as the child's process (script children, below) |

The kinds have **different model universes**, and `--model` completion is
scoped to the one you picked: `fundi` takes concrete Anthropic ids,
`<family>-latest` aliases, and OpenRouter slash ids; a script child takes no
model at all. `fundi` needs
`ANTHROPIC_API_KEY` in the **daemon-visible** environment (unconditionally),
plus `OPENROUTER_API_KEY` for any non-`anthropic/` model — both reach a
spawned child from the caller's shell via `rafiki create --forward-env`, on
by default.

## Script children

A `script` child is a child whose brain is a saved pymodule process instead
of an LLM — a workflow driver, not a worker. It gets everything a child
already has: an id, lineage (its spawns are its children), labels, a
budget/depth/`max_children` grant that bounds its whole subtree, an inbox,
`agent_list`/cockpit presence, `agent_kill`, and `logs`/`tail` over
its stdout and stderr.

Spawn it with kind `script` and a spec naming the pymodule — `rafiki
create -d --kind script --pymodule <repo>:<script> [-- args]`, Connect
`Spawn` with `script{repo, script, modules, args}`, or (from an agent) the
`pymodule_start` tool. `repo: "local"` runs one of the spawning owner's own
saved modules. The fundi/claude-only fields (model, tools, skills, MCP,
prompts, sessions) are refused on the spawn, and a script child cannot be
resumed: its exit IS its result.

The child's name defaults to the script's name on both entry points.
`pymodule_start` is gated on the MCP face exactly like `pymodule_run` (a
child with a live executor binding; the human uses `rafiki create`), and a
fundi child gets it from its registry whenever it has a spawner. The CLI
never sends a profile default or remembered model to a script spawn — a
script child has no model — and on a daemon with no executor pool its
executor pre-flight is tolerated rather than fatal, since the local fallback
hosts the child.

Its only channel to the daemon is a per-child unix socket, reached through
the `RAFIKI_CHILD_CONNECT` environment variable. The socket is the
credential — the process never holds a token, and whatever it sends arrives
at the daemon as itself, subtree-scoped on Connect like any other per-child
credential. The socket proxies the daemon's WHOLE proxy face, request path
preserved — not just the Connect route — so the injected child credential
authorizes Connect verbs AND LLM-face calls, every request attributed to the
child and its owner; any inbound `Authorization`/`X-Rafiki-*` header is
stripped first, so a caller cannot smuggle a stronger identity past the
injection. The process environment is the daemon's full environment minus
every `RAFIKI_*`/`ANTHROPIC_*`/`OPENROUTER_*` variable, so no daemon
credential leaks into a process the operator did not give them to.

Exit 0 settles the child `done`, anything else `failed`; the parent's settle
notification carries the child's `SetResult` payload (or the last 4 KiB of
its stderr when it never set one). A locally hosted script dies with its
daemon; recovery settles it `failed` with the reason "daemon restarted"
rather than leaving it running-with-no-process. Progress reports from a
script into its parent's event buffer coalesce like any subagent fragment,
and the buffer does not busy-defer them for a RUNNING script child — a
script has no turns to corrupt, so a coordinating script waiting on its
`Receive` stream hears its workers' settles after the debounce, not after
the 60s `RAFIKI_EVENTBUF_MAX_WAIT_MS` ceiling.

Killing a script child signals its process group: the script's own
subprocesses die with it. Its daemon-managed descendants (children it
spawned through its socket) are NOT swept — the same boundary every kind
has — so they keep running, keep their budget fence, and can be killed by
name.

A script child's output is published to its durable event log as coalesced
`script_output` events (4 KiB/250 ms, whole lines — see
`docs/reference/control-protocol.md` for the coalescing contract), which is
what `rafiki logs`/`rafiki tail` and the cockpit replay — the record lives on
the daemon, so watching works against a remote daemon, not just a local one.
From spawn to exit the child's status is `running` (the rail's ▶ glyph;
output never moves it — a script has no turns, so the idle/streaming
machinery never applies), and its `cost_usd` is its SUBTREE's spend: the
script drives no LLM turns of its own, but the children it spawned do, and
client-side totals (`rafiki list`'s TOTAL, the cockpit rail) stop descending
at a script node so the subtree is never counted twice.

**Where scripts run.** When an executor pool is configured and one of its
executors was started with `--launch script`, the child is hosted ON that
executor under daraja: the executor resolves the pymodule from its own synced
cache (so repo checkouts and requirements venvs work, exactly like
`pymodule_run` there), serves the child's per-child socket against the
daemon's face over the same TLS the executor itself dials with, and never
respawns a script — its exit is its result. `--launch script` implies the
owner's pymodule corpus sync and repo refreshes on that executor
(`--pymodules-sync=false` / `--repo-sync=false` refuse them
explicitly). With no script-capable executor, the daemon hosts the child
locally (the owner's own saved modules only; no git repos, no venvs).

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

## Subagents

An agent can spawn and steer its own descendants, regardless of its own
kind — `fundi` and `--kind claude` both get this surface (a `claude` agent
reaches it through the injected MCP agent-control tools described under
[Running locally](development.md#running-locally) below), and `kind` on a spawn picks the
child's runtime independently of the parent's, so the two mix freely in one
tree:

| Tool | Purpose |
|---|---|
| `agent_spawn` | start a subagent (`kind: fundi\|claude`); returns a handle immediately, does not block |
| `agent_list` | your subtree — id, name, model, status, assigned task |
| `agent_view` | the tail of a descendant's transcript |
| `agent_send` | steer a descendant mid-flight, or give it more work |
| `agent_route` | steer a descendant's provider routing — `prefer`/`sort`/`quant`; `only` is operator-only |
| `agent_kill` | stop a descendant and everything below it |
| `agent_models` | the models you may spawn on |

Every verb naming another agent is checked against stored lineage (an agent
may only see/steer/kill its own descendants) — read from `childstore`, never
from a tool argument, since arguments come from a model that can be
prompt-injected. Completion is a **signal**: `agent_spawn` returns as soon as
the child is registered, and settlement lands as one coalesced digest per
parent turn regardless of how many descendants finished together. Background
jobs (`bash_start`) settle the same way.

### Scheduling

Four tools let an agent run a prompt later — once or on a recurring schedule —
bound to the calling child, on the same agent-control surface as the verbs
above: native tools for a `fundi` child, and `mcp__rafiki__CronCreate` etc. for
a `claude` child over the injected MCP face.

| Tool | Purpose |
|---|---|
| `CronCreate` | schedule a prompt on a 5-field cron expression, evaluated in the owner's zone; `recurring: false` for a one-shot; returns a job id |
| `CronDelete` | cancel a job by id |
| `CronList` | your own pending jobs |
| `ScheduleWakeup` | wake yourself later (delay clamped to 5–60 min); `stop: true` cancels it |

A job survives daemon restarts, fires into the child's inbox queue whether or
not the child is busy, never expires, and is held while the agent tree is over
budget. See `docs/reference/agent-schedules.md` for the full contract.

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
and `workspace` (`ephemeral`/`pinned`, see [workspace lifecycle](executors.md#workspace-lifecycle)) — the entire placement
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
| **concurrency** | `--max-children`, `agent_spawn(max_children=…)` | `10` | — |

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
