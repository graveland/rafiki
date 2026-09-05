# Executor protocol

The daemon dispatches filesystem and shell tool calls to an executor process
(`rafiki executor serve`) over Connect RPC. This document describes the wire
protocol: the ten RPCs, their message shapes, the four failure codes, the
background-handle lifecycle, the workspace lifecycle, and the mtime contract.

There are two transports for the same protocol, and they differ only in what
carries the bytes:

| Transport | Command | Used when |
|---|---|---|
| Reverse-dialled TLS | `rafiki executor serve --connect` | the daemon cannot reach the executor's host (NAT, a laptop) |
| Unix socket (reverse) | `rafiki executor serve --connect-socket` | executor and daemon on one host, enrolling as a fully rowed member of the pool |

There was once a third form: the executor listened on a unix socket and the daemon dialled
it with `rafiki create --executor-socket`. It is gone. It produced an executor with **no
database row** — no labels, no `admits`, no `isolation`, no `workspace_mode`, invisible to
selectors and never inherited by a child — which made it the direct cause of two separate
confinement defects. `--connect-socket` reverses the direction so the executor enrolls, and
the row is again the only authority on what an executor is.

A container executor uses one of those two like any other host. There was once a
third — `rafiki executor serve-stdio`, spoken over the stdio of a `docker exec
-i` — because rafiki started the container itself and gave it no network. It
does not start containers any more, so the container has whatever network the
operator gave it in `docker run`, and the stdio transport had no callers left.

A fourth transport is the **client-run executor**: `rafiki create` and `rafiki
attach` start an executor in-process and reverse-dial the daemon, so the
operator's own machine becomes the workspace by default. It is not a new wire
path — it uses `--connect`'s TLS transport when `RAFIKI_URL` names a remote
daemon, and `--connect-socket`'s unix path when the daemon is local.

## Transient executors

A client-run executor is **transient**: it has **no database row** at all. The
daemon mints a one-shot session ticket over the already-authenticated control
connection, the client connects with the ticket (`ExecutorHelloRequest.Ticket`),
and the executor lives exactly as long as the control connection that asked for
it. Closing the connection revokes the ticket and evicts the executor from the
pool.

This confronts a documented invariant — "the row is the only authority on what
an executor is" — so the resolution is explicit rather than an exception. The
row is authority because an operator wrote it and the machine it describes
cannot assert it. For a transient executor, every such fact comes from the
**authenticated control connection**, which is a stronger source, not a weaker
one:

| Fact | Durable | Transient |
|---|---|---|
| `owner` | operator label | authenticated user on the connection |
| `admits` | operator | `owner=<that user>`, daemon-written |
| `isolation` | operator | `none` — it is the operator's own terminal |
| `workspace_mode` | operator | `pinned` |
| `roots` | operator claim | the cwd the client reported |
| `machine` | trust label at mint | the operator-written name the client read back, validated |

Nothing is self-reported, so `SelfReported`'s rule is not bent. The invariant
generalises to: **every access-gating fact is written by the daemon from
something it verified — a row an operator wrote, or a connection it
authenticated.**

Consequences:

- `refreshRow` must skip transient executors: `store.Get` answers `ErrNotFound`
  for a row that never existed, and `refreshRow` correctly treats that as
  "this row was deleted, evict now", killing a healthy executor on its first
  health tick.
- `liveConn.transient` marks an executor with no row. `Live()` returns it like
  any other; selection, admission and narrowing are unchanged.
- Because the ticket is per-connection, two concurrent TUIs on one machine get
  two distinct executors rather than colliding on one per-machine credential.

The selector returned by `ctrl_executor_session` is `owner=<user>,machine=<name>`
in BOTH the durable and transient cases, so a child can move between the two
without its stored selector — the thing its whole subtree inherits — ever being
rewritten. The durable executor is preferred (durable before session in
`sortCandidates`), and the transient one is a zero-config fallback.

## Reaching the daemon

The reverse-dial transports are reached at a **path** on the daemon's shared
listener, upgraded out of HTTP/1.1 — the TLS listener and the unix executor
socket mount the same handler:

```
GET /executor/connect HTTP/1.1
Upgrade: rafiki-executor
Connection: Upgrade
        ↓
HTTP/1.1 101 Switching Protocols
        ↓
{"type":"executor_hello",…}\n     then HTTP/2, roles inverted
```

One port and one certificate therefore serve the control plane (`/control`) and
the executor link (`/executor/connect`). When the daemon and executor are on
one host, the executor socket (`executor.sock` in the runtime directory) is the
same handler without TLS. Enable either with `RAFIKI_EXECUTORS_ENABLED=1`; the
executor link has no listener of its own.

Two consequences worth knowing:

- **ALPN is no longer load-bearing.** The outer connection negotiates
  `http/1.1` — net/http can only hijack an HTTP/1.1 connection — and the
  inverted HTTP/2 begins after the 101, directly on the byte stream, where ALPN
  is not consulted. Agreement is the `Upgrade` header's job now, and a mismatch
  fails with a readable HTTP status instead of a TLS alert.
- **An HTTP proxy can carry it.** An Upgrade tunnel is the same machinery as
  WebSocket, so an ingress may terminate TLS and forward rather than requiring
  TCP passthrough. Terminating means the daemon↔executor hop is no longer
  end-to-end encrypted and `--pin-cert` pins the proxy's certificate, so that is
  a deployment choice rather than a default.

## Transport

- **Protocol:** Connect (gRPC-compatible) over HTTP/2 cleartext (h2c).
- **Socket:** Unix domain socket, filesystem mode `0600` — no authentication
  beyond filesystem permissions in this phase.
- **Roles:** `rafikid` (the daemon) is the RPC client; the executor is the
  server. On the reverse-dialled transport the executor DIALS and then serves on
  what it dialled, so the TLS roles and the HTTP roles are inverted relative to
  each other — see pkg/execpool's package comment.
- **Codec:** Binary protobuf by default; JSON also supported (Connect's
  auto-negotiation).

## Enrollment handshake

Over the reverse-dialled transport the executor sends one newline-delimited
JSON frame before any HTTP/2 framing, and rafikid answers with one:

```jsonc
// executor -> rafikid
{"type":"executor_hello","token":"<enrollment token>"}      // first join
{"type":"executor_hello","credential":"<durable credential>"} // thereafter

// rafikid -> executor
{"type":"executor_hello","executorId":"…","credential":"…"}  // success
{"type":"executor_hello","error":"…","retryable":true}       // failure
```

Both sides must read this frame **byte at a time**. A buffered reader that
consumes past the newline swallows the start of the peer's HTTP/2 stream, and
the connection then dies with an unhelpful protocol error rather than a
diagnosable one.

### `retryable`

`retryable` discriminates *"rafikid could not check this credential"* from
*"this credential is not valid"*, and it decides whether the executor exits:

| `retryable` | Meaning | Executor behaviour |
|---|---|---|
| absent / `false` | A decision about the credential — unknown, consumed, expired, disabled, no such row, or a `machine` name already claimed by another executor of the same owner. | Stop. Retrying cannot un-revoke a row, nor free a taken name. `Connect` returns `ErrEnrollmentRejected`. |
| `true` | The store could not be reached or read. | Keep reconnecting with backoff. |

Absent means terminal, so an older daemon's responses behave as they always
did.

An unclassified error is reported as **retryable**. The failure directions are
not symmetric: quitting on a genuinely dead credential costs a log line, while
quitting on a transient one takes the machine out of service permanently — and
because executors reconnect together, one database restart would otherwise
take the entire fleet down.

The `error` string for a retryable failure is deliberately generic. The peer
has by definition not proved who it is, and a store error routinely carries a
DSN, a hostname or a query; the real error goes to rafikid's log.

A *terminal* failure does forward its text verbatim, so every terminal answer
must be a sentinel whose own message is written for the operator reading the
executor's log. The name collision is the one that carries real advice — the
enrollment token was minted with a `--name` another executor already holds, and
the fix is a token with a different name, or relabelling the existing row — and
it must never be the store's own text for the same DSN reason.

## RPCs

All ten RPCs belong to the `rafiki.executor.v1.ExecutorService` service.
`rafiki.admin.v1.AdminService` adds two more (`Launch`, `Reap`) on the same
connection — see [AdminService: Launch and Reap](#adminservice-launch-and-reap).

### Describe

```
Describe() → { executorId, platform, roots[], concurrency, isolation,
               workspaceMode, tools[], version, selfReportedLabels,
               proxies[], launchKinds[] }
```

Unary. Called at startup and periodically to discover the executor's
capabilities. `tools[]` is the executor's *actual* served set — the names its
registry materialized, not a static list — and it is what the parent builds the
child's `tools[]` from at spawn, so a name that isn't here never reaches the
child's envelope. It includes the `lsp_*` verbs only when a language server is
installed (or `--lsp-config` names one), and never the parent-side
`bash_start`/`bash_output`/`bash_kill` RPCs, which the daemon implements itself.

`proxies[]` and `launchKinds[]` are the two things an executor self-reports, and
both are safe to self-report for the same reason: each only ever NARROWS what
the executor will do. `proxies[]` names the LLM endpoints its operator declared
with `--proxy` and the relay enforces on this side — a name that is not declared
never reaches the network. `launchKinds[]` names the child protocols its
operator declared with `--launch` that the daemon may host here via
`AdminService.Launch`; the daemon still requires the child's ordinary executor
selector to match, so a wrong entry costs a failed launch rather than admitting
anyone. Both default to EMPTY: with no flag the executor forwards nothing and
hosts nothing. A machine volunteering to host other people's children because
someone forgot a flag is the self-report-gates-placement shape the isolation and
workspace_mode rules exist to forbid — unlike those two fields, which the
executor does not report at all.

### Health

```
Health() → { load, runningHandles[], diskFree, draining }
```

Unary. Liveness and load snapshot. `runningHandles` names every non-exited
background job; the daemon polls this to surface what's long-running.

### Execute

```
Execute(callId, tool, inputJSON, timeoutMs, expectMtime, background, workspaceId)
  → stream { Output(chunk) | Result(content[], isError, observedMtime)
            | Failed(code, msg) | handle }
```

Server-streaming. Dispatches a single tool call. `workspaceId` selects the
workspace this call runs in — empty means the executor's own root, the
pre-phase-08 behaviour kept as a compatibility path so an executor and a
daemon of different vintages still interoperate.

The normal path is:

1. The server checks `expectMtime` — if any path's current on-disk mtime
   does not match, it immediately streams `Failed(CODE_DENIED, …)`.
2. If `background=true` and `tool=bash`, the server starts the command as
   a background job and streams only the handle.
3. Otherwise, the tool runs synchronously; the server streams
   `Result{content[], observedMtime}` on success or `Failed{…}` on error.

**Result carries typed content**, not a bare string. `content[]` holds
`ContentBlock` entries that may be text or (in a future phase) images.

### Attach

```
Attach(handle) → stream { Output(chunk) | exitCode }
```

`Attach` streams a job's output from the beginning of what is still retained,
then incremental deltas every 200ms, then a final `exit_code`. A single reply
carries at most 100 KB, taken from the END — a poll wants the newest output —
and a reply that had to clip is prefixed with a note naming the FILE holding
the fuller record, which the model can `read` or `grep` on the same executor.
An unknown handle is `CodeNotFound`. Dropping the connection does not kill the
job — reattaching resumes from what is still retained.

### Provision

```
Provision(childId, workspaceMode, mounts[], workdir, network, memoryBytes, cpus, env)
  → { workspaceId, roots[], workdir, isolation }
```

Provision prepares a workspace and returns the handle later `Execute` calls
carry. A workspace is a handle bound to the root the executor was started with;
there is nothing else it could be, because rafiki does not start containers — a
container running the executor IS one.

Everything describing a shape the executor might build is therefore dead on this
call. `mounts`, `network`, `workdir` and `workspaceMode` are unset by the daemon
and ignored by the executor; they remain on the wire only until the proto is
next regenerated.

`isolation` in the response is deliberately **empty**, as it is in `Describe`.
An executor does not know whether it is running in a container and must not
guess: the answer gates where other people's children may run, so the
authoritative copy is the `isolation` column on the executor's row, written by
the operator at token-mint time. A daemon that reads it from here gets "none"
for every child on every machine.

### Release

```
Release(workspaceId) → {}
```

Unary. Tears a workspace down. **Idempotent:** a daemon restart that lost
track of a workspace must be able to release it again without error. A
released workspace kills **its own** background jobs — a job in a released
workspace is not a job, and reporting it as running is worse than reporting it
gone. It must not touch any other workspace's jobs.

### ProjectContext

```
ProjectContext(workspaceId) → { contextFiles }
```

Unary. Returns the instruction files belonging to a workspace: `CLAUDE.md` and
`AGENTS.md` at the git root and at the workdir, concatenated with a blank line
between sections. An `@include` reference in any of them is expanded **on the
executor** — it names a path on that filesystem, which the daemon cannot
resolve — so the returned text is fully inlined, never a dangling `@` line.

It takes a `workspaceId`, not a path, for the same reason `Execute` does: a
request naming a directory would let the daemon — or anything that reached the
executor — read instruction files anywhere on the machine. The workspace handle
already scopes it. An unknown handle is `CodeNotFound`, answered rather than
resolved against the executor's root, or the handle is a formality.

Empty `contextFiles` is the **ordinary** answer — most workspaces have neither
file — and is not an error. It is a separate call rather than a field on
`ProvisionResponse` because it returns content, and content is unbounded where
a workspace handle is not: a large `CLAUDE.md` would make every provision pay
for it. It is separate from `Describe` for a different reason — `Describe` is
per-executor and this is per-workspace.

### ProjectSkills and SkillBody

```
ProjectSkills(workspaceId) → { skills: [{name, description, dir}] }
SkillBody(workspaceId, name) → { body, dir }
```

Two calls rather than one, because an inventory is a handful of lines per skill
while a body is a document — returning every body at spawn would put the whole
project's skill text into every child. `ProjectSkills` is called at spawn;
`SkillBody` on the turn the model actually asks for one.

**Only the project tier is served.** `paths.SkillsDirs()` is the daemon operator's
own skill library, not the workspace's; discovering it on the executor would give
a child two copies of every operator skill and let an executor's local library
shadow the operator's. The two directories scanned are `<cwd>/.claude/skills` and
`<cwd>/.rafiki/skills`.

**SkillBody resolves by name, not by path.** A path parameter would let anything
reaching this executor read an arbitrary file — the same reason `ProjectContext`
takes a `workspaceId` rather than a path. An unknown name is `NotFound`, not an
empty body (which a model would read as a skill that exists and says nothing).

An empty inventory is the ordinary answer for a workspace with no project skills,
and is not an error.

### Cancel

```
Cancel(callId) → {}
```

Unary. Terminates the background job named by `callId`. Does not wait for
the process to reap.

### `JobOutput`

One-shot poll of a background job. Never blocks.

| Field | Direction | Meaning |
|---|---|---|
| `handle` | → | the handle returned by a background `Execute` |
| `since` | → | byte offset into lifetime output; pass back the previous `total` |
| `data` | ← | bytes from `since` to `total`, capped at 100 KB from the end |
| `total` | ← | bytes the job has ever written, including any not returned |
| `exited` | ← | whether the process has been reaped |
| `exit_code` | ← | meaningful only when `exited` |
| `found` | ← | false when the handle is unknown, or its output was evicted by the workspace's byte budget |

`Attach` remains the streaming path. Use `JobOutput` when you want a snapshot.

## AdminService: Launch and Reap

`rafiki.admin.v1.AdminService` is the machine-admin surface: starting and
ending daraja hosts. It is deliberately NOT part of `ExecutorService` — the
executor is a data path for one child's filesystem and shell tools, with its
own lifecycle hazards, while a daraja outlives both the turn and the
connection that asked for it. The two services share the executor's process
and its reverse-dialled connection and nothing else: AdminService mounts on
the SAME mux as ExecutorService, so one connection reaches both. Like the
capability it serves it is opt-in — an executor started without `--launch`
mounts no AdminService at all, and neither do the short-lived enroll and
session-executor surfaces, which host nothing by construction.

### Launch

```
Launch(childId, cwd, spec: rafiki.daraja.v1.ChildSpec, dialAddr, ticket string)
  → { pid, pgid }
```

Starts one `rafiki daraja serve`, re-executed from the executor's own binary,
hosting one child. `spec` is typed rather than raw argv because the executor
and daraja would otherwise each need an argv builder on opposite sides of an
RPC; both call `pkg/claudeargv` instead.

`dialAddr` is a host:port or Unix socket path that daraja dials back to the
rafikid that asked for it — the reverse-dial pattern replaces the 1b-i direct
connect where the caller dialled a socket path returned by the response.
`ticket` is a one-shot credential delivered via environment variable
(`RAFIKI_DARAJA_TICKET`, never argv — `ps` visibility is why). It is replaced
by a durable credential on first successful hello.

`ChildSpec.ClaudeParams` also carries Phase 2's passthrough-billing fields —
`proxy_url`, `proxy_token`, `passthrough_auth`, `auto_compact_window`,
`record_requests` — all LAUNCH-ONLY (daraja reads them once, at its own
process startup, to build the environment `proxyenv.Claude` produces; a later
`Restart`'s spec may leave them unset and daraja ignores them there, since env
is fixed for a daraja process while only argv is rebuilt per restart). The
executor forwards the four non-secret ones as `daraja serve` flags
(`--proxy-url`, `--passthrough`, `--auto-compact-window`,
`--record-requests`) and, for `proxy_token` alone, via environment
(`RAFIKI_DARAJA_PROXY_TOKEN`) — the same ps-visibility treatment the ticket
gets, and for the same reason.

`Setpgid` makes daraja a group leader and claude joins the group (daraja spawns
its child with `SpawnSpec.InheritProcessGroup`, opting out of the runner's
default own-group). `pgid` is therefore the one handle that reaches the whole
child for its whole life: restarts stay in the group, and one `kill(-pgid)`
still reaches a claude orphaned by a SIGKILLed daraja — darwin has no
subreaper, so the orphan reparents to launchd and the group is what covers it,
not any wait by this executor.

Refusals:

| Condition | Code |
|---|---|
| empty `childId` | `InvalidArgument` |
| kind other than claude | `InvalidArgument` |
| kind not declared with `--launch` | `FailedPrecondition` — the operator's declaration wins |
| `childId` already hosted here | `AlreadyExists` |

`pgid` is NEVER taken from the request: a process group id is recycled once its
group empties, so signalling a number a peer handed over could reach an
unrelated group. Reap resolves the pgid from this executor's own launch table,
and drops the entry when daraja exits — which is what makes a later Reap
against a recycled pgid answer `reaped=false` instead of signalling a stranger.

### Reap

```
Reap(childId, graceMs) → { reaped }
```

Ends one launched daraja and its child: SIGTERM to the process group, wait out
`graceMs` (zero means the server default, 3s — matching daraja's own
`stopLocked` rather than inventing a second escalation policy), then SIGKILL
the group. An unknown `childId` returns `reaped=false`, NOT an error —
reaping something already gone is the normal case, so Reap is idempotent.

### The `socket` field is retired

1b-i returned `LaunchResponse.socket` — a Unix path for the caller to dial
directly into `DarajaService`. 1b-ii replaces that with a reverse-dial pattern:
the caller passes its own address as `dialAddr` on the request, and daraja dials
back. The `socket` field (number 3) is reserved and no longer served.

### Wire break in 1b: `RestartRequest`

`RestartRequest.spec` (typed `ChildSpec`) replaced `RestartRequest.argv` in 1b.
A 1a client speaking raw argv is incompatible with a 1b daraja.

## Not implemented: MCP servers on an executor

MCP servers are defined **at the daemon** and run there. This section records a
design that was worked through and deliberately not built, so the next person
starts from the decisions rather than from the question.

**Why it would be worth building.** A sidecar next to rafikid can serve a
*service* MCP server — GitHub, Slack, Linear — perfectly well over
`MCPServerConfig.URL`, and should. What it structurally cannot serve is a
**workspace-local** server: a filesystem or git server pointed at the project.
Those need to run where the checkout is, and the checkout is on the executor,
possibly a laptop behind NAT that rafikid cannot reach at all. The axis is
therefore **"does this server need to see the workspace"**, not "is it stdio or
HTTP" — that is the sentence worth remembering.

A second case only an executor can serve: an MCP endpoint reachable on the
executor's network — a cluster-internal service, something behind its VPN —
which rafikid has no route to.

**The shape.** One **bidirectional streaming** RPC carrying opaque bytes, opened
by the daemon on first use and reopened if it drops. rafikid stays the MCP
client throughout; the executor either execs a server and pipes its stdio, or
relays to a URL only it can reach. The stream-open message carries
`{command, args, env}`.

This does **not** violate §Transport's rule that *"rafikid is the RPC client;
the executor is the server"*. That is about which side initiates calls, and the
executor still never does. A bidi *stream* inside a call the daemon opened
leaves the property intact — the design doc's "no bidirectional transport" means
neither side is an RPC client to the other, not that a single RPC cannot stream
both ways. The two were conflated once already, and the mistake cost a wrong
"this is blocked" verdict.

**Credentials.** stdio MCP is JSON-RPC over a pipe with no header channel, so a
credential cannot ride the messages; it reaches the server through its process
environment (`MCPServerConfig.Env`). On an executor that means the token is
readable at `/proc/<pid>/environ` by any same-uid process — including the
agent's own `bash`. That is **accepted**: an executor holding the workspace
already holds the source, `.git/config`, `.env`, and on a laptop the operator's
SSH keys, so a service-scoped MCP token is not a meaningful addition to the
pile. `URL`-mode servers avoid the question entirely, because rafikid applies
`Headers` before the bytes leave it.

If that ever stops being acceptable, the escape hatch is an **MCP-only
executor**, serving no workspace tools so no agent-controlled process shares its
uid. Note that is not a label: a child gets exactly one executor
(`resolveExecutor` returns a single client, `ToolOpts.Executor` is one field), so
MCP-only means a child needs *two*, which is an architectural change.

**A hazard to design for.** MCP discovers tools at `initialize`, but a child's
`tools[]` is fixed at build because the tool block sits inside the prompt-cache
breakpoint. A reconnect that returns a different tool list therefore cannot add
to it. Pin to the first discovery: tools that vanished return `is_error`, tools
that appeared are ignored until the child restarts.

**Before building any of it**, spike Connect bidi streaming over
`ServeInverted`'s inverted HTTP/2. That transport has never carried a bidi
stream, it is the least conventional part of this system, and a subtle bug there
presents as intermittent tool-call failures under load.

**Related, already fixed:** a stdio MCP server used to inherit the daemon's
entire environment — `RAFIKI_DB`, `RAFIKI_TOKEN`, and both provider API keys.
`mcpServerEnv` now filters through `paths.IsReservedEnvKey`. If executor-hosted
MCP is ever built, the stream-open message must carry **only** the configured
`Env`, never the daemon's `os.Environ()`, or that leak returns with the whole
executor fleet as its blast radius.

## Failure codes

Failures are typed, never collapsed into a bare string. The parent makes a
different retry decision for each code:

| Code | Meaning | Retry? |
|---|---|---|
| `CODE_TOOL_FAILED` (3) | The tool itself errored (missing file, unknown tool name, bash exit ≠ 0 in a *synchronous* call). | Yes — the error is tool-specific. |
| `CODE_DENIED` (1) | Structural refusal (stale mtime, sandbox violation, tool not permitted). | **Never.** The condition is permanent for this request. |
| `CODE_EXECUTOR_LOST` (2) | The executor disappeared mid-call. | Yes — a different executor may still serve the request. |
| `CODE_TIMEOUT` (4) | The tool call exceeded `timeoutMs`. | Yes — a longer timeout may succeed. |

**`bash` exit ≠ 0 in a synchronous call is a `Result` (with `is_error=true`),
not a `Failure`.** That is existing behaviour and the protocol preserves it.
A non-zero exit inside a background job is reported via the exit code in
`AttachResponse`.

## Background-handle lifecycle

```
Start: Execute(background=true, tool=bash) → stream { handle }
Run:   Attach(handle) → stream { Output*, ExitCode }
Kill:  Cancel(callId) → {}
```

- A handle is the `callId` from `ExecuteRequest`. The parent picks it.
- A job's output goes to a FILE on the executor, retained with drop-oldest at
  8 MB, and a single reply returns at most 100 KB of it from the end. Byte
  offsets are tracked against a monotonic total, so a re-`Attach` after data
  has been dropped can tell the caller it missed output rather than silently
  replaying or skipping bytes — and unlike the in-memory ring this replaces,
  what it missed usually still exists, at a path the reply names. That is the
  same "spill, never destroy" rule foreground tool results follow; background
  output was the one place output was both unbounded and thrown away.
- **Retention has no time limit.** A finished job's output lives until its
  workspace is released, which is exactly as long as the agent that might ask
  for it. A wall-clock window cannot work here: an async agent's turn can end
  and resume hours later, so a 10-minute timer expired output while the agent
  that started the job was still alive and idle. Memory is bounded instead by
  a per-workspace BYTE budget (256 MB by default, `--job-output-budget-mb`),
  evicting the oldest FINISHED job first. Running jobs are never evicted:
  their output is a live stream, not an archive.
- Background commands are never rewritten through `rtk`, whatever `--rtk` says.
  The rewrite is only safe because the foreground path can watch stderr after
  the command exits and re-run the original when rtk itself refused; a
  long-running job offers no exit to inspect and no output it could take back.
- A job survives a dropped `Attach` connection — the parent may re-attach
  at any time.
- `Health().runningHandles` lists every handle whose process has not yet
  exited.

## Mtime (TOCTOU) contract

The `FileTracker` splits state from check: the **parent** holds the map
(conversation state that survives executor respawn), and the **executor**
verifies at the moment of the write.

1. After a successful `read` (or `write`/`edit`), the executor populates
   `Result.observedMtime` with the on-disk mtime of each file touched.
2. The parent stores these in its own `FileTracker`.
3. On a subsequent `write` or `edit`, the parent sets
   `ExecuteRequest.expectMtime` to the values it previously recorded.
4. The executor stats each path **before** dispatching the tool. If any
   mtime differs, it streams `Failed(CODE_DENIED, …)` and the write does
   not proceed.

The stat happens in the executor process, which is the only place the file is
visible: if that process is in a container, the path it stats is the container's.

Parent-side checking alone would be a TOCTOU — the file can change between
the parent's check and the executor's write. The executor-side check at the
last possible moment closes that window.

## Not built: a presence tier

A third tool tier was reserved and deleted without ever carrying a tool. This
section records why, because the design that proposed it is persuasive and its
absence otherwise reads as an oversight.

**What was proposed.** Verbs a deployed executor structurally cannot serve,
because they need the operator's own machine *and their presence*: read the
system clipboard, open a file in their editor, open a URL in their browser,
resolve a path they just dragged in, pick a file interactively. A client-run
executor (`kind=client`, `interactive=true`) would serve them, and a child would
somehow hold two executors at once — a workspace one and a presence one.

**Why it was rejected.** One test settles every verb on the list: **who
initiates, and does the answer have to arrive inside the turn?**

| Initiator | Correct home |
|---|---|
| The human | a message, not a tool |
| The daemon, on a state change | a notification, not a tool |
| The model, mid-turn, blocking on the result | a tool |

- Clipboard read — the human can paste.
- Clipboard write — a TUI keybinding does it better, and it is a capability an
  injected prompt would enjoy having.
- Browser open — print the URL; terminals make it clickable. Also
  exfiltration-shaped: a URL with data in the query string, opened on the
  operator's machine.
- Editor open — print `file.go:42`, which is clickable, and which this repo's
  own conventions already prefer.
- File picker — "which file did you mean?" is a sentence.

None of them is model-initiated-and-blocking. Claude Code, the most mature
build of this architecture, ships none of them either; what it ships is one
verb where the model does initiate and does need the answer, `AskUserQuestion`.

**And that verb is not a presence tool.** The agent *emits* a question; whatever
is subscribed to its event stream decides who is present and how to render it —
a desktop notification, a phone push with the choices as buttons, a coordinating
parent agent, or nobody. Presence is a property of **subscribers**, not a tier of
tools. That is why no second executor binding, no `ToolOpts.PresenceExecutor`,
and no child-to-attached-client link were needed: the problem the tier existed
to solve dissolves once the question is an event rather than a routed call.

**Two consequences, both load-bearing.**

`tools[]` is **immutable** for a child's lifetime, not merely monotonic. The
earlier rule allowed one growth, on first human attach, purely to admit this
tier. Nothing else would ever have exercised it.

The genuinely useful items on the list are **TUI-local affordances** — paste,
drag-and-drop, `@file` completion over the executor's tree, clickable
`file:line`. They need no tier and no wire protocol, and they are blocked on a
real rafiki TUI existing rather than on anything here.

**What is still open, and must not be smuggled back in under this name.** A
child that needs its container for the checkout *and* its operator's laptop for
browser-based auth or secrets is a second **workspace** binding — `read` and
`bash` on two machines. That is a real, undesigned problem. It is not presence,
and reviving `TierPresence` is not how to solve it.
