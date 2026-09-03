# Phase 2 spike findings

Empirical checks against the **live local rafiki database** (`postgres://…@127.0.0.1:5432/rafiki`),
the rafiki source, and live `claude` / `pi` binaries, done before writing the phase 2 plan.

Why this exists: phase 1a's plan was written by reading code, and eleven of its factual premises
turned out to be wrong — each one caught only because a human or a reviewing agent was present to
adjudicate. Phase 2 is intended to run **unattended overnight**, where a false premise either
stalls the run or gets silently worked around. So every premise the plan rests on gets verified
here first, and anything still unverified is labelled as such.

Status: **complete.** All seven listed premises are settled. Three were falsified, one of them
fatally to the design as written (premise 5). Two additional blockers were discovered that were
never on the list, and one of those stops any worktree-based run dead.

Method note: every "verified" below was settled by **running** something — a throwaway Go program
against the live database, a live `claude` / `pi` invocation against a capture server, or SQL over
real rows. Nothing in the verified column was settled by reading alone. Scratch databases,
capture servers, the probe worktree and all temp dirs were removed afterwards; the tree is clean.

---

## Summary table

| # | premise | verdict |
|---|---|---|
| 1 | `store.Messages.Load` returns what attach-prime needs, in usable order | **verified**, with two gaps |
| 2 | `insights.Search` / `UnfinishedConversations` supply last-activity, turn count, cost | **falsified** — 1 of 3 |
| 3 | `fundidb.Migrate` + `rafikistore.Migrate` coexist on one pool | **verified** |
| 4 | `ANTHROPIC_CUSTOM_HEADERS`' exact encoding for multiple headers | **verified** — newline only |
| 5 | `renderRing` becomes unnecessary once normalized messages come from the DB | **falsified, decisively** |
| 6 | who writes `conversation_attachment` rows, and whether fundi should | **settled** — nobody does |
| 7 | pi honours a provider override via the `fundi-helpers` extension | **verified live** |
| A | *(not on the list)* fundi builds from a clean checkout | **falsified — blocker** |
| B | *(not on the list)* rafiki's `DrivenBy` doc comment is accurate | **falsified** — stale comment |

---

## A. BLOCKER: fundi does not build from a clean checkout

Found while checking which rafiki version the spike should run against. This gates everything
else, so it is first.

`go.mod` pins `git.graveland.dev/brent/rafiki v0.0.0-20260726010043-10a8ca5bf6f6` (commit
`10a8ca5`, 2026-07-25). That version does **not** contain `llm.StreamingSender`,
`llm.WithStreamHandler`, or the 3-argument `agentloop.Run` that `internal/agent/engine.go`
requires — those landed in `742c15f`, `c8cfba9` and `3751669` on 2026-07-30.

The build only works because of an **untracked, gitignored `go.work`**:

```
go 1.26.5
use (
	.
	../rafiki
)
```

`.gitignore:36-37` ignores `go.work` and `go.work.sum`. Measured:

| build | result |
|---|---|
| `go build ./...` in `~/home/fundi` | ok |
| `GOWORK=off go build ./...` | **2 errors**: `too many arguments in call to agentloop.Run`, `undefined: llm.WithStreamHandler` |
| `go build ./...` inside a fresh `git worktree` | **the same 2 errors** (`go env GOWORK` is empty there) |

**Why this stops an unattended run.** The house convention is to implement multi-step plans in a
git worktree. A worktree gets no `go.work` (it is gitignored, so it is not checked out), and if
one is placed under `~/home/fundi/.claude/worktrees/…` the *parent* `go.work` is discovered
instead — whose `use .` points at the **main checkout**, not the worktree. So an agent in a
worktree either cannot build, or silently builds the wrong tree. Every task's test step fails at
step one.

The stale pin also predates `store/pricing.go` and the `model_pricing` table, which is the only
price-correct cost source — see premise 2.

**This is Task 0 of phase 2 and it is not optional.**

### SUPERSEDED 2026-07-31 — the fix changed, and so did rafiki

The original fix recorded here (`go get git.graveland.dev/brent/rafiki@fee183f`) **no longer
works, because that module path no longer exists.** Do not follow it.

rafiki was open-sourced on 2026-07-31 and is now:

- module path **`go.graveland.dev/rafiki`** (a vanity path; `?go-get=1` serves a `go-import` meta
  tag pointing at `github.com/graveland/rafiki`)
- **`pkg/`-prefixed**: `go.graveland.dev/rafiki/pkg/{llm,routing,store,agentloop,insights,server,analyze,agentcli}`
- published and anonymously resolvable: verified `go get` through `proxy.golang.org` with
  `sum.golang.org` enforced and no credentials

So fundi's fix is no longer a pin bump but a repoint: rewrite the imports to
`go.graveland.dev/rafiki/pkg/…`, require the published module, and **delete `go.work`** — which
removes the failure mode described above by construction rather than papering over it.

That work is expected to happen *as part of* merging fundi into rafiki (one repo, one module),
which makes fundi's own build irrelevant in the meantime — so do not spend effort fixing it
standalone.

---

## B. rafiki's `DrivenBy` doc comment is stale, and it matters

`store/constants.go`:

> `DrivenBy` is who drives a conversation's history: the library (stateful, message rows + turns)
> or a pass-through proxy client that **owns its own history (turns only)**.

"turns only" is wrong. Proxy-captured conversations have 10,109 `conversation_message` rows in the
live database. The writer is not `store.Messages.Append` (whose only caller is the library path,
`llm/conversation.go:427`) but **`routing.CaptureStore.DecomposeRequest`** (`routing/store.go:188`,
insert at `:210`) plus the response-append at `routing/store.go:391`.

Chasing this comment is what uncovered premise 5's real answer. Per phase 1a's own lesson — "a
wrong comment near subtle lifecycle code is a live hazard in this codebase" — it should be fixed
upstream.

---

## 1. `store.Messages.Load` — verified, with two gaps

Called against the live database. On a library-driven conversation with tool calls
(`019fa52d-e403-77d8-9beb-98baccc6e7b4`, 7 messages):

- **Ordering is ascending by ordinal**, confirmed row by row (`ORDER BY ordinal` in the SQL).
- **Block fidelity is complete.** Observed `text`, `thinking` (with its `signature`), `tool_use`
  (with `id`, `name`, `input`), and `tool_result` (with `tool_use_id`, `is_error`, nested content).
- `StopReason` is populated on assistant rows (`tool_use`, `end_turn`).
- `ToolUseIDs` is populated on both sides of a tool call — the assistant row that requested it and
  the user row that answered it.
- **Consecutive same-role rows occur** (ordinals 4 and 5 were both `user`: a `tool_result`
  followed by a fresh prompt). Anything rendering this must not assume roles alternate;
  `llm/conversation.go:450`'s `mergeForRequest` exists to merge them for the wire.

Two gaps the design does not account for:

- **No timestamp.** The `SELECT` is `ordinal, role, content, tool_use_ids, stop_reason` — it does
  not read `created_at`, and `store.Message` has no time field. But `recent`/tail-backfill filter
  on time: `ring.Query.Since` is a unix-ms bound and `ring.Event.Timestamp` is unix ms. **So
  DB-backed `recent --since` cannot be built on `Messages.Load` as it stands.** Options: add
  `created_at` upstream (smallest — one column, one struct field, rides the same PR as premise 6's
  enum value); write fundi-side SQL (against the design's "prefer rafiki's API"); or redefine
  `--since` as ordinal-based, which is a user-visible CLI change.
- **An unknown conversation id is indistinguishable from an empty one.** `Load` on a
  nonexistent uuid returns `(nil, nil)` — nil slice, nil error. So `attach <conversation-id>`
  with a typo'd id primes an empty history and proceeds to spawn a driver rather than failing.
  Needs an explicit existence check.

---

## 2. `insights.Search` — falsified: 1 of the 3 claimed columns exists

The design's `list` split promises a conversations view "with last activity, turn count, and cost
— all of which the database has and the ring never did". Ran `insights.Search` and reflected over
`ConversationSummary`. Its complete field set:

```
ID, Owner, Persona, Source, Model, Status, DrivenBy,
CreatedAt, Turns, InputTokens, OutputTokens, CacheReadTokens, FirstMessage
```

- **Turn count: yes.** `Turns`, from `count(*)` over `conversation_turn` in a LATERAL.
- **Last activity: no.** There is no `LastActivity`/`UpdatedAt` field. The SQL selects and orders
  by `c.created_at` only, and the LATERAL computes no `max(created_at)`. The `conversation` table
  *does* maintain `updated_at` — 62 of 65 rows have `updated_at > created_at`, so it is a viable
  source — it is simply not exposed. Needs new SQL or an upstream change.
- **Cost: no.** Token counts only, no money. Worse, the tokens present are **not sufficient to
  compute cost**: the LATERAL sums `input_tokens`, `output_tokens`, `cache_read_tokens` and omits
  `cache_creation_tokens`, which the `conversation_turn` table does record. Live totals:

  | component | tokens |
  |---|---|
  | input | 4,446,403 |
  | output | 6,906,160 |
  | cache read | 1,757,811,652 |
  | **cache creation (omitted)** | **98,878,079** |

  Cache-creation is billed at a premium over base input and is **22× larger than the input-token
  figure** the summary does expose. A cost computed from `ConversationSummary` alone is wrong by
  the largest billable input component. Price-correct cost needs `model_pricing` /
  `routing.ModelPricing.Cost` — which is a separate API, and which the currently pinned rafiki
  does not have (see §A).

**`UnfinishedConversations` supplies none of the three.** It returns
`{ConversationID, PendingTurns, OrphanToolUses, ResumeAttempts}` — a recovery query, correctly
described by the design as "resumable conversations", but it cannot back a list view. It returned
**0 rows for both `server` and `client` scope** against live data, so its recovery behaviour
remains unexercised here.

---

## 3. Dual migration chains on one pool — verified

Ran the real `rafikistore.Migrate` against scratch databases. There is no `fundidb.Migrate` in
fundi today (phase 2 creates it), so fundi's chain was simulated in the shape phase 2 would build
it: its own schema, its own `public.fundi_schema_migrations` table, and a **different advisory
lock key** from rafiki's `0x7261_6669_6b69`.

| case | result |
|---|---|
| A: rafiki alone on an empty database | ok — chain to version 7; re-run idempotent |
| B: fundi chain **first**, then rafiki (the design's stated order) | ok — both tables coexist, `rafiki=[1..7]` and `fundi=[1]`; re-running either afterwards is idempotent |
| C: a fundi-owned table created *inside* the `conversations` schema | ok — rafiki still ran its full chain; the extra table did **not** trip its partial-schema hard error |

Case C is the one worth knowing: rafiki's baseline classifier keys on the presence of **its own**
expected tables, so a foreign table in its schema is invisible to it. Coexistence is genuinely
solved. fundi's own schema is still the right home, for ownership clarity rather than necessity.

---

## 4. `ANTHROPIC_CUSTOM_HEADERS` — verified: a literal newline, and nothing else

Captured against a local HTTP server standing in for rafiki's `/v1/messages` face, driven by real
`claude` v2.1.220 invocations (the exact version the design cites).

| value passed | what arrived on the wire |
|---|---|
| `"X-Rafiki-Session: …\nX-Rafiki-Source: …"` (real newline) | **two correct headers**, values exact, whitespace after the colon trimmed |
| `'X-Probe-A: comma-a, X-Probe-B: comma-b'` | ONE header: `X-Probe-A: "comma-a, X-Probe-B: comma-b"` |
| `'X-Probe-C: esc-c\nX-Probe-D: esc-d'` (backslash-n) | ONE header: `X-Probe-C: "esc-c\\nX-Probe-D: esc-d"` — the escape is **not** interpreted |
| `'X-Probe-E: semi-e; X-Probe-F: semi-f'` | ONE header: `X-Probe-E: "semi-e; X-Probe-F: semi-f"` |

**All three wrong encodings fail silently.** The request still succeeds; the header is merely
malformed and the second one is swallowed into the first one's value. So a mis-encoded value means
correlation breaks with no error anywhere — the same "instrumentation that silently drops data"
class as I1 and the `ctrl_child_status` loss.

**Consequence for the service templates.** A *literal newline* must survive into the daemon's
environment. A launchd plist `<string>` can hold one. `systemd`'s `Environment=` **cannot** carry
a raw newline and does not interpret `\n`, so the Linux unit needs an `EnvironmentFile` or
equivalent. This is a concrete deploy-prerequisite detail, not a formatting nicety.

Two bonus findings from the same captures:

- **claude already sends `X-Claude-Code-Session-Id`** natively (e.g.
  `e137d1c7-6864-4080-889b-b2da5a5ba40a`). So claude's own session identity reaches the proxy with
  no custom header at all — the design's "also record each kind's own session identity" has a
  zero-cost server-side source for the claude kind.
- **claude preflights `HEAD /api/hello`** before `POST /v1/messages`. rafiki's mux registers only
  `/v1/messages`, `/v1/chat/completions`, `/metrics` and `/healthz`, so that preflight 404s.
  Verified against a server reproducing exactly that (404 on everything unregistered): **claude
  tolerates the 404 and proceeds**, headers intact. rafiki needs no `/api/hello` handler.

---

## 5. `renderRing` does NOT become unnecessary — falsified with evidence

This is the finding that reshapes the plan.

The design routes pi and claude through rafiki's proxy so their transcripts land in
`conversation_message`, then reads them back with `Messages.Load`. The content really is
captured — but **the proxy's ordinals are not conversation ordinals**, and the stored transcript
is provably corrupt for a meaningful fraction of conversations.

**The mechanism.** `routing/store.go:210` inserts `ordinal = index within THIS request's
messages[]`, with `ON CONFLICT (conversation_id, ordinal) DO NOTHING`. So the first request to
write a given ordinal wins permanently. rafiki's own comment on `StoreTurnPrefix` says so
outright: those ordinals "don't align with conversation-history ordinals once history is merged or
trimmed."

**Measured consequence.** Message rows are dense with no gaps and `count(*) = max(ordinal)+1` for
every proxy conversation — i.e. the row set is the *high-water mark of any single request's array*,
not the conversation:

| turns | message rows | `max(ordinal)+1` | dense |
|---|---|---|---|
| 5,765 | 1,043 | 1,043 | yes |
| 1,270 | 949 | 949 | yes |
| 580 | 1,003 | 1,003 | yes |

5,765 turns collapse into 1,043 rows. When claude compacts its context — which Claude Code does
automatically — the low ordinals still hold the *pre-compaction* messages while later ordinals
hold *post-compaction* ones. The result is an interleaving of two different histories.

**Proof that this is corruption, not just re-indexing.** Parsing block content directly (the
`tool_use_ids` column is unusable here, see below): **32 `tool_result` blocks across 9 of the 62
proxy conversations reference a `tool_use_id` that appears nowhere in the stored conversation.**
That is exactly the dangling-`tool_use` shape the Anthropic API rejects, and exactly what
`RepairOrphans` exists to repair.

**And the orphan detector is blind to it.** Both proxy inserts omit `tool_use_ids`:
`DecomposeRequest` writes `(conversation_id, ordinal, role, content)`; the response append writes
`(…, content, input_tokens, output_tokens, stop_reason)`. Neither populates the column — so all
10,109 proxy rows have it empty, versus a populated column on every library-path row.
`UnfinishedConversations`' `orphans` CTE unnests `m.tool_use_ids`, so it can **never** surface an
orphan in a proxy conversation. `store.ToolUseIDsOf(param)` is exported for exactly this
compute-on-the-fly case, so the gap is closable — but it is not closed today.

**What `Messages.Load` *does* survive.** Worth recording, because it is better than expected: run
against two proxy conversations containing `system`-role rows and plain-JSON-string content —
neither of which the library path ever produces — it returned **874/874 and 956/956 rows, no
error, and zero messages that decoded to zero content blocks**. The SDK tolerates both. Note
though that `role: "system"` is not valid in a `MessageParam` sent *to* the API, so those rows are
fine to display and must be filtered before being used to drive a turn.

**Why renderRing survives regardless.** Even setting corruption aside, `renderRing` holds
**fundi's own pi-vocabulary bus frames** (`child.go:435`'s `publishBus`), which is what every
existing consumer of `recent --rendered` expects. `Messages.Load` returns
`anthropic.MessageParam`. Replacing one with the other is not a deletion — it requires a new
`MessageParam → bus frame` renderer. Combined with the corruption above:

> **`renderRing` stays in phase 2.** Retiring it requires (a) the proxy writing stable
> conversation ordinals, (b) `tool_use_ids` populated on the proxy path, and (c) a new
> MessageParam→bus-frame renderer. All three are upstream-or-net-new work, none of it a deletion,
> and (a) is a change to how rafiki captures every proxied conversation.

The corollary for slicing: **DB-backed transcript reads are sound for the agent kind** (library
path — stable conversation ordinals, populated `tool_use_ids`, divergence checked rather than
`DO NOTHING`) **and unsound for proxy-captured kinds.**

---

## 6. `conversation_attachment` — nobody writes it, and its index is a gift

Settled. The table is deliberately unimplemented: the scadmin migration that creates it says so
in a comment — *"Schema-only in this increment: multi-entrypoint attach/detach (design §9)."*

- **Zero writers.** The only Go reference in all of rafiki is `store/migrate.go:158`, a
  `to_regclass(...) IS NOT NULL` existence probe used for baseline classification.
- **Zero rows** in the live database.
- rafiki's `Migrate` *creates* it, so it is rafiki-owned schema.

Its definition carries something phase 2 explicitly wants:

```sql
CREATE UNIQUE INDEX conversation_attachment_active_uniq
  ON conversations.conversation_attachment (conversation_id, entrypoint)
  WHERE detached_at IS NULL;
```

That partial unique index **is** the design's "at most one live driver per conversation, enforced
by the database across restarts and across daemons" — the constraint meant to replace the
in-memory `childClaimSet`. Using it means writing to rafiki's schema, which cuts against fundi
owning its own tables. That is a real decision, recorded here rather than silently taken.

Live `origin_entrypoint` / `driven_by` values, which settle design open question 3:

| origin_entrypoint | driven_by | conversations | turns | messages |
|---|---|---|---|---|
| `claude` | `client` | 62 | 10,450 | 10,109 |
| `agent` | `server` | 3 | 5 | 11 |

So fundi's agent kind is **`DrivenByServer`**, and anything proxied becomes `DrivenByClient`.
Since `UnfinishedConversations` is scoped to `DrivenByServer` by design, **pi and claude
conversations will never appear in the resumable list** once they are proxied. That is consistent
but it is a behaviour the `list` split and `attach` must state explicitly.

---

## 7. pi provider override — verified live

The mechanism is real and fundi has now actually done it. `model-registry.ts:1026`'s
`else if (config.baseUrl || config.headers)` branch is the override-only path; a config with no
`models` array skips all of `validateProviderConfig`'s requirements.

A minimal extension (`export default function (pi) { pi.registerProvider("anthropic", {baseUrl,
headers}) }`) was loaded with `pi -e`, against real pi v0.80.6 and the capture server:

```
req: POST /v1/messages
  X-Api-Key: "spike-not-real"
  X-Rafiki-Session: "pi-sess-probe-42"
  X-Rafiki-Source: "pi"
```

Both custom headers delivered exactly; baseUrl override honoured. Four operational notes:

- pi authenticates with **`X-Api-Key`**, not `Authorization: Bearer`. rafiki's face accepts
  either, so this is compatible — but it is the other branch from claude's.
- pi sends **no native session-id header** (no counterpart to claude's
  `X-Claude-Code-Session-Id`). For pi the extension is the *only* source of the ref.
- **`pi -p` hangs indefinitely without `< /dev/null`.** The first attempt was killed at 4
  minutes; with stdin closed it completed in seconds. Any automated pi invocation in an
  unattended run must redirect stdin or it will burn the whole window.
- pi makes **no** `/api/hello` preflight — it goes straight to `POST /v1/messages`.

---

## Constraints that bind an unattended run

- **§A is a hard prerequisite.** Bump the rafiki pin before anything else; otherwise every task
  fails to build, and a worktree-based run fails at its first test step.
- **Phase 2 makes the database required and deletes the `mem-…` fallback.** Getting that wrong
  unattended leaves `fundid` unable to start — and fundi is about to become Zoe's runtime. The
  work stays on a branch and is never deployed by the run.
- **Live-daemon verification cannot happen at 3am.** Phase 1a's verification task needed real API
  calls and a human reading daemon logs. Phase 2's equivalent must either be genuinely automated
  or explicitly deferred to a morning gate — not asserted by an agent that could not perform it.
- **Not a phase 2 constraint: the service templates.** `service_darwin.go` and `service_linux.go`
  hardcode `HOME` and `PATH`, so a service-managed daemon can receive neither `FUNDI_AGENT_DB` nor
  `ANTHROPIC_API_KEY`. That does **not** gate making the database required, because development and
  the overnight run use a foreground `fundid` with `.env` sourced — the pattern `.env.example`
  documents and the one phase 1a's live verification used throughout. It is a **deploy
  prerequisite**: fix it before `main` is deployed and before the `default_kind: "agent"` flip.
  Premise 4 adds a requirement to that fix: the Linux unit must be able to carry a literal
  newline in `ANTHROPIC_CUSTOM_HEADERS`.

## rafiki changes this spike implies

**Framing correction, 2026-07-31:** these were written as changes to send *upstream* to
`github.com/timescale/rafiki`. That is no longer the relationship — `go.graveland.dev/rafiki` is
now the maintained home and Timescale's copy is downstream. So these are simply changes to make,
in the same repository, with no PR to shepherd and nobody's review to wait on. Once fundi merges
in, they stop being cross-repo work altogether.

1. `ConversationStatus = "completed"` — a new enum value on an existing `text NOT NULL` column
   with an existing setter. No DDL. (Design's `forget` → `done`; premise 6 confirms the column.)
2. `created_at` on `store.Message` and in `Messages.Load`'s `SELECT` — without it there is no
   DB-backed `recent --since` (premise 1).
3. `last_activity` on `insights.ConversationSummary` (`max(conversation_turn.created_at)`, or
   expose `conversation.updated_at`) and `CacheCreationTokens` in its LATERAL — without the
   latter, cost is wrong by its largest input component (premise 2).
4. Fix the `DrivenBy` doc comment (§B).
5. Populate `tool_use_ids` on the proxy write path, via the already-exported
   `store.ToolUseIDsOf` (premise 5). Prerequisite for orphan repair ever working for pi/claude.

Items 1-4 are small and independent. Item 5 is a behaviour change to shared capture, and the
ordinal-stability problem behind premise 5 is larger still — neither belongs in phase 2's
critical path.
