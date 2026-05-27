# pi-controller daemon — implementation plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build the `pi-controller` daemon in Go: a single binary that hosts `pi --mode rpc` children as subprocesses, multiplexes their JSONL event streams to multiple concurrent clients over a Unix domain socket, and exposes the control plane described in `tasks/pi-controller-protocol.md`.

**Architecture:** Per-child supervise goroutine owns process state (fork/exec/wait/reap) and three I/O goroutines (writer, stdout reader, stderr reader). All state outside the supervisor lives in an indexed in-memory `Store` keyed by `ChildID`. A `Bus[T]` per child fans events to subscribers with non-blocking drop semantics. A bounded ring buffer per child backs `ctrl_get_recent`. Disk persistence (gzipped per-child logs + atomic-rename state records) supports forensics and resume across controller restarts. UDS server with a per-connection JSONL dispatch loop. Two pi-RPC commands (`new_session`, `switch_session`) are intercepted via kill-and-respawn rather than forwarded.

**Tech Stack:** Go (>= 1.22 for generics), [`puzpuzpuz/xsync/v4`](https://github.com/puzpuzpuz/xsync) for concurrent maps, [`oklog/ulid/v2`](https://github.com/oklog/ulid) for `ChildID` generation. Standard library otherwise (`net`, `os/exec`, `compress/gzip`, `encoding/json`, `bufio`, `context`, `testing`).

**Spec:** `tasks/pi-controller-protocol.md`. The plan references section numbers (e.g. §6.3) — always cross-check against the spec, don't reimplement from memory.

**Scope deferral:** The `pi-ctl` CLI is a separate follow-up plan. This plan targets a daemon that can be exercised with `nc -U` + a small Python or shell harness, plus an integration test using a fake `pi` shell script.

---

## Repo layout (target)

```
~/home/pi-controller/
├── README.md
├── go.mod
├── go.sum
├── Makefile
├── .gitignore
├── cmd/
│   ├── pi-controller/main.go      # daemon entry point
│   └── pi-ctl/                    # (deferred to plan B)
├── internal/
│   ├── protocol/                  # wire types + JSONL framing
│   │   ├── types.go
│   │   ├── frame.go
│   │   └── frame_test.go
│   ├── bus/
│   │   ├── bus.go
│   │   └── bus_test.go
│   ├── ring/
│   │   ├── ring.go
│   │   └── ring_test.go
│   ├── store/
│   │   ├── session.go             # Session struct + Snapshot DTO
│   │   ├── store.go               # indexed Store + lookups
│   │   └── store_test.go
│   ├── child/
│   │   ├── state.go               # StateMachine + Status enum
│   │   ├── state_test.go
│   │   ├── child.go               # Child struct + supervise loop
│   │   ├── child_test.go          # uses fake-pi script
│   │   ├── sniff.go               # opportunistic metadata extraction
│   │   └── sniff_test.go
│   ├── intercept/
│   │   ├── intercept.go           # decoder + new_session/switch_session
│   │   └── intercept_test.go
│   ├── persist/
│   │   ├── records.go             # state record JSON, atomic-rename
│   │   ├── records_test.go
│   │   ├── logs.go                # gzip dump on exit
│   │   └── logs_test.go
│   └── server/
│       ├── server.go              # UDS listen, accept, per-conn goroutine
│       ├── dispatch.go            # ctrl_* command dispatch
│       └── server_test.go
├── tasks/
│   ├── pi-controller-protocol.md  # spec (already present)
│   └── 2026-05-25-implementation-plan-daemon.md  # this plan
└── test/
    └── integration/
        ├── fake-pi.sh             # canned-JSONL stub for tests
        └── integration_test.go    # end-to-end smoke
```

The `internal/client` directory in the bootstrap is reserved for the `pi-ctl` plan; leave it empty for now.

---

## Conventions for every task

- **Test-driven.** Each task writes the failing test first, then the implementation, then verifies green, then commits. Where a task is plumbing-only and can't be meaningfully tested in isolation, that's called out explicitly.
- **Small commits.** One commit per task minimum; sub-commits within a task are fine if natural.
- **No `fmt.Println` for runtime logging.** Use the stdlib `log/slog` with a JSON handler from day one. Output goes to stderr by default; the daemon's `controller.log` is a v1+ concern.
- **No global state outside `main`.** Everything is constructor-injected. The `Controller` struct in `cmd/pi-controller/main.go` is the only top-level composition root.
- **Cross-platform-clean.** No Linux-only syscalls in v1; everything must build on macOS too (where the project will be developed and tested). The `kill(pid, 0)` check uses `process.Signal(syscall.Signal(0))`, not raw `unix.Kill`.
- **No `panic` outside `init` or main wiring.** Errors are returned, never recovered.

---

### Task 1: Repo bootstrap and `go.mod`

**Files:**
- Create: `~/home/pi-controller/go.mod`
- Create: `~/home/pi-controller/Makefile`
- Modify: `~/home/pi-controller/.gitignore` (already present; verify only)

- [ ] **Step 1: Initialize the Go module.**

```bash
cd ~/home/pi-controller
go mod init git.graveland.dev/brent/pi-controller
```

Note: if your monorepo namespace differs (`github.com/<user>/pi-controller` etc.), use that. Whatever you pick is the import path used throughout the rest of the plan.

- [ ] **Step 2: Pin Go toolchain.**

Edit `go.mod` to require Go 1.22 or later (generics + `range` over function values aren't required yet, but 1.22 has the loopvar fix that matters):

```
go 1.22
```

- [ ] **Step 3: Add base dependencies.**

```bash
go get github.com/puzpuzpuz/xsync/v4@latest
go get github.com/oklog/ulid/v2@latest
go mod tidy
```

Don't pull cobra yet — that's CLI work. Don't pull testify — stdlib `testing` is fine.

- [ ] **Step 4: Write the Makefile.**

```makefile
.PHONY: build test test-race vet lint fmt clean

GO ?= go
BIN_DIR := bin

build:
	mkdir -p $(BIN_DIR)
	$(GO) build -o $(BIN_DIR)/pi-controller ./cmd/pi-controller

test:
	$(GO) test ./...

test-race:
	$(GO) test -race ./...

vet:
	$(GO) vet ./...

fmt:
	$(GO) fmt ./...

clean:
	rm -rf $(BIN_DIR)
```

- [ ] **Step 5: Verify build infrastructure works on an empty module.**

```bash
make vet
make test
```

Expected: vet returns silently; test reports "no Go files in <path>" or "no test files" — both are fine. Anything else, fix before committing.

- [ ] **Step 6: Commit.**

```bash
git add .
git commit -m "bootstrap: go module, Makefile, .gitignore, README, protocol spec, plan"
```

---

### Task 2: Protocol package — types

**Files:**
- Create: `internal/protocol/types.go`
- Create: `internal/protocol/types_test.go`

The wire types are the most-referenced piece of the codebase. Get the names right and the rest follows naturally. Cross-reference §6 and §7 of the spec for every type.

- [ ] **Step 1: Write tests for round-trip JSON serialization of every command request shape.**

```go
package protocol_test

import (
	"encoding/json"
	"testing"

	"git.graveland.dev/brent/pi-controller/internal/protocol"
)

func TestSpawnRequest_RoundTrip(t *testing.T) {
	req := protocol.SpawnRequest{
		Type:     "ctrl_spawn",
		ID:       "req-1",
		Name:     "afk",
		Cwd:      "/tmp/x",
		Model:    "claude-sonnet-4",
		Thinking: "medium",
	}
	b, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	var got protocol.SpawnRequest
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if got != req {
		t.Fatalf("round-trip mismatch:\n got  %+v\n want %+v", got, req)
	}
}

// Repeat for: ListRequest, GetRequest, ResumeRequest, KillRequest,
// AuthRequest, SubscribeRequest (with Filter), UnsubscribeRequest,
// GlobalSubscribeRequest, GlobalUnsubscribeRequest, GetRecentRequest,
// SendRequest, ForgetRequest, ForgetAllExitedRequest, SearchRequest,
// StatusRequest.
```

For each request, write one round-trip test with the *complete* shape from the spec (all optional fields populated). This locks both the JSON tags and the Go field names.

- [ ] **Step 2: Run tests — should fail compilation (no types yet).**

```bash
go test ./internal/protocol/...
```

Expected: build errors about undefined types.

- [ ] **Step 3: Define request types.**

Each `ctrl_*` command gets a typed struct. Use `omitempty` on every optional field so missing fields stay missing in the wire form.

```go
package protocol

// Type constants for the type field.
const (
	TypeCtrlList                 = "ctrl_list"
	TypeCtrlGet                  = "ctrl_get"
	TypeCtrlSpawn                = "ctrl_spawn"
	TypeCtrlResume               = "ctrl_resume"
	TypeCtrlKill                 = "ctrl_kill"
	TypeCtrlAuth                 = "ctrl_auth"
	TypeCtrlSubscribe            = "ctrl_subscribe"
	TypeCtrlUnsubscribe          = "ctrl_unsubscribe"
	TypeCtrlGlobalSubscribe      = "ctrl_global_subscribe"
	TypeCtrlGlobalUnsubscribe    = "ctrl_global_unsubscribe"
	TypeCtrlGetRecent            = "ctrl_get_recent"
	TypeCtrlSend                 = "ctrl_send"
	TypeCtrlForget               = "ctrl_forget"
	TypeCtrlForgetAllExited      = "ctrl_forget_all_exited"
	TypeCtrlSearch               = "ctrl_search"
	TypeCtrlStatus               = "ctrl_status"
	TypeCtrlResponse             = "ctrl_response"
	TypeCtrlEvent                = "ctrl_event"
	TypeCtrlChildSpawned         = "ctrl_child_spawned"
	TypeCtrlChildExited          = "ctrl_child_exited"
	TypeCtrlChildStatus          = "ctrl_child_status"
	TypeCtrlChildRenamed         = "ctrl_child_renamed"
)

// SpawnRequest mirrors §6.3.
type SpawnRequest struct {
	Type     string `json:"type"`             // always TypeCtrlSpawn
	ID       string `json:"id,omitempty"`
	Name     string `json:"name,omitempty"`
	Cwd      string `json:"cwd"`              // required
	Provider string `json:"provider,omitempty"`
	Model    string `json:"model,omitempty"`
	Thinking string `json:"thinking,omitempty"`
	APIKey   string `json:"apiKey,omitempty"`
	// ... (every field from §6.3, with json tag matching the spec exactly)
}

// All other request types, modeled directly off §6 of the spec.
```

Reference the spec for the complete field list of every command. Don't shortcut — every optional field in the spec needs a Go field with `omitempty`.

- [ ] **Step 4: Verify tests pass.**

```bash
go test ./internal/protocol/...
```

Expected: all round-trip tests pass.

- [ ] **Step 5: Add response types.**

```go
// Response is the generic envelope for any ctrl_response frame.
// The Data field is left as json.RawMessage so consumers can decode
// into the per-command response data shape lazily.
type Response struct {
	Type    string          `json:"type"`     // always TypeCtrlResponse
	Command string          `json:"command"`
	ID      string          `json:"id,omitempty"`
	Success bool            `json:"success"`
	Data    json.RawMessage `json:"data,omitempty"`
	Error   *ErrorBody      `json:"error,omitempty"`
}

type ErrorBody struct {
	Code    string `json:"code"`
	Message string `json:"message,omitempty"`
}

// Per-command response data types (used by callers that want typed access):
type SpawnResponseData struct {
	ChildID     string `json:"childId"`
	SessionID   string `json:"sessionId,omitempty"`
	SessionFile string `json:"sessionFile,omitempty"`
	Model       string `json:"model,omitempty"`
	Stalled     bool   `json:"stalled"`
}

type ListResponseData struct {
	Children []ChildSummary `json:"children"`
}

type ChildSummary struct {
	ChildID      string `json:"childId"`
	PID          *int   `json:"pid"`              // null when exited
	Cwd          string `json:"cwd"`
	Name         string `json:"name,omitempty"`
	Model        string `json:"model,omitempty"`
	SessionID    string `json:"sessionId,omitempty"`
	SessionFile  string `json:"sessionFile,omitempty"`
	Status       string `json:"status"`
	StartedAt    int64  `json:"startedAt"`
	LastActivity int64  `json:"lastActivity"`
	ExitCode     *int   `json:"exitCode"`
	ExitSignal   string `json:"exitSignal,omitempty"`
}

// ... GetRecentResponseData, SearchResponseData, StatusResponseData, etc.
```

For every response data shape in §6, define a typed struct. Tests for these are deferred to the dispatch task; the round-trip is implicitly exercised end-to-end there.

- [ ] **Step 6: Add event types** (§7).

```go
// Event is the generic envelope. Use Decode() to extract.
type CtrlEvent struct {
	Type    string          `json:"type"`     // always TypeCtrlEvent
	ChildID string          `json:"childId"`
	Event   json.RawMessage `json:"event"`    // verbatim pi-RPC event
}

type CtrlChildSpawned struct {
	Type    string `json:"type"`             // TypeCtrlChildSpawned
	ChildID string `json:"childId"`
	Name    string `json:"name,omitempty"`
	Cwd     string `json:"cwd"`
	PID     int    `json:"pid"`
	Model   string `json:"model,omitempty"`
	At      int64  `json:"at"`
}

// ... CtrlChildExited, CtrlChildStatus, CtrlChildRenamed (§7.3-7.5)
```

- [ ] **Step 7: Add error code constants** (§8).

```go
const (
	ErrChildNotFound       = "child_not_found"
	ErrChildExited         = "child_exited"
	ErrChildInGrace        = "child_in_grace"
	ErrChildShuttingDown   = "child_shutting_down"
	ErrNotResumable        = "not_resumable"
	ErrNotExited           = "not_exited"
	ErrSessionFileMissing  = "session_file_missing"
	ErrBackpressure        = "backpressure"
	ErrInvalidArgs         = "invalid_args"
	ErrSpawnFailed         = "spawn_failed"
	ErrAuthRequired        = "auth_required"
	ErrAuthInvalid         = "auth_invalid"
	ErrNotFound            = "not_found"
	ErrInternal            = "internal"
)
```

- [ ] **Step 8: Add status constants** (§10).

```go
type Status string

const (
	StatusSpawning     Status = "spawning"
	StatusIdle         Status = "idle"
	StatusStreaming    Status = "streaming"
	StatusToolRunning  Status = "tool_running"
	StatusCompacting   Status = "compacting"
	StatusBlockedUI    Status = "blocked_ui"
	StatusShuttingDown Status = "shutting_down"
	StatusExited       Status = "exited"
)
```

- [ ] **Step 9: Verify everything still builds and tests pass.**

```bash
make vet
make test
```

- [ ] **Step 10: Commit.**

```bash
git add internal/protocol/
git commit -m "protocol: wire types for ctrl_* commands, responses, events"
```

---

### Task 3: Protocol package — JSONL framing

**Files:**
- Create: `internal/protocol/frame.go`
- Create: `internal/protocol/frame_test.go`

§3 of the spec is the only "hard requirement" in framing: split on `\n` only, never on Unicode line separators, accept optional trailing `\r`. Default `bufio.Scanner` with `bufio.ScanLines` is *not* protocol-compliant because Go's `ScanLines` only handles `\r\n` / `\n` — but the buffer size limit (default 64KB) is the real footgun and a single pi event can easily exceed it.

- [ ] **Step 1: Write the failing test for the line splitter.**

```go
package protocol_test

import (
	"bytes"
	"io"
	"strings"
	"testing"

	"git.graveland.dev/brent/pi-controller/internal/protocol"
)

func TestFrameReader_SplitsOnLFOnly(t *testing.T) {
	// U+2028 (LINE SEPARATOR) and U+2029 (PARAGRAPH SEPARATOR)
	// must not split frames — they're valid inside JSON strings.
	input := `{"a":"first\u2028second"}` + "\n" + `{"b":"second\u2029line"}` + "\n"
	r := protocol.NewFrameReader(strings.NewReader(input), 16*1024*1024)
	var got []string
	for {
		line, err := r.ReadFrame()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, string(line))
	}
	want := []string{
		`{"a":"first\u2028second"}`,
		`{"b":"second\u2029line"}`,
	}
	if len(got) != len(want) {
		t.Fatalf("got %d lines, want %d", len(got), len(want))
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("line %d:\n got  %q\n want %q", i, got[i], want[i])
		}
	}
}

func TestFrameReader_StripsTrailingCR(t *testing.T) {
	input := "line1\r\nline2\n"
	r := protocol.NewFrameReader(strings.NewReader(input), 1024)
	line, err := r.ReadFrame()
	if err != nil {
		t.Fatal(err)
	}
	if string(line) != "line1" {
		t.Fatalf("got %q, want %q", line, "line1")
	}
}

func TestFrameReader_LargeFrame(t *testing.T) {
	// 4MB frame must fit when max is 16MB.
	big := strings.Repeat("a", 4*1024*1024)
	input := big + "\n"
	r := protocol.NewFrameReader(strings.NewReader(input), 16*1024*1024)
	line, err := r.ReadFrame()
	if err != nil {
		t.Fatal(err)
	}
	if len(line) != 4*1024*1024 {
		t.Fatalf("got len %d, want 4MB", len(line))
	}
}

func TestFrameReader_FrameTooLarge(t *testing.T) {
	// Set max to 1KB, send 2KB. Should error.
	big := strings.Repeat("a", 2*1024)
	input := big + "\n"
	r := protocol.NewFrameReader(strings.NewReader(input), 1024)
	_, err := r.ReadFrame()
	if err == nil {
		t.Fatal("expected ErrFrameTooLarge, got nil")
	}
	if !bytes.Contains([]byte(err.Error()), []byte("too large")) {
		t.Fatalf("got %v, want error containing 'too large'", err)
	}
}
```

- [ ] **Step 2: Run tests — should fail (no FrameReader yet).**

```bash
go test ./internal/protocol/...
```

- [ ] **Step 3: Implement `FrameReader`.**

```go
package protocol

import (
	"bufio"
	"bytes"
	"errors"
	"io"
)

var ErrFrameTooLarge = errors.New("frame too large")

// FrameReader reads LF-terminated frames with a hard max-size cap.
// It only splits on '\n'. Trailing '\r' is stripped.
// Lines longer than maxBytes return ErrFrameTooLarge.
type FrameReader struct {
	r        *bufio.Reader
	maxBytes int
	buf      bytes.Buffer
}

func NewFrameReader(r io.Reader, maxBytes int) *FrameReader {
	return &FrameReader{
		r:        bufio.NewReaderSize(r, 64*1024),
		maxBytes: maxBytes,
	}
}

// ReadFrame returns the next frame (without trailing \n or \r).
// Returns io.EOF only when no partial data remained.
func (f *FrameReader) ReadFrame() ([]byte, error) {
	f.buf.Reset()
	for {
		chunk, err := f.r.ReadSlice('\n')
		if len(chunk) > 0 {
			if f.buf.Len()+len(chunk) > f.maxBytes {
				return nil, ErrFrameTooLarge
			}
			f.buf.Write(chunk)
		}
		if err == bufio.ErrBufferFull {
			continue
		}
		if err == nil {
			// Complete line, ending in '\n'.
			line := f.buf.Bytes()
			line = line[:len(line)-1] // drop \n
			if len(line) > 0 && line[len(line)-1] == '\r' {
				line = line[:len(line)-1]
			}
			out := make([]byte, len(line))
			copy(out, line)
			return out, nil
		}
		if err == io.EOF {
			if f.buf.Len() > 0 {
				// Trailing partial frame without newline — treat as a frame.
				line := f.buf.Bytes()
				if len(line) > 0 && line[len(line)-1] == '\r' {
					line = line[:len(line)-1]
				}
				out := make([]byte, len(line))
				copy(out, line)
				// Mark next call to return io.EOF cleanly.
				f.buf.Reset()
				return out, nil
			}
			return nil, io.EOF
		}
		return nil, err
	}
}
```

- [ ] **Step 4: Add a writer helper** (small, but mirrors the reader symmetrically).

```go
// WriteFrame writes one JSONL frame: payload + '\n'.
// payload must not contain '\n'.
func WriteFrame(w io.Writer, payload []byte) error {
	if _, err := w.Write(payload); err != nil {
		return err
	}
	_, err := w.Write([]byte{'\n'})
	return err
}
```

- [ ] **Step 5: Verify tests pass.**

```bash
go test ./internal/protocol/...
```

- [ ] **Step 6: Add a round-trip test for `WriteFrame` + `FrameReader`.**

```go
func TestFrameWriteRead_RoundTrip(t *testing.T) {
	var b bytes.Buffer
	frames := [][]byte{
		[]byte(`{"type":"prompt","message":"hi"}`),
		[]byte(`{"type":"agent_end"}`),
	}
	for _, f := range frames {
		if err := protocol.WriteFrame(&b, f); err != nil {
			t.Fatal(err)
		}
	}
	r := protocol.NewFrameReader(&b, 1024)
	for i, want := range frames {
		got, err := r.ReadFrame()
		if err != nil {
			t.Fatalf("frame %d: %v", i, err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("frame %d:\n got  %q\n want %q", i, got, want)
		}
	}
	if _, err := r.ReadFrame(); err != io.EOF {
		t.Fatalf("expected EOF after last frame, got %v", err)
	}
}
```

- [ ] **Step 7: Verify and commit.**

```bash
make test
git add internal/protocol/frame.go internal/protocol/frame_test.go
git commit -m "protocol: JSONL frame reader/writer with size cap and CR/LF handling"
```

---

### Task 4: Bus — generic publish/subscribe

**Files:**
- Create: `internal/bus/bus.go`
- Create: `internal/bus/bus_test.go`

Per §11.2: each subscriber gets a bounded channel; on full, drop and increment a drop counter. The producer is single-goroutine (the supervise loop), so the bus doesn't need internal locking on the publish path beyond protecting the subscriber set.

- [ ] **Step 1: Write tests for the core operations.**

```go
package bus_test

import (
	"testing"
	"time"

	"git.graveland.dev/brent/pi-controller/internal/bus"
)

func TestBus_SubscribeAndPublish(t *testing.T) {
	b := bus.New[int](bus.Options{PerSubBuffer: 4})
	defer b.Close()

	ch, cancel := b.Subscribe()
	defer cancel()

	b.Publish(1)
	b.Publish(2)

	got := []int{}
	for i := 0; i < 2; i++ {
		select {
		case v := <-ch:
			got = append(got, v)
		case <-time.After(time.Second):
			t.Fatalf("timeout waiting for event %d", i)
		}
	}
	if got[0] != 1 || got[1] != 2 {
		t.Fatalf("got %v, want [1 2]", got)
	}
}

func TestBus_MultipleSubscribers_IndependentChannels(t *testing.T) {
	b := bus.New[int](bus.Options{PerSubBuffer: 4})
	defer b.Close()

	ch1, c1 := b.Subscribe()
	ch2, c2 := b.Subscribe()
	defer c1()
	defer c2()

	b.Publish(42)
	for _, ch := range []<-chan int{ch1, ch2} {
		select {
		case v := <-ch:
			if v != 42 {
				t.Fatalf("got %d", v)
			}
		case <-time.After(time.Second):
			t.Fatal("subscriber missed event")
		}
	}
}

func TestBus_DropsOnFullChannel_DoesNotBlock(t *testing.T) {
	b := bus.New[int](bus.Options{PerSubBuffer: 2})
	defer b.Close()

	_, cancel := b.Subscribe()
	defer cancel()

	// Publish more than buffer can hold. Must not block.
	done := make(chan struct{})
	go func() {
		for i := 0; i < 10; i++ {
			b.Publish(i)
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Publish blocked on slow subscriber")
	}

	// Subscriber's drop counter must show at least 8 drops.
	stats := b.Stats()
	if stats.SubscriberCount != 1 {
		t.Fatalf("expected 1 subscriber, got %d", stats.SubscriberCount)
	}
	if stats.TotalDrops < 8 {
		t.Fatalf("expected at least 8 drops, got %d", stats.TotalDrops)
	}
}

func TestBus_CancelRemovesSubscriber(t *testing.T) {
	b := bus.New[int](bus.Options{PerSubBuffer: 1})
	defer b.Close()

	ch, cancel := b.Subscribe()
	cancel()

	// After cancel, the channel must close eventually.
	select {
	case _, ok := <-ch:
		if ok {
			t.Fatal("expected channel to close")
		}
	case <-time.After(time.Second):
		t.Fatal("channel did not close after cancel")
	}
	if b.Stats().SubscriberCount != 0 {
		t.Fatal("subscriber not removed")
	}
}

func TestBus_CloseClosesAllSubscriberChannels(t *testing.T) {
	b := bus.New[int](bus.Options{PerSubBuffer: 1})
	ch1, _ := b.Subscribe()
	ch2, _ := b.Subscribe()
	b.Close()
	for i, ch := range []<-chan int{ch1, ch2} {
		select {
		case _, ok := <-ch:
			if ok {
				t.Fatalf("ch%d: expected closed", i+1)
			}
		case <-time.After(time.Second):
			t.Fatalf("ch%d: not closed after Close", i+1)
		}
	}
}
```

- [ ] **Step 2: Run tests — fail.**

- [ ] **Step 3: Implement the Bus.**

```go
package bus

import (
	"sync"
	"sync/atomic"
)

type Options struct {
	PerSubBuffer int // default 256
}

type Stats struct {
	SubscriberCount int
	TotalDrops      uint64
}

type Bus[T any] struct {
	opts        Options
	mu          sync.Mutex
	subs        map[uint64]*subscriber[T]
	nextID      uint64
	totalDrops  atomic.Uint64
	closed      atomic.Bool
}

type subscriber[T any] struct {
	id    uint64
	ch    chan T
	drops atomic.Uint64
}

func New[T any](opts Options) *Bus[T] {
	if opts.PerSubBuffer <= 0 {
		opts.PerSubBuffer = 256
	}
	return &Bus[T]{
		opts: opts,
		subs: make(map[uint64]*subscriber[T]),
	}
}

func (b *Bus[T]) Subscribe() (<-chan T, func()) {
	b.mu.Lock()
	if b.closed.Load() {
		b.mu.Unlock()
		ch := make(chan T)
		close(ch)
		return ch, func() {}
	}
	b.nextID++
	s := &subscriber[T]{
		id: b.nextID,
		ch: make(chan T, b.opts.PerSubBuffer),
	}
	b.subs[s.id] = s
	b.mu.Unlock()

	cancel := func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		if _, ok := b.subs[s.id]; ok {
			delete(b.subs, s.id)
			close(s.ch)
		}
	}
	return s.ch, cancel
}

func (b *Bus[T]) Publish(v T) {
	if b.closed.Load() {
		return
	}
	b.mu.Lock()
	subs := make([]*subscriber[T], 0, len(b.subs))
	for _, s := range b.subs {
		subs = append(subs, s)
	}
	b.mu.Unlock()
	for _, s := range subs {
		select {
		case s.ch <- v:
		default:
			s.drops.Add(1)
			b.totalDrops.Add(1)
		}
	}
}

func (b *Bus[T]) Stats() Stats {
	b.mu.Lock()
	n := len(b.subs)
	b.mu.Unlock()
	return Stats{
		SubscriberCount: n,
		TotalDrops:      b.totalDrops.Load(),
	}
}

func (b *Bus[T]) Close() {
	if !b.closed.CompareAndSwap(false, true) {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, s := range b.subs {
		close(s.ch)
	}
	b.subs = nil
}
```

- [ ] **Step 4: Verify tests pass.**

```bash
go test -race ./internal/bus/...
```

- [ ] **Step 5: Commit.**

```bash
git add internal/bus/
git commit -m "bus: generic publish/subscribe with bounded channels and drop counters"
```

---

### Task 5: Ring buffer

**Files:**
- Create: `internal/ring/ring.go`
- Create: `internal/ring/ring_test.go`

Per §11.1: 5000 events / 64 MB, LRU. The ring holds `Event` records (each carries the raw JSONL bytes plus a publish timestamp).

- [ ] **Step 1: Write tests for the core operations.**

```go
package ring_test

import (
	"strings"
	"testing"

	"git.graveland.dev/brent/pi-controller/internal/ring"
)

func TestRing_AppendAndRecent_InOrder(t *testing.T) {
	r := ring.New(ring.Options{MaxEvents: 100, MaxBytes: 1 << 20})
	for i := 0; i < 5; i++ {
		r.Append([]byte("event-" + string(rune('a'+i))), int64(i))
	}
	got := r.Recent(ring.Query{Limit: 10})
	if len(got) != 5 {
		t.Fatalf("got %d, want 5", len(got))
	}
	for i, ev := range got {
		want := "event-" + string(rune('a'+i))
		if string(ev.Bytes) != want {
			t.Fatalf("event %d: got %q, want %q", i, ev.Bytes, want)
		}
		if ev.Timestamp != int64(i) {
			t.Fatalf("event %d ts: got %d, want %d", i, ev.Timestamp, i)
		}
	}
}

func TestRing_EvictsByEventCount(t *testing.T) {
	r := ring.New(ring.Options{MaxEvents: 3, MaxBytes: 1 << 20})
	for i := 0; i < 5; i++ {
		r.Append([]byte{byte('a' + i)}, int64(i))
	}
	got := r.Recent(ring.Query{Limit: 10})
	if len(got) != 3 {
		t.Fatalf("got %d, want 3", len(got))
	}
	wantFirst := byte('c') // a, b evicted
	if got[0].Bytes[0] != wantFirst {
		t.Fatalf("first: got %q, want %q", got[0].Bytes, wantFirst)
	}
}

func TestRing_EvictsByByteSize(t *testing.T) {
	r := ring.New(ring.Options{MaxEvents: 1000, MaxBytes: 30})
	for i := 0; i < 5; i++ {
		// each event is 10 bytes
		r.Append([]byte(strings.Repeat(string(rune('a'+i)), 10)), int64(i))
	}
	got := r.Recent(ring.Query{Limit: 10})
	if len(got) != 3 {
		t.Fatalf("got %d, want 3 (kept under 30 bytes)", len(got))
	}
}

func TestRing_RecentSince(t *testing.T) {
	r := ring.New(ring.Options{MaxEvents: 100, MaxBytes: 1 << 20})
	for i := int64(0); i < 10; i++ {
		r.Append([]byte("x"), i*100)
	}
	got := r.Recent(ring.Query{Since: 500})
	if len(got) != 5 {
		t.Fatalf("got %d events since 500, want 5", len(got))
	}
}

func TestRing_RecentLimit(t *testing.T) {
	r := ring.New(ring.Options{MaxEvents: 100, MaxBytes: 1 << 20})
	for i := 0; i < 20; i++ {
		r.Append([]byte("x"), int64(i))
	}
	got := r.Recent(ring.Query{Limit: 5})
	if len(got) != 5 {
		t.Fatalf("got %d, want 5", len(got))
	}
	// Returns the *most recent* 5.
	if got[0].Timestamp != 15 || got[4].Timestamp != 19 {
		t.Fatalf("expected [15..19], got [%d..%d]", got[0].Timestamp, got[4].Timestamp)
	}
}
```

- [ ] **Step 2: Run tests — fail.**

- [ ] **Step 3: Implement the ring.**

```go
package ring

import "sync"

type Event struct {
	Bytes     []byte
	Timestamp int64 // unix ms
}

type Options struct {
	MaxEvents int // default 5000
	MaxBytes  int // default 64 << 20 = 64 MiB
}

type Query struct {
	Limit int   // 0 means no limit
	Since int64 // 0 means no time filter; otherwise return events with Timestamp > Since
}

type Ring struct {
	opts       Options
	mu         sync.Mutex
	events     []Event // append-only with periodic eviction; not a circular buffer (keeps code simple)
	totalBytes int
}

func New(opts Options) *Ring {
	if opts.MaxEvents <= 0 {
		opts.MaxEvents = 5000
	}
	if opts.MaxBytes <= 0 {
		opts.MaxBytes = 64 << 20
	}
	return &Ring{opts: opts}
}

func (r *Ring) Append(payload []byte, ts int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	// Copy the bytes — caller may reuse the slice.
	cp := make([]byte, len(payload))
	copy(cp, payload)
	r.events = append(r.events, Event{Bytes: cp, Timestamp: ts})
	r.totalBytes += len(cp)
	r.evict()
}

func (r *Ring) evict() {
	for len(r.events) > r.opts.MaxEvents {
		r.totalBytes -= len(r.events[0].Bytes)
		r.events = r.events[1:]
	}
	for r.totalBytes > r.opts.MaxBytes && len(r.events) > 0 {
		r.totalBytes -= len(r.events[0].Bytes)
		r.events = r.events[1:]
	}
	// Avoid unbounded backing-array growth from repeated slicing.
	if cap(r.events) > 2*r.opts.MaxEvents && len(r.events) < r.opts.MaxEvents/2 {
		copy(r.events, r.events)
		r.events = append([]Event(nil), r.events...)
	}
}

func (r *Ring) Recent(q Query) []Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	src := r.events
	if q.Since > 0 {
		i := 0
		for i < len(src) && src[i].Timestamp <= q.Since {
			i++
		}
		src = src[i:]
	}
	if q.Limit > 0 && len(src) > q.Limit {
		src = src[len(src)-q.Limit:]
	}
	out := make([]Event, len(src))
	for i, ev := range src {
		// Defensive copy so callers can't mutate the ring's storage.
		cp := make([]byte, len(ev.Bytes))
		copy(cp, ev.Bytes)
		out[i] = Event{Bytes: cp, Timestamp: ev.Timestamp}
	}
	return out
}

func (r *Ring) Stats() (events, bytes int, oldestTimestamp int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.events) == 0 {
		return 0, 0, 0
	}
	return len(r.events), r.totalBytes, r.events[0].Timestamp
}
```

- [ ] **Step 4: Verify tests pass.**

```bash
go test -race ./internal/ring/...
```

- [ ] **Step 5: Commit.**

```bash
git add internal/ring/
git commit -m "ring: bounded LRU event ring with bytes + count eviction"
```

---

### Task 6: Store — Session struct and snapshots

**Files:**
- Create: `internal/store/session.go`
- Create: `internal/store/session_test.go`

Per the design discussion, `Session` is pure queryable state (no I/O references). The `Bus` and `Ring` for each child live on the `Child` struct (Task 9), referenced from `Session` by handle.

- [ ] **Step 1: Write tests for Snapshot semantics.**

```go
package store_test

import (
	"testing"
	"time"

	"git.graveland.dev/brent/pi-controller/internal/protocol"
	"git.graveland.dev/brent/pi-controller/internal/store"
)

func TestSession_Snapshot_CopiesFields(t *testing.T) {
	s := &store.Session{
		ChildID: "c_1", Name: "x",
		Status: protocol.StatusIdle, StartedAt: time.Unix(100, 0),
	}
	snap := s.Snapshot()

	// Mutate the original; snapshot must not change.
	s.Name = "y"
	s.Status = protocol.StatusStreaming
	if snap.Name != "x" {
		t.Fatalf("snapshot Name aliased: got %q, want %q", snap.Name, "x")
	}
	if snap.Status != protocol.StatusIdle {
		t.Fatalf("snapshot Status aliased: got %v, want %v", snap.Status, protocol.StatusIdle)
	}
}

func TestSession_Snapshot_CopiesSlices(t *testing.T) {
	s := &store.Session{
		ChildID:    "c_1",
		Extensions: []string{"a", "b"},
	}
	snap := s.Snapshot()
	s.Extensions[0] = "MUTATED"
	if snap.Extensions[0] != "a" {
		t.Fatalf("slice aliased: %v", snap.Extensions)
	}
}
```

- [ ] **Step 2: Run tests — fail.**

- [ ] **Step 3: Implement the Session struct.**

```go
package store

import (
	"sync"
	"time"

	"git.graveland.dev/brent/pi-controller/internal/protocol"
)

// Session is the controller's per-child record. Pure metadata —
// I/O lives on the Child struct (internal/child).
//
// Holders must use Snapshot() to read; never expose *Session pointers
// outside this package.
type Session struct {
	mu sync.Mutex

	ChildID    string
	PID        int
	Name       string
	Cwd        string

	Provider   string
	Model      string
	Thinking   string

	SessionID   string
	SessionFile string

	Status       protocol.Status
	StartedAt    time.Time
	LastActivity time.Time
	ExitedAt     time.Time
	ExitCode     *int
	ExitSignal   string

	// Spawn configuration (subset persisted to state record).
	NoSession         bool
	SessionDir        string
	ResumeSession     string
	ForkSession       string
	Tools             []string
	NoTools           bool
	NoBuiltinTools    bool
	Extensions        []string
	NoExtensions      bool
	Skills            []string
	NoSkills          bool
	PromptTemplates   []string
	NoPromptTemplates bool
	Themes            []string
	NoThemes          bool
	NoContextFiles    bool
	SystemPrompt      string
	AppendSystemPrompt string
	Verbose           bool
	PiBinary          string
	ExtraArgs         []string

	// Counters
	ExtensionErrors int
	AutoRetries     int
	LastRetryError  string
	LastRetryFinal  string

	// Handles into the live Child. Set by store.Insert from
	// Child setup; nil for sessions in "exited" state without an
	// associated Child.
	cmdCh chan<- []byte
	done  <-chan struct{}
}

// Snapshot is a defensive copy used at every boundary.
type Snapshot struct {
	ChildID, Name, Cwd            string
	PID                           int
	Provider, Model, Thinking     string
	SessionID, SessionFile        string
	Status                        protocol.Status
	StartedAt, LastActivity       time.Time
	ExitedAt                      time.Time
	ExitCode                      *int
	ExitSignal                    string

	NoSession         bool
	SessionDir        string
	ResumeSession     string
	ForkSession       string
	Tools             []string
	NoTools, NoBuiltinTools bool
	Extensions        []string
	NoExtensions      bool
	Skills            []string
	NoSkills          bool
	PromptTemplates   []string
	NoPromptTemplates bool
	Themes            []string
	NoThemes          bool
	NoContextFiles    bool
	SystemPrompt      string
	AppendSystemPrompt string
	Verbose           bool
	PiBinary          string
	ExtraArgs         []string

	ExtensionErrors int
	AutoRetries     int
	LastRetryError  string
	LastRetryFinal  string
}

func (s *Session) Snapshot() Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()

	exitCode := s.ExitCode
	if exitCode != nil {
		v := *exitCode
		exitCode = &v
	}
	return Snapshot{
		ChildID: s.ChildID, Name: s.Name, Cwd: s.Cwd, PID: s.PID,
		Provider: s.Provider, Model: s.Model, Thinking: s.Thinking,
		SessionID: s.SessionID, SessionFile: s.SessionFile,
		Status: s.Status, StartedAt: s.StartedAt, LastActivity: s.LastActivity,
		ExitedAt: s.ExitedAt, ExitCode: exitCode, ExitSignal: s.ExitSignal,

		NoSession: s.NoSession, SessionDir: s.SessionDir,
		ResumeSession: s.ResumeSession, ForkSession: s.ForkSession,
		Tools: copyStrings(s.Tools),
		NoTools: s.NoTools, NoBuiltinTools: s.NoBuiltinTools,
		Extensions: copyStrings(s.Extensions),
		NoExtensions: s.NoExtensions,
		Skills: copyStrings(s.Skills),
		NoSkills: s.NoSkills,
		PromptTemplates: copyStrings(s.PromptTemplates),
		NoPromptTemplates: s.NoPromptTemplates,
		Themes: copyStrings(s.Themes),
		NoThemes: s.NoThemes,
		NoContextFiles: s.NoContextFiles,
		SystemPrompt: s.SystemPrompt, AppendSystemPrompt: s.AppendSystemPrompt,
		Verbose: s.Verbose, PiBinary: s.PiBinary,
		ExtraArgs: copyStrings(s.ExtraArgs),

		ExtensionErrors: s.ExtensionErrors,
		AutoRetries:     s.AutoRetries,
		LastRetryError:  s.LastRetryError,
		LastRetryFinal:  s.LastRetryFinal,
	}
}

func copyStrings(s []string) []string {
	if len(s) == 0 {
		return nil
	}
	out := make([]string, len(s))
	copy(out, s)
	return out
}
```

- [ ] **Step 4: Verify and commit.**

```bash
make test
git add internal/store/session.go internal/store/session_test.go
git commit -m "store: Session struct and Snapshot DTO"
```

---

### Task 7: Store — indexed lookups and mutations

**Files:**
- Create: `internal/store/store.go`
- Create: `internal/store/store_test.go`

Per the design discussion, indexes use `xsync.Map[K, *xsync.Map[ChildID, struct{}]]` for many-to-many. Per the ordering rule: insert primary first then index entries; delete index entries first then primary. Verify-on-read filters stale-index hits.

- [ ] **Step 1: Write tests for the lookup methods.**

```go
package store_test

import (
	"sort"
	"testing"
	"time"

	"git.graveland.dev/brent/pi-controller/internal/protocol"
	"git.graveland.dev/brent/pi-controller/internal/store"
)

func newSess(id, name, cwd string) *store.Session {
	return &store.Session{
		ChildID: id, Name: name, Cwd: cwd,
		Status: protocol.StatusIdle, StartedAt: time.Now(),
	}
}

func TestStore_InsertAndGet(t *testing.T) {
	s := store.New()
	s.Insert(newSess("c_1", "foo", "/x"))

	snap, ok := s.Get("c_1")
	if !ok {
		t.Fatal("missing after insert")
	}
	if snap.Name != "foo" || snap.Cwd != "/x" {
		t.Fatalf("got %+v", snap)
	}
}

func TestStore_FindByName_Multiple(t *testing.T) {
	s := store.New()
	s.Insert(newSess("c_1", "afk", "/a"))
	s.Insert(newSess("c_2", "afk", "/b"))
	s.Insert(newSess("c_3", "other", "/c"))

	got := s.FindByName("afk")
	if len(got) != 2 {
		t.Fatalf("got %d, want 2", len(got))
	}
	ids := []string{got[0].ChildID, got[1].ChildID}
	sort.Strings(ids)
	if ids[0] != "c_1" || ids[1] != "c_2" {
		t.Fatalf("got %v", ids)
	}
}

func TestStore_Rename_UpdatesIndex(t *testing.T) {
	s := store.New()
	s.Insert(newSess("c_1", "old", "/x"))

	if err := s.Rename("c_1", "new"); err != nil {
		t.Fatal(err)
	}

	if got := s.FindByName("old"); len(got) != 0 {
		t.Fatalf("old name still found: %v", got)
	}
	if got := s.FindByName("new"); len(got) != 1 || got[0].ChildID != "c_1" {
		t.Fatalf("new name lookup: %v", got)
	}
}

func TestStore_FindByCwd(t *testing.T) {
	s := store.New()
	s.Insert(newSess("c_1", "a", "/x"))
	s.Insert(newSess("c_2", "b", "/x"))
	s.Insert(newSess("c_3", "c", "/y"))
	if got := s.FindByCwd("/x"); len(got) != 2 {
		t.Fatalf("got %d, want 2", len(got))
	}
}

func TestStore_Delete_RemovesFromAllIndexes(t *testing.T) {
	s := store.New()
	s.Insert(newSess("c_1", "afk", "/x"))
	s.Delete("c_1")
	if _, ok := s.Get("c_1"); ok {
		t.Fatal("still in primary")
	}
	if got := s.FindByName("afk"); len(got) != 0 {
		t.Fatalf("name index leak: %v", got)
	}
	if got := s.FindByCwd("/x"); len(got) != 0 {
		t.Fatalf("cwd index leak: %v", got)
	}
}

func TestStore_VerifyOnRead_FiltersStaleIndex(t *testing.T) {
	// Direct unit test of the verify path: insert under one name,
	// mutate the session's name field through Update, then ensure
	// the old-name lookup returns nothing.
	s := store.New()
	s.Insert(newSess("c_1", "old", "/x"))
	s.Update("c_1", func(sess *store.Session) {
		sess.Name = "new"  // bypasses the index update intentionally for this test
	})
	got := s.FindByName("old")
	if len(got) != 0 {
		t.Fatalf("verify-on-read failed: %v", got)
	}
}

func TestStore_SetStatus(t *testing.T) {
	s := store.New()
	s.Insert(newSess("c_1", "a", "/x"))
	prev, ok := s.SetStatus("c_1", protocol.StatusStreaming)
	if !ok {
		t.Fatal("missing child")
	}
	if prev != protocol.StatusIdle {
		t.Fatalf("prev: %v", prev)
	}
	snap, _ := s.Get("c_1")
	if snap.Status != protocol.StatusStreaming {
		t.Fatalf("status: %v", snap.Status)
	}
}

func TestStore_ListSortedByStartedAtDesc(t *testing.T) {
	s := store.New()
	now := time.Now()
	a := newSess("c_a", "a", "/")
	a.StartedAt = now.Add(-2 * time.Hour)
	b := newSess("c_b", "b", "/")
	b.StartedAt = now.Add(-1 * time.Hour)
	s.Insert(a)
	s.Insert(b)
	got := s.List()
	if len(got) != 2 {
		t.Fatalf("got %d", len(got))
	}
	if got[0].ChildID != "c_b" || got[1].ChildID != "c_a" {
		t.Fatalf("sort wrong: %v %v", got[0].ChildID, got[1].ChildID)
	}
}
```

- [ ] **Step 2: Run tests — fail.**

- [ ] **Step 3: Implement the Store with the verified-on-read pattern.**

```go
package store

import (
	"errors"
	"sort"

	"github.com/puzpuzpuz/xsync/v4"
	"git.graveland.dev/brent/pi-controller/internal/protocol"
)

var ErrNotFound = errors.New("session not found")

type Store struct {
	sessions *xsync.Map[string, *Session]
	byName   *xsync.Map[string, *xsync.Map[string, struct{}]]
	byCwd    *xsync.Map[string, *xsync.Map[string, struct{}]]
	byStatus *xsync.Map[protocol.Status, *xsync.Map[string, struct{}]]
}

func New() *Store {
	return &Store{
		sessions: xsync.NewMap[string, *Session](),
		byName:   xsync.NewMap[string, *xsync.Map[string, struct{}]](),
		byCwd:    xsync.NewMap[string, *xsync.Map[string, struct{}]](),
		byStatus: xsync.NewMap[protocol.Status, *xsync.Map[string, struct{}]](),
	}
}

func (s *Store) Insert(sess *Session) {
	// Ordering rule for ADD: primary first, then indexes.
	s.sessions.Store(sess.ChildID, sess)
	if sess.Name != "" {
		s.addToBucket(s.byName, sess.Name, sess.ChildID)
	}
	if sess.Cwd != "" {
		s.addToBucket(s.byCwd, sess.Cwd, sess.ChildID)
	}
	s.addToBucket(s.byStatus, sess.Status, sess.ChildID)
}

func (s *Store) Delete(id string) {
	// Ordering rule for REMOVE: indexes first, then primary.
	sess, ok := s.sessions.Load(id)
	if !ok {
		return
	}
	snap := sess.Snapshot()
	if snap.Name != "" {
		s.removeFromBucket(s.byName, snap.Name, id)
	}
	if snap.Cwd != "" {
		s.removeFromBucket(s.byCwd, snap.Cwd, id)
	}
	s.removeFromBucket(s.byStatus, snap.Status, id)
	s.sessions.Delete(id)
}

func (s *Store) Get(id string) (Snapshot, bool) {
	sess, ok := s.sessions.Load(id)
	if !ok {
		return Snapshot{}, false
	}
	return sess.Snapshot(), true
}

func (s *Store) List() []Snapshot {
	var out []Snapshot
	s.sessions.Range(func(_ string, sess *Session) bool {
		out = append(out, sess.Snapshot())
		return true
	})
	sort.Slice(out, func(i, j int) bool {
		return out[i].StartedAt.After(out[j].StartedAt)
	})
	return out
}

func (s *Store) FindByName(name string) []Snapshot {
	return s.lookupBucket(s.byName, name, func(snap Snapshot) bool {
		return snap.Name == name
	})
}

func (s *Store) FindByCwd(cwd string) []Snapshot {
	return s.lookupBucket(s.byCwd, cwd, func(snap Snapshot) bool {
		return snap.Cwd == cwd
	})
}

func (s *Store) FindByStatus(status protocol.Status) []Snapshot {
	return s.lookupBucket(s.byStatus, status, func(snap Snapshot) bool {
		return snap.Status == status
	})
}

// Update applies fn under the session's lock. The caller is responsible
// for keeping index entries in sync if fn mutates indexed fields;
// prefer the dedicated Rename / SetStatus / etc. methods that handle
// the indexes correctly.
func (s *Store) Update(id string, fn func(*Session)) error {
	sess, ok := s.sessions.Load(id)
	if !ok {
		return ErrNotFound
	}
	sess.mu.Lock()
	defer sess.mu.Unlock()
	fn(sess)
	return nil
}

func (s *Store) Rename(id, newName string) error {
	sess, ok := s.sessions.Load(id)
	if !ok {
		return ErrNotFound
	}

	sess.mu.Lock()
	old := sess.Name
	sess.mu.Unlock()
	if old == newName {
		return nil
	}

	// Apply rename pivot: index-remove old, mutate primary, index-add new.
	if old != "" {
		s.removeFromBucket(s.byName, old, id)
	}
	sess.mu.Lock()
	sess.Name = newName
	sess.mu.Unlock()
	if newName != "" {
		s.addToBucket(s.byName, newName, id)
	}
	return nil
}

func (s *Store) SetStatus(id string, newStatus protocol.Status) (prev protocol.Status, ok bool) {
	sess, ok := s.sessions.Load(id)
	if !ok {
		return "", false
	}
	sess.mu.Lock()
	prev = sess.Status
	sess.mu.Unlock()
	if prev == newStatus {
		return prev, true
	}

	s.removeFromBucket(s.byStatus, prev, id)
	sess.mu.Lock()
	sess.Status = newStatus
	sess.mu.Unlock()
	s.addToBucket(s.byStatus, newStatus, id)

	return prev, true
}

// helpers

func (s *Store) addToBucket(idx *xsync.Map[string, *xsync.Map[string, struct{}]], key, id string) {
	bucket, _ := idx.LoadOrCompute(key, func() *xsync.Map[string, struct{}] {
		return xsync.NewMap[string, struct{}]()
	})
	bucket.Store(id, struct{}{})
}

func (s *Store) removeFromBucket(idx *xsync.Map[string, *xsync.Map[string, struct{}]], key, id string) {
	if bucket, ok := idx.Load(key); ok {
		bucket.Delete(id)
	}
}

func (s *Store) lookupBucket(idx *xsync.Map[string, *xsync.Map[string, struct{}]], key string, verify func(Snapshot) bool) []Snapshot {
	bucket, ok := idx.Load(key)
	if !ok {
		return nil
	}
	var out []Snapshot
	bucket.Range(func(id string, _ struct{}) bool {
		if sess, ok := s.sessions.Load(id); ok {
			snap := sess.Snapshot()
			if verify(snap) {
				out = append(out, snap)
			}
		}
		return true
	})
	return out
}
```

Note: the `byStatus` map's key type is `protocol.Status` (a string under the hood), but `lookupBucket` is typed against `string`. Define a parallel set of helpers for `protocol.Status` keys (or generic on the key type). Same shape, different signature. Add `addToStatusBucket`, `removeFromStatusBucket`, `lookupStatusBucket`. (Generic helper using `xsync.Map[K, *xsync.Map[string, struct{}]]` is cleaner; pick whichever is less awkward.)

- [ ] **Step 4: Verify tests pass.**

```bash
go test -race ./internal/store/...
```

- [ ] **Step 5: Commit.**

```bash
git add internal/store/
git commit -m "store: indexed concurrent session store with verify-on-read lookups"
```

---

### Task 8: State machine

**Files:**
- Create: `internal/child/state.go`
- Create: `internal/child/state_test.go`

Implements §10 of the spec. Pure data — no I/O. Owned by the supervise goroutine; not directly thread-safe (the caller serializes).

- [ ] **Step 1: Write tests for transitions, modal stack, and the parallel-tool counter.**

```go
package child_test

import (
	"testing"

	"git.graveland.dev/brent/pi-controller/internal/child"
	"git.graveland.dev/brent/pi-controller/internal/protocol"
)

func TestStateMachine_BasicLifecycle(t *testing.T) {
	sm := child.NewStateMachine()
	// Initial state assumed by callers: post-construction the SM sits in
	// "spawning". The supervise loop transitions to idle on first response.
	if sm.Current() != protocol.StatusSpawning {
		t.Fatalf("initial: %v", sm.Current())
	}

	// First response → idle.
	changed, prev := sm.OnFirstResponse()
	if !changed || prev != protocol.StatusSpawning || sm.Current() != protocol.StatusIdle {
		t.Fatalf("first response: changed=%v prev=%v cur=%v", changed, prev, sm.Current())
	}

	// agent_start → streaming.
	changed, prev = sm.OnPiEvent("agent_start", nil)
	if !changed || prev != protocol.StatusIdle || sm.Current() != protocol.StatusStreaming {
		t.Fatalf("agent_start: %v %v", prev, sm.Current())
	}

	// agent_end → idle.
	changed, _ = sm.OnPiEvent("agent_end", nil)
	if !changed || sm.Current() != protocol.StatusIdle {
		t.Fatalf("agent_end: %v", sm.Current())
	}
}

func TestStateMachine_ParallelTools(t *testing.T) {
	sm := child.NewStateMachine()
	sm.OnFirstResponse()
	sm.OnPiEvent("agent_start", nil)

	// Three tools start: state goes streaming→tool_running on first only.
	sm.OnPiEvent("tool_execution_start", nil)
	if sm.Current() != protocol.StatusToolRunning {
		t.Fatalf("first tool: %v", sm.Current())
	}
	sm.OnPiEvent("tool_execution_start", nil)
	sm.OnPiEvent("tool_execution_start", nil)
	if sm.Current() != protocol.StatusToolRunning {
		t.Fatalf("3rd tool started: %v", sm.Current())
	}

	// First two end: still tool_running.
	sm.OnPiEvent("tool_execution_end", nil)
	sm.OnPiEvent("tool_execution_end", nil)
	if sm.Current() != protocol.StatusToolRunning {
		t.Fatalf("2 of 3 ended: %v", sm.Current())
	}

	// Last end: back to streaming.
	changed, prev := sm.OnPiEvent("tool_execution_end", nil)
	if !changed || prev != protocol.StatusToolRunning || sm.Current() != protocol.StatusStreaming {
		t.Fatalf("all tools ended: %v→%v", prev, sm.Current())
	}
}

func TestStateMachine_ModalStack_Compaction(t *testing.T) {
	sm := child.NewStateMachine()
	sm.OnFirstResponse()
	sm.OnPiEvent("agent_start", nil)
	// streaming → compacting (push), then compaction_end → streaming (pop)
	sm.OnPiEvent("compaction_start", nil)
	if sm.Current() != protocol.StatusCompacting {
		t.Fatalf("compaction_start: %v", sm.Current())
	}
	sm.OnPiEvent("compaction_end", nil)
	if sm.Current() != protocol.StatusStreaming {
		t.Fatalf("compaction_end did not restore: %v", sm.Current())
	}
}

func TestStateMachine_DialogUI_Push_OnlyForDialogMethods(t *testing.T) {
	sm := child.NewStateMachine()
	sm.OnFirstResponse()
	sm.OnPiEvent("agent_start", nil)

	// fire-and-forget: no transition.
	sm.OnPiEvent("extension_ui_request", &child.PiUIRequestMeta{
		ID: "u1", Method: "notify",
	})
	if sm.Current() != protocol.StatusStreaming {
		t.Fatalf("notify must not block: %v", sm.Current())
	}

	// dialog: push.
	sm.OnPiEvent("extension_ui_request", &child.PiUIRequestMeta{
		ID: "u2", Method: "confirm",
	})
	if sm.Current() != protocol.StatusBlockedUI {
		t.Fatalf("confirm must block: %v", sm.Current())
	}

	// Response: pop.
	sm.OnExtensionUIResponse("u2")
	if sm.Current() != protocol.StatusStreaming {
		t.Fatalf("response did not pop: %v", sm.Current())
	}
}

func TestStateMachine_ExtensionError_Counter_NoTransition(t *testing.T) {
	sm := child.NewStateMachine()
	sm.OnFirstResponse()
	sm.OnPiEvent("agent_start", nil)
	before := sm.Current()
	sm.OnPiEvent("extension_error", nil)
	if sm.Current() != before {
		t.Fatalf("extension_error changed state: %v→%v", before, sm.Current())
	}
	if sm.Counters().ExtensionErrors != 1 {
		t.Fatalf("counter not incremented")
	}
}

func TestStateMachine_ShuttingDown(t *testing.T) {
	sm := child.NewStateMachine()
	sm.OnFirstResponse()
	sm.OnPiEvent("agent_start", nil)
	changed, prev := sm.OnShutdownStart()
	if !changed || prev != protocol.StatusStreaming || sm.Current() != protocol.StatusShuttingDown {
		t.Fatalf("shutdown start: %v→%v", prev, sm.Current())
	}
	changed, _ = sm.OnProcessExit()
	if !changed || sm.Current() != protocol.StatusExited {
		t.Fatalf("process exit: %v", sm.Current())
	}
}

func TestStateMachine_DefensivePopOnEmptyStack(t *testing.T) {
	// compaction_end with nothing on the stack must be a no-op.
	sm := child.NewStateMachine()
	sm.OnFirstResponse()
	before := sm.Current()
	sm.OnPiEvent("compaction_end", nil)
	if sm.Current() != before {
		t.Fatalf("defensive pop changed state")
	}
}
```

- [ ] **Step 2: Run tests — fail.**

- [ ] **Step 3: Implement the state machine.**

```go
package child

import (
	"git.graveland.dev/brent/pi-controller/internal/protocol"
)

// PiUIRequestMeta carries the minimum fields needed from an
// extension_ui_request event to make a dialog/non-dialog decision.
type PiUIRequestMeta struct {
	ID     string
	Method string
}

type Counters struct {
	ExtensionErrors int
	AutoRetries     int
	LastRetryError  string
	LastRetryFinal  string
}

// dialogMethods are the extension_ui methods that block the agent loop.
var dialogMethods = map[string]bool{
	"select":  true,
	"confirm": true,
	"input":   true,
	"editor":  true,
}

const pendingUICapacity = 64

type StateMachine struct {
	current     protocol.Status
	stack       []protocol.Status

	activeTools int

	counters    Counters
	pendingUI   map[string]struct{} // dialog request ids awaiting response
}

func NewStateMachine() *StateMachine {
	return &StateMachine{
		current:   protocol.StatusSpawning,
		pendingUI: make(map[string]struct{}),
	}
}

func (sm *StateMachine) Current() protocol.Status {
	return sm.current
}

func (sm *StateMachine) Counters() Counters {
	return sm.counters
}

// OnFirstResponse transitions from spawning to idle when pi's first
// response arrives.
func (sm *StateMachine) OnFirstResponse() (changed bool, prev protocol.Status) {
	if sm.current == protocol.StatusSpawning {
		prev = sm.current
		sm.current = protocol.StatusIdle
		return true, prev
	}
	return false, sm.current
}

// OnPiEvent processes a pi-RPC event. eventType is the "type" field.
// meta is non-nil only for events where the SM needs payload data
// (currently only extension_ui_request).
func (sm *StateMachine) OnPiEvent(eventType string, meta *PiUIRequestMeta) (changed bool, prev protocol.Status) {
	prev = sm.current
	switch eventType {
	case "agent_start":
		sm.transition(protocol.StatusStreaming)
	case "agent_end":
		sm.activeTools = 0
		sm.transition(protocol.StatusIdle)
	case "tool_execution_start":
		sm.activeTools++
		if sm.activeTools == 1 && sm.current == protocol.StatusStreaming {
			sm.transition(protocol.StatusToolRunning)
		}
	case "tool_execution_end":
		if sm.activeTools > 0 {
			sm.activeTools--
		}
		if sm.activeTools == 0 && sm.current == protocol.StatusToolRunning {
			sm.transition(protocol.StatusStreaming)
		}
	case "compaction_start":
		sm.push(protocol.StatusCompacting)
	case "compaction_end":
		sm.pop()
	case "extension_ui_request":
		if meta != nil && dialogMethods[meta.Method] {
			if len(sm.pendingUI) < pendingUICapacity {
				sm.pendingUI[meta.ID] = struct{}{}
			}
			sm.push(protocol.StatusBlockedUI)
		}
	case "extension_error":
		sm.counters.ExtensionErrors++
	case "auto_retry_start":
		sm.counters.AutoRetries++
	}
	return sm.current != prev, prev
}

// OnExtensionUIResponse releases blocked_ui when the matching response
// is forwarded by the controller.
func (sm *StateMachine) OnExtensionUIResponse(id string) (changed bool, prev protocol.Status) {
	if _, ok := sm.pendingUI[id]; !ok {
		return false, sm.current
	}
	delete(sm.pendingUI, id)
	if sm.current == protocol.StatusBlockedUI && len(sm.pendingUI) == 0 {
		prev = sm.current
		sm.pop()
		return sm.current != prev, prev
	}
	return false, sm.current
}

// OnAutoRetryFinalFailure records a non-success auto_retry_end.
func (sm *StateMachine) OnAutoRetryFinalFailure(finalError string) {
	sm.counters.LastRetryFinal = finalError
}

// OnShutdownStart transitions to shutting_down. Called by Child when
// it begins graceful shutdown (ctrl_kill or interception).
func (sm *StateMachine) OnShutdownStart() (changed bool, prev protocol.Status) {
	if sm.current == protocol.StatusExited || sm.current == protocol.StatusShuttingDown {
		return false, sm.current
	}
	prev = sm.current
	sm.current = protocol.StatusShuttingDown
	// Clear modal stack: shutdown overrides any push.
	sm.stack = nil
	return true, prev
}

// OnProcessExit transitions to exited. Always changes state unless
// already exited.
func (sm *StateMachine) OnProcessExit() (changed bool, prev protocol.Status) {
	if sm.current == protocol.StatusExited {
		return false, sm.current
	}
	prev = sm.current
	sm.current = protocol.StatusExited
	sm.activeTools = 0
	sm.stack = nil
	return true, prev
}

func (sm *StateMachine) transition(to protocol.Status) {
	sm.current = to
}

func (sm *StateMachine) push(to protocol.Status) {
	sm.stack = append(sm.stack, sm.current)
	sm.current = to
}

func (sm *StateMachine) pop() {
	if len(sm.stack) == 0 {
		return
	}
	sm.current = sm.stack[len(sm.stack)-1]
	sm.stack = sm.stack[:len(sm.stack)-1]
}
```

- [ ] **Step 4: Verify tests pass.**

```bash
go test -race ./internal/child/...
```

- [ ] **Step 5: Commit.**

```bash
git add internal/child/state.go internal/child/state_test.go
git commit -m "child: state machine with modal stack and parallel-tool counter"
```

---

### Task 9: Persistence — state records

**Files:**
- Create: `internal/persist/records.go`
- Create: `internal/persist/records_test.go`

§11.5: atomic-rename writes for crash safety, JSON format, one file per child at `<dir>/<childId>.json`.

- [ ] **Step 1: Write tests for atomic write, read, and scan.**

```go
package persist_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"git.graveland.dev/brent/pi-controller/internal/persist"
	"git.graveland.dev/brent/pi-controller/internal/protocol"
)

func TestRecordWriter_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	w := persist.NewRecordWriter(dir)

	rec := persist.Record{
		ChildID:      "c_1",
		Name:         "afk",
		Cwd:          "/tmp/x",
		Model:        "claude-sonnet-4",
		SessionFile:  "/tmp/x/session.jsonl",
		SpawnedAt:    time.Now().Unix(),
		LastSeenAlive: time.Now().Unix(),
		PID:          12345,
		LastStatus:   string(protocol.StatusStreaming),
	}
	if err := w.Write(rec); err != nil {
		t.Fatal(err)
	}

	got, err := persist.ReadRecord(filepath.Join(dir, "c_1.json"))
	if err != nil {
		t.Fatal(err)
	}
	if got.ChildID != "c_1" || got.Name != "afk" {
		t.Fatalf("got %+v", got)
	}
}

func TestRecordWriter_AtomicRename(t *testing.T) {
	dir := t.TempDir()
	w := persist.NewRecordWriter(dir)

	if err := w.Write(persist.Record{ChildID: "c_1", PID: 1}); err != nil {
		t.Fatal(err)
	}
	// No .tmp files left behind.
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".tmp" {
			t.Fatalf("temp file leaked: %s", e.Name())
		}
	}
}

func TestScanRecords_LoadsAndIgnoresJunk(t *testing.T) {
	dir := t.TempDir()
	w := persist.NewRecordWriter(dir)
	for i := 0; i < 3; i++ {
		w.Write(persist.Record{ChildID: "c_" + string(rune('a'+i)), PID: i})
	}
	// Write garbage that should be skipped.
	os.WriteFile(filepath.Join(dir, "garbage.json"), []byte("not json"), 0o600)
	os.WriteFile(filepath.Join(dir, "other.txt"), []byte("ignore"), 0o600)

	recs, err := persist.ScanRecords(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 3 {
		t.Fatalf("got %d records, want 3", len(recs))
	}
}

func TestDeleteRecord(t *testing.T) {
	dir := t.TempDir()
	w := persist.NewRecordWriter(dir)
	w.Write(persist.Record{ChildID: "c_1", PID: 1})
	if err := persist.DeleteRecord(dir, "c_1"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "c_1.json")); !os.IsNotExist(err) {
		t.Fatalf("file still present: %v", err)
	}
}
```

- [ ] **Step 2: Run tests — fail.**

- [ ] **Step 3: Implement record persistence.**

```go
package persist

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

// Record mirrors §11.5. Keep field names matching the spec exactly.
type Record struct {
	ChildID            string   `json:"childId"`
	Name               string   `json:"name,omitempty"`
	Cwd                string   `json:"cwd"`
	Provider           string   `json:"provider,omitempty"`
	Model              string   `json:"model,omitempty"`
	Thinking           string   `json:"thinking,omitempty"`
	APIKey             *string  `json:"apiKey"` // always null on disk
	SessionFile        string   `json:"sessionFile,omitempty"`
	SessionID          string   `json:"sessionId,omitempty"`
	SessionDir         string   `json:"sessionDir,omitempty"`
	NoSession          bool     `json:"noSession,omitempty"`
	Tools              []string `json:"tools,omitempty"`
	NoTools            bool     `json:"noTools,omitempty"`
	NoBuiltinTools     bool     `json:"noBuiltinTools,omitempty"`
	Extensions         []string `json:"extensions,omitempty"`
	NoExtensions       bool     `json:"noExtensions,omitempty"`
	Skills             []string `json:"skills,omitempty"`
	NoSkills           bool     `json:"noSkills,omitempty"`
	PromptTemplates    []string `json:"promptTemplates,omitempty"`
	NoPromptTemplates  bool     `json:"noPromptTemplates,omitempty"`
	Themes             []string `json:"themes,omitempty"`
	NoThemes           bool     `json:"noThemes,omitempty"`
	NoContextFiles     bool     `json:"noContextFiles,omitempty"`
	SystemPrompt       string   `json:"systemPrompt,omitempty"`
	AppendSystemPrompt string   `json:"appendSystemPrompt,omitempty"`
	Verbose            bool     `json:"verbose,omitempty"`
	PiBinary           string   `json:"piBinary,omitempty"`
	ExtraArgs          []string `json:"extraArgs,omitempty"`

	SpawnedAt     int64  `json:"spawnedAt"`
	LastSeenAlive int64  `json:"lastSeenAlive"`
	PID           int    `json:"pid"`
	LastStatus    string `json:"lastStatus"`
}

type RecordWriter struct {
	dir string
}

func NewRecordWriter(dir string) *RecordWriter {
	return &RecordWriter{dir: dir}
}

func (w *RecordWriter) Write(rec Record) error {
	if rec.ChildID == "" {
		return fmt.Errorf("record has empty childId")
	}
	// apiKey must never reach disk.
	rec.APIKey = nil

	if err := os.MkdirAll(w.dir, 0o700); err != nil {
		return err
	}

	final := filepath.Join(w.dir, rec.ChildID+".json")
	tmp, err := os.CreateTemp(w.dir, rec.ChildID+".*.tmp")
	if err != nil {
		return err
	}
	cleanup := tmp.Name()
	defer func() {
		if cleanup != "" {
			os.Remove(cleanup)
		}
	}()

	enc := json.NewEncoder(tmp)
	enc.SetIndent("", "  ")
	if err := enc.Encode(rec); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), final); err != nil {
		return err
	}
	cleanup = ""
	return nil
}

func ReadRecord(path string) (Record, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return Record{}, err
	}
	var rec Record
	if err := json.Unmarshal(b, &rec); err != nil {
		return Record{}, err
	}
	return rec, nil
}

func ScanRecords(dir string) ([]Record, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []Record
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".json") {
			continue
		}
		// Skip stray temp files left from crashed renames.
		if strings.Contains(name, ".tmp") {
			continue
		}
		rec, err := ReadRecord(filepath.Join(dir, name))
		if err != nil {
			slog.Warn("scan: ignoring malformed record",
				"file", name, "error", err)
			continue
		}
		out = append(out, rec)
	}
	return out, nil
}

func DeleteRecord(dir, childID string) error {
	err := os.Remove(filepath.Join(dir, childID+".json"))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}
```

Note: add `import "errors"` if Go's import organizer doesn't add it.

- [ ] **Step 4: Verify tests pass.**

```bash
go test -race ./internal/persist/...
```

- [ ] **Step 5: Commit.**

```bash
git add internal/persist/records.go internal/persist/records_test.go
git commit -m "persist: atomic-rename JSON state records"
```

---

### Task 10: Persistence — log dumps

**Files:**
- Create: `internal/persist/logs.go`
- Create: `internal/persist/logs_test.go`

§11.3. Gzip-compressed JSONL dumps for the three streams (`in`, `out`, `err`) plus a `meta.json`. Single sequential write on child exit; no live streaming.

- [ ] **Step 1: Tests covering each persistence mode.**

```go
package persist_test

import (
	"compress/gzip"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"git.graveland.dev/brent/pi-controller/internal/persist"
)

func TestLogDump_AlwaysMode_WritesAllStreams(t *testing.T) {
	dir := t.TempDir()
	d := persist.NewLogDumper(dir, persist.ModeOnExit)

	exitInfo := persist.ExitInfo{ExitCode: 0}
	in := [][]byte{[]byte(`{"type":"prompt","message":"hi"}`)}
	out := [][]byte{[]byte(`{"type":"agent_start"}`), []byte(`{"type":"agent_end"}`)}
	err := []byte("warning: trivial\n")
	meta := persist.Meta{ChildID: "c_1", Cwd: "/x"}

	if e := d.Dump("c_1", in, out, err, meta, exitInfo); e != nil {
		t.Fatal(e)
	}

	childDir := filepath.Join(dir, "c_1")
	for _, name := range []string{"in.jsonl.gz", "out.jsonl.gz", "err.log.gz", "meta.json"} {
		if _, e := os.Stat(filepath.Join(childDir, name)); e != nil {
			t.Fatalf("missing: %s (%v)", name, e)
		}
	}

	// Check that out.jsonl.gz contains the two events in order.
	got := readGzLines(t, filepath.Join(childDir, "out.jsonl.gz"))
	if len(got) != 2 ||
		!strings.Contains(got[0], "agent_start") ||
		!strings.Contains(got[1], "agent_end") {
		t.Fatalf("out content wrong: %v", got)
	}
}

func TestLogDump_OnFailure_SkipsCleanExit(t *testing.T) {
	dir := t.TempDir()
	d := persist.NewLogDumper(dir, persist.ModeOnFailure)
	exitInfo := persist.ExitInfo{ExitCode: 0}
	d.Dump("c_1", nil, nil, nil, persist.Meta{ChildID: "c_1"}, exitInfo)
	if _, err := os.Stat(filepath.Join(dir, "c_1")); !os.IsNotExist(err) {
		t.Fatalf("dir created on clean exit in ModeOnFailure: %v", err)
	}
}

func TestLogDump_OnFailure_DumpsBadExit(t *testing.T) {
	dir := t.TempDir()
	d := persist.NewLogDumper(dir, persist.ModeOnFailure)
	exitInfo := persist.ExitInfo{ExitCode: 1}
	d.Dump("c_1", nil, [][]byte{[]byte(`{}`)}, nil,
		persist.Meta{ChildID: "c_1"}, exitInfo)
	if _, err := os.Stat(filepath.Join(dir, "c_1", "out.jsonl.gz")); err != nil {
		t.Fatalf("expected dump on bad exit: %v", err)
	}
}

func TestLogDump_NeverMode(t *testing.T) {
	dir := t.TempDir()
	d := persist.NewLogDumper(dir, persist.ModeNever)
	d.Dump("c_1", nil, [][]byte{[]byte(`{}`)}, nil,
		persist.Meta{ChildID: "c_1"}, persist.ExitInfo{ExitCode: 1})
	if _, err := os.Stat(filepath.Join(dir, "c_1")); !os.IsNotExist(err) {
		t.Fatal("ModeNever wrote to disk")
	}
}

func readGzLines(t *testing.T, path string) []string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	defer gz.Close()
	b, err := io.ReadAll(gz)
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	if len(parts) == 1 && parts[0] == "" {
		return nil
	}
	return parts
}

var _ = json.Marshal // keep import used
```

- [ ] **Step 2: Run tests — fail.**

- [ ] **Step 3: Implement.**

```go
package persist

import (
	"compress/gzip"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
)

type Mode string

const (
	ModeOnExit    Mode = "on_exit"
	ModeOnFailure Mode = "on_failure"
	ModeNever     Mode = "never"
)

type ExitInfo struct {
	ExitCode   int
	Signal     string
	LastStatus string
}

type Meta struct {
	ChildID     string   `json:"childId"`
	Name        string   `json:"name,omitempty"`
	Cwd         string   `json:"cwd"`
	Model       string   `json:"model,omitempty"`
	SessionFile string   `json:"sessionFile,omitempty"`
	SpawnedAt   int64    `json:"spawnedAt"`
	ExitedAt    int64    `json:"exitedAt"`
	ExitCode    int      `json:"exitCode"`
	ExitSignal  string   `json:"exitSignal,omitempty"`
	Argv        []string `json:"argv,omitempty"`
}

type LogDumper struct {
	dir  string
	mode Mode
}

func NewLogDumper(dir string, mode Mode) *LogDumper {
	if mode == "" {
		mode = ModeOnExit
	}
	return &LogDumper{dir: dir, mode: mode}
}

func (d *LogDumper) Dump(childID string,
	in [][]byte, out [][]byte, errBytes []byte,
	meta Meta, exit ExitInfo,
) error {
	if d.mode == ModeNever {
		return nil
	}
	if d.mode == ModeOnFailure && exit.ExitCode == 0 && exit.Signal == "" &&
		exit.LastStatus != "error" {
		return nil
	}

	childDir := filepath.Join(d.dir, childID)
	if err := os.MkdirAll(childDir, 0o700); err != nil {
		return err
	}

	if err := writeGzLines(filepath.Join(childDir, "in.jsonl.gz"), in); err != nil {
		return err
	}
	if err := writeGzLines(filepath.Join(childDir, "out.jsonl.gz"), out); err != nil {
		return err
	}
	if err := writeGzBytes(filepath.Join(childDir, "err.log.gz"), errBytes); err != nil {
		return err
	}
	return writeMeta(filepath.Join(childDir, "meta.json"), meta)
}

func writeGzLines(path string, lines [][]byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewWriterLevel(f, gzip.DefaultCompression)
	if err != nil {
		return err
	}
	for _, line := range lines {
		if _, err := gz.Write(line); err != nil {
			gz.Close()
			return err
		}
		if _, err := gz.Write([]byte{'\n'}); err != nil {
			gz.Close()
			return err
		}
	}
	return gz.Close()
}

func writeGzBytes(path string, b []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	gz, err := gzip.NewWriterLevel(f, gzip.DefaultCompression)
	if err != nil {
		return err
	}
	if len(b) > 0 {
		if _, err := gz.Write(b); err != nil {
			gz.Close()
			return err
		}
	}
	return gz.Close()
}

func writeMeta(path string, meta Meta) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	enc.SetIndent("", "  ")
	return errors.Join(enc.Encode(meta), f.Sync())
}
```

- [ ] **Step 4: Verify and commit.**

```bash
make test
git add internal/persist/logs.go internal/persist/logs_test.go
git commit -m "persist: gzipped log dumps with mode-based emission"
```

---

### Task 11: Child supervise — process lifecycle (no pi interaction yet)

**Files:**
- Create: `internal/child/child.go`
- Create: `internal/child/child_test.go`
- Create: `test/integration/fake-pi.sh` — used by tests in this task and Task 12.

This task builds the *skeleton* of the supervise goroutine: spawn a subprocess, wire three goroutines, graceful shutdown. The fake-pi script is the testing harness — a shell script that emits canned JSONL on stdout, reads commands from stdin, and respects EOF.

- [ ] **Step 1: Write the fake-pi script.**

```bash
mkdir -p test/integration
cat > test/integration/fake-pi.sh <<'EOF'
#!/usr/bin/env bash
# Minimal stand-in for `pi --mode rpc` used by controller tests.
# Behavior:
#   - On any get_state command, replies with a canned response immediately.
#   - On set_session_name, replies with success.
#   - On `__emit_event:<json>` command, echoes the JSON to stdout as an event.
#   - On `__exit:<code>`, exits with that code.
#   - On EOF, exits 0 after a brief delay (simulating shutdown handlers).
#
# Anything else: echoes a generic success response.

# Allow tests to slow the shutdown.
SHUTDOWN_DELAY="${FAKE_PI_SHUTDOWN_DELAY:-0}"

# Predefined session info
SESSION_ID="${FAKE_PI_SESSION_ID:-fake-sid-123}"
SESSION_FILE="${FAKE_PI_SESSION_FILE:-/tmp/fake/session.jsonl}"
SESSION_NAME="${FAKE_PI_SESSION_NAME:-}"
MODEL="${FAKE_PI_MODEL:-fake/model-1}"

while IFS= read -r line; do
  case "$line" in
    '{"type":"get_state"'*)
      id=$(printf '%s' "$line" | sed -E 's/.*"id":"([^"]*)".*/\1/' 2>/dev/null)
      [ "$id" = "$line" ] && id=""
      printf '{"type":"response","command":"get_state","id":"%s","success":true,"data":{"sessionId":"%s","sessionFile":"%s","sessionName":"%s","model":{"id":"%s","provider":"fake"},"isStreaming":false,"messageCount":0,"thinkingLevel":"medium"}}\n' "$id" "$SESSION_ID" "$SESSION_FILE" "$SESSION_NAME" "$MODEL"
      ;;
    '{"type":"set_session_name"'*)
      name=$(printf '%s' "$line" | sed -E 's/.*"name":"([^"]*)".*/\1/')
      SESSION_NAME="$name"
      printf '{"type":"response","command":"set_session_name","success":true}\n'
      ;;
    __emit_event:*)
      json="${line#__emit_event:}"
      printf '%s\n' "$json"
      ;;
    __exit:*)
      code="${line#__exit:}"
      exit "$code"
      ;;
    *)
      printf '{"type":"response","success":true}\n'
      ;;
  esac
done

# Stdin closed; simulate shutdown delay then exit cleanly.
if [ "$SHUTDOWN_DELAY" -gt 0 ]; then
  sleep "$SHUTDOWN_DELAY"
fi
exit 0
EOF
chmod +x test/integration/fake-pi.sh
```

- [ ] **Step 2: Verify the script works manually.**

```bash
echo '{"type":"get_state","id":"x"}' | ./test/integration/fake-pi.sh
```

Expected: one JSONL line back with `"command":"get_state"`.

- [ ] **Step 3: Write tests for the Child supervise lifecycle.**

```go
package child_test

import (
	"context"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"git.graveland.dev/brent/pi-controller/internal/child"
)

func fakePiPath(t *testing.T) string {
	t.Helper()
	_, here, _, _ := runtime.Caller(0)
	repoRoot := filepath.Join(filepath.Dir(here), "..", "..")
	return filepath.Join(repoRoot, "test", "integration", "fake-pi.sh")
}

func TestChild_SpawnAndCleanShutdown(t *testing.T) {
	spec := child.SpawnSpec{
		ChildID:  "c_test",
		Cwd:      t.TempDir(),
		PiBinary: fakePiPath(t),
	}

	c, err := child.Spawn(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}

	// Wait for the supervise loop to enter the read/write loop.
	select {
	case <-c.Ready():
	case <-time.After(2 * time.Second):
		t.Fatal("Ready timed out")
	}

	// Graceful shutdown should exit cleanly without escalation.
	res, err := c.Shutdown(time.Second*5, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if res.Escalated {
		t.Fatal("clean exit should not need SIGTERM")
	}
	if res.ExitCode != 0 {
		t.Fatalf("exit code: %d", res.ExitCode)
	}
}

func TestChild_StuckProcess_Escalates(t *testing.T) {
	spec := child.SpawnSpec{
		ChildID:  "c_test",
		Cwd:      t.TempDir(),
		PiBinary: fakePiPath(t),
		Env:      []string{"FAKE_PI_SHUTDOWN_DELAY=999"},
	}

	c, err := child.Spawn(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-c.Ready():
	case <-time.After(2 * time.Second):
		t.Fatal("Ready timed out")
	}

	// Short shutdown timeout; expect escalation to SIGTERM.
	res, err := c.Shutdown(100*time.Millisecond, 500*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Escalated {
		t.Fatal("expected escalation")
	}
}

func TestChild_BinaryMissing_SpawnFails(t *testing.T) {
	spec := child.SpawnSpec{
		ChildID:  "c_test",
		Cwd:      t.TempDir(),
		PiBinary: "/this/path/does/not/exist",
	}
	_, err := child.Spawn(context.Background(), spec)
	if err == nil {
		t.Fatal("expected spawn failure")
	}
}

func TestChild_ProcessExits_ReadyStillFires(t *testing.T) {
	// If pi exits immediately we should still be observable —
	// Ready closes whether things went well or not, and Done indicates exit.
	spec := child.SpawnSpec{
		ChildID:  "c_test",
		Cwd:      t.TempDir(),
		PiBinary: exec.Command("/bin/sh", "-c", "exit 0").Path, // trivial command
		ExtraArgs: []string{"-c", "exit 0"},
	}
	c, err := child.Spawn(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-c.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("Done never closed for instantly-exiting child")
	}
}
```

- [ ] **Step 4: Run tests — fail.**

- [ ] **Step 5: Implement the Child struct with spawn + supervise loop.**

```go
package child

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"git.graveland.dev/brent/pi-controller/internal/bus"
	"git.graveland.dev/brent/pi-controller/internal/protocol"
	"git.graveland.dev/brent/pi-controller/internal/ring"
)

type SpawnSpec struct {
	ChildID   string
	Cwd       string
	PiBinary  string
	Argv      []string // full argv excluding piBinary itself
	ExtraArgs []string // for tests that use a different binary; appended after Argv
	Env       []string // additions/overrides; nil means inherit
}

type ShutdownResult struct {
	ExitCode  int
	Signal    string
	Escalated bool
	Duration  time.Duration
}

type Child struct {
	ID       string
	spec     SpawnSpec
	cmd      *exec.Cmd
	stdin    io.WriteCloser
	stdout   io.ReadCloser
	stderr   io.ReadCloser

	cmdCh    chan []byte
	ready    chan struct{}
	done     chan struct{}
	exit     ShutdownResult

	bus  *bus.Bus[[]byte]
	ring *ring.Ring

	inBuf   [][]byte      // commands sent (Task 7 will populate)
	errBuf  bytes.Buffer  // bounded later

	mu sync.Mutex // protects exit + closed flags
	closed bool
}

func Spawn(ctx context.Context, spec SpawnSpec) (*Child, error) {
	if spec.PiBinary == "" {
		return nil, errors.New("pi binary path required")
	}
	if !filepath.IsAbs(spec.Cwd) {
		return nil, fmt.Errorf("cwd must be absolute: %q", spec.Cwd)
	}
	if _, err := os.Stat(spec.Cwd); err != nil {
		return nil, fmt.Errorf("cwd: %w", err)
	}

	argv := append([]string{}, spec.Argv...)
	argv = append(argv, spec.ExtraArgs...)
	cmd := exec.CommandContext(ctx, spec.PiBinary, argv...)
	cmd.Dir = spec.Cwd
	if len(spec.Env) > 0 {
		cmd.Env = append(os.Environ(), spec.Env...)
	}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start: %w", err)
	}

	c := &Child{
		ID:     spec.ChildID,
		spec:   spec,
		cmd:    cmd,
		stdin:  stdin,
		stdout: stdout,
		stderr: stderr,
		cmdCh:  make(chan []byte, 16),
		ready:  make(chan struct{}),
		done:   make(chan struct{}),
		bus:    bus.New[[]byte](bus.Options{}),
		ring:   ring.New(ring.Options{}),
	}

	go c.supervise()
	return c, nil
}

func (c *Child) Ready() <-chan struct{} { return c.ready }
func (c *Child) Done() <-chan struct{}  { return c.done }
func (c *Child) Bus() *bus.Bus[[]byte]  { return c.bus }
func (c *Child) Ring() *ring.Ring       { return c.ring }

// Send forwards a JSONL frame to pi's stdin. Returns immediately;
// the write happens in the writer goroutine.
func (c *Child) Send(frame []byte) error {
	c.mu.Lock()
	closed := c.closed
	c.mu.Unlock()
	if closed {
		return errors.New("child shutting down")
	}
	select {
	case c.cmdCh <- frame:
		return nil
	default:
		return errors.New("backpressure")
	}
}

func (c *Child) supervise() {
	defer close(c.done)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); c.readStdout() }()
	go func() { defer wg.Done(); c.readStderr() }()

	// Signal ready once goroutines are up. The Ready channel
	// promises "supervise loop is processing"; it does NOT promise
	// pi has responded to anything. State-machine wiring (Task 12)
	// will refine this.
	close(c.ready)

	// Writer + lifecycle loop.
	for {
		select {
		case frame, ok := <-c.cmdCh:
			if !ok {
				return
			}
			c.inBuf = append(c.inBuf, append([]byte(nil), frame...))
			if _, err := c.stdin.Write(frame); err != nil {
				slog.Warn("stdin write failed", "child", c.ID, "error", err)
				goto cleanup
			}
			if _, err := c.stdin.Write([]byte{'\n'}); err != nil {
				goto cleanup
			}
		case <-c.done:
			// Process exited (readStdout closed it). Drain remaining cmds and finish.
			wg.Wait()
			return
		}
	}
cleanup:
	wg.Wait()
}

func (c *Child) readStdout() {
	r := protocol.NewFrameReader(c.stdout, 16<<20)
	for {
		line, err := r.ReadFrame()
		if err == io.EOF {
			break
		}
		if err != nil {
			slog.Warn("stdout read", "child", c.ID, "error", err)
			break
		}
		ts := time.Now().UnixMilli()
		c.ring.Append(line, ts)
		c.bus.Publish(line)
	}
	// Wait for process exit and record exit info.
	state, err := c.cmd.Process.Wait()
	c.mu.Lock()
	if err != nil {
		c.exit.ExitCode = -1
	} else {
		if state.ExitCode() >= 0 {
			c.exit.ExitCode = state.ExitCode()
		}
		if ws, ok := state.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			c.exit.Signal = ws.Signal().String()
		}
	}
	c.closed = true
	c.mu.Unlock()
}

func (c *Child) readStderr() {
	br := bufio.NewReader(c.stderr)
	buf := make([]byte, 4096)
	const maxErr = 4 << 20
	for {
		n, err := br.Read(buf)
		if n > 0 {
			if c.errBuf.Len()+n > maxErr {
				// Drop-oldest: trim front.
				trim := c.errBuf.Len() + n - maxErr
				if trim >= c.errBuf.Len() {
					c.errBuf.Reset()
				} else {
					c.errBuf.Next(trim)
				}
			}
			c.errBuf.Write(buf[:n])
		}
		if err != nil {
			return
		}
	}
}

// Shutdown closes stdin, waits, escalates SIGTERM/SIGKILL on timeouts.
func (c *Child) Shutdown(shutdownTimeout, killTimeout time.Duration) (ShutdownResult, error) {
	start := time.Now()

	c.mu.Lock()
	alreadyClosed := c.closed
	c.mu.Unlock()
	if alreadyClosed {
		c.exit.Duration = time.Since(start)
		return c.exit, nil
	}

	_ = c.stdin.Close()

	select {
	case <-c.done:
		c.exit.Duration = time.Since(start)
		return c.exit, nil
	case <-time.After(shutdownTimeout):
	}

	c.exit.Escalated = true
	_ = c.cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-c.done:
		c.exit.Duration = time.Since(start)
		return c.exit, nil
	case <-time.After(killTimeout):
	}

	_ = c.cmd.Process.Kill()
	<-c.done
	c.exit.Duration = time.Since(start)
	return c.exit, nil
}
```

Note: this version of `supervise` doesn't yet wire to a state machine or store — that's Task 12. It does establish the goroutine layout and shutdown contract.

- [ ] **Step 6: Verify tests pass.**

```bash
go test -race ./internal/child/...
```

If the fake-pi script has line-ending issues on macOS, ensure the shebang and file mode are correct. `chmod +x` on the script if it lost the bit.

- [ ] **Step 7: Commit.**

```bash
git add internal/child/child.go internal/child/child_test.go test/integration/fake-pi.sh
git commit -m "child: spawn supervise loop with stdin writer + stdout/stderr readers + graceful shutdown"
```

---

### Task 12: Child supervise — wire to state machine, store, metadata sniffing

**Files:**
- Modify: `internal/child/child.go`
- Create: `internal/child/sniff.go`
- Create: `internal/child/sniff_test.go`
- Modify: `internal/child/child_test.go`

Wire the supervise loop's stdout path to: state-machine `OnPiEvent`, metadata sniffing, store update, ring + bus publish (already there). Add the spawn kickstart (initial `get_state`).

- [ ] **Step 1: Write tests for the metadata sniffer.**

```go
package child_test

import (
	"testing"

	"git.graveland.dev/brent/pi-controller/internal/child"
)

func TestSniff_GetStateResponse(t *testing.T) {
	frame := []byte(`{"type":"response","command":"get_state","success":true,"data":{"sessionId":"sid","sessionFile":"/x/s.jsonl","sessionName":"named","model":{"id":"m","provider":"p"}}}`)
	md, ok := child.ExtractMetadata(frame)
	if !ok {
		t.Fatal("expected extraction")
	}
	if md.SessionID != "sid" || md.SessionFile != "/x/s.jsonl" ||
		md.SessionName != "named" || md.Model != "p/m" {
		t.Fatalf("got %+v", md)
	}
}

func TestSniff_SetSessionNameResponse(t *testing.T) {
	frame := []byte(`{"type":"response","command":"set_session_name","success":true,"data":{"name":"renamed"}}`)
	md, ok := child.ExtractMetadata(frame)
	if !ok || md.SessionName != "renamed" {
		t.Fatalf("got %+v ok=%v", md, ok)
	}
}

func TestSniff_NonMetadataFrame(t *testing.T) {
	frame := []byte(`{"type":"agent_start"}`)
	if _, ok := child.ExtractMetadata(frame); ok {
		t.Fatal("expected no extraction from non-response")
	}
}

func TestSniff_MalformedJson(t *testing.T) {
	frame := []byte(`{not json}`)
	if _, ok := child.ExtractMetadata(frame); ok {
		t.Fatal("expected no extraction from invalid JSON")
	}
}
```

- [ ] **Step 2: Implement the sniffer.**

```go
package child

import "encoding/json"

type SnifferMetadata struct {
	SessionID   string
	SessionFile string
	SessionName string
	Model       string // formatted as "provider/id"
}

// ExtractMetadata inspects a pi-RPC frame and returns metadata fields
// found in known response shapes. ok is false if the frame isn't a
// response we recognize or carries no relevant fields.
func ExtractMetadata(frame []byte) (md SnifferMetadata, ok bool) {
	var generic struct {
		Type    string          `json:"type"`
		Command string          `json:"command"`
		Success bool            `json:"success"`
		Data    json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(frame, &generic); err != nil {
		return md, false
	}
	if generic.Type != "response" || !generic.Success {
		return md, false
	}

	switch generic.Command {
	case "get_state":
		var d struct {
			SessionID   string          `json:"sessionId"`
			SessionFile string          `json:"sessionFile"`
			SessionName string          `json:"sessionName"`
			Model       json.RawMessage `json:"model"`
		}
		if err := json.Unmarshal(generic.Data, &d); err != nil {
			return md, false
		}
		md.SessionID = d.SessionID
		md.SessionFile = d.SessionFile
		md.SessionName = d.SessionName
		if len(d.Model) > 0 && string(d.Model) != "null" {
			var m struct {
				ID       string `json:"id"`
				Provider string `json:"provider"`
			}
			if err := json.Unmarshal(d.Model, &m); err == nil {
				if m.Provider != "" && m.ID != "" {
					md.Model = m.Provider + "/" + m.ID
				} else if m.ID != "" {
					md.Model = m.ID
				}
			}
		}
		return md, true
	case "set_session_name":
		var d struct {
			Name string `json:"name"`
		}
		_ = json.Unmarshal(generic.Data, &d)
		md.SessionName = d.Name
		return md, md.SessionName != ""
	case "set_model", "cycle_model":
		var d struct {
			Model json.RawMessage `json:"model"`
			ID    string          `json:"id"`
		}
		_ = json.Unmarshal(generic.Data, &d)
		// Some responses wrap the model in {model: {...}}; others put fields directly.
		if len(d.Model) > 0 && string(d.Model) != "null" {
			var m struct {
				ID       string `json:"id"`
				Provider string `json:"provider"`
			}
			if err := json.Unmarshal(d.Model, &m); err == nil {
				if m.Provider != "" && m.ID != "" {
					md.Model = m.Provider + "/" + m.ID
				}
			}
		}
		return md, md.Model != ""
	}
	return md, false
}
```

- [ ] **Step 3: Add tests for the sniffer's integration into the supervise loop.**

```go
func TestChild_KickstartAndMetadata(t *testing.T) {
	spec := child.SpawnSpec{
		ChildID:  "c_test",
		Cwd:      t.TempDir(),
		PiBinary: fakePiPath(t),
		Env:      []string{"FAKE_PI_SESSION_ID=test-sid", "FAKE_PI_SESSION_NAME=initial"},
	}

	c, err := child.Spawn(context.Background(), spec)
	if err != nil {
		t.Fatal(err)
	}
	// Spawning loop sends the kickstart get_state.
	select {
	case <-c.Idle():
	case <-time.After(2 * time.Second):
		t.Fatalf("did not transition to idle: %v", c.Status())
	}

	md := c.Metadata()
	if md.SessionID != "test-sid" || md.SessionName != "initial" {
		t.Fatalf("metadata: %+v", md)
	}

	_, _ = c.Shutdown(time.Second*5, time.Second)
}
```

- [ ] **Step 4: Extend Child to wire SM + metadata.**

Add to the `Child` struct (in `child.go`):

```go
sm       *StateMachine
metaMu   sync.Mutex
meta     SnifferMetadata

idle     chan struct{}
idleOnce sync.Once
```

In `Spawn`, after constructing `c`:

```go
c.sm = NewStateMachine()
c.idle = make(chan struct{})
```

Replace the inner stdout loop's body with:

```go
for {
    line, err := r.ReadFrame()
    if err == io.EOF { break }
    if err != nil { /* log; break */ }
    ts := time.Now().UnixMilli()
    c.ring.Append(line, ts)
    c.bus.Publish(line)
    c.handleFrame(line)
}
```

Add:

```go
func (c *Child) handleFrame(line []byte) {
    // Decode just the type for state-machine dispatch.
    var hdr struct {
        Type    string          `json:"type"`
        Command string          `json:"command"`
        Method  string          `json:"method,omitempty"`
        ID      string          `json:"id,omitempty"`
        Data    json.RawMessage `json:"data,omitempty"`
    }
    _ = json.Unmarshal(line, &hdr)

    // Metadata sniff regardless of state.
    if md, ok := ExtractMetadata(line); ok {
        c.metaMu.Lock()
        if md.SessionID != "" { c.meta.SessionID = md.SessionID }
        if md.SessionFile != "" { c.meta.SessionFile = md.SessionFile }
        if md.SessionName != "" { c.meta.SessionName = md.SessionName }
        if md.Model != "" { c.meta.Model = md.Model }
        c.metaMu.Unlock()

        // First successful response transitions out of spawning.
        if hdr.Type == "response" && hdr.Command == "get_state" {
            c.sm.OnFirstResponse()
            c.idleOnce.Do(func() { close(c.idle) })
        }
    }

    // State machine for pi events.
    if hdr.Type != "" && hdr.Type != "response" {
        var meta *PiUIRequestMeta
        if hdr.Type == "extension_ui_request" {
            meta = &PiUIRequestMeta{ID: hdr.ID, Method: hdr.Method}
        }
        c.sm.OnPiEvent(hdr.Type, meta)
    }
}

func (c *Child) Idle() <-chan struct{} { return c.idle }
func (c *Child) Status() protocol.Status { return c.sm.Current() }
func (c *Child) Metadata() SnifferMetadata {
    c.metaMu.Lock(); defer c.metaMu.Unlock()
    return c.meta
}
```

And kick off the initial `get_state` immediately after `Ready` closes. Modify the writer-loop's first iteration to send:

```go
// Right after close(c.ready), kick pi.
c.cmdCh <- []byte(`{"type":"get_state","id":"__bootstrap__"}`)
```

Be careful with channel semantics — push, not synchronous.

- [ ] **Step 5: Verify tests pass (existing + new).**

```bash
go test -race ./internal/child/...
```

- [ ] **Step 6: Commit.**

```bash
git add internal/child/
git commit -m "child: wire state machine + metadata sniffing + spawn kickstart"
```

---

### Task 13: Interception (new_session, switch_session)

**Files:**
- Create: `internal/intercept/intercept.go`
- Create: `internal/intercept/intercept_test.go`

§5.1. The interceptor: decode a `ctrl_send.frame`, if it's `new_session` or `switch_session`, perform graceful shutdown + respawn, synthesize the pi response.

This task is harder to test in isolation than the others because it touches Child lifecycle. Focus the tests on the decode + decision logic; integration coverage comes in Task 17.

- [ ] **Step 1: Write tests for the frame inspection.**

```go
package intercept_test

import (
	"testing"

	"git.graveland.dev/brent/pi-controller/internal/intercept"
)

func TestInspect_NewSession(t *testing.T) {
	frame := []byte(`{"type":"new_session","id":"x"}`)
	got, ok := intercept.Inspect(frame)
	if !ok {
		t.Fatal("expected intercept")
	}
	if got.Type != "new_session" || got.PiRequestID != "x" {
		t.Fatalf("got %+v", got)
	}
}

func TestInspect_SwitchSession(t *testing.T) {
	frame := []byte(`{"type":"switch_session","id":"y","sessionPath":"/path"}`)
	got, ok := intercept.Inspect(frame)
	if !ok || got.Type != "switch_session" || got.SessionPath != "/path" {
		t.Fatalf("got %+v ok=%v", got, ok)
	}
}

func TestInspect_PassThrough(t *testing.T) {
	for _, f := range []string{
		`{"type":"prompt","message":"hi"}`,
		`{"type":"fork","entryId":"x"}`,
		`{"type":"clone"}`,
		`{"not":"json"}`,
		``,
	} {
		_, ok := intercept.Inspect([]byte(f))
		if ok {
			t.Fatalf("expected no intercept for %q", f)
		}
	}
}

func TestSynthesizeResponse_Shape(t *testing.T) {
	got := intercept.SynthesizeResponse("new_session", "req-1")
	var parsed struct {
		Type    string `json:"type"`
		Command string `json:"command"`
		ID      string `json:"id"`
		Success bool   `json:"success"`
		Data    struct {
			Cancelled bool `json:"cancelled"`
		} `json:"data"`
	}
	if err := json.Unmarshal(got, &parsed); err != nil {
		t.Fatal(err)
	}
	if parsed.Type != "response" || parsed.Command != "new_session" ||
		parsed.ID != "req-1" || !parsed.Success || parsed.Data.Cancelled {
		t.Fatalf("parsed: %+v\nraw: %s", parsed, got)
	}
}
```

(Add `import "encoding/json"` to the test file.)

- [ ] **Step 2: Implement.**

```go
package intercept

import (
	"encoding/json"
	"fmt"
)

type InterceptType string

const (
	InterceptNewSession    InterceptType = "new_session"
	InterceptSwitchSession InterceptType = "switch_session"
)

type Decision struct {
	Type        InterceptType
	PiRequestID string
	SessionPath string // for switch_session only
}

func Inspect(frame []byte) (Decision, bool) {
	if len(frame) == 0 {
		return Decision{}, false
	}
	var hdr struct {
		Type        string `json:"type"`
		ID          string `json:"id"`
		SessionPath string `json:"sessionPath"`
	}
	if err := json.Unmarshal(frame, &hdr); err != nil {
		return Decision{}, false
	}
	switch hdr.Type {
	case "new_session":
		return Decision{
			Type:        InterceptNewSession,
			PiRequestID: hdr.ID,
		}, true
	case "switch_session":
		return Decision{
			Type:        InterceptSwitchSession,
			PiRequestID: hdr.ID,
			SessionPath: hdr.SessionPath,
		}, true
	}
	return Decision{}, false
}

func SynthesizeResponse(command, piRequestID string) []byte {
	type data struct {
		Cancelled bool `json:"cancelled"`
	}
	type response struct {
		Type    string `json:"type"`
		Command string `json:"command"`
		ID      string `json:"id,omitempty"`
		Success bool   `json:"success"`
		Data    data   `json:"data"`
	}
	b, err := json.Marshal(response{
		Type:    "response",
		Command: command,
		ID:      piRequestID,
		Success: true,
		Data:    data{Cancelled: false},
	})
	if err != nil {
		// json.Marshal of these fixed types cannot realistically fail.
		panic(fmt.Sprintf("synth response: %v", err))
	}
	return b
}
```

- [ ] **Step 3: Verify and commit.**

```bash
make test
git add internal/intercept/
git commit -m "intercept: decode new_session/switch_session frames and synthesize responses"
```

---

### Task 14: UDS server — listen, accept, framing

**Files:**
- Create: `internal/server/server.go`
- Create: `internal/server/server_test.go`

§2.1, §3. Connection per goroutine, JSONL framing, simple dispatch placeholder. Real `ctrl_*` dispatch comes in Task 15.

- [ ] **Step 1: Write tests for connection accept + echo.**

```go
package server_test

import (
	"bufio"
	"net"
	"path/filepath"
	"testing"
	"time"

	"git.graveland.dev/brent/pi-controller/internal/server"
)

func TestServer_AcceptsAndEchoes(t *testing.T) {
	dir := t.TempDir()
	sockPath := filepath.Join(dir, "test.sock")

	handler := func(frame []byte) []byte {
		return append([]byte("echo:"), frame...)
	}

	srv, err := server.Listen(sockPath, handler)
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	conn, err := net.Dial("unix", sockPath)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(2 * time.Second))

	if _, err := conn.Write([]byte(`{"type":"ping"}` + "\n")); err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(conn)
	got, err := br.ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if got != `echo:{"type":"ping"}`+"\n" {
		t.Fatalf("got %q", got)
	}
}

func TestServer_StaleSocketUnlinked(t *testing.T) {
	dir := t.TempDir()
	sockPath := filepath.Join(dir, "test.sock")

	// First listener.
	srv1, err := server.Listen(sockPath, func([]byte) []byte { return nil })
	if err != nil { t.Fatal(err) }
	srv1.Close() // does not unlink (process is presumed dead in real scenario)

	// Place a stray file at the path to simulate a leftover socket.
	// (srv1.Close should have unlinked, but make sure the second listener
	// can recover even if not.)
	if err := os.WriteFile(sockPath, nil, 0o600); err != nil {
		// Ignore if it doesn't exist; not the test's focus.
	}

	srv2, err := server.Listen(sockPath, func([]byte) []byte { return nil })
	if err != nil {
		t.Fatalf("recover from stale: %v", err)
	}
	srv2.Close()
}
```

(Add `import "os"` to the test file.)

- [ ] **Step 2: Implement.**

```go
package server

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"sync"

	"git.graveland.dev/brent/pi-controller/internal/protocol"
)

type FrameHandler func(frame []byte) []byte

type Server struct {
	ln       net.Listener
	handler  FrameHandler
	wg       sync.WaitGroup
	cancel   context.CancelFunc
	ctx      context.Context
}

func Listen(path string, handler FrameHandler) (*Server, error) {
	// Probe for a stale socket and try to unlink if no one's listening.
	if _, err := os.Stat(path); err == nil {
		if dialErr := tryDial(path); dialErr != nil {
			_ = os.Remove(path)
		} else {
			return nil, errors.New("socket in use by a live process")
		}
	}

	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		ln.Close()
		return nil, err
	}

	ctx, cancel := context.WithCancel(context.Background())
	s := &Server{ln: ln, handler: handler, ctx: ctx, cancel: cancel}
	go s.acceptLoop()
	return s, nil
}

func tryDial(path string) error {
	conn, err := net.Dial("unix", path)
	if err != nil {
		return err
	}
	conn.Close()
	return nil
}

func (s *Server) acceptLoop() {
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			if s.ctx.Err() != nil {
				return
			}
			slog.Warn("accept", "error", err)
			continue
		}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.handleConn(conn)
		}()
	}
}

func (s *Server) handleConn(conn net.Conn) {
	defer conn.Close()
	r := protocol.NewFrameReader(conn, 16<<20)
	for {
		frame, err := r.ReadFrame()
		if err == io.EOF {
			return
		}
		if err != nil {
			slog.Warn("read frame", "error", err)
			return
		}
		resp := s.handler(frame)
		if len(resp) > 0 {
			if err := protocol.WriteFrame(conn, resp); err != nil {
				slog.Warn("write frame", "error", err)
				return
			}
		}
	}
}

func (s *Server) Close() error {
	s.cancel()
	err := s.ln.Close()
	s.wg.Wait()
	return err
}
```

- [ ] **Step 3: Verify and commit.**

```bash
go test -race ./internal/server/...
git add internal/server/
git commit -m "server: UDS listener with JSONL framing and connection-per-goroutine"
```

---

### Task 15: Dispatch — wire ctrl_* commands

**Files:**
- Create: `internal/server/dispatch.go`
- Create: `internal/server/dispatch_test.go`
- Modify: `internal/server/server.go` (if needed for shared types)

This is the meat: route incoming `ctrl_*` frames to handler functions that talk to the Store + Children. Per-connection state tracks subscriptions.

The task is large; subdivide into substeps by handler family. Keep dispatch isolated from the actual Store/Child machinery using a `Controller` interface that the test implements with a fake.

- [ ] **Step 1: Define the Controller interface that dispatch depends on.**

```go
// internal/server/dispatch.go

package server

import (
	"context"
	"encoding/json"

	"git.graveland.dev/brent/pi-controller/internal/store"
)

// Controller is the surface dispatch needs from the rest of the daemon.
// Real implementation in cmd/pi-controller; tests use a fake.
type Controller interface {
	List(filter ListFilter) []store.Snapshot
	Get(childID string) (store.Snapshot, bool)
	Spawn(ctx context.Context, spec SpawnInput) (SpawnResult, error)
	Resume(ctx context.Context, childID string, apiKey string) (SpawnResult, error)
	Kill(ctx context.Context, childID string, shutdownTimeoutMs, killTimeoutMs int) (KillResult, error)
	Forget(childID string) error
	ForgetAllExited(olderThanMs int64) (int, error)
	Send(childID string, frame json.RawMessage) error
	GetRecent(childID string, q RecentQuery) (RecentResult, error)
	Search(q SearchQuery) SearchResult
	Status() ControllerStatus

	// Subscribe registers the per-connection subscription state.
	Subscribe(childID string, conn ConnSubscriber, filter SubscribeFilter) error
	Unsubscribe(childID string, conn ConnSubscriber) error
	GlobalSubscribe(conn ConnSubscriber) error
	GlobalUnsubscribe(conn ConnSubscriber) error
}

// ConnSubscriber is implemented by the per-connection handler;
// the controller calls Deliver to push events to the connection.
type ConnSubscriber interface {
	Deliver(frame []byte)
}

// Per-call result/input types mirror the protocol response shapes.
// Define each here.
type ListFilter struct {
	Status        string
	Name          string
	NameContains  string
	CwdContains   string
	Since         int64
}
type SpawnInput struct {
	Name string
	Cwd  string
	// ... full set from protocol.SpawnRequest
}
type SpawnResult struct {
	ChildID     string
	SessionID   string
	SessionFile string
	Model       string
	Stalled     bool
}
type KillResult struct {
	ExitCode  int
	Signal    string
	DurationMs int64
	Escalated bool
}
type RecentQuery struct {
	Limit   int
	Since   int64
	Include []string
	Exclude []string
}
type RecentResult struct {
	Events           []json.RawMessage
	TotalInBuffer    int
	OldestTimestamp  int64
	TruncatedByLimit bool
}
type SearchQuery struct {
	Query         string
	Regex         bool
	Limit         int
	Context       int
	SessionFilter ListFilter
}
type SearchResult struct {
	Hits      []SearchHit
	TotalHits int
	Scanned   int
	ElapsedMs int64
}
type SearchHit struct {
	ChildID, SessionFile, SessionID, SessionName string
	EntryID, Role                                 string
	Timestamp                                     int64
	Snippet                                       string
	MatchStart, MatchEnd                          int
}
type SubscribeFilter struct {
	Profile string
	Include []string
	Exclude []string
}
type ControllerStatus struct {
	Version      string
	StartedAt    int64
	LiveChildren int
	ExitedChildren int
	MemoryBytes  int64
	Socket       string
	LogsDir      string
}
```

- [ ] **Step 2: Implement Dispatch, returning the controller through a constructor.**

```go
func NewDispatch(c Controller) func(frame []byte) []byte {
    d := &dispatcher{c: c}
    return d.handle
}

type dispatcher struct{ c Controller }

func (d *dispatcher) handle(frame []byte) []byte {
    var hdr struct {
        Type string `json:"type"`
        ID   string `json:"id"`
    }
    if err := json.Unmarshal(frame, &hdr); err != nil {
        return errResp("", hdr.ID, "invalid_args", "malformed JSON")
    }
    switch hdr.Type {
    case "ctrl_list":   return d.list(frame, hdr.ID)
    case "ctrl_get":    return d.get(frame, hdr.ID)
    case "ctrl_spawn":  return d.spawn(frame, hdr.ID)
    // ... one case per ctrl_* command
    default:
        return errResp(hdr.Type, hdr.ID, "invalid_args", "unknown command")
    }
}
```

Implement each case. The longer ones (spawn, kill, send) get their own methods. Each:
- Unmarshals the typed request.
- Validates fields (return `invalid_args` early).
- Calls the Controller interface method.
- Marshals a typed response and wraps in `ctrl_response`.

Tests for each handler use a fake Controller that returns canned data; the test asserts the wire-level response shape matches the spec.

This is the longest single task. Plan ~15-30 substeps total, one per handler. Cross-reference §6.X for each.

- [ ] **Step 3: Tests for the read-only handlers first** (`ctrl_list`, `ctrl_get`, `ctrl_status`, `ctrl_get_recent`, `ctrl_search`).

Each test sets up a fake Controller, sends a frame, asserts the response shape.

- [ ] **Step 4: Tests for the lifecycle handlers** (`ctrl_spawn`, `ctrl_resume`, `ctrl_kill`, `ctrl_forget`, `ctrl_forget_all_exited`).

- [ ] **Step 5: Tests for streaming/subscription handlers** (`ctrl_subscribe`, `ctrl_unsubscribe`, `ctrl_send`, `ctrl_global_*`).

These need a per-connection state object that dispatch threads through; not just stateless function. Refactor the dispatcher to receive a `*Connection` and route subscribe/unsubscribe through it.

- [ ] **Step 6: Verify each handler passes its tests.**

- [ ] **Step 7: Commit.**

```bash
git add internal/server/dispatch.go internal/server/dispatch_test.go
git commit -m "server: ctrl_* dispatch with typed handlers and fake-controller tests"
```

---

### Task 16: Controller glue and main

**Files:**
- Create: `cmd/pi-controller/main.go`
- Create: `cmd/pi-controller/controller.go` (or split as needed)

The composition root: wire Store, the supervise tracker for live Children, the dispatcher, the server, signal handling, log setup, the state-record scanner that runs at startup.

- [ ] **Step 1: Write a placeholder integration test that boots the daemon, connects, sends `ctrl_status`, expects a response.**

```go
// test/integration/integration_test.go
// (Skeleton only — fully fleshed in Task 17.)

package integration_test

import (
    "net"
    "os/exec"
    "path/filepath"
    "testing"
    "time"
)

func TestDaemon_BootsAndAnswersStatus(t *testing.T) {
    // build binary, launch, dial, send ctrl_status, validate response, kill
}
```

- [ ] **Step 2: Implement `cmd/pi-controller/main.go`.**

- Parse env / flags for socket path, persistence mode, ring sizes, dirs.
- Set up `slog` with JSON handler to stderr.
- `os.MkdirAll` for `~/.pi/run/{state,logs}` with 0700.
- Construct: Store, RecordWriter, LogDumper, ChildManager (a new type that owns the per-childId map of `*Child` and the spawn/resume/kill plumbing), Controller (implements the dispatch interface), server.Listen.
- `signal.Notify` on SIGINT/SIGTERM/SIGHUP → graceful shutdown.
- On startup: `persist.ScanRecords(stateDir)`. For each, attempt `kill(pid, 0)`. If alive, log + SIGTERM. Either way, populate the Store with status `exited`.

Implementing the `Controller` interface is the bulk of this task. Each method bridges the dispatch layer to the live machinery:

```go
type Controller struct {
    store      *store.Store
    children   *ChildManager     // new type, owns live Children
    records    *persist.RecordWriter
    dumper     *persist.LogDumper
    config     Config
    startedAt  time.Time

    globalSubsMu sync.Mutex
    globalSubs   map[server.ConnSubscriber]struct{}
}
```

Each method (`Spawn`, `Resume`, etc.) coordinates: update store, manage Child, persist record, emit events to subscribers. This is non-trivial but straightforward once each piece is in place.

- [ ] **Step 3: Verify the daemon builds.**

```bash
make build
ls -la bin/pi-controller
```

- [ ] **Step 4: Smoke-test by hand.**

```bash
./bin/pi-controller &
echo '{"type":"ctrl_status","id":"1"}' | nc -U ~/.pi/run/controller.sock
kill %1
```

Expected: one `ctrl_response` with the controller status.

- [ ] **Step 5: Commit.**

```bash
git add cmd/pi-controller/
git commit -m "cmd/pi-controller: main + Controller composition root"
```

---

### Task 17: Integration tests

**Files:**
- Create: `test/integration/integration_test.go`
- Modify: `test/integration/fake-pi.sh` (extend with new behaviors as tests require)

End-to-end: spin up the daemon, drive it through the UDS socket using the fake-pi binary as the child. Cover the major flows.

- [ ] **Step 1: Helper for boot-daemon-with-fake-pi.**

```go
type daemonHarness struct {
    socket string
    proc   *exec.Cmd
    conn   net.Conn
}

func startDaemon(t *testing.T) *daemonHarness { /* ... */ }
func (h *daemonHarness) Stop() { /* ... */ }
func (h *daemonHarness) Send(frame []byte) []byte { /* JSONL request, read response */ }
```

- [ ] **Step 2: Test: spawn → send prompt → see events → kill → confirm exited → forget.**

- [ ] **Step 3: Test: spawn → subscribe → cause some pi events → verify subscriber sees them.**

- [ ] **Step 4: Test: spawn → kill → resume (verifies state record + respawn).**

- [ ] **Step 5: Test: interception path — spawn → `ctrl_send {type: new_session}` → verify graceful shutdown + respawn + synthesized response → verify same childId.**

- [ ] **Step 6: Test: profile filtering — subscribe with `coarse`, verify firehose events don't arrive, lifecycle events do.**

- [ ] **Step 7: Test: get_recent — push events, query subset, verify filter.**

- [ ] **Step 8: Verify all integration tests pass.**

```bash
make test
```

- [ ] **Step 9: Commit.**

```bash
git add test/integration/
git commit -m "test: end-to-end integration tests against fake-pi"
```

---

## Verification before declaring done

- [ ] **All tests pass with `-race`.**

```bash
make test-race
```

- [ ] **`go vet ./...` is clean.**

```bash
make vet
```

- [ ] **The binary builds.**

```bash
make build
```

- [ ] **Manual end-to-end smoke against real pi.**

```bash
./bin/pi-controller &
echo '{"type":"ctrl_spawn","id":"1","cwd":"'"$HOME"'/some-real-project","name":"smoke"}' | nc -U ~/.pi/run/controller.sock
# inspect ~/.pi/run/state/, ~/.pi/run/logs/, verify Child exists
echo '{"type":"ctrl_list","id":"2"}' | nc -U ~/.pi/run/controller.sock
echo '{"type":"ctrl_kill","id":"3","childId":"<from-list>"}' | nc -U ~/.pi/run/controller.sock
kill %1
```

- [ ] **Commit the final state.**

```bash
git log --oneline
```

Expected: ~17 commits, one per major task, with clean conventional-commit-style messages.

---

## Follow-up plan

The `pi-ctl` CLI is **plan B** — a separate document. Its skeleton:

- `cmd/pi-ctl/main.go` + Cobra root
- `internal/client/` — shared client lib (connection, framing, request/response correlation)
- One file per subcommand (`cmd_list.go`, `cmd_spawn.go`, ...)
- Tab completion via Cobra
- `pi-ctl tail` rendering (markdown + tool-call summaries)
- Symbol+name resolution (`pi-ctl tail afk` → fuzzy childId)

Defer to a fresh plan once the daemon is solid.

---

## Self-review notes

Items deliberately *not* covered by this plan and worth tracking separately:

- **TCP transport with shared-secret auth (§2.2)** — UDS only for v1.
- **Auto-resume on startup (§14)** — deferred; coordinator-coordinator concern.
- **Background grace-window sweeper** — needed but trivial; add as a small Task 18 if not folded into Task 16.
- **`ctrl_search` cancellation on disconnect (§6.15)** — desirable; needs a context plumbed from the connection into the Controller call.
- **Pi binary path discovery in `ctrl_spawn`** — §6.3.1 specifies the order; verify the Spawn handler implements all three fallback steps.
- **Reserved env vars `PI_CONTROLLER_CHILD_ID` / `PI_CONTROLLER_SOCKET`** (§13) — make sure the spawn path injects these.
- **`set_session_name` after the kickstart `get_state`** (§6.3.3 step 10) — wire in Task 16's Spawn implementation.
- **`shutting_down` rejection of `ctrl_send`** (§10.1) — verify Task 15's `ctrl_send` handler checks status before pushing to the channel.
