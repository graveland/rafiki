# The conversation tree

A conversation's messages are a **tree of content-addressed nodes**, not a linear
ordinal sequence. Every write path — claude capture through the proxy, fundi's
own appends, a compaction, a `/clear` — goes through one primitive,
`store.AppendChain`, and identity is a hash of the node's position and content.

The code, in the order this doc reads it:

| File | What it owns |
|---|---|
| `pkg/convtree/convtree.go` | `Canon`, `RootSeed`, `NodeHash`, `Chain`, `HashVersion` — the identity |
| `pkg/store/tree.go` | `AppendChain`, `LoadPath`, `RetractHead` — the write path |
| `pkg/store/tree_backfill.go` | migration 0057's Go step, and `SameMessage` |
| `pkg/store/tree_check.go` | `CheckTree` — the invariant check |
| `pkg/capture/capture.go` | claude's attach decision |
| `pkg/store/compaction.go`, `pkg/llm/` | fundi's mapping onto the tree |
| `cmd/rafikid/tree_check.go` | the `rafikid tree check` verb |

## The model

A **node** is one `conversations.conversation_message` row.

- `parent_id` is the previous message in the history the node was sent after, NULL
  for a root. A compaction summary and a post-`/clear` first message are roots.
- `path_hash` is the node's identity, `hash_version` the algorithm that produced
  it. The two move together (a CHECK enforces it): a different canonicalization
  is a new version, never a redefinition.
- `depth` is the parent's depth + 1, 0 for a root. It is stored so the working
  path can be ordered and validated without a recursive count.
- `provider_message_id` is the provider's id for an assistant reply (`msg_…`) —
  the value a later request names in `diagnostics.previous_message_id`, copied
  onto the node from `conversation_turn.response_message_id` so the lookup does
  not touch the hypertable.
- `derived_from_id` is a lineage edge that is NOT a parent: a compaction summary
  or a `/clear` root points at the node it replaced. It lets a history view
  follow "before the compaction" without making the summary a child of what it
  replaced.
- `(conversation_id, path_hash)` is unique. The write path reads which of a
  request's hashes already exist before inserting, so the index is a tripwire,
  not the matching mechanism.

`conversation.head_id` is the last node on the **working path**; the working path
is the chain from `head_id` up `parent_id` to a root.

**`ordinal` stays, and stays unique per conversation, but it means insertion
sequence now** — immutable, gap-free, allocated from `conversation.next_ordinal`
under the conversation row lock, never reused. It is no longer a position in the
history. That keeps every reader that walks `ORDER BY ordinal` (GetHistory
cursors `m:<ordinal>.<sub>`, recall windows, the insights export, `agent_view`)
working unchanged on the chronological view, which includes abandoned branches
in the order they happened. Because identity carries no timestamp, the same
message sent twice under the same parent (a prompt re-sent after an interrupt)
is ONE node, and the chronological view shows it once.

The active path is a different read (`LoadPath`), and the two must not be
confused: full history is every row in ordinal order, the working set is the
head's path.

## Identity: `Canon` and the path hash

`Canon(role, content)` is **generic JSON**, not the API's typed blocks:

```
decode content with json.Decoder + UseNumber   (trailing data after the value is an error)
a bare JSON string s becomes [{"type":"text","text":s}]
recursively: drop every "cache_control" key and every key whose value is null
json.Marshal([]any{role, v})                   (encoding/json sorts map keys)
```

`NodeHash(parentSlot, msg) = sha256(byte(HashVersion) ‖ parentSlot ‖ Canon(msg))`,
where `parentSlot` is the parent's `path_hash` or, for a root, a `RootSeed`.

**The hashed bytes are Go's `encoding/json` output, exactly.** `Canon` returns
`json.Marshal([]any{role, v})` verbatim and `NodeHash` hashes those bytes, so the
standard library's string escaping is part of the compatibility contract: `<`,
`>`, `&` are written `\u003c`, `\u003e`, `\u0026`, and U+2028/U+2029 are written
`\u2028`/`\u2029`. That is `json.Marshal`'s default HTML-safe escaping, not an
option this code turns off — a reimplementation in another language must
reproduce it (Go's `json.Encoder` only avoids it behind `SetEscapeHTML(false)`,
which would change every hash). Pinned by
`TestCanonEscapesHTMLAndLineSeparators` (`pkg/convtree`), which asserts the
escaped sequences are present and the raw characters are not.

```
RootSeed(kind, derivedFrom) = sha256("root" ‖ kind ‖ d)
    d = *derivedFrom   only when kind == "clear" and derivedFrom != nil
    d = 32 zero bytes  otherwise
```

Root kinds are `root` (a plain root: `kind` column NULL), `compaction_summary`
and `clear` (the row's `kind` column holds the root kind).

**Why generic JSON and not the SDK round trip.** The obvious canonicalizer is a
round trip through `anthropic.ContentBlockUnion`, and it is wrong: that is the
*response* union, so it silently drops `tool_result.content`,
`tool_result.is_error` and an image's `source`. A synthetic error result and the
real result for one tool call would hash as one node. Generic JSON also keeps the
hash independent of SDK upgrades. Pinned by `TestCanonKeepsToolResultContent`,
`TestCanonStripsCacheControl`, `TestCanonDropsNullKeys`, `TestCanonKeyOrderIrrelevant`
and `TestCanonPreservesNumbers` (`pkg/convtree`).

**Why only a clear is seeded by `derived_from`.** fundi's `/clear` boundary is the
constant `store.ClearBoundaryText`, so two clears are distinguishable only by the
node they were cut from — without the seed, the second `/clear` of a conversation
would find the first clear's node and graft the new history under it. A
compaction summary is unique model output, so an equal summary under an equal
history IS the same history; and seeding a compaction root by derived-from would
make the backfill disagree with live capture, where the summary names the reply
it summarised while the last stored row before it can be the summarization side
branch. Pinned by `TestRootSeedDistinguishesKindAndDerivedFrom`,
`TestTwoClearsAreDistinctRoots` (`pkg/store`).

Hashes are taken over **the bytes that are stored** (after `jsonbSafe`), which is
why `Canon`'s input is the jsonb text and not the request body.

## `SameMessage`: one notion of identity across writer, backfill and checker

`store.SameMessage` (`pkg/store/tree_backfill.go`) is the *comparison* identity,
and it is deliberately looser than the hash:

- `convtree.Equal` (canon equality) first;
- otherwise, with the roles equal, both sides are decoded the way `Canon` decodes
  them and compared by **projection**: object keys pairwise, `cache_control`
  dropped exactly as `Canon` drops it, lists element for element, and a key only
  one side has is tolerated **only when its value cannot carry meaning** — null,
  `false`, `""`, zero, a list of such values, or an object whose every value
  other than its `type` tag is such a value.

A one-sided field whose value is NOT zero — a tool result's content, an
`is_error`, an image, a citation, a tag carrying a payload — is a difference,
never noise, so a rewritten message stays distinct.

**Canon equality alone is not enough on real data**, and that is the whole
reason the second pass exists: capture stored an assistant reply as the SDK's
marshalled message, so one side carries a field for every variant of the block
union (`"text":""`, `"id":""`, `"signature":""`, `"content":{"type":"web_search_tool_result_error","error_code":""}`, …)
while the other side is the provider's own JSON for the same reply and may carry
fields the raw form omits (such as `"caller":{"type":"direct"}`). Round-tripping
both through the SDK does not remove the asymmetry, so the comparison projects
instead.

Three callers, one implementation — two would drift:

- **capture** verifies that the message a `previous_message_id` names is the
  message the request replays at that node's depth (`pkg/capture/capture.go`);
- **the backfill** recognises a re-stored copy of a node already placed
  (`pkg/store/tree_backfill.go`);
- **the check** recognises a compaction summary that restates a root
  (`pkg/store/tree_check.go`).

Identity and hashing stay `Canon`: `SameMessage` decides "these two rows are the
same message", never "these two rows are the same node". Pinned by
`TestBackfillSameMessage` (`pkg/store`).

## Writing: `AppendChain`

`AppendChain(ctx, pool, lease, ChainAppend)` (`pkg/store/tree.go`) appends a chain
of one or more nodes under one parent. `AppendNode`/`AppendCompaction`/
`AppendClear`/`SeedChain` are all this call.

`ChainAppend` names exactly one parent mode — `ParentID`, `ParentFromHead` (the
head read under the lock), or `Root` (with an optional `DerivedFromID`/
`DerivedFromHead`) — plus `Fallback` and `ConsumeClearPending`. One transaction:

```
SELECT head_id, next_ordinal FROM conversation WHERE id = $c FOR UPDATE
fence the lease (FOR SHARE on conversation_lease; ErrLeaseLost if it is gone)
resolve the parent slot: the parent's path_hash, or RootSeed(kind, derived-from)
hash the whole chain in Go, from that slot
SELECT id, path_hash, ordinal, depth, created_at FROM conversation_message
 WHERE conversation_id = $c AND path_hash = ANY($hashes)
the existing nodes must form a PREFIX of the chain; m = the first missing index
INSERT nodes m..n (ON CONFLICT (conversation_id, path_hash) DO NOTHING)
UPDATE conversation SET next_ordinal += inserted, head_id = <last node>,
       node_count += inserted, fork_count += forks, fallback_count += fallback,
       payload_bytes += sum(octet_length(content::text)) of the inserted rows,
       clear_pending = false (only when ConsumeClearPending)
```

- The row lock is what allocates a gap-free ordinal range and what makes the head
  move atomic with the insert. Ordinals are assigned `next_ordinal + (i - m)`, so
  only inserted nodes consume one.
- **The head moves to the last node of the chain whether that node was inserted
  or already existed**, which is what makes a client that rewinds onto a branch
  and gets back a stored reply still end up on the right path. An empty `Nodes`
  with a `ParentID` just moves the head there, inserting nothing. `RetractHead`
  (`pkg/store/tree.go`) makes the same move in its own transaction, with its own
  checks (the node must be the head and a user row).
- **Existing nodes must form a prefix of the chain.** A gap means the tree
  disagrees with what is being appended, and it is reported rather than repaired
  — silently rewriting history is the failure this scheme exists to prevent. The
  `ON CONFLICT … DO NOTHING` stays as a tripwire: if the insert returns no row,
  the prefix invariant was broken and the call errors.
- A held lease is re-checked inside the same transaction with `FOR SHARE` on the
  lease row, so a takeover's `UPDATE` blocks until this transaction ends and a
  stalled writer that wakes after takeover writes nothing.
- `fork_count` + 1 means "this append created a branch point" (the parent of the
  first inserted node already had a child); `fallback_count` + 1 means "this
  append attached without a usable `previous_message_id`".

`LoadPath` walks `head_id → parent_id` to a root and returns the path in depth
order; a conversation with no head returns an empty slice. `RetractHead` moves the
head to the parent of the current head, which must be a user row: it is a head
move, never a delete, so the retracted row survives as an abandoned leaf and the
smaller retry is its sibling.

Pinned by `TestAppendChainInsertsChainWithConsecutiveOrdinals`,
`TestAppendChainPartialPrefixInsertsOnlyTheRest`, `TestAppendChainAllExistingMovesHead`,
`TestAppendChainSiblingCountsFork`, `TestAppendChainPrefixInvariant`,
`TestAppendChainConcurrentWritersGetDisjointGapFreeOrdinals`,
`TestAppendChainLeaseFenceBlocksTakeover`, `TestLoadPathFollowsHeadNotOrdinal`,
`TestRetractHeadMovesHeadAndKeepsRow` (`pkg/store`).

## claude: the request names its parent

A replaying client sends its whole history every turn. Within the conversation
the parent is **named**, by `diagnostics.previous_message_id` — the Anthropic id
of the response the request follows, which capture stores on the response node as
`provider_message_id`. It is the same field thread routing already uses.

**The earlier replayed history is never compared.** Only the one message at the
attach node's depth is verified, so a client changing how it replays old messages
— cache markers, thinking blocks, cleared tool results — cannot fork anything:
ancestry is our stored ancestry, named by id. No prefix hashing, no lenient
tier, no aliases.

`planAttach` (`pkg/capture/capture.go`) decides where the request hangs, first
match wins:

1. **No `previous_message_id`.** `conversation.clear_pending` set → a root of
   kind `clear` derived from the current head, the flag consumed in the same
   transaction as the root. Else a root of the conversation whose content equals
   `messages[0]` → attach the rest under it (this is how a retried first request,
   including the first request after a `/clear` whose flag the original consumed,
   lands on the root it already created). Else a plain root.
2. **The id resolves to a node of THIS conversation and `messages[depth]` is the
   assistant message that node holds** (`store.SameMessage`): append
   `messages[depth+1:]` under it. A message already stored hashes to the node it
   occupies and is not duplicated; one that differs (the synthetic
   `No response requested.` where a `tool_use` was stored) becomes a sibling.
3. **The id resolves, verification failed, and `messages[0]` reads as a
   compaction summary**: a `compaction_summary` root derived from that node,
   carrying the prior turn's `input_tokens` as the size of the context it
   replaced. Detected by id and shape, and not counted as a fallback.
4. **Otherwise** (unknown id, or a failed verification that is not a compaction):
   the request's whole hash chain is matched against this conversation's stored
   `path_hash` values in one query and the request attaches at the deepest match.
   `fallback_count` + 1, WARN with the reason.
5. **Nothing matches**: a plain root, derived from the named node when there is
   one. `fallback_count` + 1, WARN with the reason.

A parent is always in the same conversation: a hit in another is treated as
unknown, so a thread's founding request (which names nothing) cannot attach to a
sibling thread's tree.

The **response** is appended under the request's head with its `msg_…` id, and
`conversation_turn` records the request head (`request_head_id`) and the
response's ordinal. The same reply twice is the same node (no new row, head still
moves to it); a different reply to the same history is a sibling, not an error.

**Known limit of the fallback.** The hash-chain match hashes from
`RootSeed(RootKindRoot, nil)`, so it only finds histories under a plain root. A
request whose history begins at a compaction or clear root and names no usable id
attaches as a new root. If telemetry shows the fallback is taken often, the next
step is a named, tested lenient tier plus an alias table mapping a drifted hash
to its stored node — neither is part of this cut.

Pinned by `TestCapturedClaudeSessionReplays` (`pkg/capture`), which drives the
committed session in `testdata/claude-capture` (recorded by `make capture-claude`)
and asserts no request falls back and none re-stores history: the resume's
synthetic message is a sibling of the stored `tool_use`, the `/compact`
summarization request is a sibling of the message it extends, and the post-clear
request is a root derived from the head it cut.

## fundi: the parent is given

fundi holds its working history in memory and appends to its end, so it needs no
hash lookup to find the attach point. The mapping (`pkg/store/compaction.go`,
`pkg/llm/conversation.go`, `pkg/llm/conversation_compact.go`):

| Operation | Tree |
|---|---|
| `Messages.AppendNode` | one `AppendChain` with `ParentFromHead` — the ordinal comes from the counter, never `len(history)`; a message re-sent on resume is the same hash, so it is idempotent |
| `Messages.AppendCompaction` | ONE `AppendChain`: the summary as a `compaction_summary` root (`derived_from` = the head) followed by the tail copies under it, `kind='compaction_tail'` so every reader that skips tails keeps working |
| `Messages.AppendClear` | one node as a `clear` root, `derived_from` = the head; the head moves onto it, so the working path restarts there |
| `Messages.LoadWorking` | a thin alias for `LoadPath` — walk `parent_id` from `head_id` to a root |
| `Conversation.RetractTailUser` | `RetractHead`: move the head back to the parent, never a `DELETE`; the retracted prompt stays as an abandoned leaf and the smaller retry is its sibling |
| `Messages.SeedChain` | a chain from a plain root; an existing prefix is matched by hash so only the missing suffix is inserted |

The in-memory mode used by tests and tools without a database (`messages == nil`)
keeps its slice and its `memHorizon`; it has no tree.

See `docs/reference/fundi-compaction.md` for the policy, the tail rule and the
events.

## The turn descriptor: a pointer into the path, not the request body

`conversation_turn` does not store the request body. It stores a descriptor of
where in the tree that body's content already lives:

- `request_head_id` — the `conversation_message.id` of the last path row the
  request covered. The claude proxy sets it itself, from the head
  `DecomposeRequest` returned; an `llm.Client` turn sets it from its own working
  path. Empty records NULL (an in-memory conversation, whose row ids are
  synthetic).
- `request_trim_from` / `request_trim_to` — the half-open range of path
  positions the request dropped, as indexes into the working path. They are set
  together or not at all, and an inverted range is refused before the row is
  written (`InsertTurnIntent`); both NULL means no trim was recorded.
  `request_trim_from` is where the **anchor message ends**, not a constant 1: a
  merged run of consecutive user rows makes the first wire message cover several
  path rows, so the dropped run starts at the anchor message's end.

The rows a request covered are the working path from its root through
`request_head_id`, minus `[request_trim_from, request_trim_to)`. The exact wire
body is not reproducible from them — the descriptor is best-effort, not a
rebuild: consecutive user rows merge into one message, empty tool names are
rewritten, and cache markers are added. `prefix_content` holds the envelope (the
request minus `messages`), stored on change.

## Reading

- **Chronological** — `store.Messages.Load`, `ORDER BY ordinal`: every row ever
  written, including the branches the working path no longer reaches. This is
  what `GetHistory` (and so `rafiki logs`), `Controller.dbRecent`/`agent_view`,
  recall and the insights export read.
- **Working path** — `LoadPath` (or `ORDER BY depth` over it): the history the
  model last saw. fundi's working set, and what anything that renders only the
  live history needs.
- **Siblings** at a node (`WHERE parent_id = $1`) are the branch points.

A full-history reader must skip `kind='compaction_tail'` rows or it shows a
compaction's tail twice: `eventconv.EventsFromMessages`, `pkg/fundi.DBToPiFramesMessages`,
the `pkg/insights` readers, `pkg/recall` (which also skips `compaction_summary`).
`store.Messages.Load` is deliberately NOT filtered by anything else — a reader
that wants the working set asks for it by name.

## Stats

Four denormalised counters on the `conversation` row, updated by the same
statement that reserves ordinals (migration 0059; `fillfactor 85` on the table so
the per-turn update stays HOT, since none of them is indexed):

| Column | Meaning |
|---|---|
| `node_count` | nodes this conversation's appends inserted |
| `fork_count` | appends that created a branch point (a parent that already had a child) |
| `fallback_count` | appends that attached by hash-chain match or as a new root without a usable `previous_message_id` |
| `payload_bytes` | stored content bytes of the inserted nodes |

0059 seeds `node_count`/`payload_bytes`/`fork_count` from the rows that exist
(`fork_count` there counts parents with more than one child, the same quantity by
another route — exact for a two-way fork, and an upper bound beyond it: a
3+-way fork is one seeded fork point against the live counter's one increment
per branch-creating insert, so the two diverge only in that case; all real forks
are two-way). Derived on read, not stored: active length (`head.depth + 1`),
abandoned nodes (`node_count − active length`), branch points. Token and cost
totals stay on `conversation_turn`.

## The backfill (migration 0057's Go step)

`pkg/store/migrate.go` embeds `*.up.sql` only, so the backfill is a version-keyed
Go step (`goSteps[57] = backfillAllConversations`) that runs in 0057's own
transaction, immediately after its SQL: SQL and backfill commit together or not
at all. On a fresh database it finds no rows and is a no-op.

Per conversation, rows in ordinal order, with a map from hash to node:

- ordinal 0, and every row of kind `compaction_summary` or `clear`, starts a
  **root**, seeded as above. A boundary root's `derived_from_id` is the node the
  walk's cursor stood on — the last node it placed, or the node a matched copy
  moved it onto — so for a claude compaction it can be the summarization side
  branch. Lineage, not identity.
- every other row chains under the cursor: `parent_id`, `depth`, `path_hash`,
  `hash_version`.
- **a run of `compaction_tail` rows directly after a summary is a real tail**
  (fundi's compaction): it chains under the summary like any other row.
- **a run of `compaction_tail` rows that does not follow a summary is an
  interim-fork copy** — the pre-tree capture re-stored a replayed request prefix
  and tagged only the copies. The run's j-th copy duplicates the j-th node of the
  path as it stood when the run began, and is matched to it by `store.SameMessage`
  (not by hash: the original may sit under a root whose seed a fresh chain would
  not reproduce). A matched copy keeps `path_hash`/`hash_version`/`parent_id`
  NULL — a recognised duplicate — and the cursor moves onto the node it matched,
  so the divergent row that follows becomes the sibling it always was.
- response rows get `provider_message_id` from the turn that produced them
  (`conversation_turn.response_message_id` joined on `response_ordinal`).
- `conversation.head_id` = the node the walk ended on (a conversation with no
  rows keeps a NULL head); `next_ordinal` = `max(ordinal)+1`, set for every
  conversation by one statement after the walk (see "Cutover procedure" for why
  that statement matters).

Nothing is deleted or renumbered, and re-running the walk over the same rows
assigns the same values (`TestBackfillIsIdempotent`).

**A copy that matches nothing is logged and becomes a node.** A rehearsal must
report zero `UnmatchedCopies`, or the rule is wrong. That count appears ONLY in
the migrate log line (`conversation tree backfill … unmatched_copies=`), so
capture the migrate output: `tree check` cannot reproduce it. Over the
committed fixture
(`pkg/capture/testdata/interim-capture-rows.json`, 100 rows) the report is 43
nodes, 57 interim copies, 0 unmatched, 0 other duplicates
(`TestBackfillSnapshotFixture`), and the tree it builds equals live capture of the
same session node for node (`TestBackfillMatchesTreeCapture`, `pkg/capture`).

**Residual risk of the looser copy rule.** A copy wrongly recognised as a
duplicate is content-different from the node it claims to duplicate, and it keeps
no position of its own — so the tree would be missing a message. The counters
cannot see this: a tree missing a message is still a well-formed tree. The rule
is pinned only by the committed fixture (`TestBackfillSnapshotFixture`,
`pkg/store`), so a rehearsal must also capture the migrate log's
`unmatched_copies=0`.

## `rafikid tree check`

```
rafikid tree check [--db <dsn>]
```

`--db` defaults to `RAFIKI_DB`, then `RAFIKI_TEST_DSN`. It opens the database
directly — no daemon, no socket — and it is a structural health probe, valid at
any time. Output, one field per line:

| Field | Meaning |
|---|---|
| `conversations` | conversations examined |
| `missing_hash` | rows with no tree position that are neither an interim-fork copy nor a duplicate of a summary root |
| `duplicate_rows` | rows whose content already is a `compaction_summary` root of the conversation, so they hold no position (informational) |
| `bad_hash` | nodes whose stored hash is not the hash their position implies — recomputed from the parent's hash or the root seed — plus a root that is not at depth 0 and a parent that is not a row of this conversation |
| `bad_depth` | children whose depth is not their parent's + 1 |
| `missing_head` | conversations with messages but no `head_id` |
| `ordinal_counter_behind` | conversations whose `next_ordinal` is not ahead of their highest ordinal |
| `ordinal_not_increasing` | edges along which `ordinal` does not strictly increase: a child whose ordinal is not greater than its parent's, and a root whose ordinal is not greater than the node it is `derived_from`. Cursors (`after_ordinal`, the web join, `agent_view`'s `m:<ordinal>`) need ordinals to be monotonic along every linear history |
| `provider_id_missing` | **placed** assistant rows (`path_hash` set) whose turn recorded a `response_message_id` for their ordinal but that carry no `provider_message_id`. A row with no tree position is expected and NOT counted: the backfill deliberately leaves an interim-fork copy and a duplicate summary root positionless, and neither ever gets the id. Among placed rows a non-zero value is a real defect — a backfill or writer that lost the id its turn named — so it gates the cutover |

`missing_hash`, `bad_hash`, `bad_depth`, `missing_head`, `ordinal_counter_behind`,
`ordinal_not_increasing` and `provider_id_missing` fail the check;
`duplicate_rows` does not. It exits non-zero when the check fails, so it can gate
a cutover.

**Scope.** Two different conversation lists, and the difference matters.
`bad_hash`, `bad_depth`, `missing_head`, `ordinal_counter_behind`,
`ordinal_not_increasing`, `provider_id_missing`, `missing_hash` and
`duplicate_rows` cover **every conversation with messages**: `bad_hash` walks
every conversation with messages (`checkConversationsWithMessages`), `bad_depth`,
`missing_head`, `ordinal_counter_behind`, `ordinal_not_increasing` and
`provider_id_missing` are single queries over every conversation, and the two
legacy-row counters classify the unplaced rows of every conversation that holds
one (`checkConversationsWithLegacyRows`, `pkg/store/tree_check.go`), so a
conversation with no unplaced row contributes nothing. A tree's shape and its hashes are properties of the tree, however its
storage was written. Pinned by `TestCheckTreeDetectsEachClass`,
`TestCheckTreeDetectsABadHash`, `TestCheckTreeDetectsBadDepthWithoutALegacyHorizon`,
`TestCheckTreeHealthProbeStaysCleanAfterGoLiveWrites` and
`TestCheckTreeAcceptsABackfilledTree` (`pkg/store`).

**What the check cannot see.** Three blind spots to hold alongside a clean run.
The backfill's `unmatched_copies` appears only in the migrate log line
(`conversation tree backfill … unmatched_copies=`), never in this report, so the
migrate output is what a rehearsal has to capture. A `compaction_tail` row with a
NULL `path_hash` is invisible to every count — expected for a recognised
interim-fork copy, but a backfill misclassification leaves no trace. A non-root
row with a NULL `depth` escapes both `bad_depth` (`NULL <> x` is not TRUE) and
`bad_hash` (depth is checked only for roots). No defect here is
one the writers can produce: `AppendChain` always writes a position and a depth,
and the only positionless rows the backfill leaves are the copies it recognised.
Each therefore means data edited by hand — which is also why none is repaired.

## Cutover procedure

The cutover is offline, and **rolling back is restoring the backup taken just
before it**. The migration runner applies `*.up.sql` only, so a down path was
never real; designing for a restore instead removes the lazy backfill, the
background sweep, partial-index exemptions for legacy rows, and any period in
which old and new daemons share the data. One daemon writes a database; the
cutover is a restart.

1. `TRUNCATE conversations.raw_http_request` (29 GB; a debugging aid). This is
   what makes the backup and the rehearsal copy cheap.
2. Stop the daemon. `pg_dump -Fc` the database.
3. **Rehearse:** restore the dump into a new database `rafiki_migrate`
   (`timescaledb_pre_restore()` before, `timescaledb_post_restore()` after).
   Restoring, rather than `CREATE DATABASE … TEMPLATE`, is the point: it proves
   the backup restores. Run `rafikid migrate` against it, then `rafikid tree
   check`, and record the time of each step. Repeat from the dump until clean.
   The restore needs prod's extension versions (timescaledb 2.30.1,
   pg_textsearch 1.5.0-dev at the time of writing): restore on prod's own host as
   a superuser, or on a local server upgraded to match — the local `pg` container
   (timescaledb 2.28.3, pg_textsearch 1.3.0) is not one. Pass `--db` to every
   `rafikid` command: `service.env` sets `RAFIKI_DB` to prod and every invocation
   loads it. Earlier iterations run against a data-only shadow copy instead
   (schema from our own migrations, so the version skew does not apply).
4. Migrate the real database, run `rafikid tree check`, `VACUUM FULL
   conversations.conversation` (repacks the pages `fillfactor 85` only applies to
   new writes), start the new daemon. Keep the dump until the new daemon has run
   for a while.

**The gate is `rafikid tree check` BEFORE the new daemon starts**, and the same
check is the post-go-live health probe: it reports structure only, so it must
exit 0 then too (pinned by
`TestCheckTreeHealthProbeStaysCleanAfterGoLiveWrites`, `pkg/store`, which forks
and compacts a conversation through the real writers).

The pre-start working-set comparison (`--cutover`) was removed with the legacy
`resume_from_ordinal` column (migration 0062). An installation upgrading from a
release before the conversation tree must first upgrade to the last release that
carries migrations 0057-0061 and run the gate there; the structural `rafikid tree
check` remains.

**Rollback** is: stop the daemon, restore the dump from step 2, start the previous
release. Anything captured after the cutover is lost, which is why the dump is
taken with the daemon stopped and the cutover is a short window.

**Measured on the real cutover** (3,583 conversations, 321,165 messages, a remote
database): `rafikid migrate` 1 m 07 s — the backfill line reads
`conversations=3583 nodes=321165 interim_copies=1500 unmatched_copies=0
other_duplicates=2` — then `rafikid tree check` 49 s, exit 0, with `bad_hash 0`,
`missing_hash 0`, `provider_id_missing 0` and `duplicate_rows 2`. Peak RSS of the migrating
process is 61-63 MB — the backfill is bounded per conversation, so its memory
does not track the size of the database. Expect roughly 2 minutes of downtime;
budget the rehearsal's number for the machine it will really run on, because the
same migration took 24 s against a local copy of the same data.

**Ordering: migrate, then start — never start, then migrate.**
`conversation.next_ordinal` is seeded by the backfill step (migration 0057), not
by 0059, and only a pre-tree daemon writing after the migration leaves it stale.
The next tree append then fails loudly on `UNIQUE (conversation_id, ordinal)`
rather than corrupting anything. The one-line repair for a database that broke the
rule is the statement the backfill itself runs:

```sql
UPDATE conversations.conversation SET next_ordinal = coalesce((
  SELECT max(ordinal)+1 FROM conversations.conversation_message
   WHERE conversation_id = conversation.id), 0);
```

**A conversation that predates the tree and was never backfilled fails loudly,
but incidentally.** There is no explicit refusal: `next_ordinal` is still 0
against existing ordinals, so the first append collides on the pre-existing
`UNIQUE (conversation_id, ordinal)` (SQLSTATE 23505) and the write errors. That is
loud and never silent, but it is a by-product of the unique index rather than a
designed check; it catches a conversation captured between the migration and the
backfill, which is possible only by skipping the stop-the-daemon rule. The repair
is the backfill, or the one-line `UPDATE` above, which is the same statement it
runs — never a retry.

## Tests

- `pkg/convtree` — the canon rules (including `TestCanonEscapesHTMLAndLineSeparators`,
  the escaped-bytes pin), the root seed, and `TestGoldenFixtureChains`
  / `TestParityStoredVersusReplayed` over `testdata/claude-capture`.
- `pkg/store` — `TestAppendChain*`, `TestLoadPath*`, `TestRetractHead*`,
  `TestBackfill*`, `TestCheckTree*`, `TestCompaction*`, `TestTwoClearsAreDistinctRoots`.
- `pkg/capture` — `TestCapturedClaudeSessionReplays`, `TestReplayTwiceAddsNoRows`,
  `TestBackfillMatchesTreeCapture`, `TestARewindAttachesAtTheNamedNode`,
  `TestACompactionSummaryAfterAKnownReplyIsARoot`, `clear_boundary_test.go`.
- `pkg/llm` — `TestCompactionWorkingSetAfterCompaction`,
  `TestCompactionSecondCompactionComposes`, `TestClearRestartsWorkingSet`.
