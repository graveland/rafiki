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
`anthropic/<tier>-latest` names (`anthropic/opus-latest`), OpenRouter slash
ids, and a Responses-kind operator provider's `<provider>/gpt-<tier>-latest`
(`gpt/gpt-terra-latest`); a script child takes no model at all. Your own model
aliases (`rafiki models alias`, see
[the proxy guide](proxy.md#model-aliases)) are offered wherever their target
is eligible — an Anthropic-target alias in either kind, an OpenRouter-target
one in `fundi` — and a slash-free `--model` that names no alias is refused.
`fundi` needs
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
at the daemon as itself, bounded on Connect like any other per-child
credential — its subtree for acts, its owner for reads. The socket proxies the daemon's WHOLE proxy face, request path
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
| `agent_spawn` | start a subagent (`kind: fundi\|claude`); returns a handle immediately, does not block. `fork_from` or `from_tag` starts it from a point in another agent's history, by reference |
| `agent_list` | your subtree — id, name, model, status, assigned task; `scope="owner"` also lists every other agent your owner owns, marked read-only. One page is at most 50 agents (`status=` narrows it): a longer list keeps the first 50 and appends an exact "N more agents not shown" trailer, for every caller including a user credential's whole-daemon list |
| `agent_view` | a readable agent's transcript — your subtree's, or another agent your owner owns: the newest entries, or a page (`before=`/`after=`), one entry in full (`entry=`), or a search (`find=`) by the cursors its markers print. Reads LIVE agents only; a closed agent's conversation is read with `conversation_export` instead |
| `agent_send` | steer a descendant mid-flight, or give it more work |
| `agent_route` | steer a descendant's provider routing — `prefer`/`sort`/`quant`; `only` is operator-only |
| `agent_kill` | stop a descendant and everything below it |
| `agent_models` | the models you may spawn on |
| `agent_tag` | create a named, scoped handle others in its scope may fork from, or move one by `tag_id` + `expect_node_id` |
| `agent_untag` | tombstone a tag you created, or one of your descendants created |
| `agent_tags` | the tags you can see, in resolution order (your own lineage, then your ancestors', then tree, then owner, then tags rooted at your descendants) |

Reads widen to your owner while acts stay on your tree: `agent_list` and
`agent_view` admit any agent your owner owns — your own subtree, or a sibling
tree's — while `agent_send`, `agent_kill`, `agent_route` and
`agent_set_budget` reach only your own descendants. Both checks read stored
state from `childstore`, never a tool argument, since arguments come
from a model that can be prompt-injected; a foreign owner, an unknown id and an
agent with no owner all get the one identical refusal. Completion is a
**signal**: `agent_spawn` returns as soon as
the child is registered, and settlement lands as one coalesced digest per
parent turn regardless of how many descendants finished together. Background
jobs (`bash_start`, and a `bash` command promoted to a job at its timeout)
settle the same way. That is why both runtimes' prompts tell a coordinator to
end its turn instead of polling — fundi's `## Waiting` section and
`claudeargv.CoordinationPrompt`'s waiting paragraph: a turn held open by
`sleep` cannot receive the settlement until it ends. The MCP log channel is a
different channel and no substitute: it carries a *top-level* child's
settlement to its owner's sessions, is dropped unless the client negotiated
`logging/setLevel`, and never carries a worker's own children.

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

## MCP servers and OAuth

A child's MCP servers come from an `.mcp.json` file (`--mcp-config`, else
`./.mcp.json`, else `$RAFIKI_MCP_CONFIG`, else the config directory's
`mcp.json`), and an entry is either a local process (`command`) or a direct
`url`. A direct-URL server that requires OAuth is logged in **explicitly**, by
the user, with `rafiki mcp login`:

```
rafiki mcp login linear
rafiki mcp login linear --scope tools:read --scope tools:write
rafiki mcp login linear --no-browser          # browser on another machine
rafiki mcp logout linear
rafiki mcp logout --resource https://mcp.example.net/mcp
```

The daemon runs the whole flow — SDK discovery, dynamic client registration,
the code exchange, and an authenticated `initialize` + tool listing to prove
the grant works — and stores the result encrypted against **your** user id. The
CLI only ferries the consent URL to a browser and the redirect query back, so
the access token, the refresh token and the client secret never leave the
daemon and never enter a child process.

Two things the CLI does print, which are worth knowing before you run it: the
authorization URL (so you can open it, or paste it when the browser is on
another machine) and, on success, the server, resource and tool names. The
authorization URL carries the configured endpoint **verbatim** as its RFC 8707
`resource` parameter — that is the protocol's own requirement, not an extra
field — so an endpoint whose own query holds something you would not paste into
a browser is visible to whoever reads that output. Nothing else about the
credential is.

What a login is **not**:

- **Not interactive during a spawn.** Nothing prompts while a child starts.
  A child whose server answers 401 gets a skipped server with the hint
  ``server X needs a login: run `rafiki mcp login X` `` — that child keeps its
  other tools, and the fix is a login you run.
- **Not automatic from a project file.** An entry has no `oauth: true` switch,
  and `MCPServerConfig` gained no field: the credential is out of band.
- **Not available for a stdio server** (a local process needs no OAuth) or for
  a `via_executor` server (its argv, environment and credentials live on the
  executor machine; the daemon cannot log in on its behalf). Not for an entry
  that sets its own `Authorization` header either — that header deliberately
  disables stored OAuth, and the login is refused with a message telling you to
  remove it. Unrelated headers coexist with a stored grant.
- **Not usable across users.** A grant belongs to the user who made it. An
  admin gets only their own grants, and a child credential cannot start,
  finish or delete one.

The config path is the **daemon's**: the daemon reads the absolute path the CLI
resolves, so with a remote profile that file must exist on the daemon host.
Nothing uploads a project file and the CLI never reads or parses one; it only
stat's `./.mcp.json` to decide whether the working directory's copy is the path
to name — which is why running from a project directory points the daemon at
that directory, whether or not it has one.

A login may take more than one consent round (a server can ask for a wider
scope once it has seen your tool list). Each round opens a new URL on the same
login, and the grant is stored only when the last round succeeds — a
continuation is never reported as a successful login. The callback is served on
port 1456 at `/mcp/oauth/callback` on 127.0.0.1, the redirect URI rafiki
registers: a busy port is an error naming the port (free it and retry, or use
`--no-browser` and paste the redirect URL), never a silent substitution.

Once a grant exists, the child spends it **passively**: the daemon resolves the
exact configured endpoint's binding (no rediscovery, no guessing a resource from
the URL's origin), asks the database for the credential on every request, and
refreshes under a row lock. A logout invalidates locally first, so a revocation
that fails upstream does not bring the grant back; a running child then fails
its next authenticated MCP request with the re-login hint.

OAuth's reach is bounded by the daemon operator's `[mcp_oauth]` section in
`rafikid.toml` (`allow_hosts`, `allow_cidrs`, `allow_http_hosts`): public HTTPS
only unless the operator says otherwise. A project entry, a login flag or a
discovery response cannot widen it.

Two boundaries worth stating plainly: **Claude children do not get stored MCP
OAuth credentials** — nothing is injected into a claude child's argv,
environment or staged MCP config, so a claude child's direct-URL servers behave
exactly as they did before (its own MCP client does whatever the entry says) —
and **an executor never performs an OAuth login**, so a `via_executor` server
gets its credentials from the executor's operator, not from rafiki.
