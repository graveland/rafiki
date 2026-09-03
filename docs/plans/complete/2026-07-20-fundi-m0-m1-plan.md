# Fundi M0+M1 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Fork rafiki and pi-controller into `~/home/rafiki` and `~/home/fundi`, and build `fundi agent` — a direct-API agent runtime (rafiki engine, pi-rpc stdio front-end, core coding tools, skills/MCP/CLAUDE.md) — as a new child kind, through the point where it replaces Claude Code as the default subagent backend.

**Architecture:** `fundi` is a multi-call binary: the existing daemon (unchanged supervision plane) plus a new `agent` subcommand exec'd as a child process. The agent speaks pi's rpc protocol natively on stdio (so the daemon uses the identity `PiProvider`, no translator), and drives rafiki's `llm.Conversation` + `agentloop` for LLM turns. Design doc: `docs/plans/2026-07-20-fundi-design.md`.

**Tech Stack:** Go 1.26, `git.graveland.dev/brent/rafiki` (forked from `github.com/timescale/rafiki`), `github.com/anthropics/anthropic-sdk-go`, `github.com/modelcontextprotocol/go-sdk` (MCP), `github.com/bmatcuk/doublestar/v4` (glob), `gopkg.in/yaml.v3` (SKILL.md frontmatter).

## Global Constraints

- **Repos:** rafiki fork lives at `~/home/rafiki` (gitea `git.graveland.dev/brent/rafiki`); fundi at `~/home/fundi` (gitea `git.graveland.dev/brent/fundi`). Tasks say which repo they're in.
- **No Co-Authored-By trailers in commits.**
- **macOS:** `sed -i ''` (BSD sed). Do not use `timeout(1)`.
- **Never `git add -A` / `git add .`** — add files by name; `git diff --cached --stat` before every commit.
- **Module renames:** rafiki fork module = `git.graveland.dev/brent/rafiki`; fundi module = `git.graveland.dev/brent/fundi`. All self-imports rewritten.
- **Compatibility non-goals for M1 (deliberate, do not "fix"):** keep `PI_CONTROLLER_CHILD_ID` / `PI_CONTROLLER_SOCKET` env names, keep `~/.pi/run/` state dir, keep `pic` CLI name. Renaming those is post-M1 cleanup.
- **rafiki fork changes must be upstreamable:** additive only (new options/callbacks), never signature changes to existing exported API.
- **Tool definition order must be deterministic** (sorted by name) — a reordered tools array busts the Anthropic prompt-cache prefix.
- **All new code: no silently swallowed errors.** Log or return; tool failures become `is_error` results, not dropped.
- Existing test suites in both repos must stay green after every task.

---

## M0 — Forks

### Task 1: Fork rafiki to ~/home/rafiki with module rename

**Files (repo: rafiki):**
- Create: `~/home/rafiki` (clone), `tools/rename-module.sh`

**Interfaces:**
- Produces: module `git.graveland.dev/brent/rafiki` tagged `v0.1.0`, importable via `GOPRIVATE=git.graveland.dev`.

- [ ] **Step 1: Clone and set remotes**

```bash
git clone ~/ts/dev/rafiki ~/home/rafiki
cd ~/home/rafiki
git checkout main   # fork from main, not the WIP branch
git remote rename origin upstream
# Create empty repo brent/rafiki on git.graveland.dev (web UI or `tea repo create`), then:
git remote add origin ssh://git@git.graveland.dev/brent/rafiki.git
```

- [ ] **Step 2: Write the rename script (kept in-repo so upstream merges re-apply it)**

```bash
#!/usr/bin/env bash
# tools/rename-module.sh — re-apply the fork's module rename after an upstream merge.
set -euo pipefail
OLD=github.com/timescale/rafiki
NEW=git.graveland.dev/brent/rafiki
cd "$(git rev-parse --show-toplevel)"
go mod edit -module "$NEW"
grep -rl --include='*.go' "$OLD" . | xargs sed -i '' "s|$OLD|$NEW|g" || true
gofmt -w .
```

```bash
chmod +x tools/rename-module.sh && ./tools/rename-module.sh
```

- [ ] **Step 3: Verify build and tests**

```bash
cd ~/home/rafiki && go build ./... && go test ./...
```
Expected: PASS (DB-integration tests skip without a configured TimescaleDB — that's fine; unit + golden-wire tests must pass).

- [ ] **Step 4: Commit, tag, push**

```bash
git add go.mod tools/rename-module.sh $(git diff --name-only -- '*.go')
git diff --cached --stat   # verify: go.mod, the script, and only import-line changes
git commit -m "fork: rename module to git.graveland.dev/brent/rafiki"
git tag v0.1.0
git push -u origin main --tags
```

### Task 2: rafiki fork primitives (Primary, ThinkingBudget, tool-call IDs, PendingUser)

Four small additive changes fundi needs. Each is TDD'd; each is an upstream candidate.

**Files (repo: rafiki):**
- Modify: `llm/conversation.go` (two ConvOptions + `sendWithTrim`/`assemble`)
- Modify: `agentloop/agentloop.go` (Events additions, ctx tool-call id, steer hook)
- Test: `llm/conversation_test.go`, `agentloop/agentloop_test.go` (extend existing files)

**Interfaces:**
- Produces: `llm.Primary(u Upstream) ConvOption`; `llm.ThinkingBudget(tokens int64) ConvOption`; `agentloop.Events{OnToolStart func(id, name string, input json.RawMessage); OnToolEnd func(id, name, result string, err error); PendingUser func() []anthropic.ContentBlockParamUnion}`; `agentloop.ToolCallID(ctx context.Context) string`.

- [ ] **Step 1: Write failing tests**

```go
// llm/conversation_test.go
func TestPrimaryOptionRoutesUpstream(t *testing.T) {
	fake := &recordingSender{} // records params, returns a canned end_turn message
	c, err := NewClient(WithUpstream(UpstreamOpenRouter, fake), WithDefaultModel("test-model"))
	if err != nil { t.Fatal(err) }
	conv, err := c.Conversation(context.Background(),
		NewConversation("t", "test"), Primary(UpstreamOpenRouter))
	if err != nil { t.Fatal(err) }
	if _, err := conv.Send(context.Background(), UserText("hi")); err != nil { t.Fatal(err) }
	if fake.calls != 1 { t.Fatalf("openrouter sender not used as primary: %d calls", fake.calls) }
}

func TestThinkingBudgetSetsParam(t *testing.T) {
	fake := &recordingSender{}
	c, _ := NewClient(WithUpstream(UpstreamAnthropic, fake), WithDefaultModel("test-model"))
	conv, _ := c.Conversation(context.Background(),
		NewConversation("t", "test"), ThinkingBudget(8192))
	_, _ = conv.Send(context.Background(), UserText("hi"))
	if fake.lastParams.Thinking.OfEnabled == nil || fake.lastParams.Thinking.OfEnabled.BudgetTokens != 8192 {
		t.Fatalf("thinking budget not set: %+v", fake.lastParams.Thinking)
	}
}
```

```go
// agentloop/agentloop_test.go
func TestOnToolStartEndCarryID(t *testing.T) {
	// scripted sender: turn 1 = tool_use id "tu_1" name "echo", turn 2 = end_turn
	var startID, endID, ctxID string
	tools := toolFunc("echo", func(ctx context.Context, in json.RawMessage) (string, error) {
		ctxID = ToolCallID(ctx)
		return "ok", nil
	})
	ev := &Events{
		OnToolStart: func(id, name string, in json.RawMessage) { startID = id },
		OnToolEnd:   func(id, name, result string, err error) { endID = id },
	}
	runScripted(t, tools, ev /* uses existing fake-sender test helper pattern */)
	if startID != "tu_1" || endID != "tu_1" || ctxID != "tu_1" {
		t.Fatalf("ids: start=%q end=%q ctx=%q", startID, endID, ctxID)
	}
}

func TestPendingUserInjectedBetweenIterations(t *testing.T) {
	// scripted: turn 1 tool_use, turn 2 end_turn. PendingUser returns "steer!" once.
	injected := []anthropic.ContentBlockParamUnion{anthropic.NewTextBlock("steer!")}
	ev := &Events{PendingUser: func() []anthropic.ContentBlockParamUnion {
		out := injected; injected = nil; return out
	}}
	conv := runScriptedConv(t, ev)
	hist, _ := conv.History(context.Background())
	// the steer text must appear as a user row AFTER the tool results, BEFORE turn 2's assistant row
	assertHistoryContainsUserText(t, hist, "steer!")
}
```

Note: reuse the file's existing fake-sender/scripted-turn helpers (the agentloop tests already script turns by building `*anthropic.Message` via `json.Unmarshal`); add small helpers `toolFunc`, `runScripted`, `runScriptedConv`, `assertHistoryContainsUserText` in the test file if absent.

- [ ] **Step 2: Run tests to verify they fail**

```bash
cd ~/home/rafiki && go test ./llm/ ./agentloop/ -run 'TestPrimary|TestThinking|TestOnToolStart|TestPendingUser' -v
```
Expected: FAIL (undefined: Primary, ThinkingBudget, OnToolStart, ToolCallID, PendingUser).

- [ ] **Step 3: Implement**

```go
// llm/conversation.go — add to convConfig: primary Upstream; thinkingBudget int64

// Primary selects the upstream used first for every send in this
// conversation (default UpstreamAnthropic). Fallback still applies.
func Primary(u Upstream) ConvOption { return func(c *convConfig) { c.primary = u } }

// ThinkingBudget enables extended thinking with the given token budget
// (0 = disabled, the default).
func ThinkingBudget(tokens int64) ConvOption {
	return func(c *convConfig) { c.thinkingBudget = tokens }
}
```

In `sendWithTrim`, set `meta.Primary = conv.cfg.primary` when non-empty. In `assemble`, after building params:

```go
	if conv.cfg.thinkingBudget > 0 {
		params.Thinking = anthropic.ThinkingConfigParamOfEnabled(conv.cfg.thinkingBudget)
	}
```

```go
// agentloop/agentloop.go — additive Events fields + ctx id

type toolCallIDKey struct{}

// ToolCallID returns the tool_use id of the call being executed, or "".
func ToolCallID(ctx context.Context) string {
	id, _ := ctx.Value(toolCallIDKey{}).(string)
	return id
}
```

Extend `Events` (keep existing fields untouched):

```go
	// OnToolStart/OnToolEnd mirror OnToolCall/OnToolResult but carry the
	// tool_use id, so hosts can correlate execution events with the
	// assistant message's toolCall blocks.
	OnToolStart func(id, name string, input json.RawMessage)
	OnToolEnd   func(id, name, result string, err error)
	// PendingUser, when non-nil, is polled after each tool batch's results
	// are persisted and before the next Continue. Returned content is
	// appended as an additional user message — the mid-turn steer seam.
	PendingUser func() []anthropic.ContentBlockParamUnion
```

In `drive()`'s tool-execution section: where `OnToolCall` fires, also fire `OnToolStart(block.ID, name, input)`; wrap each `Execute` ctx with `context.WithValue(ctx, toolCallIDKey{}, block.ID)`; where `OnToolResult` fires, also fire `OnToolEnd(id, ...)`. After the batch's results are persisted, before looping to the next `Continue`:

```go
		if ev != nil && ev.PendingUser != nil {
			if extra := ev.PendingUser(); len(extra) > 0 {
				if err := conv.AppendUser(ctx, extra); err != nil {
					return result, fmt.Errorf("agentloop: appending steer content: %w", err)
				}
			}
		}
```

- [ ] **Step 4: Run tests to verify they pass; run the full suite**

```bash
cd ~/home/rafiki && go test ./... 
```
Expected: PASS.

- [ ] **Step 5: Commit, retag, push**

```bash
git add llm/conversation.go agentloop/agentloop.go llm/conversation_test.go agentloop/agentloop_test.go
git diff --cached --stat
git commit -m "feat: Primary/ThinkingBudget conv options; tool-call ids and PendingUser steer hook in agentloop"
git tag v0.2.0 && git push origin main --tags
```

### Task 3: Fork pi-controller to ~/home/fundi with module + cmd rename

**Files (repo: fundi):**
- Create: `~/home/fundi` (clone)
- Modify: `go.mod`, all `*.go` self-imports, `git mv cmd/pi-controller cmd/fundi`

**Interfaces:**
- Produces: module `git.graveland.dev/brent/fundi`; binary builds as `fundi`; `pic`, daemon behavior, env names, `~/.pi/run` all unchanged.

- [ ] **Step 1: Clone, remotes, rename**

```bash
git clone ~/home/pi-controller ~/home/fundi
cd ~/home/fundi
git remote rename origin upstream   # local pi-controller
# create brent/fundi on gitea, then:
git remote add origin ssh://git@git.graveland.dev/brent/fundi.git
go mod edit -module git.graveland.dev/brent/fundi
grep -rl --include='*.go' 'git.graveland.dev/brent/pi-controller' . \
  | xargs sed -i '' 's|git.graveland.dev/brent/pi-controller|git.graveland.dev/brent/fundi|g'
git mv cmd/pi-controller cmd/fundi
gofmt -w .
```

- [ ] **Step 2: Fix any remaining references**

```bash
grep -rn "pi-controller" --include='*.go' . | grep -v '_test.go' | grep -vi 'comment\|//' || true
grep -rn "cmd/pi-controller" Makefile* .gitea 2>/dev/null || true
```
Update build scripts/Makefile targets that reference `cmd/pi-controller` or the `pi-controller` binary name to `cmd/fundi` / `fundi`. Leave `~/.pi` paths, `PI_CONTROLLER_*` env vars, and prose comments alone (Global Constraints).

- [ ] **Step 3: Build + full test suite**

```bash
cd ~/home/fundi && go build ./... && go test ./...
```
Expected: PASS — the suite is substantial (dispatch, integration, claude provider tests); all must stay green.

- [ ] **Step 4: Commit and push**

```bash
git add -u && git add cmd/fundi
git diff --cached --stat
git commit -m "fork: rename module to git.graveland.dev/brent/fundi, cmd/pi-controller -> cmd/fundi"
git push -u origin main
```
(`git add -u` is acceptable here — the rename touches every import; verify the stat shows only renames + import lines.)

### Task 4: fundi depends on the rafiki fork

**Files (repo: fundi):**
- Modify: `go.mod`, `.gitignore`
- Create: `go.work` (uncommitted)

- [ ] **Step 1: Wire the dependency**

```bash
cd ~/home/fundi
export GOPRIVATE=git.graveland.dev
go get git.graveland.dev/brent/rafiki@v0.2.0
printf "go.work\ngo.work.sum\n" >> .gitignore
go work init . ../rafiki   # local-dev override; NOT committed
```

- [ ] **Step 2: Verify both resolution paths**

```bash
go build ./...                          # via go.work (local rafiki checkout)
GOFLAGS=-workfile=off go build ./...    # via gitea fetch — the CI path
```
Expected: both succeed. If the gitea fetch fails, fix git auth for `git.graveland.dev` (ssh insteadOf https in `~/.gitconfig`) before proceeding — CI depends on it.

- [ ] **Step 3: Commit**

```bash
git add go.mod go.sum .gitignore
git diff --cached --stat && git commit -m "deps: add git.graveland.dev/brent/rafiki" && git push
```

---

## M1 — The agent runtime

All remaining tasks are in the **fundi** repo. New code lives in `internal/agent/` (engine, front-end) and `internal/agent/tools/` (toolset). `internal/agent` imports `internal/child` for the `Pi*` event constructors — same module, so `internal/` visibility is satisfied.

### Task 5: pi-rpc stdio front-end

**Files:**
- Create: `internal/agent/frontend.go`, `internal/agent/frontend_test.go`

**Interfaces:**
- Produces:
  ```go
  type Handler interface {
      HandlePrompt(text string)
      HandleSteer(text string)
      HandleAbort()
      State() StateData
  }
  type StateData struct{ SessionID, SessionName string; ModelID, Provider string }
  func NewFrontend(in io.Reader, out io.Writer, h Handler) *Frontend
  func (f *Frontend) Run() error        // blocks until stdin EOF
  func (f *Frontend) Emit(v any)        // marshal v, write one ndjson line (mutex-serialized)
  ```
- Consumes: inbound frames `{"type":"get_state","id":...}`, `{"type":"prompt","message":...}`, `{"type":"steer","message":...}`, `{"type":"abort"}` (the daemon's PiProvider vocabulary — see `internal/child/provider_pi.go`, `internal/child/provider_claude_state.go:79,129`).

- [ ] **Step 1: Write failing tests**

```go
// internal/agent/frontend_test.go
package agent

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"git.graveland.dev/brent/fundi/internal/child"
)

type fakeHandler struct{ prompts, steers []string; aborts int }

func (h *fakeHandler) HandlePrompt(t string) { h.prompts = append(h.prompts, t) }
func (h *fakeHandler) HandleSteer(t string)  { h.steers = append(h.steers, t) }
func (h *fakeHandler) HandleAbort()          { h.aborts++ }
func (h *fakeHandler) State() StateData {
	return StateData{SessionID: "conv-abc", SessionName: "w1", ModelID: "m1", Provider: "anthropic"}
}

func TestGetStateResponseSniffableByDaemon(t *testing.T) {
	in := strings.NewReader(`{"type":"get_state","id":"__bootstrap__"}` + "\n")
	var out bytes.Buffer
	f := NewFrontend(in, &out, &fakeHandler{})
	if err := f.Run(); err != nil { t.Fatal(err) }
	line := strings.TrimSpace(out.String())
	// the daemon's real sniffer must extract our session id + model
	md, ok := child.ExtractMetadata([]byte(line))
	if !ok || md.SessionID != "conv-abc" || md.Model != "anthropic/m1" {
		t.Fatalf("sniff failed: ok=%v md=%+v line=%s", ok, md, line)
	}
	// and PiProvider must see it as the readiness signal
	if !(child.PiProvider{}).Parse([]byte(line)).FirstResponse {
		t.Fatal("get_state response did not signal FirstResponse")
	}
}

func TestPromptSteerAbortDispatch(t *testing.T) {
	in := strings.NewReader(
		`{"type":"prompt","message":"do the thing"}` + "\n" +
			`{"type":"steer","message":"also this"}` + "\n" +
			`{"type":"abort"}` + "\n")
	h := &fakeHandler{}
	f := NewFrontend(in, &bytes.Buffer{}, h)
	if err := f.Run(); err != nil { t.Fatal(err) }
	if len(h.prompts) != 1 || h.prompts[0] != "do the thing" { t.Fatalf("prompts: %v", h.prompts) }
	if len(h.steers) != 1 || h.steers[0] != "also this" { t.Fatalf("steers: %v", h.steers) }
	if h.aborts != 1 { t.Fatalf("aborts: %d", h.aborts) }
}

func TestUnknownRequestGetsErrorResponse(t *testing.T) {
	in := strings.NewReader(`{"type":"set_model","id":"7"}` + "\n")
	var out bytes.Buffer
	f := NewFrontend(in, &out, &fakeHandler{})
	_ = f.Run()
	var resp struct{ Type, Command, ID string; Success bool }
	if err := json.Unmarshal(out.Bytes(), &resp); err != nil { t.Fatal(err) }
	if resp.Type != "response" || resp.Command != "set_model" || resp.ID != "7" || resp.Success {
		t.Fatalf("bad error response: %+v", resp)
	}
}
```

- [ ] **Step 2: Run to verify failure**

```bash
cd ~/home/fundi && go test ./internal/agent/ -v
```
Expected: FAIL (package doesn't exist yet).

- [ ] **Step 3: Implement**

```go
// internal/agent/frontend.go
package agent

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"sync"
)

// Frontend speaks pi's rpc protocol over stdio: ndjson frames in (prompt,
// steer, abort, get_state), AgentSessionEvent frames out. It is the only
// writer to out; Emit is safe from any goroutine.
type Frontend struct {
	in      io.Reader
	out     io.Writer
	mu      sync.Mutex
	handler Handler
}

type Handler interface {
	HandlePrompt(text string)
	HandleSteer(text string)
	HandleAbort()
	State() StateData
}

// StateData feeds the get_state response the daemon sniffs for session id +
// model (internal/child/sniff.go expects data.sessionId and data.model{id,provider}).
type StateData struct {
	SessionID   string
	SessionName string
	ModelID     string
	Provider    string
}

func NewFrontend(in io.Reader, out io.Writer, h Handler) *Frontend {
	return &Frontend{in: in, out: out, handler: h}
}

func (f *Frontend) Emit(v any) {
	b, err := json.Marshal(v)
	if err != nil {
		fmt.Fprintf(f.out, `{"type":"agent_error","error":%q}`+"\n", err.Error())
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.out.Write(b)
	io.WriteString(f.out, "\n")
}

type stateResponse struct {
	Type    string    `json:"type"`
	Command string    `json:"command"`
	ID      string    `json:"id,omitempty"`
	Success bool      `json:"success"`
	Data    stateData `json:"data"`
}
type stateData struct {
	SessionID   string     `json:"sessionId"`
	SessionFile string     `json:"sessionFile"`
	SessionName string     `json:"sessionName"`
	Model       modelField `json:"model"`
}
type modelField struct {
	ID       string `json:"id"`
	Provider string `json:"provider"`
}

func (f *Frontend) Run() error {
	sc := bufio.NewScanner(f.in)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		var hdr struct {
			Type    string `json:"type"`
			ID      string `json:"id,omitempty"`
			Message string `json:"message,omitempty"`
		}
		if err := json.Unmarshal(line, &hdr); err != nil {
			continue // unparseable input is dropped, matching pi's tolerance
		}
		switch hdr.Type {
		case "get_state":
			s := f.handler.State()
			f.Emit(stateResponse{Type: "response", Command: "get_state", ID: hdr.ID, Success: true,
				Data: stateData{SessionID: s.SessionID, SessionName: s.SessionName,
					Model: modelField{ID: s.ModelID, Provider: s.Provider}}})
		case "prompt":
			f.handler.HandlePrompt(hdr.Message)
		case "steer":
			f.handler.HandleSteer(hdr.Message)
		case "abort":
			f.handler.HandleAbort()
		default:
			if hdr.ID != "" { // request-shaped: answer so clients don't hang
				f.Emit(map[string]any{"type": "response", "command": hdr.Type,
					"id": hdr.ID, "success": false, "error": "unsupported by fundi agent"})
			}
		}
	}
	return sc.Err()
}
```

- [ ] **Step 4: Run tests**

```bash
go test ./internal/agent/ -v
```
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/agent/frontend.go internal/agent/frontend_test.go
git diff --cached --stat && git commit -m "feat(agent): pi-rpc stdio front-end"
```

### Task 6: event emitter (anthropic.Message → pi frames)

**Files:**
- Create: `internal/agent/emit.go`, `internal/agent/emit_test.go`

**Interfaces:**
- Consumes: `Frontend.Emit`, `internal/child` Pi* constructors, `*anthropic.Message`.
- Produces:
  ```go
  func NewEmitter(fe *Frontend, provider, modelID string) *Emitter
  func (e *Emitter) AgentStart()
  func (e *Emitter) UserMessage(text string)                  // echo: message_start+message_end role:user
  func (e *Emitter) AssistantTurn(resp *anthropic.Message)    // message_start+message_update+message_end; accumulates
  func (e *Emitter) ToolStart(id, name string, input json.RawMessage)
  func (e *Emitter) ToolEnd(id, name, result string, isErr bool)  // also accumulates a toolResult message
  func (e *Emitter) AgentEnd()                                // agent_end(messages, usage) + agent_settled; resets accumulation
  func MapAssistantMessage(resp *anthropic.Message, provider string) child.PiAssistantMessage
  ```

Key mapping rules (test each):
- Content blocks: `text`→`PiTextBlock`, `thinking`→`PiThinkingBlock`, `tool_use`→`PiToolCallBlock(id, name, input-as-map)`.
- StopReason: `tool_use`→`toolUse`, `max_tokens`→`length`, everything else→`stop`.
- Usage: token counts from `resp.Usage` (input, output, cache read/write); `TotalTokens` = sum; `Cost` zeros (unknown at this layer). `API: "anthropic-messages"`.
- `Timestamp`: `time.Now().UnixMilli()` at emit.
- The user echo is REQUIRED: `PiProvider.OutboundEcho` returns nil (a pi child echoes user messages itself), so the agent must emit `PiUserMessageStart`/`PiUserMessageEnd` for every accepted prompt/steer or the TUI never renders the user bubble.
- `AgentEnd` marshals every accumulated message (user echo, assistant turns, toolResult messages, in order) into `[]json.RawMessage` for `child.PiAgentEnd(messages, usage)`, with usage summed over the turn's `AssistantTurn` calls.

- [ ] **Step 1: Write failing test** — build an `*anthropic.Message` via `json.Unmarshal` (the SDK unions are awkward to construct directly):

```go
// internal/agent/emit_test.go
package agent

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
)

const sampleResp = `{
 "id":"msg_1","type":"message","role":"assistant","model":"claude-x",
 "stop_reason":"tool_use",
 "content":[{"type":"text","text":"on it"},
            {"type":"tool_use","id":"tu_1","name":"bash","input":{"command":"ls"}}],
 "usage":{"input_tokens":10,"output_tokens":5,"cache_read_input_tokens":3,"cache_creation_input_tokens":0}}`

func TestAssistantTurnEmitsPiFrames(t *testing.T) {
	var resp anthropic.Message
	if err := json.Unmarshal([]byte(sampleResp), &resp); err != nil { t.Fatal(err) }
	var out bytes.Buffer
	fe := NewFrontend(strings.NewReader(""), &out, &fakeHandler{})
	em := NewEmitter(fe, "anthropic", "claude-x")
	em.AgentStart()
	em.UserMessage("go")
	em.AssistantTurn(&resp)
	em.ToolStart("tu_1", "bash", json.RawMessage(`{"command":"ls"}`))
	em.ToolEnd("tu_1", "bash", "file.txt", false)
	em.AgentEnd()

	var types []string
	for _, l := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		var f struct{ Type string `json:"type"` }
		if err := json.Unmarshal([]byte(l), &f); err != nil { t.Fatalf("bad frame %q: %v", l, err) }
		types = append(types, f.Type)
	}
	want := []string{"agent_start", "message_start", "message_end", // user echo
		"message_start", "message_update", "message_end", // assistant
		"tool_execution_start", "tool_execution_end",
		"agent_end", "agent_settled"}
	if strings.Join(types, ",") != strings.Join(want, ",") {
		t.Fatalf("frame sequence:\n got %v\nwant %v", types, want)
	}
	// spot-check mapping on the assistant message_end frame
	var me struct{ Message struct {
		StopReason string `json:"stopReason"`
		Content []map[string]any `json:"content"`
		Usage struct{ Input, Output int } `json:"usage"`
	} `json:"message"` }
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	_ = json.Unmarshal([]byte(lines[5]), &me)
	if me.Message.StopReason != "toolUse" { t.Fatalf("stopReason: %s", me.Message.StopReason) }
	if me.Message.Content[1]["type"] != "toolCall" { t.Fatalf("content[1]: %v", me.Message.Content[1]) }
	if me.Message.Usage.Input != 10 || me.Message.Usage.Output != 5 { t.Fatalf("usage: %+v", me.Message.Usage) }
	// agent_end carries the 3 accumulated messages: user echo, assistant, toolResult
	var ae struct{ Messages []json.RawMessage `json:"messages"` }
	_ = json.Unmarshal([]byte(lines[8]), &ae)
	if len(ae.Messages) != 3 { t.Fatalf("agent_end messages: %d", len(ae.Messages)) }
}
```

- [ ] **Step 2: Run to verify failure** — `go test ./internal/agent/ -run TestAssistantTurn -v` → FAIL (undefined NewEmitter).

- [ ] **Step 3: Implement `emit.go`** per the interface block above. Core of the mapper:

```go
func MapAssistantMessage(resp *anthropic.Message, provider string) child.PiAssistantMessage {
	var blocks []child.PiContentBlock
	for _, b := range resp.Content {
		switch v := b.AsAny().(type) {
		case anthropic.TextBlock:
			blocks = append(blocks, child.PiTextBlock(v.Text))
		case anthropic.ThinkingBlock:
			blocks = append(blocks, child.PiThinkingBlock(v.Thinking))
		case anthropic.ToolUseBlock:
			var args map[string]any
			if err := json.Unmarshal(v.Input, &args); err != nil {
				args = map[string]any{"_raw": string(v.Input)}
			}
			blocks = append(blocks, child.PiToolCallBlock(v.ID, v.Name, args))
		}
	}
	stop := "stop"
	switch resp.StopReason {
	case anthropic.StopReasonToolUse:
		stop = "toolUse"
	case anthropic.StopReasonMaxTokens:
		stop = "length"
	}
	u := resp.Usage
	usage := child.PiUsage{
		Input: int(u.InputTokens), Output: int(u.OutputTokens),
		CacheRead: int(u.CacheReadInputTokens), CacheWrite: int(u.CacheCreationInputTokens),
	}
	usage.TotalTokens = usage.Input + usage.Output + usage.CacheRead + usage.CacheWrite
	return child.PiAssistantMessage{
		Role: "assistant", Content: blocks, API: "anthropic-messages",
		Provider: provider, Model: string(resp.Model), Usage: usage,
		StopReason: stop, Timestamp: time.Now().UnixMilli(),
	}
}
```

`Emitter` accumulates `[]json.RawMessage` (marshal each user/assistant/toolResult message as it's emitted; `ToolEnd` appends a `child.PiToolResultMessage{Role:"toolResult", ...}` with a single `PiTextBlock(result)` and `IsError`), sums `PiUsage` across `AssistantTurn` calls, and `AgentEnd()` emits `child.PiAgentEnd(msgs, &summedUsage)` then `child.PiAgentSettled()` and clears state.

- [ ] **Step 4: Run tests** — PASS. Also run the daemon's frame-consumer tests to prove renderability: `go test ./internal/child/ -run TestPiEvents -v`.

- [ ] **Step 5: Commit** — `git add internal/agent/emit.go internal/agent/emit_test.go && git commit -m "feat(agent): pi event emitter"`.

### Task 7: engine (prompt → rafiki loop → frames)

**Files:**
- Create: `internal/agent/engine.go`, `internal/agent/engine_test.go`, `internal/agent/faketurns.go`

**Interfaces:**
- Consumes: rafiki `llm.Client/Conversation`, `agentloop.Run`, Task 2 hooks, Emitter, Frontend Handler.
- Produces:
  ```go
  type EngineConfig struct {
      Client *llm.Client; ConvOpts []llm.ConvOption
      Tools  agentloop.ToolSet
      Provider, ModelID, Name string
  }
  func NewEngine(cfg EngineConfig, fe *Frontend) (*Engine, error)  // creates the conversation
  // Engine implements Handler. Prompts are serialized (one turn at a time,
  // queued FIFO). Steer during a turn buffers into PendingUser; steer while
  // idle is treated as a prompt. Abort cancels the turn ctx.
  func (e *Engine) Wait()   // blocks until queued turns drain (for tests/shutdown)
  ```
  Plus the test seam used by daemon integration tests later: `faketurns.go`:
  ```go
  // LoadFakeSender reads a file of newline-separated JSON anthropic.Message
  // bodies and returns an llm.Sender that replays them in order.
  func LoadFakeSender(path string) (llm.Sender, error)
  ```

Engine turn flow (`HandlePrompt`):
1. enqueue; worker goroutine per queue item:
2. `em.UserMessage(text)`; `em.AgentStart()`
3. `ctx, cancel := context.WithCancel(baseCtx)`; store `cancel` for `HandleAbort`
4. `agentloop.Run(ctx, e.conv, e.tools, e.events(), llm.UserText(text))` where `events()` wires: `OnTurn` → `em.AssistantTurn(resp)` (skip on `err != nil`); `OnToolStart` → `em.ToolStart`; `OnToolEnd` → `em.ToolEnd`; `PendingUser` → drain steer buffer, emitting `em.UserMessage` for each drained steer text and returning the blocks
5. on `ctx.Err() == context.Canceled`: run `RepairOrphans` (Task 10) — until Task 10 lands, note the TODO in a failing-test-tracked way: this task's tests only cover the happy path
6. `em.AgentEnd()`

- [ ] **Step 1: Write failing test** — scripted two-turn run through a real (in-memory) rafiki conversation:

```go
// internal/agent/engine_test.go
package agent

// fake sender scripting: turn 1 returns sampleResp (tool_use tu_1 bash),
// turn 2 returns end_turn text "done". Registry has a fake "bash" tool
// returning "file.txt". Assert the full stdout frame-type sequence:
// agent_start after user echo, tool frames between assistant turns,
// agent_end + agent_settled last; and that State().SessionID is non-empty.

func TestEngineRunsScriptedTurn(t *testing.T) {
	fake := scriptedSender(t, sampleResp, sampleEndTurn) // json bodies from emit_test + a plain end_turn
	client, err := llm.NewClient(llm.WithUpstream(llm.UpstreamAnthropic, fake),
		llm.WithDefaultModel("claude-x"))
	if err != nil { t.Fatal(err) }
	var out syncBuffer // bytes.Buffer + mutex (Emit is cross-goroutine)
	fe := NewFrontend(strings.NewReader(""), &out, nil)
	// inline fake ToolSet — the real Registry arrives in Task 8
	ts := fakeToolSet{"bash": func(ctx context.Context, in json.RawMessage) (string, error) {
		return "file.txt", nil
	}}
	eng, err := NewEngine(EngineConfig{Client: client, Tools: ts,
		Provider: "anthropic", ModelID: "claude-x",
		ConvOpts: []llm.ConvOption{llm.NewConversation("fundi", "agent")}}, fe)
	if err != nil { t.Fatal(err) }
	fe.handler = eng
	eng.HandlePrompt("go")
	eng.Wait()
	assertFrameTypes(t, out.String(), []string{
		"message_start", "message_end", // user echo
		"agent_start",
		"message_start", "message_update", "message_end", // assistant turn 1 (tool_use)
		"tool_execution_start", "tool_execution_end",
		"message_start", "message_update", "message_end", // assistant turn 2 (end_turn)
		"agent_end", "agent_settled"})
}
```
(Write `assertFrameTypes`, `scriptedSender`, `syncBuffer`, and `fakeToolSet` — a `map[string]func` implementing `agentloop.ToolSet` with one `anthropic.ToolParam` definition per key — as helpers in the test file. The user-echo-before-`agent_start` order is locked by this test; it's the documented order.)

- [ ] **Step 2: Run to verify failure.**

- [ ] **Step 3: Implement `engine.go` + `faketurns.go`.** Engine skeleton:

```go
type Engine struct {
	conv     *llm.Conversation
	tools    agentloop.ToolSet
	fe       *Frontend
	em       *Emitter
	state    StateData
	queue    chan string
	wg       sync.WaitGroup
	mu       sync.Mutex
	cancel   context.CancelFunc // non-nil while a turn is running
	steerBuf []string
}
```
`HandleSteer`: under `mu`, if `cancel != nil` append to `steerBuf`, else forward to `HandlePrompt`. `PendingUser` drains `steerBuf` under `mu`, emits a `UserMessage` echo per entry, returns one `llm.UserText`-style block slice with all drained texts joined by newline. `HandleAbort`: under `mu`, call `cancel()` if set. `State()` returns `StateData{SessionID: e.conv.ID, ...}`. Errors from `agentloop.Run` other than cancellation: emit the error text as a final assistant-style frame is WRONG — instead `Emit` a `{"type":"agent_error","error":...}` line and log to stderr, then still `AgentEnd()` so the state machine settles.

- [ ] **Step 4: Run** — `go test ./internal/agent/ -v` → PASS.

- [ ] **Step 5: Commit** — `git add internal/agent/engine.go internal/agent/engine_test.go internal/agent/faketurns.go && git commit -m "feat(agent): engine wiring rafiki agentloop to pi frames"`.

### Task 8: file tools (registry, read, write, edit, glob, grep)

**Files:**
- Create: `internal/agent/tools/registry.go`, `read.go`, `write.go`, `edit.go`, `glob.go`, `grep.go`, `tracker.go`, plus `_test.go` for each

**Interfaces:**
- Produces:
  ```go
  func NewRegistry() *Registry            // implements agentloop.ToolSet
  func (r *Registry) Register(def anthropic.ToolUnionParam, fn ToolFunc)
  type ToolFunc func(ctx context.Context, input json.RawMessage) (string, error)
  func Def(name, description, jsonSchema string) anthropic.ToolUnionParam
  func NewFileTracker() *FileTracker      // read-before-write state, shared by read/write/edit
  func RegisterFileTools(r *Registry, tr *FileTracker)
  ```
- `Definitions()` returns tools **sorted by name** (Global Constraints: cache stability).

Tool contracts (each gets a table-driven test):
- **read** `{path, offset?, limit?}`: absolute path required; output `cat -n` numbered lines, default cap 2000 lines (then instruct offset/limit — no spill needed, paging is the mechanism); records path+mtime in tracker. Missing file → error result.
- **write** `{path, content}`: refuses if file exists and was not read since last modification (tracker); creates parent dirs.
- **edit** `{path, old_string, new_string, replace_all?}`: requires prior read; `old_string` must match exactly once unless `replace_all`; stale-mtime check via tracker.
- **glob** `{pattern, path?}`: `doublestar.Glob` over the base dir, results sorted by mtime descending, cap 200.
- **grep** `{pattern, path?, glob?, max_matches?}`: `regexp` compile; walk files (respect `.git` exclusion); output `path:line:text`; default cap 100 matches with `[+N more]` trailer.

- [ ] **Step 1: Write failing tests** (representative — repeat the pattern per tool):

```go
func TestEditRequiresPriorRead(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "a.txt")
	os.WriteFile(p, []byte("hello world"), 0o644)
	r, tr := NewRegistry(), NewFileTracker()
	RegisterFileTools(r, tr)
	_, err := r.Execute(context.Background(), "edit",
		json.RawMessage(`{"path":"`+p+`","old_string":"hello","new_string":"bye"}`))
	if err == nil || !strings.Contains(err.Error(), "read") {
		t.Fatalf("expected read-before-edit error, got %v", err)
	}
	if _, err := r.Execute(context.Background(), "read", json.RawMessage(`{"path":"`+p+`"}`)); err != nil { t.Fatal(err) }
	if _, err := r.Execute(context.Background(), "edit",
		json.RawMessage(`{"path":"`+p+`","old_string":"hello","new_string":"bye"}`)); err != nil { t.Fatal(err) }
	b, _ := os.ReadFile(p)
	if string(b) != "bye world" { t.Fatalf("edit result: %s", b) }
}

func TestDefinitionsSortedByName(t *testing.T) {
	r, tr := NewRegistry(), NewFileTracker()
	RegisterFileTools(r, tr)
	defs := r.Definitions()
	names := toolNames(defs)
	if !sort.StringsAreSorted(names) { t.Fatalf("not sorted: %v", names) }
}
```

- [ ] **Step 2: Run to verify failure.**
- [ ] **Step 3: Implement.** Registry core:

```go
func (r *Registry) Execute(ctx context.Context, name string, input json.RawMessage) (string, error) {
	r.mu.RLock()
	fn, ok := r.fns[name]
	r.mu.RUnlock()
	if !ok {
		return "", fmt.Errorf("unknown tool %q", name)
	}
	return fn(ctx, input)
}
```
(agentloop already converts returned errors into `is_error` tool results — do NOT panic or drop.)

- [ ] **Step 4: Run full package tests** — PASS.
- [ ] **Step 5: Commit** — `git add internal/agent/tools/ && git commit -m "feat(agent): registry and file tools"`.

### Task 9: output policy (spill/elision) + bash tool

**Files:**
- Create: `internal/agent/tools/output.go`, `output_test.go`, `bash.go`, `bash_test.go`

**Interfaces:**
- Produces:
  ```go
  type OutputPolicy struct{ Budget int; SpillDir string }   // Budget in bytes, default 30_000
  // Clip returns s unchanged when within budget; otherwise writes the FULL
  // s to SpillDir/<name>, and returns head(20% of budget) + marker + tail(80%).
  // Marker: "\n[... elided N bytes: full output at <path> ...]\n"
  func (p OutputPolicy) Clip(s, name string) string
  func RegisterBash(r *Registry, p OutputPolicy, cwd string)
  ```
- **bash** `{command, timeout_ms?}`: `exec.CommandContext(ctx, "bash", "-c", command)`, `cmd.Dir = cwd`, stdout+stderr merged, default timeout 120s (max 600s) via `context.WithTimeout` derived from the tool ctx (so abort's ctx-cancel also kills the process — set `cmd.Cancel` and `cmd.WaitDelay = 5*time.Second`). Exit code appended when non-zero. Result passed through `p.Clip(out, spillName)` where `spillName` = `agentloop.ToolCallID(ctx)` (Task 2) or a counter fallback.

- [ ] **Step 1: Failing tests**

```go
func TestClipSpillsAndElides(t *testing.T) {
	dir := t.TempDir()
	p := OutputPolicy{Budget: 1000, SpillDir: dir}
	long := strings.Repeat("A", 500) + strings.Repeat("B", 2000) + "VERDICT: fail\n"
	got := p.Clip(long, "tu_9")
	if len(got) > 1200 { t.Fatalf("clip too long: %d", len(got)) }
	if !strings.HasPrefix(got, "AAAA") { t.Fatal("head missing") }
	if !strings.Contains(got, "VERDICT: fail") { t.Fatal("tail (the verdict) missing") }
	if !strings.Contains(got, filepath.Join(dir, "tu_9")) { t.Fatal("spill path missing from marker") }
	full, err := os.ReadFile(filepath.Join(dir, "tu_9"))
	if err != nil || string(full) != long { t.Fatal("full output not spilled") }
}

func TestBashMergesStderrAndReportsExit(t *testing.T) {
	r := NewRegistry()
	RegisterBash(r, OutputPolicy{Budget: 30000, SpillDir: t.TempDir()}, t.TempDir())
	out, err := r.Execute(context.Background(), "bash",
		json.RawMessage(`{"command":"echo out; echo err >&2; exit 3"}`))
	if err != nil { t.Fatal(err) } // non-zero exit is a RESULT, not a tool error
	for _, want := range []string{"out", "err", "exit status 3"} {
		if !strings.Contains(out, want) { t.Fatalf("missing %q in %q", want, out) }
	}
}
```

- [ ] **Step 2: verify failure. Step 3: implement. Step 4: PASS. Step 5: commit** `feat(agent): bash tool with spill-never-destroy output policy`.

### Task 10: abort orphan repair + steer/abort engine tests

**Files:**
- Create: `internal/agent/orphans.go`, `orphans_test.go`
- Modify: `internal/agent/engine.go` (call RepairOrphans on cancelled runs), `engine_test.go`

**Interfaces:**
- Produces: `func RepairOrphans(ctx context.Context, conv *llm.Conversation) (int, error)` — scans `conv.History()`; for the trailing assistant message's `tool_use` ids lacking a matching `tool_result` in subsequent user rows, appends one user message of synthetic error results:

```go
	blocks = append(blocks, anthropic.NewToolResultBlock(id,
		"Tool execution aborted by user.", true))
```
Returns the number of synthesized results. Works in both memory and DB modes (it only uses `History` + `AppendUser`).

- [ ] **Step 1: Failing tests.** (a) unit: seed a memory conversation via a scripted sender that returns a tool_use response, cancel before the tool completes, call `RepairOrphans`, assert history now ends with a user row containing a `tool_result` for `tu_1` and a follow-up `Continue` with a scripted end_turn succeeds (the API-shape invariant: no dangling tool_use). (b) engine: `HandleAbort` mid-turn (slow fake tool blocking on ctx) → frames end with `tool_execution_end` (isError), `agent_end`, `agent_settled`; a second `HandlePrompt` runs cleanly.
- [ ] **Step 2: verify failure. Step 3: implement + wire into engine's cancelled-run path. Step 4: `go test ./internal/agent/... -v` PASS. Step 5: commit** `feat(agent): in-band abort with orphaned tool_use repair`.

### Task 11: context assembly (CLAUDE.md/AGENTS.md + env block + system prompt)

**Files:**
- Create: `internal/agent/contextfiles.go`, `contextfiles_test.go`, `sysprompt.go`, `sysprompt_test.go`

**Interfaces:**
- Produces:
  ```go
  // LoadContextFiles returns instruction-file content for cwd:
  // ~/.claude/CLAUDE.md (user-global), then CLAUDE.md/AGENTS.md at the git
  // root and at cwd (deduped when equal). @-includes: a line matching
  // ^@(\S+)$ inlines the referenced file (relative to the including file),
  // recursively, depth cap 5, cycle-safe; missing includes become
  // "[missing include: path]".
  func LoadContextFiles(cwd string) (string, error)
  type SysPromptConfig struct {
      Base, Override, Append string   // Override = SpawnRequest.SystemPrompt (replaces Base)
      ContextFiles           string   // "" when disabled
      SkillsInventory        string   // Task 12; "" until then
      Cwd, ModelID           string
  }
  func BuildSystemPrompt(c SysPromptConfig) string
  ```
- Assembly order (cache stability — static first): base (or Override), Append, context files, skills inventory, env block (`cwd`, `platform: darwin/linux`, `model`, today's date). Write a short const `defaultBasePrompt` describing the agent (coding agent, tool conventions, "your output streams to a controller; be concise").

- [ ] **Step 1: Failing tests** — temp dir with nested git root: root `CLAUDE.md` containing `@docs/extra.md`, cwd deeper with `AGENTS.md`; assert both appear, include is inlined, cycle (`a.md`@→`b.md`@→`a.md`) terminates with the missing/cycle marker, and `BuildSystemPrompt` orders sections base→append→files→skills→env.
- [ ] **Step 2–5:** fail → implement → PASS → commit `feat(agent): CLAUDE.md/AGENTS.md context loading and system prompt assembly`.

### Task 12: skills (discovery, inventory, Skill tool)

**Files:**
- Create: `internal/agent/skills.go`, `skills_test.go`, `internal/agent/tools/skill.go`, `skill_test.go`

**Interfaces:**
- Produces:
  ```go
  type SkillMeta struct{ Name, Description, Dir, Path string }
  // DiscoverSkills scans dirs (each containing <skill-name>/SKILL.md),
  // parsing YAML frontmatter (--- name/description ---). Later dirs
  // override earlier on name collision. Default dirs (built by caller):
  // ~/.claude/skills, <gitroot>/.claude/skills, plus --skills-dir extras.
  func DiscoverSkills(dirs []string, only []string) ([]SkillMeta, error)  // only = SpawnRequest.Skills filter; nil = all
  func SkillsInventory(skills []SkillMeta) string  // "- name: description" lines for the system prompt
  func RegisterSkillTool(r *Registry, skills []SkillMeta)
  ```
- **skill** tool `{skill}`: returns `"Base directory for this skill: <dir>\n\n" + <SKILL.md body minus frontmatter>`. Unknown skill → error listing available names. Frontmatter via `yaml.v3` into `struct{ Name, Description string }`; a file with unparseable frontmatter is skipped with a stderr log, never fatal.

- [ ] **Step 1: Failing tests** — temp skills tree with two skills (one user-level, one project-level shadowing the same name), assert override, inventory rendering, tool invocation returns body + base dir, `only` filter drops unlisted skills.
- [ ] **Step 2–5:** fail → implement → PASS → commit `feat(agent): SKILL.md discovery and skill tool`.

### Task 13: MCP client

**Files:**
- Create: `internal/agent/tools/mcp.go`, `mcp_test.go`

**Interfaces:**
- Produces:
  ```go
  // MCPConfig mirrors .mcp.json: {"mcpServers":{name:{command,args,env} | {url,headers}}}
  func LoadMCPConfig(path string) (MCPConfig, error)
  // ConnectMCP dials every configured server (stdio via command, HTTP via
  // url), lists tools, and registers each on r as mcp__<server>__<tool>
  // (hyphens in server/tool names normalized to underscores). Returns a
  // shutdown func. A server that fails to connect is logged to stderr and
  // skipped — one bad server must not kill the agent.
  func ConnectMCP(ctx context.Context, r *Registry, cfg MCPConfig, p OutputPolicy) (func(), error)
  ```
- Implementation uses `github.com/modelcontextprotocol/go-sdk/mcp`. **Before coding, check the current API surface with `go doc github.com/modelcontextprotocol/go-sdk/mcp | head -80`** — the SDK is young; expected shape: `mcp.NewClient(&mcp.Implementation{Name:"fundi"}, nil)`, `client.Connect(ctx, &mcp.CommandTransport{Command: exec.Command(cmd, args...)})` (stdio) / `&mcp.StreamableClientTransport{Endpoint: url}` (HTTP), `session.ListTools(ctx, nil)`, `session.CallTool(ctx, &mcp.CallToolParams{Name, Arguments})`. Tool results: concatenate text content blocks; `IsError` → returned as Go error (so agentloop marks `is_error`). Results pass through `p.Clip`.
- Tool input schemas from `ListTools` are passed through verbatim into `anthropic.ToolInputSchemaParam`.

- [ ] **Step 1: Failing test** — use the SDK's in-memory transport: construct an `mcp.Server` in the test with one `add` tool, connect via `mcp.NewInMemoryTransports()`, run `ConnectMCP`-equivalent registration against it, assert `mcp__test__add` appears in `Definitions()` and `Execute` returns the sum. (If the installed SDK version lacks in-memory transports, fall back to a stdio subprocess test using `go run ./internal/agent/tools/testdata/mcpserver`.)
- [ ] **Step 2–5:** fail → implement → PASS → commit `feat(agent): MCP client tools`.

### Task 14: `fundi agent` subcommand — config, model routing, main wiring

**Files:**
- Create: `cmd/fundi/agent.go`, `internal/agent/config.go`, `config_test.go`
- Modify: `cmd/fundi/main.go` (dispatch `agent` subcommand before the daemon's flag parsing)

**Interfaces:**
- Produces: `fundi agent` flags — the exact argv contract Task 16's `buildAgentArgv` targets:
  ```
  --model <id|family-latest>      default "sonnet-latest"
  --provider <anthropic|openrouter>  default: openrouter if model contains "/", else anthropic
  --thinking <off|low|medium|high|xhigh>  → ThinkingBudget 0/4096/8192/16384/32768
  --system-prompt <s>  --append-system-prompt <s>
  --no-context-files   --skills-dir <dir> (repeatable)  --skills <csv>  --no-skills
  --mcp-config <path>  default: <cwd>/.mcp.json if present
  --ref <external-ref> default: $PI_CONTROLLER_CHILD_ID (conversation correlation key)
  --db <postgres-url>  default: $FUNDI_AGENT_DB (empty = in-memory)
  --spill-dir <dir>    default: os.TempDir()/fundi-spill-<childID>
  --name <session-name>
  --fake-turns <path>  hidden test seam: replaces the sender with LoadFakeSender
  ```
- `internal/agent/config.go`: `type Config struct{...}` + `func (c Config) BuildEngine(ctx context.Context, fe *Frontend) (*Engine, func(), error)` — constructs `llm.Client` (senders from `ANTHROPIC_API_KEY` / `OPENROUTER_API_KEY`; error at startup if the chosen primary's key is missing), `llm.Fallback(UpstreamOpenRouter)` only when provider=anthropic AND the OpenRouter key exists, DB pool via `pgxpool` + `llm.WithStore` when `--db` set, conversation opts (`ByExternalRef(ref)`, `Entrypoint("agent")`, `Model`, `Primary`, `ThinkingBudget`, `SystemText(BuildSystemPrompt(...))`), registry (file tools + bash + skills + MCP), and the Emitter/Engine. The returned shutdown func closes MCP + the pool.
- `cmd/fundi/agent.go` `runAgent(args []string) int`: parse flags → `BuildEngine` → `fe.Run()` (stdin EOF = clean exit 0) → `eng.Wait()` → shutdown.

- [ ] **Step 1: Failing tests** for the pure parts: flag→Config parsing (thinking map, provider default heuristic, ref-from-env), and `BuildEngine` with `--fake-turns` producing a working Engine (reuse Task 7's scripted flow end-to-end through Config).
- [ ] **Step 2–5:** fail → implement → PASS → commit `feat(fundi): agent subcommand`.

Manual gate before commit: `printf '{"type":"get_state","id":"x"}\n' | ANTHROPIC_API_KEY=dummy go run ./cmd/fundi agent --fake-turns /dev/null` prints a `response.get_state` line and exits 0.

### Task 15: resume

**Files:**
- Modify: `internal/agent/config.go` (boot-time orphan repair), `cmd/fundi/agent.go`
- Create: `internal/agent/resume_test.go`

Resume model: the conversation key is `ByExternalRef(childID)` — `ctrl_resume` re-execs the agent with the same childID env, so the same DB-backed conversation reattaches automatically. On boot with a DB: run `RepairOrphans` once (synthetic error results for any turn interrupted by a crash/kill), log the repaired count, and report `conv.ID` as `sessionId` in `get_state` (the daemon sniffs + persists it). In-memory mode: resume yields a fresh conversation (documented degradation; daemon rings still hold scrollback).

- [ ] **Step 1: Failing test** — DB-gated: `t.Skip` unless `FUNDI_TEST_DB` env set (same pattern rafiki's integration tests use). Seed a conversation with a dangling tool_use via scripted sender + cancel, then `BuildEngine` again with the same ref: assert history gained the synthetic tool_result and a scripted `Continue` succeeds.
- [ ] **Step 2–5:** fail → implement → PASS (run with a local TimescaleDB: `FUNDI_TEST_DB=postgres://... go test ./internal/agent/ -run TestResume -v`) → commit `feat(agent): resume via external-ref conversation reattach`.

### Task 16: daemon integration — kind=agent

**Files:**
- Modify: `cmd/fundi/controller.go` (`resolveSpawnPlan`, new `buildAgentArgv`, `buildEnv` passthrough of `ANTHROPIC_API_KEY`/`OPENROUTER_API_KEY` when `req.APIKey`/inherited env supply them, spill cleanup in the forget path)
- Create: `cmd/fundi/controller_agent_test.go`
- Modify: `cmd/fundi/integration_test.go` (one end-to-end agent-kind test)

**Interfaces:**
- Consumes: Task 14's flag contract; `child.PiProvider{}` (identity — the agent speaks pi natively).
- Produces: `kind:"agent"` accepted by `ctrl_spawn`/`ctrl_resume`; `pic spawn --kind agent` works.

- [ ] **Step 1: Failing tests**

```go
func TestResolveSpawnPlanAgentKind(t *testing.T) {
	req := protocol.SpawnRequest{Kind: "agent", Cwd: "/tmp",
		Model: "deepseek/deepseek-chat", Thinking: "low",
		SystemPrompt: "sp", Skills: []string{"a", "b"}, NoContextFiles: true}
	bin, argv, prov, err := resolveSpawnPlan(req)
	if err != nil { t.Fatal(err) }
	self, _ := os.Executable()
	if bin != self { t.Fatalf("bin = %s", bin) }
	if _, ok := prov.(child.PiProvider); !ok { t.Fatalf("provider %T", prov) }
	joined := strings.Join(argv, " ")
	for _, want := range []string{"agent", "--model deepseek/deepseek-chat",
		"--thinking low", "--system-prompt sp", "--skills a,b", "--no-context-files"} {
		if !strings.Contains(joined, want) { t.Fatalf("argv missing %q: %v", want, argv) }
	}
}
```
Plus: `spawnKindLabel("agent") == "agent"`; forget removes the spill dir (create a sentinel file under the deterministic spill path `<stateDir>/spill/<childID>`, forget, assert gone — and align Task 14's `--spill-dir` default: `buildAgentArgv` passes `--spill-dir <stateDir>/spill/<childID>` explicitly so daemon and agent agree).

Integration test (pattern-match the existing claude integration test in `integration_test.go`): build the real binary via the test harness, `ctrl_spawn {kind:"agent", env:{"FUNDI_FAKE_TURNS":...}}` — note: pass the seam via `ExtraArgs: ["--fake-turns", path]` — send a prompt, assert `ctrl_child_status` reaches `streaming` then `idle`, `sessionId` gets sniffed non-empty, `ctrl_send {"type":"abort"}` mid-turn settles without a process restart (same PID).

- [ ] **Step 2: verify failure. Step 3: implement:**

```go
	case "agent":
		self, err := os.Executable()
		if err != nil {
			return "", nil, nil, fmt.Errorf("resolving own binary for agent kind: %w", err)
		}
		return self, buildAgentArgv(req), child.PiProvider{}, nil
```
`buildAgentArgv` maps: Model, Thinking, SystemPrompt, AppendSystemPrompt, Skills (csv), NoSkills, NoContextFiles, Name, ExtraArgs appended last. Spill dir as above. `ResumeSession`/resume path: nothing extra — the childID env is the ref.

- [ ] **Step 4: Full suite** — `go test ./...` PASS (including all pre-existing dispatch/claude tests).
- [ ] **Step 5: Commit** `feat(fundi): agent child kind`.

### Task 17: Sentinel plugin passthrough

**Files (repo: `~/home/sentinel-plugins`):**
- Modify: `plugins/node/pi/pi.go` (kind enums at lines ~69 and ~235), `plugins/node/pi/config.go` (add `DefaultKind`), `plugins/node/pi/tools_lifecycle.go` (default-kind resolution)
- Test: `plugins/node/pi/tools_lifecycle_spawn_test.go`

- [ ] **Step 1: Failing test** — spawn tool with no `kind` and plugin config `default_kind: "agent"` sends `kind:"agent"` in the SpawnRequest to the fake controller; explicit `kind:"claude"` still wins; `kind:"agent"` accepted by the schema enum.
- [ ] **Step 2: verify failure. Step 3: implement** — add `"agent"` to both enums, `DefaultKind string` in config (empty = current behavior, claude), resolve in the spawn tool. Update the two tool descriptions to mention the agent kind.
- [ ] **Step 4:** `go vet ./... && go test ./plugins/node/pi/` PASS. **Step 5: commit** `feat(pi): agent kind passthrough + default_kind config` — and update `sentinel-plugins` docs if the plugin README documents kinds.

### Task 18: live smoke gate (M1 exit criterion)

No new code — a scripted manual acceptance run, recorded in `docs/plans/2026-07-20-fundi-m0-m1-smoke.md` as you go.

- [ ] **Step 1:** Build + install: `cd ~/home/fundi && go build -o ~/bin/fundi ./cmd/fundi` (or the repo's install target). Restart the daemon with the new binary.
- [ ] **Step 2:** Real-model spawn: `pic spawn --kind agent --model haiku-latest --cwd /tmp/fundi-smoke` (with `ANTHROPIC_API_KEY` in the daemon env). Prompt: "Create hello.txt containing 'hi', then run `wc -c hello.txt` and report the byte count." Verify: file exists, frames render in `pic attach`, per-turn usage frame carries non-zero tokens.
- [ ] **Step 3:** Steer mid-turn (give it a multi-step task, `pic send --steer` an extra instruction) — verify the steer lands within the same turn.
- [ ] **Step 4:** Abort mid-turn — verify settle without process restart (`pic get` shows same PID, status idle) and that a follow-up prompt works.
- [ ] **Step 5:** Skill + cheap worker: drop a test SKILL.md under the cwd's `.claude/skills/`, spawn `--kind agent --model deepseek/deepseek-chat` (OPENROUTER_API_KEY set), prompt it to use the skill. Verify inventory + invocation.
- [ ] **Step 6:** From Zoe: `subagent_spawn` with kind=agent end-to-end; verify `_fleet` line and signal batching behave as with claude children.
- [ ] **Step 7:** Record results in the smoke doc; commit it. **Claude kind flip** (config `default_kind: "agent"`) is a deliberate user decision after this gate — do not flip it in code.

---

## Self-review notes

- Spec coverage: design §"Agent-loop internals" → Tasks 5–15; §"Architecture/components 3–4" → Tasks 14, 16; §Sentinel → Task 17; M0 → Tasks 1–4; spill lifecycle (forget cleanup) → Task 16; streaming sender, spawn tool, coordinator skill, TCP/k8s are M2/M3 — intentionally absent.
- The engine's frame ordering (user echo before `agent_start`) is locked by Task 7's test and matches the TUI's rendering expectations; if attach rendering looks wrong in Task 18, adjust the order there and update the test — it's a one-line swap.
- rafiki fork API additions (Task 2) are consumed by Tasks 7 (hooks), 9 (`ToolCallID`), 14 (`Primary`, `ThinkingBudget`) — signatures match.
