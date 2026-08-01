# `fundi conversations` — persisted conversation insights over the daemon socket — design

`rafiki agent <stats|search|export>` already answers "how much did this cost", "find me
the conversation where X went wrong", and "show me that transcript" — but only as a
separate, DSN-backed binary invocation (`rafiki agent stats --db postgres://...`). `fundi`,
the CLI you actually reach for day to day, has no path to that data at all: `fundi search`
only walks live children's in-memory ring buffers, and there is no `fundi stats`.

This gives `fundi` the same three read-only verbs, routed through the daemon it already
talks to, rather than by teaching the CLI to open its own database connection.

`analyze`/`findings` (the LLM-driven skill-gap pipeline) are explicitly out of scope here —
noted for later, not designed now.

## Why route through the daemon instead of connecting directly

`fundid` already opens a `*pgxpool.Pool` from `FUNDI_AGENT_DB` at startup
(`cmd/fundid/main.go`) to persist agent conversations for cost accounting. `rafiki agent`'s
three read-only verbs are themselves a thin CLI over `pkg/agentcli/local.Backend`, which is
nothing but that same kind of pool plus `pkg/insights` queries.

Two ways to give `fundi` the same verbs:

1. **Route through the daemon** (chosen). Wrap the daemon's existing pool in
   `agentcli/local.Backend` once at startup, and add three new `ctrl_*` RPCs that delegate to
   it. `fundi` stays what it already is architecturally: a pure socket client that never
   touches Postgres directly.
2. **Give `cmd/fundi` its own DSN and pool**, mirroring `cmd/rafiki/agent.go` almost verbatim.
   Substantially less code — the query and render logic already exists and could be copied
   wholesale. Rejected because it breaks the boundary the README documents (fundi talks to
   the local daemon socket; nothing else), and means every machine invoking `fundi
   conversations stats` needs direct Postgres network access and credentials, instead of just
   the daemon host needing them (which it already does, today, for accounting writes).

Option 1 also means there is exactly one place a Postgres connection to the `conversations`
schema is opened from the fundi/rafiki side of things (`rafiki serve` is the other, separate
process). No second pool to size, no second DSN to rotate, no risk of the two query paths
drifting.

## Command surface

```
fundi conversations stats [conv-id] [--since --until --owner --persona --source --model --path]
fundi conversations search [--since --until --owner --persona --source --model --path --status --min-tokens --text --limit]
fundi conversations export <conv-id>
```

New top-level group, not bare `fundi stats`/`fundi search`, and not folded into the existing
`fundi search`. `fundi search` already means something specific and incompatible: live,
in-memory, regex/context-lines/session-label search across *currently running* children — the
protocol doc says so explicitly (§6.15: *"Out of scope for v1: scanning historical session
files on disk that are not currently owned by any live child."*). The new verbs search the
opposite population — persisted history in Postgres, filtered by owner/model/time-range/text,
independent of whether anything is still running. Same verb name, incompatible flag surface
and semantics, would be a footgun. `conversations` as the group name also matches the schema
these verbs actually query (`rafiki` "owns the `conversations` schema" per its own README).

Flag names and semantics copy `rafiki agent` exactly (`--since`/`--until` accept RFC3339 or a
duration like `24h`, same filter set, same `--min-tokens`/`--text`/`--limit` on search) so
muscle memory carries over between the two tools.

## Protocol

Three new `ctrl_*` types, added to the existing JSONL control-plane protocol described in
`docs/reference/pi-controller-protocol.md` — same transport, same framing, same
`Request{Type,ID,...}` → `Response{Success,Data,Error}` envelope, same `id` correlation. New
§6.17–6.19 sections, following the §6.15/§6.16 (`ctrl_search`/`ctrl_status`) precedent.

**These three are fundi-specific**, not shared vocabulary with stock pi-controller. Everything
currently in the protocol (`ctrl_spawn`, `ctrl_search`, ...) is answerable by either daemon,
which is the whole point of forking pi-controller's wire format — `pic` and the pi TUI attach
to either. A pi-controller daemon has no `agent` child kind, no `FUNDI_AGENT_DB`, no
`conversations` schema; `pic` sending `ctrl_conversation_stats` to a real pi-controller daemon
gets `unknown command`. The protocol doc should say this plainly next to these three sections,
rather than imply symmetry that isn't there — fundi has done this before (the `agent` child
kind itself is the same kind of divergence).

### Wire shapes

`pkg/protocol` is deliberately a pure-data, zero-dependency package (its own header: *"no
logic, no I/O"*) — it imports nothing but `encoding/json`, so that the wire contract stays
readable as a single file independent of the query/storage engine behind it. The new request
and response types keep that invariant: they do **not** reuse `pkg/insights` types directly,
even though the response shapes are structurally identical to `insights.Stats` /
`insights.ConversationSummary` / `insights.Transcript`. Converting between them is a few lines
in `pkg/control` (see below), matching the existing `snapshotToSummary(childstore.Snapshot)
protocol.ChildSummary` precedent for `ctrl_list`/`ctrl_get`.

`Since`/`Until` cross the wire as **already-resolved Unix seconds** (`0` = unset), not as raw
duration strings — the CLI resolves `--since 24h` client-side (reusing
`agentcli.BindStatsFilter`/`BindSearchFilter`, the exact same parsing `rafiki agent` uses, so
there's no second implementation of "24h" parsing to keep in sync) before putting the request
on the wire. This matches the protocol's existing convention of carrying resolved Unix-second
timestamps elsewhere (`ctrl_search`'s `sessionFilter.since`, `ctrl_status`'s `startedAt`).

```go
// pkg/protocol/types.go

const (
	TypeCtrlConversationStats  = "ctrl_conversation_stats"
	TypeCtrlConversationSearch = "ctrl_conversation_search"
	TypeCtrlConversationExport = "ctrl_conversation_export"
)

// ConversationStatsRequest queries persisted conversation stats: global
// (filtered) if ConversationID is empty, scoped to one conversation
// otherwise — in which case the filter fields below are ignored (§6.17).
type ConversationStatsRequest struct {
	Type           string `json:"type"`
	ID             string `json:"id,omitempty"`
	ConversationID string `json:"conversationId,omitempty"`
	SinceUnix      int64  `json:"sinceUnix,omitempty"`
	UntilUnix      int64  `json:"untilUnix,omitempty"`
	Owner          string `json:"owner,omitempty"`
	Persona        string `json:"persona,omitempty"`
	Source         string `json:"source,omitempty"`
	Model          string `json:"model,omitempty"`
	Path           string `json:"path,omitempty"`
}

// ConversationSearchRequest searches persisted conversation history (§6.18).
type ConversationSearchRequest struct {
	Type      string `json:"type"`
	ID        string `json:"id,omitempty"`
	SinceUnix int64  `json:"sinceUnix,omitempty"`
	UntilUnix int64  `json:"untilUnix,omitempty"`
	Owner     string `json:"owner,omitempty"`
	Persona   string `json:"persona,omitempty"`
	Source    string `json:"source,omitempty"`
	Model     string `json:"model,omitempty"`
	Path      string `json:"path,omitempty"`
	Status    string `json:"status,omitempty"`
	MinTokens int64  `json:"minTokens,omitempty"`
	Text      string `json:"text,omitempty"`
	Limit     int    `json:"limit,omitempty"`
}

// ConversationExportRequest fetches one conversation's full transcript (§6.19).
type ConversationExportRequest struct {
	Type           string `json:"type"`
	ID             string `json:"id,omitempty"`
	ConversationID string `json:"conversationId"`
}
```

Response payload types (`ConversationStatsResponseData`, `ConversationSearchResponseData` —
a `{"rows": [...]}` wrapper matching `ListResponseData`'s `{"children": [...]}` shape rather
than a bare array, so a future field can be added without breaking the envelope —
`ConversationExportResponseData`) are field-for-field mirrors of `insights.Stats` /
`insights.ConversationSummary` / `insights.Transcript`, same JSON tags. Deliberately
mechanical; not reproduced in full here.

### Errors

No DB configured on the daemon (`FUNDI_AGENT_DB` unset ⇒ nil pool ⇒ nil backend) is not a
crash or a 5xx-shaped thing — it's an expected, already-precedented state (the daemon already
runs fine with agent conversations in-memory, no cost data). The three new handlers return a
`ctrl_response` with `success:false` and a new error code, `no_agent_db` (matching §8's existing
lowercase-`snake_case` codes — `child_not_found`, `invalid_args`, `internal`, etc.; not the
`ERR_`-prefixed style used elsewhere in this codebase), message pointing at `FUNDI_AGENT_DB` and
`fundi service install` — same actionable spirit as the daemon's own startup log line, surfaced
now to the client that actually asked instead of only to whoever's watching the daemon's logs.

## Daemon wiring

- `cmd/fundid/main.go`: right after the existing `pool` construction, build
  `agentcli/local.New(agentcli_local.Options{Pool: pool})` when `pool != nil`; `nil` backend
  when `pool == nil`, mirroring the existing branch exactly. Pass the backend into whatever
  constructs the real `Controller`.
- `pkg/control`: `Controller` interface gains
  `ConversationStats(ctx, ConversationStatsQuery) (*insights.Stats, error)`,
  `ConversationSearch(ctx, ConversationSearchQuery) ([]insights.ConversationSummary, error)`,
  `ConversationExport(ctx, id string) (*insights.Transcript, error)` — three new `case` arms in
  `dispatch.go` alongside the existing `TypeCtrlSearch`/`TypeCtrlStatus` ones, each: decode
  request → build the query/filter type → call through → convert the domain result to the wire
  response type → `okResponse`/`errResponse`. A nil backend short-circuits to
  `ERR_NO_AGENT_DB` before touching the (nil) pool.
- `pkg/control/dispatch_test.go`'s fake `Controller` gets the three new methods (canned data),
  so dispatch tests cover encode/decode and error-code plumbing without a real database —
  same shape as every other dispatch test in that file today.

## CLI wiring

- New `cmd/fundi/cmd_conversations.go`: a `conversations` parent `cobra.Command` with three
  subcommands, each following `cmd_status.go`'s existing dial → `c.Request(...)` →
  check-`Success` → decode pattern verbatim.
- Flag binding reuses `agentcli.FilterVals` + `agentcli.BindStatsFilter`/`BindSearchFilter`
  (imported from the same module, already used by `cmd/rafiki`) to parse and validate
  `--since`/`--until`/etc. client-side, then the resolved `*time.Time`s convert to
  `SinceUnix`/`UntilUnix` for the wire request.
- Rendering: reuse `pkg/agentcli.RenderStats` / `RenderSearch` / `RenderTranscriptMD` — decode
  `resp.Data` into the corresponding `insights.*` shape (same JSON tags, so this is a plain
  `json.Unmarshal`) and hand it to the existing render functions for the human-readable
  default. `--output json` (fundi's existing global flag) short-circuits to raw `resp.Data`
  passthrough, same as `cmd_status.go` does today.

## Testing

- `pkg/control/dispatch_test.go`: table cases per new `ctrl_*` type — success, malformed
  request, nil-backend `ERR_NO_AGENT_DB`.
- `cmd/fundi`: one request/response round-trip test per new subcommand against a fake socket
  server, matching the existing `cmd_*_test.go` pattern in that package.
- Manual verification: `FUNDI_AGENT_DB=... fundi service restart && fundi conversations stats`
  against a live database, mirroring the verification style already recorded for `rafiki agent
  stats` in `docs/agent-cli.md`.

## Out of scope

`analyze` and `findings` stay `rafiki agent`-only for now: `analyze` needs LLM credentials and
runs as a long batch job (streamed progress, not a quick round-trip), and `findings` is triage
tooling on top of it. Both are plausible future `fundi` verbs but weren't asked for here.
