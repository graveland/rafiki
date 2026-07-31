# Phase 2 spike findings

Empirical checks against the **live local rafiki database** (`postgres://…@127.0.0.1:5432/rafiki`)
and the rafiki source, done before writing the phase 2 plan.

Why this exists: phase 1a's plan was written by reading code, and eleven of its factual premises
turned out to be wrong — each one caught only because a human or a reviewing agent was present to
adjudicate. Phase 2 is intended to run **unattended overnight**, where a false premise either
stalls the run or gets silently worked around. So every premise the plan rests on gets verified
here first, and anything still unverified is labelled as such.

Status: **incomplete.** The table below tracks what is settled and what is not.

## Verified

### The transcript table is `conversation_message`, not `conversation_msg`

The design doc calls it `conversation_msg` throughout. The actual table is
`conversations.conversation_message`. Columns:

```
id uuid, conversation_id uuid, ordinal int, role text, content jsonb,
tool_use_ids ARRAY, input_tokens bigint, output_tokens bigint,
stop_reason text, created_at timestamptz
```

`tool_use_ids` is directly relevant to the dangling-`tool_use` repair that `RepairOrphans` exists
for.

### `conversation.status` is `text NOT NULL` — adding `completed` is a value change, not a migration

```
id uuid, owner text, persona text, model text, origin_entrypoint text NOT NULL,
driven_by text NOT NULL, external_ref text, status text NOT NULL,
created_at timestamptz, updated_at timestamptz, resume_attempts int
```

All 65 live rows are `active`; no `failed` rows exist in practice. So the `forget` → `done`
design (adding `ConversationStatus = "completed"`) needs no DDL, and `external_ref` confirms the
proxy-correlation mechanism has a home.

### `conversation_turn` carries the cost and analysis substrate

```
id, conversation_id, ordinal, status, model, request jsonb, response jsonb,
stop_reason, input_tokens, output_tokens, cache_read_tokens,
cache_creation_tokens, upstream, error, latency_ms, created_at, source,
author, author_kind, prefix_hash, protocol, response_ordinal,
prefix_content jsonb, cache_breakpoints jsonb
```

Everything the dogfood's cost-per-task metric needs is here, per turn, including `latency_ms`
and the full request/response JSON.

### `v_conversation` is NOT a list view

It is `conversation` plus two derived columns (`owner_canonical`, `owner_kind`) — no aggregates.
So the design's claim that a conversations list view gets "last activity, turn count, cost" must
be satisfied by `insights.Search`'s own SQL, not by this view. **Still unverified** — see below.

## Discovered, and absent from the design

### `conversations.conversation_attachment` is a multi-client attach/detach ledger

```
id uuid, conversation_id uuid, entrypoint text, external_ref text,
attached_at timestamptz, detached_at timestamptz
```

The design doc never mentions this table. It is exactly the state that rafiki's own DESIGN §5
describes as the missing piece ("the routing layer that lets Slack, TUI, and other clients attach
to an existing conversation and participate"). Phase 2's `attach`-addressed-by-conversation work
should be designed **with** this rather than inventing a parallel mechanism — and the question of
who writes `attached_at`/`detached_at` needs answering before the plan is written.

### Other tables the design does not account for

`conversation_analysis`, `analysis_finding`, `model_pricing`, `v_turn`. Probably irrelevant to
phase 2, but `model_pricing` is what makes the cost rollups price-correct and should not be
duplicated fundi-side.

## Still unverified — must be settled before the plan is written

| premise | how to settle it |
|---|---|
| `store.Messages.Load` returns what attach-prime needs, in usable order | call it against this database from a throwaway program; inspect the shape |
| `insights.Search` / `UnfinishedConversations` supply last-activity, turn count, cost | read their SQL; confirm the aggregates exist rather than assuming |
| `fundidb.Migrate` + `rafikistore.Migrate` coexist on one pool | actually run both against a scratch database, in that order |
| `ANTHROPIC_CUSTOM_HEADERS`' exact encoding for multiple headers | one live request through the proxy with two headers set |
| `renderRing` becomes unnecessary once normalized messages come from the DB | trace what claude-kind backfill actually needs; the "expected untouched" claim was never tested |
| who writes `conversation_attachment` rows, and whether fundi should | read the proxy/library write path |
| pi honours a provider override via the `fundi-helpers` extension | the mechanism is documented and there are two in-tree examples, but fundi has never done it |

## Constraints that bind an unattended run

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
