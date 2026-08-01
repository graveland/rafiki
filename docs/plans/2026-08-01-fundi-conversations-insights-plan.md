# fundi conversations (stats/search/export) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Give `fundi` three new commands — `fundi conversations stats|search|export` — that answer the same questions `rafiki agent stats|search|export` does, routed through the daemon's existing socket instead of opening a second Postgres connection.

**Architecture:** Three new `ctrl_*` wire types (`pkg/protocol`) carry filter/id requests; `pkg/control`'s dispatcher decodes them and calls four new `Controller` interface methods; the real `Controller` (`cmd/fundid`) answers them by wrapping its existing `*pgxpool.Pool` in `agentcli/local.Backend` (the same backend `rafiki agent` already uses) and delegating straight through. Response payloads are `pkg/insights` types marshaled as-is — no parallel wire-type duplication. `cmd/fundi` gets a new `conversations` command group that dials, sends, and re-emits `resp.Data` verbatim, matching every other non-`list`/`tail` fundi command.

**Tech Stack:** Go 1.26 (module `go.graveland.dev/rafiki`), `cobra` for CLI, `pgx/v5` (`pgxpool`), JSONL-over-UDS control protocol.

**Source:** `docs/plans/2026-08-01-fundi-conversations-insights-design.md` (the approved spec — read it first, this plan does not repeat its rationale).

## Global Constraints

- **Single repo, no worktree needed for this size of change** — work directly in `/Users/brent/home/rafiki` unless told otherwise.
- **`go vet ./...` = 0, `go test ./...` = 0, `golangci-lint run ./... --max-same-issues=0 --max-issues-per-linter=0` = 0.** Both are clean at HEAD; any finding after your change is yours. Do not use `go build` to check for compile errors — use `go vet ./...` (project convention).
- **rafiki tests silently skip without `RAFIKI_TEST_DSN`.** None of the new tests in this plan need a live database (the daemon-side test uses a nil pool deliberately), so this shouldn't bite — but if you add anything that does need a DSN, source `.env` first (`set -a; . ./.env; set +a`) and report the skip count, not just the exit code.
- **Capture exit codes by redirecting, never piping then reading `$?`** — `$?` after a pipeline is the last element's status. Use `cmd > /tmp/out 2>&1; echo "exit=$?"`.
- **Verify lint uncapped.** golangci-lint's default `--max-same-issues=3` truncates non-deterministically; always pass `--max-same-issues=0 --max-issues-per-linter=0` when checking.
- **No `Co-Authored-By` trailers in commit messages** (repo convention, matches user preference).
- **Field names in every new struct below are final** — copy them verbatim across tasks; a mismatched name between the protocol struct (Task 1), the dispatch handler (Task 2), and the CLI (Task 4) is a real bug, not a style nit.

---

### Task 1: Protocol wire types, error code, and reference doc

**Files:**
- Modify: `pkg/protocol/types.go`
- Modify: `pkg/protocol/types_test.go`
- Modify: `docs/reference/pi-controller-protocol.md`

**Interfaces:**
- Produces: `protocol.TypeCtrlConversationStats`, `protocol.TypeCtrlConversationSearch`, `protocol.TypeCtrlConversationExport` (string consts); `protocol.ConversationStatsRequest`, `protocol.ConversationSearchRequest`, `protocol.ConversationExportRequest` (structs, see below); `protocol.ErrNoAgentDB = "no_agent_db"`. Task 2 consumes all of these by exact name.

- [ ] **Step 1: Write the failing round-trip tests**

Add to `pkg/protocol/types_test.go`, directly after `TestStatusRequest_RoundTrip` (around line 255):

```go
func TestConversationStatsRequest_RoundTrip(t *testing.T) {
	req := protocol.ConversationStatsRequest{
		Type:           protocol.TypeCtrlConversationStats,
		ID:             "req-20",
		ConversationID: "conv-abc",
		SinceUnix:      1716000000,
		UntilUnix:      1716100000,
		Owner:          "brent",
		Persona:        "default",
		Source:         "cli",
		Model:          "claude-sonnet-5",
		Path:           "direct",
	}
	roundTrip(t, req, &protocol.ConversationStatsRequest{})
}

func TestConversationSearchRequest_RoundTrip(t *testing.T) {
	req := protocol.ConversationSearchRequest{
		Type:      protocol.TypeCtrlConversationSearch,
		ID:        "req-21",
		SinceUnix: 1716000000,
		UntilUnix: 1716100000,
		Owner:     "brent",
		Persona:   "default",
		Source:    "cli",
		Model:     "claude-sonnet-5",
		Path:      "direct",
		Status:    "failed",
		MinTokens: 5000,
		Text:      "skill gap",
		Limit:     20,
	}
	roundTrip(t, req, &protocol.ConversationSearchRequest{})
}

func TestConversationExportRequest_RoundTrip(t *testing.T) {
	req := protocol.ConversationExportRequest{
		Type:           protocol.TypeCtrlConversationExport,
		ID:             "req-22",
		ConversationID: "conv-abc",
	}
	roundTrip(t, req, &protocol.ConversationExportRequest{})
}
```

- [ ] **Step 2: Run the tests to verify they fail (compile error — the types don't exist yet)**

Run: `go test ./pkg/protocol/... > /tmp/step2.log 2>&1; echo "exit=$?"`
Expected: `exit=1`, log shows `undefined: protocol.ConversationStatsRequest` (and friends).

- [ ] **Step 3: Add the type constants**

In `pkg/protocol/types.go`, extend the `const ( ... )` block at line 17 (the one containing `TypeCtrlStatus`), adding three lines right after `TypeCtrlChildLabeled = "ctrl_child_labeled"` (line 45, just before the closing `)`):

```go
	TypeCtrlConversationStats  = "ctrl_conversation_stats"
	TypeCtrlConversationSearch = "ctrl_conversation_search"
	TypeCtrlConversationExport = "ctrl_conversation_export"
```

- [ ] **Step 4: Add the error code constant**

In the same file, extend the error-code `const ( ... )` block (starts at line 66, contains `ErrChildNotFound` etc.), adding right before the closing `)` at line 95:

```go
	// ErrNoAgentDB is returned by ctrl_conversation_* commands when the
	// daemon has no agent database configured (FUNDI_AGENT_DB unset).
	ErrNoAgentDB = "no_agent_db"
```

- [ ] **Step 5: Add the three request types**

Add after `StatusRequest` (ends at line 343, right before the `─── Response envelope ───` comment at line 345):

```go
// ConversationStatsRequest queries persisted conversation stats: global
// (filtered) when ConversationID is empty, scoped to one conversation
// otherwise — in which case the filter fields below are ignored (§6.17).
// SinceUnix/UntilUnix are Unix seconds; 0 means unbounded.
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
// SinceUnix/UntilUnix are Unix seconds; 0 means unbounded.
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

- [ ] **Step 6: Run the tests to verify they pass**

Run: `go test ./pkg/protocol/... -run RoundTrip -v > /tmp/step6.log 2>&1; echo "exit=$?"`
Expected: `exit=0`, all three new `TestConversation*Request_RoundTrip` show `PASS`.

- [ ] **Step 7: Update the protocol reference doc**

In `docs/reference/pi-controller-protocol.md`, insert three new sections after §6.16 (`ctrl_status`, ends right before `## 7. Controller → client events`):

```markdown
### 6.17 `ctrl_conversation_stats`

**fundi-specific.** Unlike every other command in this document, this is not answerable by a
stock pi-controller daemon — it requires the `agent` child kind's database
(`FUNDI_AGENT_DB`), which pi-controller has no concept of. `pic` sending this to a real
pi-controller daemon gets `unknown command`.

Global stats over persisted conversation history, or stats for one conversation when
`conversationId` is given (filter fields are then ignored). `since`/`until` are Unix seconds
(0/absent = unbounded).

```jsonc
{
  "type":  "ctrl_conversation_stats",
  "id":    "30",
  "owner": "brent",
  "since": 1716000000
}
```

Response `data` is the same JSON shape `rafiki agent stats -j` prints (`pkg/insights.Stats`):
volume, adoption, token, cost, failure, latency, cache-waste, and prefix-reuse facets. See
`docs/agent-cli.md` for the field-level description; not duplicated here since it's the exact
same struct.

Error `no_agent_db` (§8) means the daemon has no database configured.

### 6.18 `ctrl_conversation_search`

**fundi-specific** (see §6.17). Searches persisted conversation history — the opposite
population from `ctrl_search` (§6.15), which is live-only and explicitly does not scan
historical sessions.

```jsonc
{
  "type":      "ctrl_conversation_search",
  "id":        "33",
  "text":      "skill gap",
  "status":    "failed",
  "minTokens": 5000,
  "limit":     20
}
```

Response:

```jsonc
{
  "type": "ctrl_response", "command": "ctrl_conversation_search", "id": "33",
  "success": true,
  "data": { "rows": [ /* insights.ConversationSummary, same shape rafiki agent search -j prints */ ] }
}
```

Error `no_agent_db` (§8) means the daemon has no database configured.

### 6.19 `ctrl_conversation_export`

**fundi-specific** (see §6.17). Fetches one persisted conversation's full transcript.

```jsonc
{ "type": "ctrl_conversation_export", "id": "35", "conversationId": "conv-abc" }
```

Response `data` is `insights.Transcript` (same shape `rafiki agent export -j` prints):
ordered turns with role, content, per-turn token/latency/model metrics, and the recovered
skill catalog.

Errors: `invalid_args` when `conversationId` is missing; `no_agent_db` (§8) when the daemon
has no database configured.
```

Then add one row to the §8 error table (after the `internal` row):

```markdown
| `no_agent_db`           | `ctrl_conversation_*`: no agent database configured (`FUNDI_AGENT_DB` unset). |
```

- [ ] **Step 8: Commit**

```bash
git add pkg/protocol/types.go pkg/protocol/types_test.go docs/reference/pi-controller-protocol.md
git commit -m "protocol: add ctrl_conversation_stats/search/export wire types"
```

---

### Task 2: `pkg/control` — Controller interface and dispatch handlers

**Files:**
- Modify: `pkg/control/dispatch.go`
- Modify: `pkg/control/dispatch_test.go`

**Interfaces:**
- Consumes: `protocol.TypeCtrlConversationStats/Search/Export`, `protocol.ConversationStatsRequest/SearchRequest/ExportRequest`, `protocol.ErrNoAgentDB` (Task 1).
- Produces: four new `Controller` interface methods —
  `ConversationStats(ctx context.Context, f insights.StatsFilter) (*insights.Stats, error)`,
  `ConversationStatsByID(ctx context.Context, id string) (*insights.Stats, error)`,
  `ConversationSearch(ctx context.Context, f insights.SearchFilter) ([]insights.ConversationSummary, error)`,
  `ConversationExport(ctx context.Context, id string) (*insights.Transcript, error)`.
  Task 3's real `Controller` implements these exact signatures.

- [ ] **Step 1: Write the failing dispatch tests**

Add to `pkg/control/dispatch_test.go`. First, add `"go.graveland.dev/rafiki/pkg/insights"` to the import block (alongside `childstore`, `control`, `protocol`).

Add these four fields to the `fakeController` struct (alongside `searchFn`, `statusFn`):

```go
	conversationStatsFn     func(context.Context, insights.StatsFilter) (*insights.Stats, error)
	conversationStatsByIDFn func(context.Context, string) (*insights.Stats, error)
	conversationSearchFn    func(context.Context, insights.SearchFilter) ([]insights.ConversationSummary, error)
	conversationExportFn    func(context.Context, string) (*insights.Transcript, error)
```

Add these four methods right after `Status()`:

```go
func (f *fakeController) ConversationStats(ctx context.Context, filter insights.StatsFilter) (*insights.Stats, error) {
	if f.conversationStatsFn != nil {
		return f.conversationStatsFn(ctx, filter)
	}
	return &insights.Stats{}, nil
}

func (f *fakeController) ConversationStatsByID(ctx context.Context, id string) (*insights.Stats, error) {
	if f.conversationStatsByIDFn != nil {
		return f.conversationStatsByIDFn(ctx, id)
	}
	return &insights.Stats{}, nil
}

func (f *fakeController) ConversationSearch(ctx context.Context, filter insights.SearchFilter) ([]insights.ConversationSummary, error) {
	if f.conversationSearchFn != nil {
		return f.conversationSearchFn(ctx, filter)
	}
	return nil, nil
}

func (f *fakeController) ConversationExport(ctx context.Context, id string) (*insights.Transcript, error) {
	if f.conversationExportFn != nil {
		return f.conversationExportFn(ctx, id)
	}
	return &insights.Transcript{}, nil
}
```

Add these test functions after `TestDispatch_Search_EmptyHitsIsArray` (around line 669, before the `─── ctrl_spawn ───` section marker):

```go
// ─── ctrl_conversation_stats ───────────────────────────────────────────────────

func TestDispatch_ConversationStats_Global_Success(t *testing.T) {
	c := &fakeController{
		conversationStatsFn: func(_ context.Context, f insights.StatsFilter) (*insights.Stats, error) {
			if f.Owner != "brent" {
				t.Fatalf("owner: %s", f.Owner)
			}
			return &insights.Stats{Volume: insights.VolumeStats{Conversations: 5, Turns: 20}}, nil
		},
	}
	d := control.NewDispatch(c)
	frame := `{"type":"ctrl_conversation_stats","id":"30","owner":"brent"}`
	r := mustSuccess(t, d.HandleFrame(discardConn{}, []byte(frame)))

	var data insights.Stats
	if err := json.Unmarshal(r.Data, &data); err != nil {
		t.Fatal(err)
	}
	if data.Volume.Conversations != 5 || data.Volume.Turns != 20 {
		t.Errorf("volume: %+v", data.Volume)
	}
}

func TestDispatch_ConversationStats_ByID_Success(t *testing.T) {
	c := &fakeController{
		conversationStatsByIDFn: func(_ context.Context, id string) (*insights.Stats, error) {
			if id != "conv-abc" {
				t.Fatalf("id: %s", id)
			}
			return &insights.Stats{Volume: insights.VolumeStats{Conversations: 1}}, nil
		},
	}
	d := control.NewDispatch(c)
	frame := `{"type":"ctrl_conversation_stats","id":"31","conversationId":"conv-abc"}`
	r := mustSuccess(t, d.HandleFrame(discardConn{}, []byte(frame)))

	var data insights.Stats
	if err := json.Unmarshal(r.Data, &data); err != nil {
		t.Fatal(err)
	}
	if data.Volume.Conversations != 1 {
		t.Errorf("volume: %+v", data.Volume)
	}
}

func TestDispatch_ConversationStats_NoAgentDB(t *testing.T) {
	c := &fakeController{
		conversationStatsFn: func(context.Context, insights.StatsFilter) (*insights.Stats, error) {
			return nil, controllerErr(protocol.ErrNoAgentDB, "no agent database configured")
		},
	}
	d := control.NewDispatch(c)
	resp := d.HandleFrame(discardConn{}, []byte(`{"type":"ctrl_conversation_stats","id":"32"}`))
	mustError(t, resp, protocol.ErrNoAgentDB)
}

// ─── ctrl_conversation_search ──────────────────────────────────────────────────

func TestDispatch_ConversationSearch_Success(t *testing.T) {
	c := &fakeController{
		conversationSearchFn: func(_ context.Context, f insights.SearchFilter) ([]insights.ConversationSummary, error) {
			if f.Text != "skill gap" {
				t.Fatalf("text: %s", f.Text)
			}
			return []insights.ConversationSummary{{ID: "conv-abc", Owner: "brent"}}, nil
		},
	}
	d := control.NewDispatch(c)
	frame := `{"type":"ctrl_conversation_search","id":"33","text":"skill gap"}`
	r := mustSuccess(t, d.HandleFrame(discardConn{}, []byte(frame)))

	var data struct {
		Rows []insights.ConversationSummary `json:"rows"`
	}
	if err := json.Unmarshal(r.Data, &data); err != nil {
		t.Fatal(err)
	}
	if len(data.Rows) != 1 || data.Rows[0].ID != "conv-abc" {
		t.Errorf("rows: %+v", data.Rows)
	}
}

func TestDispatch_ConversationSearch_EmptyRowsIsArray(t *testing.T) {
	c := &fakeController{
		conversationSearchFn: func(context.Context, insights.SearchFilter) ([]insights.ConversationSummary, error) {
			return nil, nil
		},
	}
	d := control.NewDispatch(c)
	r := mustSuccess(t, d.HandleFrame(discardConn{}, []byte(`{"type":"ctrl_conversation_search","id":"34"}`)))
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(r.Data, &raw); err != nil {
		t.Fatal(err)
	}
	if string(raw["rows"]) == "null" {
		t.Error("rows should be [] not null")
	}
}

// ─── ctrl_conversation_export ──────────────────────────────────────────────────

func TestDispatch_ConversationExport_Success(t *testing.T) {
	c := &fakeController{
		conversationExportFn: func(_ context.Context, id string) (*insights.Transcript, error) {
			if id != "conv-abc" {
				t.Fatalf("id: %s", id)
			}
			return &insights.Transcript{ConversationID: "conv-abc", Owner: "brent"}, nil
		},
	}
	d := control.NewDispatch(c)
	frame := `{"type":"ctrl_conversation_export","id":"35","conversationId":"conv-abc"}`
	r := mustSuccess(t, d.HandleFrame(discardConn{}, []byte(frame)))

	var data insights.Transcript
	if err := json.Unmarshal(r.Data, &data); err != nil {
		t.Fatal(err)
	}
	if data.ConversationID != "conv-abc" {
		t.Errorf("conversationId: %s", data.ConversationID)
	}
}

func TestDispatch_ConversationExport_MissingConversationID(t *testing.T) {
	d := control.NewDispatch(&fakeController{})
	resp := d.HandleFrame(discardConn{}, []byte(`{"type":"ctrl_conversation_export","id":"x"}`))
	mustError(t, resp, protocol.ErrInvalidArgs)
}
```

- [ ] **Step 2: Run the tests to verify they fail (runtime failure, not a compile error — `fakeController` having extra methods beyond what `Controller` currently requires compiles fine; `dispatch.go`'s switch just doesn't route these frame types yet, so they fall into its `default:` case)**

Run: `go test ./pkg/control/... -run TestDispatch_Conversation -v > /tmp/step2.log 2>&1; echo "exit=$?"`
Expected: `exit=1`, each new test `FAIL`s inside `mustSuccess`/`mustError` with something like `expected success, got error code=invalid_args msg=unknown command type: ctrl_conversation_stats`.

- [ ] **Step 3: Add the four methods to the `Controller` interface**

In `pkg/control/dispatch.go`, add `"time"` and `"go.graveland.dev/rafiki/pkg/insights"` to the import block. Then insert into the `Controller` interface (after `Status() ControllerStatus`, before the `// Lifecycle mutations.` comment, around line 78):

```go

	// Conversation insights, backed by the daemon's agent database.
	// Implementations return a *ControllerError with Code: protocol.ErrNoAgentDB
	// when no database is configured (FUNDI_AGENT_DB unset).
	ConversationStats(ctx context.Context, f insights.StatsFilter) (*insights.Stats, error)
	ConversationStatsByID(ctx context.Context, id string) (*insights.Stats, error)
	ConversationSearch(ctx context.Context, f insights.SearchFilter) ([]insights.ConversationSummary, error)
	ConversationExport(ctx context.Context, id string) (*insights.Transcript, error)
```

- [ ] **Step 4: Add the dispatch cases and handlers**

In the `handle` switch statement (around line 176, after `case protocol.TypeCtrlStatus:`), add:

```go
	case protocol.TypeCtrlConversationStats:
		return d.conversationStats(frame, hdr.ID)
	case protocol.TypeCtrlConversationSearch:
		return d.conversationSearch(frame, hdr.ID)
	case protocol.TypeCtrlConversationExport:
		return d.conversationExport(frame, hdr.ID)
```

Add a new section after `ctrlStatus` (around line 392, before `// ─── Lifecycle handlers ───`):

```go
// ─── Conversation insight handlers ────────────────────────────────────────────

// unixToTime converts a wire Unix-seconds value to *time.Time, treating 0 as
// unset — matches insights.StatsFilter/SearchFilter's nil-means-unbounded
// convention for Since/Until.
func unixToTime(sec int64) *time.Time {
	if sec == 0 {
		return nil
	}
	t := time.Unix(sec, 0)
	return &t
}

func (d *dispatcher) conversationStats(frame []byte, id string) []byte {
	var req protocol.ConversationStatsRequest
	if err := json.Unmarshal(frame, &req); err != nil {
		return errResponse(protocol.TypeCtrlConversationStats, id, protocol.ErrInvalidArgs, "malformed request")
	}
	ctx := context.Background()
	if req.ConversationID != "" {
		st, err := d.c.ConversationStatsByID(ctx, req.ConversationID)
		if err != nil {
			return mapErr(protocol.TypeCtrlConversationStats, id, err, protocol.ErrInternal)
		}
		return okResponse(protocol.TypeCtrlConversationStats, id, st)
	}
	f := insights.StatsFilter{
		Since:   unixToTime(req.SinceUnix),
		Until:   unixToTime(req.UntilUnix),
		Owner:   req.Owner,
		Persona: req.Persona,
		Source:  req.Source,
		Model:   req.Model,
		Path:    insights.Path(req.Path),
	}
	st, err := d.c.ConversationStats(ctx, f)
	if err != nil {
		return mapErr(protocol.TypeCtrlConversationStats, id, err, protocol.ErrInternal)
	}
	return okResponse(protocol.TypeCtrlConversationStats, id, st)
}

func (d *dispatcher) conversationSearch(frame []byte, id string) []byte {
	var req protocol.ConversationSearchRequest
	if err := json.Unmarshal(frame, &req); err != nil {
		return errResponse(protocol.TypeCtrlConversationSearch, id, protocol.ErrInvalidArgs, "malformed request")
	}
	f := insights.SearchFilter{
		Since:     unixToTime(req.SinceUnix),
		Until:     unixToTime(req.UntilUnix),
		Owner:     req.Owner,
		Persona:   req.Persona,
		Source:    req.Source,
		Model:     req.Model,
		Status:    req.Status,
		Path:      insights.Path(req.Path),
		MinTokens: req.MinTokens,
		Text:      req.Text,
		Limit:     req.Limit,
	}
	rows, err := d.c.ConversationSearch(context.Background(), f)
	if err != nil {
		return mapErr(protocol.TypeCtrlConversationSearch, id, err, protocol.ErrInternal)
	}
	if rows == nil {
		rows = []insights.ConversationSummary{}
	}
	return okResponse(protocol.TypeCtrlConversationSearch, id, struct {
		Rows []insights.ConversationSummary `json:"rows"`
	}{Rows: rows})
}

func (d *dispatcher) conversationExport(frame []byte, id string) []byte {
	var req protocol.ConversationExportRequest
	if err := json.Unmarshal(frame, &req); err != nil {
		return errResponse(protocol.TypeCtrlConversationExport, id, protocol.ErrInvalidArgs, "malformed request")
	}
	if req.ConversationID == "" {
		return errResponse(protocol.TypeCtrlConversationExport, id, protocol.ErrInvalidArgs, "conversationId required")
	}
	tr, err := d.c.ConversationExport(context.Background(), req.ConversationID)
	if err != nil {
		return mapErr(protocol.TypeCtrlConversationExport, id, err, protocol.ErrInternal)
	}
	return okResponse(protocol.TypeCtrlConversationExport, id, tr)
}
```

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test ./pkg/control/... -v > /tmp/step5.log 2>&1; echo "exit=$?"`
Expected: `exit=0`, every `TestDispatch_Conversation*` shows `PASS`, and every pre-existing test in the package still passes (the fake now implements the full interface).

- [ ] **Step 6: `go vet`**

Run: `go vet ./pkg/control/... ./pkg/protocol/... > /tmp/step6.log 2>&1; echo "exit=$?"`
Expected: `exit=0`, empty output.

- [ ] **Step 7: Commit**

```bash
git add pkg/control/dispatch.go pkg/control/dispatch_test.go
git commit -m "control: dispatch ctrl_conversation_stats/search/export"
```

---

### Task 3: Real `Controller` — wire the agent database backend

**Files:**
- Modify: `cmd/fundid/controller.go`
- Create: `cmd/fundid/controller_conversations_test.go`

**Interfaces:**
- Consumes: `Controller` interface additions from Task 2; `agentcli.Backend`, `agentcli/local.New`, `agentcli/local.ErrNoPool`, `insights.StatsFilter/SearchFilter/Stats/ConversationSummary/Transcript` (all pre-existing, `pkg/agentcli`, `pkg/agentcli/local`, `pkg/insights`).
- Produces: the real `(*Controller).ConversationStats/ConversationStatsByID/ConversationSearch/ConversationExport`, satisfying `control.Controller`. No other task depends on these directly (dispatch already goes through the interface), but this task is what makes the daemon actually answer instead of failing to compile.

- [ ] **Step 1: Write the failing integration test**

Create `cmd/fundid/controller_conversations_test.go`:

```go
package main

import (
	"bufio"
	"encoding/json"
	"net"
	"path/filepath"
	"testing"

	"go.graveland.dev/rafiki/pkg/childstore"
	"go.graveland.dev/rafiki/pkg/control"
	"go.graveland.dev/rafiki/pkg/protocol"
)

// TestIntegration_CtrlConversationStats_NoAgentDB boots the controller with a
// nil pool — matching production when FUNDI_AGENT_DB is unset — and confirms
// ctrl_conversation_stats answers no_agent_db instead of panicking on the nil
// pool. testSocketDir is defined in integration_test.go (same package).
func TestIntegration_CtrlConversationStats_NoAgentDB(t *testing.T) {
	t.Parallel()

	dir := testSocketDir(t)
	socketPath := filepath.Join(dir, "c.sock")
	stateDir := filepath.Join(dir, "state")
	logsDir := filepath.Join(dir, "logs")

	st := childstore.New()
	ctrl := NewController(st, stateDir, logsDir, socketPath, nil, nil, t.Context())

	handler := control.NewDispatch(ctrl)
	srv, err := control.Listen(socketPath, handler)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { srv.Close() })

	conn, err := net.Dial("unix", socketPath)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer conn.Close()

	if _, err := conn.Write([]byte(`{"type":"ctrl_conversation_stats","id":"1"}` + "\n")); err != nil {
		t.Fatalf("write: %v", err)
	}

	reader := bufio.NewReader(conn)
	line, err := reader.ReadBytes('\n')
	if err != nil {
		t.Fatalf("read: %v", err)
	}

	var resp protocol.Response
	if err := json.Unmarshal(line, &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.Success {
		t.Fatal("expected failure with nil pool")
	}
	if resp.Error == nil || resp.Error.Code != protocol.ErrNoAgentDB {
		t.Fatalf("expected code %s, got %+v", protocol.ErrNoAgentDB, resp.Error)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails (compile error — Controller doesn't implement the new interface methods yet)**

Run: `go vet ./cmd/fundid/... > /tmp/step2.log 2>&1; echo "exit=$?"`
Expected: `exit=1`, log shows `*Controller does not implement control.Controller (missing method ConversationStats)` (or similar — `NewController(...)` is passed to `control.NewDispatch` only inside `main.go`'s wiring today via the interface satisfaction at compile time through `control.Controller`; confirm the actual error text matches "missing method").

- [ ] **Step 3: Add the `insights` field and wire it in the constructor**

In `cmd/fundid/controller.go`, add three imports to the import block: `"go.graveland.dev/rafiki/pkg/agentcli"`, `"go.graveland.dev/rafiki/pkg/agentcli/local"`, and `"go.graveland.dev/rafiki/pkg/insights"` (none of the three are currently imported in this file).

Add a field to the `Controller` struct, right after the `pool` field (line 53):

```go

	// insights answers the ctrl_conversation_* RPCs. Always constructed —
	// agentcli/local.New is nil-pool-safe, so a nil pool just means every
	// read method below returns local.ErrNoPool instead of panicking.
	insights agentcli.Backend
```

In `NewController`'s return statement, add one field to the struct literal (alongside `pool: pool,`):

```go
		insights:    local.New(local.Options{Pool: pool}),
```

- [ ] **Step 4: Add the four Controller methods**

Add after `Status()` (ends around line 443, before `func (c *Controller) Spawn(...)`):

```go
// ─── Conversation insights (backed by the agent database) ────────────────────

// noAgentDBErr translates agentcli/local.ErrNoPool — "no database pool
// configured" — to the wire error code clients can act on, distinguishing an
// expected, actionable state (daemon has no DB configured) from a genuine
// query failure.
func noAgentDBErr(err error) error {
	if errors.Is(err, local.ErrNoPool) {
		return &control.ControllerError{
			Code:    protocol.ErrNoAgentDB,
			Message: "no agent database configured (FUNDI_AGENT_DB unset); set it and run `fundi service install`",
		}
	}
	return err
}

func (c *Controller) ConversationStats(ctx context.Context, f insights.StatsFilter) (*insights.Stats, error) {
	st, err := c.insights.Stats(ctx, f)
	if err != nil {
		return nil, noAgentDBErr(err)
	}
	return st, nil
}

func (c *Controller) ConversationStatsByID(ctx context.Context, id string) (*insights.Stats, error) {
	st, err := c.insights.ConversationStats(ctx, id)
	if err != nil {
		return nil, noAgentDBErr(err)
	}
	return st, nil
}

func (c *Controller) ConversationSearch(ctx context.Context, f insights.SearchFilter) ([]insights.ConversationSummary, error) {
	rows, err := c.insights.Search(ctx, f)
	if err != nil {
		return nil, noAgentDBErr(err)
	}
	return rows, nil
}

func (c *Controller) ConversationExport(ctx context.Context, id string) (*insights.Transcript, error) {
	tr, err := c.insights.Export(ctx, id)
	if err != nil {
		return nil, noAgentDBErr(err)
	}
	return tr, nil
}
```

`errors` and `control` are already imported in this file (used by the existing `pool` field's neighbors and by `Search`/`Status`'s return types respectively) — confirm both are present in the import block; add `"errors"` if it's somehow missing (it should already be there per the existing `errors.New`/`errors.As` usage elsewhere in the file).

- [ ] **Step 5: Run the test to verify it passes**

Run: `go test ./cmd/fundid/... -run TestIntegration_CtrlConversationStats_NoAgentDB -v > /tmp/step5.log 2>&1; echo "exit=$?"`
Expected: `exit=0`, `PASS`.

- [ ] **Step 6: Run the full `cmd/fundid` test suite (no regressions)**

Run: `go test ./cmd/fundid/... > /tmp/step6.log 2>&1; echo "exit=$?"`
Expected: `exit=0`.

- [ ] **Step 7: `go vet`**

Run: `go vet ./cmd/fundid/... > /tmp/step7.log 2>&1; echo "exit=$?"`
Expected: `exit=0`, empty output.

- [ ] **Step 8: Commit**

```bash
git add cmd/fundid/controller.go cmd/fundid/controller_conversations_test.go
git commit -m "fundid: answer ctrl_conversation_stats/search/export from the agent database"
```

---

### Task 4: `fundi conversations` CLI

**Files:**
- Create: `cmd/fundi/cmd_conversations.go`
- Create: `cmd/fundi/cmd_conversations_test.go`
- Modify: `cmd/fundi/main.go`

**Interfaces:**
- Consumes: `protocol.ConversationStatsRequest/SearchRequest/ExportRequest` + `Type*` consts (Task 1); `agentcli.FilterVals`, `agentcli.BindStatsFilter`, `agentcli.BindSearchFilter` (pre-existing, `pkg/agentcli`); `mustDial`, `cmdCtx`, `client.FormatError` (pre-existing, `cmd/fundi/cli_helpers.go`).
- Produces: `newConversationsCmd() *cobra.Command`, registered in `main.go`'s `newRootCmd()`.

- [ ] **Step 1: Write the failing CLI tests**

Create `cmd/fundi/cmd_conversations_test.go`:

```go
package main

import (
	"testing"
	"time"
)

func TestConversationsStatsCmd_FlagsRegistered(t *testing.T) {
	cmd := newConversationsStatsCmd()
	for _, name := range []string{"since", "until", "owner", "persona", "source", "model", "path"} {
		if cmd.Flags().Lookup(name) == nil {
			t.Errorf("flag --%s not registered", name)
		}
	}
}

func TestConversationsSearchCmd_FlagsRegistered(t *testing.T) {
	cmd := newConversationsSearchCmd()
	for _, name := range []string{
		"since", "until", "owner", "persona", "source", "model", "path",
		"status", "min-tokens", "text", "limit",
	} {
		if cmd.Flags().Lookup(name) == nil {
			t.Errorf("flag --%s not registered", name)
		}
	}
}

func TestConversationsExportCmd_RequiresExactlyOneArg(t *testing.T) {
	cmd := newConversationsExportCmd()
	if err := cmd.Args(cmd, nil); err == nil {
		t.Error("expected error with zero args")
	}
	if err := cmd.Args(cmd, []string{"conv-abc"}); err != nil {
		t.Errorf("expected no error with one arg: %v", err)
	}
	if err := cmd.Args(cmd, []string{"a", "b"}); err == nil {
		t.Error("expected error with two args")
	}
}

func TestUnixOrZero(t *testing.T) {
	if got := unixOrZero(nil); got != 0 {
		t.Errorf("nil: got %d, want 0", got)
	}
	tm := time.Unix(1716000000, 0)
	if got := unixOrZero(&tm); got != 1716000000 {
		t.Errorf("got %d, want 1716000000", got)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail (compile error — nothing referenced exists yet)**

Run: `go vet ./cmd/fundi/... > /tmp/step2.log 2>&1; echo "exit=$?"`
Expected: `exit=1`, `undefined: newConversationsStatsCmd` (and friends).

- [ ] **Step 3: Write `cmd/fundi/cmd_conversations.go`**

```go
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"

	"go.graveland.dev/rafiki/pkg/agentcli"
	"go.graveland.dev/rafiki/pkg/client"
	"go.graveland.dev/rafiki/pkg/protocol"
)

func newConversationsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "conversations",
		Short: "Query persisted conversation history from the daemon's agent database",
		Long: `Global stats, search, and transcript export over the conversations schema
the daemon persists to when FUNDI_AGENT_DB is set. Unlike "fundi search" (live,
in-memory, currently-running children only), these query history in Postgres
regardless of whether anything is still running.`,
	}
	cmd.AddCommand(
		newConversationsStatsCmd(),
		newConversationsSearchCmd(),
		newConversationsExportCmd(),
	)
	return cmd
}

// bindConversationFilterFlags registers the filter flags shared by stats and
// search, matching rafiki agent's flag names exactly.
func bindConversationFilterFlags(cmd *cobra.Command) {
	cmd.Flags().String("since", "", "RFC3339 timestamp or duration like 24h")
	cmd.Flags().String("until", "", "RFC3339 timestamp or duration like 24h")
	cmd.Flags().String("owner", "", "filter by owner")
	cmd.Flags().String("persona", "", "filter by persona")
	cmd.Flags().String("source", "", "filter by source")
	cmd.Flags().String("model", "", "filter by model")
	cmd.Flags().String("path", "", `filter by path ("proxy" or "direct")`)
}

// conversationFilterVals reads the shared filter flags into an
// agentcli.FilterVals, the same flag-value bag rafiki agent binds from.
func conversationFilterVals(cmd *cobra.Command) agentcli.FilterVals {
	v := agentcli.FilterVals{}
	v.Since, _ = cmd.Flags().GetString("since")
	v.Until, _ = cmd.Flags().GetString("until")
	v.Owner, _ = cmd.Flags().GetString("owner")
	v.Persona, _ = cmd.Flags().GetString("persona")
	v.Source, _ = cmd.Flags().GetString("source")
	v.Model, _ = cmd.Flags().GetString("model")
	v.Path, _ = cmd.Flags().GetString("path")
	return v
}

// unixOrZero converts a resolved filter timestamp to the wire's Unix-seconds
// convention, where 0 means unset.
func unixOrZero(t *time.Time) int64 {
	if t == nil {
		return 0
	}
	return t.Unix()
}

func printResponseJSON(resp *protocol.Response) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(json.RawMessage(resp.Data))
}

// ─── stats ──────────────────────────────────────────────────────────────────

func newConversationsStatsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "stats [conv-id]",
		Short: "Global or per-conversation stats over persisted history",
		Args:  cobra.MaximumNArgs(1),
		RunE:  runConversationsStats,
	}
	bindConversationFilterFlags(cmd)
	return cmd
}

func runConversationsStats(cmd *cobra.Command, args []string) error {
	c := mustDial(cmd)
	defer c.Close()

	req := protocol.ConversationStatsRequest{Type: protocol.TypeCtrlConversationStats}
	if len(args) == 1 {
		req.ConversationID = args[0]
	} else {
		f, err := agentcli.BindStatsFilter(conversationFilterVals(cmd))
		if err != nil {
			return err
		}
		req.SinceUnix = unixOrZero(f.Since)
		req.UntilUnix = unixOrZero(f.Until)
		req.Owner = f.Owner
		req.Persona = f.Persona
		req.Source = f.Source
		req.Model = f.Model
		req.Path = string(f.Path)
	}

	resp, err := c.Request(cmdCtx(cmd), req)
	if err != nil {
		return err
	}
	if !resp.Success {
		return fmt.Errorf("ctrl_conversation_stats: %s", client.FormatError(resp))
	}
	return printResponseJSON(resp)
}

// ─── search ─────────────────────────────────────────────────────────────────

func newConversationsSearchCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "search",
		Short: "Search persisted conversation history",
		Args:  cobra.NoArgs,
		RunE:  runConversationsSearch,
	}
	bindConversationFilterFlags(cmd)
	cmd.Flags().String("status", "", "filter by status")
	cmd.Flags().Int64("min-tokens", 0, "minimum total tokens")
	cmd.Flags().String("text", "", "full-text search over first messages")
	cmd.Flags().Int("limit", 0, "max results (0 = default)")
	return cmd
}

func runConversationsSearch(cmd *cobra.Command, _ []string) error {
	c := mustDial(cmd)
	defer c.Close()

	v := conversationFilterVals(cmd)
	v.Status, _ = cmd.Flags().GetString("status")
	v.MinTokens, _ = cmd.Flags().GetInt64("min-tokens")
	v.Text, _ = cmd.Flags().GetString("text")
	v.Limit, _ = cmd.Flags().GetInt("limit")

	f, err := agentcli.BindSearchFilter(v)
	if err != nil {
		return err
	}

	req := protocol.ConversationSearchRequest{
		Type:      protocol.TypeCtrlConversationSearch,
		SinceUnix: unixOrZero(f.Since),
		UntilUnix: unixOrZero(f.Until),
		Owner:     f.Owner,
		Persona:   f.Persona,
		Source:    f.Source,
		Model:     f.Model,
		Path:      string(f.Path),
		Status:    f.Status,
		MinTokens: f.MinTokens,
		Text:      f.Text,
		Limit:     f.Limit,
	}

	resp, err := c.Request(cmdCtx(cmd), req)
	if err != nil {
		return err
	}
	if !resp.Success {
		return fmt.Errorf("ctrl_conversation_search: %s", client.FormatError(resp))
	}
	return printResponseJSON(resp)
}

// ─── export ─────────────────────────────────────────────────────────────────

func newConversationsExportCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "export <conv-id>",
		Short: "Export a persisted conversation's full transcript",
		Args:  cobra.ExactArgs(1),
		RunE:  runConversationsExport,
	}
}

func runConversationsExport(cmd *cobra.Command, args []string) error {
	c := mustDial(cmd)
	defer c.Close()

	req := protocol.ConversationExportRequest{
		Type:           protocol.TypeCtrlConversationExport,
		ConversationID: args[0],
	}

	resp, err := c.Request(cmdCtx(cmd), req)
	if err != nil {
		return err
	}
	if !resp.Success {
		return fmt.Errorf("ctrl_conversation_export: %s", client.FormatError(resp))
	}
	return printResponseJSON(resp)
}
```

- [ ] **Step 4: Register the command in `main.go`**

In `cmd/fundi/main.go`, add `newConversationsCmd(),` to the `root.AddCommand(...)` list — put it right after `newSearchCmd(),` (line 55), since the two are conceptually related (live search vs. persisted search) even though they're separate command groups.

- [ ] **Step 5: Run the tests to verify they pass**

Run: `go test ./cmd/fundi/... -run 'Conversations|UnixOrZero' -v > /tmp/step5.log 2>&1; echo "exit=$?"`
Expected: `exit=0`, all four tests `PASS`.

- [ ] **Step 6: Run the full `cmd/fundi` test suite (no regressions)**

Run: `go test ./cmd/fundi/... > /tmp/step6.log 2>&1; echo "exit=$?"`
Expected: `exit=0`.

- [ ] **Step 7: `go vet`**

Run: `go vet ./cmd/fundi/... > /tmp/step7.log 2>&1; echo "exit=$?"`
Expected: `exit=0`, empty output.

- [ ] **Step 8: Build both binaries and check the new command shows up**

Run: `go build -o /tmp/fundi ./cmd/fundi && /tmp/fundi conversations --help > /tmp/step8.log 2>&1; cat /tmp/step8.log`
Expected: shows `stats`, `search`, `export` as available subcommands, and `/tmp/fundi conversations stats --help` / `search --help` / `export --help` each list the flags from Step 1's tests.

- [ ] **Step 9: Commit**

```bash
git add cmd/fundi/cmd_conversations.go cmd/fundi/cmd_conversations_test.go cmd/fundi/main.go
git commit -m "fundi: add conversations stats/search/export commands"
```

---

### Task 5: Whole-module verification and manual smoke test

**Files:** none (verification only).

- [ ] **Step 1: Full vet**

Run: `go vet ./... > /tmp/vet.log 2>&1; echo "exit=$?"; cat /tmp/vet.log`
Expected: `exit=0`, empty log.

- [ ] **Step 2: Full lint, uncapped**

Run: `golangci-lint run ./... --max-same-issues=0 --max-issues-per-linter=0 > /tmp/lint.log 2>&1; echo "exit=$?"; cat /tmp/lint.log`
Expected: `exit=0`, empty log. (The default `--max-same-issues=3` truncates non-deterministically — always pass both flags when checking, per this repo's established convention.)

- [ ] **Step 3: Full test suite**

Run: `go test ./... > /tmp/test.log 2>&1; echo "exit=$?"; tail -50 /tmp/test.log`
Expected: `exit=0`. If any package reports `SKIP` due to a missing DSN (existing `pkg/store`/`pkg/insights`/`pkg/agentcli` tests, unrelated to this change), report the skip count explicitly rather than treating a green exit code as "everything ran" — this plan's own new tests do not require a DSN, so they should all show `PASS`, not `SKIP`.

- [ ] **Step 4: Manual smoke test against a live daemon**

If you have a `FUNDI_AGENT_DB`-configured daemon available (check `fundi service status`; if not configured, this step demonstrates the `no_agent_db` path instead, which is still a valid pass):

```bash
make install                      # or: go build -o ~/.local/bin/fundi ./cmd/fundi && go build -o ~/.local/bin/fundid ./cmd/fundid
fundi service restart
fundi conversations stats
fundi conversations search --limit 5
# pick a conversation id from the stats/search output, then:
fundi conversations export <conv-id> | head -50
```

Expected: `stats` prints the same JSON shape as `rafiki agent stats -j`; `search` prints `{"rows": [...]}`; `export` prints a transcript. If `FUNDI_AGENT_DB` is unset, `stats`/`search`/`export` should instead each fail with a `no_agent_db` error message pointing at `FUNDI_AGENT_DB` and `fundi service install` — confirm the error text is legible (not a raw Go error), since this is the everyday path for anyone who hasn't configured a database yet.

- [ ] **Step 5: Update `README.md`'s command inventory if one exists**

Check whether `README.md`'s "fundi — the agent daemon" section lists CLI commands anywhere (search for `ctrl_search` or `fundi search` in `README.md`). If it does, add a one-line mention of `fundi conversations stats|search|export` there for discoverability; if the README doesn't enumerate commands (it may only describe architecture), skip this step — don't invent a section that doesn't fit the doc's existing structure.

- [ ] **Step 6: Final status report**

Summarize: vet/lint/test results (Steps 1–3), smoke test outcome (Step 4), and whether the README needed a change (Step 5). No commit needed for this task unless Step 5 produced a change, in which case:

```bash
git add README.md
git commit -m "docs: mention fundi conversations in the fundi command inventory"
```
