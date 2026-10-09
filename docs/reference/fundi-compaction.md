# fundi context compaction

The contract for a fundi conversation that nears its context window: the earlier
history is replaced by one model-written summary plus a verbatim tail. Claude
Code children compact themselves; this is fundi's own compaction, run by
`pkg/llm` and driven by `pkg/fundi`.

## What it does

When a fundi conversation approaches its model's context window, rafiki asks the
model for a plain-text handover summary of the older history and then moves the
conversation's resume horizon past it. The working context becomes one summary
row plus a verbatim copy of the most recent tail; every earlier row is kept on
disk, untouched. Nothing is deleted or renumbered.

The mechanism lives in `pkg/llm`: `CompactionPolicy`
(`pkg/llm/compact_policy.go`) decides when and how much, `Conversation.compact`
(`pkg/llm/conversation_compact.go`) runs the summary call and writes the
boundary, and `store.Messages.AppendCompaction` (`pkg/store/compaction.go`)
persists it.

## Triggers

Two triggers, both gated on a `CompactionPolicy` being configured and on at
least `minCompactableRows` (4) working rows (`pkg/llm/compact_policy.go`):

- **Proactive (threshold).** `Conversation.Continue` compacts when
  `ContextWindow − used ≤ HeadroomBuffer` (`shouldCompact`). After a response
  `used` is the last response's reported total — input + cache read + cache
  creation + output — plus an estimate of the rows appended since it (tool
  results and steers not yet sent). Before any response in this process (fresh,
  resumed, or just compacted, so `usedTokens == 0`) it is a byte estimate of the
  whole working set plus `system/4`; tool definitions are NOT counted, so this
  under-estimates until the first response (the overflow net covers the gap).
  The default buffer is 40k tokens and is raised to at least
  `SummaryMaxTokens + 4000` so the summary call's own output and prompt fit
  beneath it (`withDefaults`).
- **Reactive (overflow).** If the API rejects the request as too large
  (`isPromptTooLarge`, an input-size error) and compaction is available, rafiki
  gets exactly ONE chance to compact and retry on the compacted history before
  falling back to the destructive trim policy (`sendWithTrim`'s `onOverflow`).
  This net is MECHANICS-ONLY: an input-size 400 recurs on the summary call
  itself (the summary call carries the same oversized history), so a model whose
  catalog window is unknown effectively never compacts and still gets the trim.

A failed proactive attempt (a failed summary call, a truncated or absent
summary) is NOT retried on every tool iteration: `Continue` suppresses the next
proactive attempt until `usedForCheck` has grown by half the headroom buffer
since the failure (`compactSuppressBelow`), cleared on a successful compaction.
The reactive overflow net stays armed regardless.

An unknown context window (`ContextWindowFn` nil, or returning ≤ 0) disables the
proactive trigger entirely — only overflow can compact it. Fewer than 4 working
rows never compacts. fundi supplies the window from the model catalog
(`compactionContextWindow`, `pkg/fundi/engine.go`); a model whose window the
catalog does not know returns 0 and so never triggers proactively.

## Rows

`AppendCompaction` writes the boundary in one transaction, locking the
conversation row `FOR UPDATE` first (serialising concurrent compactors) and
fencing every insert on the held conversation lease:

- **Summary** at the next ordinal (`max(ordinal) + 1`), with the summary
  message's role (user), `kind='compaction_summary'`, and `input_tokens` = the
  size of the context it replaced (the summary response's
  input + cache read + cache creation).
- **Tail copies** at the ordinals after the summary, `kind='compaction_tail'`,
  each carrying the original row's `stop_reason` and NULL token columns — a
  verbatim duplicate of a row that still exists earlier in the table.
- **Horizon**: `conversation.resume_from_ordinal` is set to the summary's
  ordinal, in the same transaction.

Nothing is deleted or renumbered; the call only appends. Any error rolls back,
leaving the horizon and the row set exactly as they were.

## Working set vs full history

Two reads of `conversations.conversation_message` exist, and which one a caller
uses is the whole point:

- **Working set** — `store.Messages.LoadWorking` returns rows at or after
  `resume_from_ordinal`. `llm.Conversation.loadHistory` (and so `History`,
  `agentloop.Resume`, orphan repair and `classifyPrefill`) reads only this.
- **Full history** — `store.Messages.Load` is deliberately NOT filtered by the
  horizon. `GetHistory` (which serves `rafiki logs`), `Controller.dbRecent`
  (which serves GetRecent / the `agent_view` tool) and recall read every row.

A full-history reader must skip `kind='compaction_tail'` rows or it shows the
tail twice: `eventconv.EventsFromMessages` skips them and synthesises a
`CompactionBoundary` from the summary row instead; `pkg/fundi.DBToPiFramesMessages`
(the agent_view path) skips them and renders the same boundary divider; the
`pkg/insights` readers (the export query and the tools/skills/sizes/classes
catalogue CTEs) skip them; and `pkg/recall` skips them in both its extraction
and summariser paths. `pkg/recall` also skips the `compaction_summary` row in
its extraction (`extract.go`) and cuts windows at summaries (`summarize.go`), so
a summary is never re-summarised as ordinary conversation.

## Tail rule

The verbatim tail is bounded by `CompactionPolicy.tailBudget`: `window ×
TailPercent / 100` (default 10%), capped at `TailCap` (default 50k tokens); the
cap alone when the window is unknown; zero under `NoTail`.

`cutTail` picks the earliest legal start whose suffix fits the budget. A legal
cut point is an assistant row, or a user row with no `tool_result` block — a
`tool_result` must never be separated from the `tool_use` that produced it
(`isLegalCut`). When nothing fits, the tail is empty and the boundary is
summary-only. The oldest row is always part of what the summary replaces: index
0 is never a tail start.

## Cache

The summary call reuses the previous request to keep the prompt cache warm: it
is the same message list with the compaction prompt appended as one text block
to the trailing user message (or a new user message when the last row is an
assistant turn), and the same tools, system prompt and thinking config
byte-identical (`Conversation.compact`). Only the output cap, source, tool
choice and the streaming/batch hooks differ — `tool_choice` is cleared so the
summary is text-only.

The effect: the summary call hits the cached tools + system prefix. After the
boundary the history prefix is new and cold — the summary and tail are fresh
rows — while tools and system stay cached.

## Failure

The summary call is **NON-STREAMING** (`Conversation.compact` clears
`streamHandler`): a fake Anthropic seat must answer it with a plain JSON body,
not SSE, or it silently no-ops — a plain-JSON 200 parses as an empty message,
which has no `<summary>` and writes nothing. This is the one non-streaming
request a fundi turn makes; ordinary turns stream.

A failed summary call, a summary truncated by `max_tokens`, or a response with
no extractable `<summary>` block writes nothing: `compact` returns `ok=false`
and the turn proceeds on the unchanged history (`Conversation.compact`,
`extractSummary`). A summary cut off by `max_tokens` is never stored — an
unclosed handover would otherwise be accepted downstream as complete — but an
unclosed `<summary>` tag on a normal stop IS taken verbatim to the end of the
text.

A compaction failure is **invisible at the default log level**: the child's own
`llm.Client` logs at ERROR, so pkg/llm's Warn on a failed compaction is dropped.
The daemon-side diagnostic is `Engine.onCompaction` logging an ERROR for a
non-`Succeeded` End event, and the ground truth is the DB: no
`conversation_message` row at `resume_from_ordinal` with
`kind='compaction_summary'`. The summary call is itself captured as a
`conversation_turn` with `source='compaction'` at the ordinal the summary row
lands on, so insights' `turnMetricsByOrdinal` attributes it to that row.

A failed proactive attempt is not retried on the next tool iteration:
`Conversation.compactSuppressBelow` defers the next proactive attempt until the
working set has grown by half the headroom buffer (`HeadroomBuffer/2`), cleared
on success. The reactive overflow net is armed regardless.

### Known limitations

- A `:batch` child whose compaction leaves a working set with no assistant row —
  an empty tail, or a tail of only tool_result-free user rows — re-parks its next
  turn as though it were the conversation's first call (`firstCall`,
  `pkg/llm/batch.go`): a long unexpected wait, not corruption. Narrow in
  practice: `NoTail` is never set in production, and `cutTail` normally keeps an
  assistant row.
- The daemon-side ERROR line for a failed compaction (`Engine.onCompaction`)
  names only the trigger, not the child or conversation.


## Events

A compaction is observable two ways:

- **Live frames.** The fundi emitter publishes `compaction_start` and
  `compaction_end` frames; `child.StateMachine` pushes `compacting` on the first
  and pops it on the second, so the child's status shows `compacting` for the
  duration (`pkg/child/state.go`). On a successful end the emitter also
  publishes a native `CompactionBoundary` event carrying the trigger and the
  pre/post token counts.
- **Reattach.** `eventconv.EventsFromMessages` synthesises the same
  `CompactionBoundary` from the stored `compaction_summary` row, so a client
  backfilling history renders the boundary as the same divider it saw live.

The cockpit renders a `CompactionBoundary` as a mid-conversation system block
that never settles in-flight tool calls (`pkg/tui/session/session.go`).
