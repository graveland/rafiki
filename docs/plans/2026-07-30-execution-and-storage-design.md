# In-process execution and the DB as source of truth — design

Two entangled changes, sequenced. Phase 1 moves the `agent` kind from an OS subprocess to a
goroutine inside `fundid`. Phase 2 makes the database the source of truth for conversation
history and retires the in-memory ring, the per-child JSON state records, and their bespoke
garbage collection.

They are one design because the storage write path depends on the execution model: with
in-process children there is one pool with one owner, the daemon observes messages as objects
rather than as JSON re-parsed off a pipe, and the in-flight buffer is a struct field rather
than a re-serialized frame. Building phase 2 first means constructing a 1+N-pool arrangement
and then unwinding it.

## Goal

Delete record-keeping machinery that exists only because fundi had no database, and stop
paying the serialization and process costs of a boundary that buys nothing.

Concretely: `recent`, `tail --backfill`, `logs`, and attach-prime should read from the
database. Deltas should never be persisted. A daemon restart should not be a data-loss event.
And a 20-way M2 fan-out should cost 20 goroutines rather than 20 processes with 20 connection
pools.

## Premise corrections

Two beliefs that motivated this work needed adjusting before designing against them. Both are
recorded here because the design reads differently without them.

**fundi does not currently require a database.** `DBURL` and `pgxpool` appear in exactly two
files, `internal/agent/config.go` and `cmd/fundid/agent.go`. That makes the database
agent-kind-only and optional even there — a `sessionId` of `mem-…` means in-memory. The entire
§3a verification run on 2026-07-30 (5 turns, 2 children, streaming, tool calls, abort) executed
with no database anywhere. Making the database required is therefore a change, not a
description.

**The process boundary provides no restart survivability today.** The daemon kills children
twice over. `ShutdownAllChildren` terminates them on shutdown with a 120s-per-child / 180s
global budget, explicitly to avoid pipe-death when launchd or systemd stops the daemon. Then
`loadOrphans` (`controller.go:130`) probes each recorded PID with `syscall.Kill(pid, 0)` and
**SIGTERMs any survivor**, marking the session exited. A child that outlives a daemon crash is
executed on the next boot, not reattached. The `ExitTimeOut=200` plist value and the 180s
shutdown bound exist *because* of the boundary.

## The record-keeping inventory

Six surfaces currently hold overlapping views of the same session. Three are copies of one
frame stream at different lifecycle moments.

| surface | contents | lifetime | kinds |
|---|---|---|---|
| bus / `Subscribe` | live frame fan-out | live only | all |
| `ring` | bounded frame log, 5000 events / 64 MiB | process | all |
| `renderRing` | second raw capture for claude normalization | process | claude |
| `exitedRing` + `exitedRenderRing` | ring copied into the store on exit | until forget/grace | all |
| `persist.Record` | one JSON file per child in `<stateDir>/<childID>.json` — status, PID, sessionId, exit info; scanned at boot, GC'd by a 7-day grace window | disk | all |
| gz dumps | `in.jsonl.gz`, `out.jsonl.gz`, `err.log.gz`, `render.jsonl.gz`, `meta.json` written on exit | disk | all |

`persist.Record` is the clearest case: a durable child registry hand-rolled as a directory of
JSON files with a bespoke sweeper. That is a database with extra steps.

## Decisions

### The database is required

Drop the `mem-…` fallback. **Required means `fundid` refuses to start without a reachable
database, for every kind — not merely that the agent kind loses persistence.** The child
registry lives in the database, and `list`, `attach`, and `done` all read it, so a pi-only or
claude-only deployment needs postgres too. The alternative — degrade to `persist.Record` when
the database is absent — keeps every surface this design deletes, and a fallback path that is
never exercised in development is a fallback path that does not work.

This makes the service-template gap blocking rather than cosmetic:
`service_darwin.go`'s plist template and `service_linux.go`'s unit template hardcode exactly
`HOME` and `PATH` with no passthrough, so a service-managed daemon cannot receive
`FUNDI_AGENT_DB` — or `ANTHROPIC_API_KEY`. Today's only working configuration is a foreground
`fundid` with `.env` sourced. Env passthrough in both templates is a prerequisite.

### rafiki stays a library; fundi owns the pool and both migration chains

Per `~/ts/dev/client/pkg/server/admin_pool.go`, which is the precedent: one pool, own migration
chain first, rafiki's second. rafiki's migrator uses its own advisory-locked
`rafiki_schema_migrations` table and adopts an existing schema or creates a fresh one, so
coexistence is already solved and idempotent. The governing rule from that file's doc comment:

> This is the single owner of open+migrate: every feature that needs this DB must share the
> returned pool rather than open its own.

The daemon becomes that single owner. It is a database client for the first time — today the
pool is reached only from the child.

### rafiki's storage does not become an interface

Considered and rejected. It contradicts rafiki's own stated principle that "capture is
transparent and automatic, no opt-in, the store is the source of truth" — the first
implementation written against a storage interface is a no-op one, which voids the guarantee
the `analyze/` pipeline depends on. fundi does not want a different backend; it wants the same
postgres. And rafiki's store is where the hypertables, pricing, and `prefix_hash` live, so the
abstraction is either lossy or leaky. Revisit only if a non-postgres consumer appears.

### Deltas are never persisted

No store — database or otherwise — holds SSE updates. They go to the bus and nowhere else;
their value expires within the 250ms coalescing window. Only completed messages, tool
executions, and lifecycle events are durable.

**One slot per child, in memory,** holds the current in-flight message's consolidated state. It
serves two purposes: replaying to a client that attaches mid-message, and — on abort —
supplying the partial to persist as a completed-but-aborted message. The second is the fix for
I1 (see Evidence).

The slot applies to **all kinds**, not just the agent kind: it sits next to the bus, which every
kind's frames pass through. It holds exactly one message and is superseded at `message_end`, so
it is bounded by the largest single message rather than by conversation length — this is not a
ring with a limit of one.

### pi and claude capture via rafiki's proxy

Point those children at rafiki's `/v1/messages` face. Capture is then automatic and
byte-exact, which is what the proxy was built for (rafiki DESIGN §2.3, §4.2). The face accepts
`Authorization: Bearer` or `x-api-key`, which matches Claude Code's `ANTHROPIC_BASE_URL` +
`ANTHROPIC_API_KEY` shape. fundi has no base-URL plumbing today; this is net-new but small.

### `forget` becomes `done`

Nothing is forgotten once the record is canonical. The verb marks a conversation **complete and
read-only**, which buys three things list-hygiene did not:

1. It answers a question nothing else can — when is a conversation *done*? Idle is not done.
   rafiki's `analyze/` runs out-of-band over stored conversations and currently has no signal
   for "this is a whole unit of work, safe to analyze."
2. The attachable list becomes bounded by intent rather than by a 7-day timer.
3. A completed conversation's cost total cannot move, which makes M2's cost-per-task
   comparisons stable.

**Implementation:** add `ConversationStatus = "completed"` to rafiki's existing enum
(`store/constants.go` currently has only `active` and `failed`). This is a new enum value on an
existing column with an existing setter (`SetConversationStatus`), not a schema change. The
absence of a terminal success state reads as an omission in rafiki rather than a
fundi-specific concept, and `UnfinishedConversations` then naturally excludes completed
conversations — which *is* the resumable list. It can ride the upstream PR.

Completion is explicit, plus a `kill --done` flag for the common case. Never automatic on clean
exit: resume is a first-class flow and auto-completion would fight it.

**Enforcement is honest about its limits.** "Read-only" means fundi refuses to spawn a driver
for a completed conversation. It does not mean postgres rejects writes.

### Invariant: process lifecycle never deletes a conversation

No command that manages children may delete a conversation row as a side effect. `kill`'s
auto-forget-on-clean-exit stays purely a display concern. rafiki's `analyze/`, `insights`, and
cost rollups all assume the record is complete, and M2's economics are worthless with holes in
them.

This needs writing down because `forget` is already more destructive than it advertises:
`forget --help` claims "Disk artifacts (logs, state record) are NOT removed", while the code
(`controller.go:1255-1259`, `1309-1316`) calls `persist.DeleteRecord`, `deleteLogDump`, and
`deleteSpillDir`. Fix the help text regardless of this design.

## Phase 1 — in-process agent execution

`internal/agent.Engine` is already library-shaped: `NewEngine(cfg, fe)`, `HandlePrompt`,
`HandleSteer`, `HandleAbort`, `State`, `Wait`, `Close`, with no stdio assumptions. The entire
coupling to being a separate process is one line — `cmd/fundid/agent.go:203`,
`agent.NewFrontend(os.Stdin, os.Stdout, nil)` — because `Frontend` takes an `io.Reader` and an
`io.Writer`.

**Step 1a — same protocol, in-memory transport.** `Child` becomes an interface with two
implementations: `processChild` (exec, unchanged) and `inProcessChild` (an `Engine` goroutine
with `Frontend` wired to an `io.Pipe`). The daemon's frame reader at `child.go:477` is
untouched. `BuildEngine` stops creating a pool and accepts one.

This yields, for the agent kind: no process spawn, one shared pool instead of 1+N, no
`ShutdownAllChildren` budget, no `loadOrphans` SIGTERM, per-conversation OTLP spans nesting
under the daemon's trace, per-conversation Prometheus gauges, and no separate stderr stream —
child diagnostics become daemon structured logs with a `conversation_id` field, so `err.log.gz`
and `fundi logs --err` become a query rather than a gzip read.

**Step 1b — delete the serialization.** Replace the pipe with direct calls or channels. This
removes an entire bug class that exists only because two halves of one program speak a wire
format: the `JSON.raw`-versus-struct-field divergence that made every streamed frame empty, the
2× payload duplication where `message` and `assistantMessageEvent.message` are byte-identical,
and the `_raw` unmarshal spam.

**Panic containment is day-one, not a follow-up.** `recover()` at the goroutine boundary,
converted to a child-exited event carrying error status. Without it, one conversation's panic
takes the daemon and every other conversation.

**`fundid agent` survives** as a thin wrapper over the same `Engine`. It keeps the seam honest,
remains useful for debugging a single agent standalone, and preserves the remote-child path.

### What is given up

- **Fault isolation.** A panic takes the daemon rather than one child. `recover()` covers most
  of it; OOM, stack overflow, and `runtime.Goexit` are not recoverable, and in k8s an OOM takes
  the pod. The counterweight is phase 2: once the record is durable, a daemon crash means
  "resume everything," which is cheap.
- **Per-conversation memory caps.** The OS provides one for free today. rafiki's trim policy and
  50 KB tool-result truncation bound context but not RSS.
- **A second execution mode rather than a replacement.** pi and claude are foreign binaries and
  stay processes, so `internal/child` keeps its exec path and gains an in-process one. Net
  simplification only if the agent kind eventually becomes the only kind.
- **Nothing, for remote children.** The framed-stream seam is what lets in-memory, stdio, and
  TCP coexist as transports; the 2026-07-20 design's reverse-dial path is unaffected.

## Phase 2 — the database as source of truth

### Read paths bind to rafiki's existing API

Most of this is already written. Prefer these over new SQL:

| need | rafiki API |
|---|---|
| attach-prime, tail-backfill | `store.Messages.Load(ctx, conversationID)` |
| full transcript + turn metrics | `insights.Export(ctx, conversationID)` |
| conversations list | `insights.Search(ctx, SearchFilter) → []ConversationSummary` |
| resumable conversations | `store.UnfinishedConversations(ctx, pool, scope)` |
| resume bookkeeping | `Increment/ResetResumeAttempts` |
| completion | `store.SetConversationStatus` |

### fundi's own schema

One genuinely new table: a **child registry** — childId, conversation id, pid (nullable, absent
for in-process), cwd, kind, model, labels, status, exit info, timestamps. rafiki knows nothing
about OS children, so this has no counterpart there. It replaces `persist.Record`'s
directory-of-JSON-files, and the 7-day grace sweeper becomes ordinary retention on this table.

### Two identities, explicitly separated

A **child** is an execution context (process or goroutine); a **conversation** is the durable
record. A conversation has 0-or-1 live child and 0-to-N historical ones, because every resume
is a new child against the same conversation. Today conversation identity is derived *from*
childId via `--ref` / `ByExternalRef`, which is backwards.

**Inverting it fixes a bug class.** The `childClaimSet` guarding double-spawn is in-memory, so
it does not survive a daemon restart — a crash mid-resume, or two daemons against one database,
can still produce two drivers sharing one `--ref`. Make "at most one live driver per
conversation" a uniqueness constraint and the database enforces it across restarts and across
daemons. `childClaimSet` is then deleted.

### CLI changes

- **`list`** splits. Default stays "what is running" (children). A conversations view answers
  "what can I attach to", with last activity, turn count, and cost — all of which the database
  has and the ring never did. This view needs filtering and a limit from day one, since it is
  no longer bounded by memory and a grace window.
- **`attach <conversation-id>`** subsumes `resume`: join the live stream if a child is driving,
  otherwise spawn a driver. History primes from `Messages.Load`, plus a replay of the one-slot
  in-flight partial when joining mid-message. This is rafiki DESIGN §4.1's "another engineer
  connects from a TUI: `fundi attach <id>`", where the id is the conversation. `resume` remains
  as an alias for the spawn case.
- **`forget`** → `done` / `close`, with `forget` kept as a deprecated alias.
- **`logs`** splits by kind: a query for in-process agent children, gz files for pi and claude.

### What gets deleted

`ring`, `exitedRing` and `exitedRenderRing`, `RingSnapshot`/`Recent` as the backing for
`recent` / tail-backfill / attach-prime, `persist.Record`, the grace sweeper, `childClaimSet`.

Retained deliberately: the **bus**, which is live fan-out and has no database equivalent —
rafiki DESIGN §5 lists the routing layer for multi-client join as still missing. And gz dumps
for pi and claude, whose stderr is arbitrary process output; a gzipped file is the right tool
for "what did this process print" and postgres is the wrong one.

## Decomposition

Phase 1 and phase 2 are one design but **two implementation plans**. Phase 1 is independently
shippable and independently valuable — it stands on telemetry, k8s behaviour, and M2 fan-out
economics alone, with no schema work. Phase 2 should be re-planned after phase 1 lands, because
step 1b determines how much of the frame plumbing still exists to point at the database.

Within phase 1, step 1a (in-memory transport, protocol unchanged) and step 1b (delete the
serialization) are separable, and 1a is the one that carries the risk — it changes the child
lifecycle. Ship and verify 1a before starting 1b.

## Open questions for planning

1. **Proxy correlation.** rafiki's proxy keys conversations by its own ids, so relating a
   pi/claude child to its captured conversation needs a ref travelling with the request.
   Unverified whether the proxy supports that; if not, this is upstream work.
2. **Does `renderRing` survive?** It exists because claude's native frames must be re-rendered
   for backfill (`docs/plans/2026-06-15-claude-rendered-backfill-design.md`). Normalized
   messages from `Messages.Load` may remove the need.
3. **Which `DrivenBy`?** `Messages.Load` and `UnfinishedConversations` are scoped to
   `DrivenByServer` "by callers per the design". fundi's conversations must match that scoping.
4. **Spill directories.** `--spill-dir` is per-child today. Whether it stays per-child or
   becomes per-conversation depends on step 1b.
5. **Retention policy** for the child registry, replacing `FUNDI_GRACE_HOURS`.

## Evidence

The I1 and C4 items driving the durability half were measured on 2026-07-30 against a live
daemon, 5 turns across 2 children. Frame captures in `/tmp/fundi-dogfood/`.

**C4 — `agent_end.messages[]` is turn-scoped.** Four consecutive turns reported 2, 2, 6, 2
messages: `[user, assistant]` for plain turns and `[user, assistant, toolResult, assistant,
toolResult, assistant]` for the tool turn. Each turn reports only itself. `emit.go:165` does
`e.messages = nil`; `provider_claude_state.go:303` documents the opposite contract — "the
messages are retained (not cleared) so a later ctrl_get or a subsequent turn's agent_end still
reflects the whole conversation" — and both attach consumers were written to claude's version.

**I1 — an aborted partial survives nowhere.** After aborting mid-stream with 1038 characters
streamed, the abort's own `agent_end` carried `msgs: 1` and zero assistant text: the assistant
message was never accumulated, because `accumulate` only runs on a complete message. Searching
for a distinctive 45-character needle from the partial, with the live capture as a control:

| surface | matches |
|---|---|
| live capture (control) | 4 |
| `fundi recent` | 0 |
| `fundi logs` | 0 |
| fresh tail backfill, deltas on | 0 |

Billed output, unrecoverable. Only a subscriber attached at the time ever saw it.

**Streaming itself is healthy** — 42 `message_update` frames per turn, none empty, monotonic
23 → 3768 characters, `message_end` at 3833. The final 250ms window lands in `message_end`,
which is lossless by design.
