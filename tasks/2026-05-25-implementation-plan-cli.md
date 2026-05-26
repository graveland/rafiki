# pi-ctl CLI — implementation plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build `pi-ctl`, a Go + Cobra command-line client for the pi-controller daemon. It speaks the JSONL protocol over the daemon's UDS socket and exposes 11 subcommands that map 1:1 to controller verbs (plus a few quality-of-life wrappers).

**Architecture:** Pi-ctl lives in the same monorepo as the daemon (`cmd/pi-ctl/`, `internal/client/`). It depends on `internal/protocol` for wire types and `internal/client` (new) for the shared connection + request/response correlation + identifier resolution. Each subcommand is a thin Cobra command that constructs a typed protocol request, sends it via the client lib, and formats the response. The `tail` subcommand additionally consumes the streaming event channel and renders human-readable per-event output.

**Tech Stack:** Go (>= 1.24, same as the daemon), [`spf13/cobra`](https://github.com/spf13/cobra) for the CLI, `golang.org/x/term` for TTY detection. Standard library otherwise. No third-party output libraries; render tables and color via small in-package helpers.

**Spec:** `tasks/pi-controller-protocol.md` defines the wire protocol (§6 commands, §7 events, §8 errors). §12 describes the pi-ctl CLI surface specifically.

---

## Repo layout (additions to existing pi-controller monorepo)

```
~/home/pi-controller/
├── cmd/pi-ctl/                    # CLI binary
│   ├── main.go                    # root cobra command, global flags
│   ├── cmd_list.go                # 12 cmd_<name>.go files, one per subcommand
│   ├── cmd_get.go
│   ├── cmd_status.go
│   ├── cmd_spawn.go
│   ├── cmd_resume.go
│   ├── cmd_kill.go
│   ├── cmd_forget.go
│   ├── cmd_send.go
│   ├── cmd_recent.go
│   ├── cmd_tail.go
│   ├── cmd_search.go
│   ├── cmd_logs.go
│   ├── output.go                  # JSON/table/color helpers
│   ├── active.go                  # ~/.pi/run/active read/write
│   ├── render_tail.go             # event-stream renderer for tail subcommand
│   └── main_test.go               # one or two smoke tests (most coverage is integration)
├── internal/client/               # shared lib (reserved by daemon plan)
│   ├── client.go                  # Dial, Request, Subscribe
│   ├── client_test.go
│   ├── resolve.go                 # childId/name resolution
│   └── resolve_test.go
└── test/integration/              # add pi-ctl integration tests
    └── cli_integration_test.go    # boots daemon + runs CLI subprocess for end-to-end
```

`internal/client` is currently empty (placeholder from the daemon plan); this is where it gets populated.

The daemon's `Makefile` gets a new build target: `pi-ctl` binary alongside `pi-controller`.

---

## Conventions

Same as the daemon plan:
- TDD per task: write failing test → implement → verify green → commit.
- Small commits, one per task minimum.
- `log/slog` for logging if any (CLI itself shouldn't be chatty; errors to stderr, results to stdout).
- No global state outside `main`; pass dependencies via Cobra context or explicit args.
- macOS+Linux clean; no Linux-only syscalls.

CLI-specific conventions:
- Exit codes: 0 success, 1 user error (bad args, ambiguous identifier), 2 system error (can't connect, daemon error). `cobra.Command.RunE` returning an error → exit 1 by default; explicit `os.Exit(2)` for connection failures.
- Output: machine-readable on `--output json`, human-readable table by default. Errors always on stderr. Stdout is for results only (so `pi-ctl list --output json | jq` works cleanly).
- Color: auto-detect via `term.IsTerminal(int(os.Stdout.Fd()))`; honor `NO_COLOR` env var; `--color=always|never|auto` flag (default auto).
- Argument resolution: every command that takes `<id|name>` accepts childId, exact name, or unambiguous prefix. Always resolve via a single `ctrl_list` round-trip.

---

### Task 1: Project bootstrap (Cobra dep, root command, global flags)

**Files:**
- Modify: `go.mod` (add cobra + x/term)
- Modify: `Makefile` (new build target)
- Create: `cmd/pi-ctl/main.go`

- [ ] **Step 1: Add Cobra and x/term to go.mod.**

```bash
cd ~/home/pi-controller
go get github.com/spf13/cobra@latest
go get golang.org/x/term@latest
go mod tidy
```

Verify both appear in `go.mod` and `go.sum`.

- [ ] **Step 2: Add the pi-ctl build target to the Makefile.**

In `Makefile`, change:

```makefile
build:
	mkdir -p $(BIN_DIR)
	$(GO) build -o $(BIN_DIR)/pi-controller ./cmd/pi-controller
```

to:

```makefile
build: build-controller build-ctl

build-controller:
	mkdir -p $(BIN_DIR)
	$(GO) build -o $(BIN_DIR)/pi-controller ./cmd/pi-controller

build-ctl:
	mkdir -p $(BIN_DIR)
	$(GO) build -o $(BIN_DIR)/pi-ctl ./cmd/pi-ctl
```

Add `build-controller` and `build-ctl` to `.PHONY` at the top.

- [ ] **Step 3: Write the root command in `cmd/pi-ctl/main.go`.**

```go
package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

var version = "0.1.0"

func main() {
	root := newRootCmd()
	if err := root.Execute(); err != nil {
		// Cobra's RunE error path: print to stderr, exit 1.
		// Connection errors get exit 2 via direct os.Exit in subcommands.
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:           "pi-ctl",
		Short:         "Control the pi-controller daemon",
		Long:          "pi-ctl is the command-line client for the pi-controller daemon.\nIt speaks the JSONL protocol over the daemon's UDS socket.",
		Version:       version,
		SilenceUsage:  true, // don't print usage on RunE errors
		SilenceErrors: true, // main() prints errors itself
	}

	root.PersistentFlags().String("socket", "", "controller socket path (default ~/.pi/run/controller.sock)")
	root.PersistentFlags().String("output", "auto", "output format: auto|json|table")
	root.PersistentFlags().String("color", "auto", "color output: auto|always|never")

	// Subcommands wired up in later tasks; nothing yet.

	return root
}
```

- [ ] **Step 4: Verify it builds and runs.**

```bash
make build-ctl
./bin/pi-ctl --help
./bin/pi-ctl --version
```

Expected: help text shows up; version prints `pi-ctl version 0.1.0`.

- [ ] **Step 5: Commit.**

```bash
git add go.mod go.sum Makefile cmd/pi-ctl/main.go
git commit -m "pi-ctl: bootstrap root command with global flags"
```

---

### Task 2: Client library — connection + framing

**Files:**
- Create: `internal/client/client.go`
- Create: `internal/client/client_test.go`

The client speaks JSONL over UDS. Use the same `protocol.FrameReader` / `protocol.WriteFrame` from the daemon side.

- [ ] **Step 1: Write the failing tests.**

```go
package client_test

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"graveland.dev/pi-controller/internal/client"
	"graveland.dev/pi-controller/internal/protocol"
)

// startEchoServer spins up a tiny UDS that echoes every received frame
// back wrapped in a ctrl_response. Returns the socket path + a cleanup.
func startEchoServer(t *testing.T) (string, func()) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "test.sock")

	ln, err := net.Listen("unix", path)
	if err != nil { t.Fatal(err) }

	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, err := ln.Accept()
		if err != nil { return }
		defer conn.Close()
		r := protocol.NewFrameReader(conn, 1<<20)
		for {
			frame, err := r.ReadFrame()
			if err != nil { return }
			// Echo as ctrl_response with the original frame as data.
			var req struct{ Type, ID string }
			_ = json.Unmarshal(frame, &req)
			resp := map[string]any{
				"type":    "ctrl_response",
				"command": req.Type,
				"id":      req.ID,
				"success": true,
				"data":    json.RawMessage(frame),
			}
			b, _ := json.Marshal(resp)
			_ = protocol.WriteFrame(conn, b)
		}
	}()

	return path, func() {
		ln.Close()
		<-done
	}
}

func TestClient_DialAndRequest(t *testing.T) {
	path, cleanup := startEchoServer(t)
	defer cleanup()

	c, err := client.Dial(path)
	if err != nil { t.Fatal(err) }
	defer c.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	resp, err := c.Request(ctx, protocol.StatusRequest{
		Type: protocol.TypeCtrlStatus,
		ID:   "req-1",
	})
	if err != nil { t.Fatal(err) }
	if resp.ID != "req-1" {
		t.Fatalf("ID echo: got %q, want %q", resp.ID, "req-1")
	}
	if !resp.Success { t.Fatalf("Success: got false") }
	if resp.Command != protocol.TypeCtrlStatus {
		t.Fatalf("Command: got %q, want %q", resp.Command, protocol.TypeCtrlStatus)
	}
}

func TestClient_DialFailureReturnsError(t *testing.T) {
	_, err := client.Dial("/nonexistent/path/to/socket")
	if err == nil {
		t.Fatal("expected error dialing nonexistent socket")
	}
}

func TestClient_ContextCancel(t *testing.T) {
	path, cleanup := startEchoServer(t)
	defer cleanup()

	c, err := client.Dial(path)
	if err != nil { t.Fatal(err) }
	defer c.Close()

	// Cancel before request.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err = c.Request(ctx, protocol.StatusRequest{Type: protocol.TypeCtrlStatus})
	if err == nil || !isContextErr(err) {
		t.Fatalf("expected context error, got %v", err)
	}
}

func isContextErr(err error) bool {
	return err == context.Canceled || err == context.DeadlineExceeded
}
```

- [ ] **Step 2: Run tests — they fail (no client package).**

```bash
go test ./internal/client/...
```

- [ ] **Step 3: Implement `internal/client/client.go`.**

```go
package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"

	"graveland.dev/pi-controller/internal/protocol"
)

// Client is a connected JSONL client to the pi-controller daemon.
// Safe for concurrent use; the request/response correlator multiplexes
// in-flight requests by id.
type Client struct {
	conn    net.Conn
	encMu   sync.Mutex                            // serializes writes
	pending sync.Map                              // map[string]chan *protocol.Response
	nextID  atomic.Uint64
	closed  atomic.Bool
	closeCh chan struct{}
	// readErr stores the first read-loop error for reporting on later requests.
	readErr atomic.Value
}

// Dial opens a connection to the UDS at path. If path is empty,
// resolves to $PI_CONTROLLER_SOCKET or ~/.pi/run/controller.sock.
func Dial(path string) (*Client, error) {
	if path == "" {
		path = DefaultSocketPath()
	}
	conn, err := net.Dial("unix", path)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", path, err)
	}
	c := &Client{
		conn:    conn,
		closeCh: make(chan struct{}),
	}
	go c.readLoop()
	return c, nil
}

// DefaultSocketPath returns the conventional socket location.
func DefaultSocketPath() string {
	if p := os.Getenv("PI_CONTROLLER_SOCKET"); p != "" {
		return p
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".pi", "run", "controller.sock")
}

// Request sends a typed request and waits for the matching response.
// req must marshal to a JSON object with a "type" field; if the
// request has no ID, one is assigned automatically.
func (c *Client) Request(ctx context.Context, req any) (*protocol.Response, error) {
	if c.closed.Load() {
		return nil, errClosedConn(c.readErr.Load())
	}

	// Marshal req to discover/assign id.
	b, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal: %w", err)
	}
	id, b, err := c.ensureID(b)
	if err != nil {
		return nil, err
	}

	respCh := make(chan *protocol.Response, 1)
	c.pending.Store(id, respCh)
	defer c.pending.Delete(id)

	if err := c.send(b); err != nil {
		return nil, err
	}

	select {
	case resp := <-respCh:
		return resp, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-c.closeCh:
		return nil, errClosedConn(c.readErr.Load())
	}
}

// ensureID inspects the marshaled request, assigns a request ID if
// none was set, and returns the (possibly modified) bytes plus the id.
func (c *Client) ensureID(b []byte) (string, []byte, error) {
	var hdr struct {
		Type string `json:"type"`
		ID   string `json:"id,omitempty"`
	}
	if err := json.Unmarshal(b, &hdr); err != nil {
		return "", nil, fmt.Errorf("inspect: %w", err)
	}
	if hdr.ID != "" {
		return hdr.ID, b, nil
	}
	// Assign an auto-id and re-marshal as a generic map.
	id := fmt.Sprintf("c%d", c.nextID.Add(1))
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		return "", nil, err
	}
	m["id"] = json.RawMessage(`"` + id + `"`)
	nb, err := json.Marshal(m)
	if err != nil {
		return "", nil, err
	}
	return id, nb, nil
}

func (c *Client) send(b []byte) error {
	c.encMu.Lock()
	defer c.encMu.Unlock()
	return protocol.WriteFrame(c.conn, b)
}

func (c *Client) readLoop() {
	defer close(c.closeCh)
	r := protocol.NewFrameReader(c.conn, 16<<20)
	for {
		frame, err := r.ReadFrame()
		if err != nil {
			if err != io.EOF {
				c.readErr.Store(err)
			}
			c.closed.Store(true)
			return
		}

		// Parse top-level type to route.
		var hdr struct {
			Type string `json:"type"`
			ID   string `json:"id,omitempty"`
		}
		if err := json.Unmarshal(frame, &hdr); err != nil {
			continue // malformed frame; ignore
		}

		switch hdr.Type {
		case protocol.TypeCtrlResponse:
			var resp protocol.Response
			if err := json.Unmarshal(frame, &resp); err != nil {
				continue
			}
			if chAny, ok := c.pending.Load(resp.ID); ok {
				select {
				case chAny.(chan *protocol.Response) <- &resp:
				default:
				}
			}
		default:
			// Event frames (ctrl_event, ctrl_child_*). Hand off to Subscribe
			// when implemented (Task 3). For now, drop.
			c.dispatchEvent(frame)
		}
	}
}

// dispatchEvent is a no-op for now; Task 3 replaces this with the
// subscription dispatcher.
func (c *Client) dispatchEvent(frame []byte) {}

// Close shuts down the connection. Any in-flight Request returns
// io.EOF-ish (the closed channel arm).
func (c *Client) Close() error {
	if c.closed.Swap(true) {
		return nil
	}
	return c.conn.Close()
}

func errClosedConn(stored any) error {
	if stored == nil {
		return errors.New("client connection closed")
	}
	return fmt.Errorf("client connection closed: %w", stored.(error))
}
```

- [ ] **Step 4: Run tests — should now pass.**

```bash
go test -race ./internal/client/...
```

- [ ] **Step 5: Commit.**

```bash
git add internal/client/client.go internal/client/client_test.go
git commit -m "client: connection with request/response correlation over JSONL UDS"
```

---

### Task 3: Client library — subscribe stream

**Files:**
- Modify: `internal/client/client.go`
- Modify: `internal/client/client_test.go`

For `tail` to work, Client needs to deliver event frames to subscribers separately from responses.

- [ ] **Step 1: Add a failing test.**

```go
func TestClient_Subscribe_ReceivesEvents(t *testing.T) {
	// Use a hand-rolled UDS that, after receiving a ctrl_subscribe request,
	// pushes 3 ctrl_event frames at known intervals.
	dir := t.TempDir()
	path := filepath.Join(dir, "test.sock")
	ln, err := net.Listen("unix", path)
	if err != nil { t.Fatal(err) }
	defer ln.Close()

	go func() {
		conn, err := ln.Accept()
		if err != nil { return }
		defer conn.Close()
		r := protocol.NewFrameReader(conn, 1<<20)
		for {
			frame, err := r.ReadFrame()
			if err != nil { return }
			var hdr struct{ Type, ID string }
			_ = json.Unmarshal(frame, &hdr)
			if hdr.Type == protocol.TypeCtrlSubscribe {
				// Ack the subscribe.
				ack := fmt.Sprintf(`{"type":"ctrl_response","command":"ctrl_subscribe","id":%q,"success":true}`, hdr.ID)
				_ = protocol.WriteFrame(conn, []byte(ack))
				// Push 3 events.
				for i := 0; i < 3; i++ {
					ev := fmt.Sprintf(`{"type":"ctrl_event","childId":"c_x","event":{"type":"agent_start","i":%d}}`, i)
					_ = protocol.WriteFrame(conn, []byte(ev))
				}
				return
			}
		}
	}()

	c, err := client.Dial(path)
	if err != nil { t.Fatal(err) }
	defer c.Close()

	events, cancel, err := c.Subscribe()
	if err != nil { t.Fatal(err) }
	defer cancel()

	ctx, ctxCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer ctxCancel()

	_, err = c.Request(ctx, protocol.SubscribeRequest{
		Type:    protocol.TypeCtrlSubscribe,
		ChildID: "c_x",
	})
	if err != nil { t.Fatal(err) }

	got := []json.RawMessage{}
	for i := 0; i < 3; i++ {
		select {
		case ev := <-events:
			got = append(got, ev)
		case <-time.After(2*time.Second):
			t.Fatalf("timed out waiting for event %d", i)
		}
	}
	if len(got) != 3 {
		t.Fatalf("got %d events, want 3", len(got))
	}
}
```

- [ ] **Step 2: Run tests — fails (no Subscribe).**

- [ ] **Step 3: Add Subscribe + an event channel inside Client.**

In `client.go`, replace the `dispatchEvent` no-op with real event routing:

```go
// Subscribe returns a channel that receives every non-response frame
// the client reads. Multiple Subscribe calls are independent. The
// returned cancel func removes this subscriber from the dispatch list.
//
// The channel has a small buffer; events are dropped on a full channel
// to prevent slow consumers from blocking the read loop.
func (c *Client) Subscribe() (<-chan []byte, func(), error) {
	if c.closed.Load() {
		return nil, nil, errClosedConn(c.readErr.Load())
	}
	ch := make(chan []byte, 256)
	c.subMu.Lock()
	c.nextSubID++
	id := c.nextSubID
	c.subs[id] = ch
	c.subMu.Unlock()

	cancel := func() {
		c.subMu.Lock()
		defer c.subMu.Unlock()
		if _, ok := c.subs[id]; ok {
			delete(c.subs, id)
			close(ch)
		}
	}
	return ch, cancel, nil
}
```

And add the fields to Client:

```go
type Client struct {
	// ... existing fields ...

	subMu      sync.Mutex
	subs       map[uint64]chan []byte
	nextSubID  uint64
}
```

Initialize in `Dial`:

```go
c := &Client{
	conn:    conn,
	closeCh: make(chan struct{}),
	subs:    make(map[uint64]chan []byte),
}
```

Replace `dispatchEvent` body:

```go
func (c *Client) dispatchEvent(frame []byte) {
	c.subMu.Lock()
	chs := make([]chan []byte, 0, len(c.subs))
	for _, ch := range c.subs {
		chs = append(chs, ch)
	}
	c.subMu.Unlock()

	for _, ch := range chs {
		// Copy the frame because the reader's buffer is reused.
		cp := make([]byte, len(frame))
		copy(cp, frame)
		select {
		case ch <- cp:
		default:
			// Drop on full; subscriber is slow.
		}
	}
}
```

In `Close`, close all subscriber channels:

```go
func (c *Client) Close() error {
	if c.closed.Swap(true) {
		return nil
	}
	c.subMu.Lock()
	for id, ch := range c.subs {
		close(ch)
		delete(c.subs, id)
	}
	c.subMu.Unlock()
	return c.conn.Close()
}
```

- [ ] **Step 4: Tests pass.**

```bash
go test -race ./internal/client/...
```

- [ ] **Step 5: Commit.**

```bash
git add internal/client/client.go internal/client/client_test.go
git commit -m "client: subscribe channel for event frames"
```

---

### Task 4: Client library — identifier resolution

**Files:**
- Create: `internal/client/resolve.go`
- Create: `internal/client/resolve_test.go`

Users type `pi-ctl tail afk` and expect it to resolve `afk` to the right childId. Algorithm:
1. If input starts with `c_` (childId prefix), treat as childId directly.
2. Otherwise, call `ctrl_list`.
3. Match against `name` first (exact), then `childId` (exact), then prefix of either.
4. Multiple matches → error with the list.
5. No match → error.

- [ ] **Step 1: Write failing tests.**

```go
package client_test

import (
	"context"
	"strings"
	"testing"

	"graveland.dev/pi-controller/internal/client"
	"graveland.dev/pi-controller/internal/protocol"
)

// Use a fake Client interface for resolve tests (no real socket).
// Resolve calls ctrl_list; we mock that.

type fakeLister struct {
	children []protocol.ChildSummary
}

func (f *fakeLister) List(ctx context.Context, filter protocol.ListRequestFilter) ([]protocol.ChildSummary, error) {
	return f.children, nil
}

func TestResolve_ExactChildID(t *testing.T) {
	f := &fakeLister{children: []protocol.ChildSummary{
		{ChildID: "c_01HX01", Name: "afk"},
		{ChildID: "c_01HX02", Name: "other"},
	}}
	got, err := client.ResolveWith(context.Background(), f, "c_01HX01")
	if err != nil { t.Fatal(err) }
	if got != "c_01HX01" { t.Fatalf("got %q", got) }
}

func TestResolve_ExactName(t *testing.T) {
	f := &fakeLister{children: []protocol.ChildSummary{
		{ChildID: "c_01HX01", Name: "afk-impl"},
	}}
	got, err := client.ResolveWith(context.Background(), f, "afk-impl")
	if err != nil { t.Fatal(err) }
	if got != "c_01HX01" { t.Fatalf("got %q", got) }
}

func TestResolve_PrefixMatch(t *testing.T) {
	f := &fakeLister{children: []protocol.ChildSummary{
		{ChildID: "c_01HX01", Name: "afk-impl-2026"},
		{ChildID: "c_01HX02", Name: "other"},
	}}
	got, err := client.ResolveWith(context.Background(), f, "afk")
	if err != nil { t.Fatal(err) }
	if got != "c_01HX01" { t.Fatalf("got %q", got) }
}

func TestResolve_AmbiguousPrefix_Errors(t *testing.T) {
	f := &fakeLister{children: []protocol.ChildSummary{
		{ChildID: "c_01HX01", Name: "afk-impl"},
		{ChildID: "c_01HX02", Name: "afk-test"},
	}}
	_, err := client.ResolveWith(context.Background(), f, "afk")
	if err == nil { t.Fatal("expected ambiguity error") }
	if !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("error doesn't mention ambiguity: %v", err)
	}
}

func TestResolve_NoMatch_Errors(t *testing.T) {
	f := &fakeLister{children: []protocol.ChildSummary{
		{ChildID: "c_01HX01", Name: "afk"},
	}}
	_, err := client.ResolveWith(context.Background(), f, "nope")
	if err == nil { t.Fatal("expected no-match error") }
}
```

- [ ] **Step 2: Run tests — fails.**

- [ ] **Step 3: Implement `internal/client/resolve.go`.**

```go
package client

import (
	"context"
	"fmt"
	"strings"

	"graveland.dev/pi-controller/internal/protocol"
)

// Lister is the subset of Client that Resolve needs. A concrete *Client
// implements this via Request; tests pass a fake.
type Lister interface {
	List(ctx context.Context, filter protocol.ListRequestFilter) ([]protocol.ChildSummary, error)
}

// ResolveWith is the inner implementation, kept exported for testing
// without a real Client.
func ResolveWith(ctx context.Context, l Lister, input string) (string, error) {
	// Empty input is a user error.
	if input == "" {
		return "", fmt.Errorf("empty identifier")
	}
	// Fast path: childIds start with c_.
	if strings.HasPrefix(input, "c_") {
		return input, nil
	}

	children, err := l.List(ctx, protocol.ListRequestFilter{})
	if err != nil {
		return "", fmt.Errorf("list children: %w", err)
	}

	// 1. Exact name match.
	for _, ch := range children {
		if ch.Name == input {
			return ch.ChildID, nil
		}
	}
	// 2. Exact childId match.
	for _, ch := range children {
		if ch.ChildID == input {
			return ch.ChildID, nil
		}
	}
	// 3. Prefix match on name or childId. Collect all candidates.
	var candidates []protocol.ChildSummary
	for _, ch := range children {
		if strings.HasPrefix(ch.Name, input) || strings.HasPrefix(ch.ChildID, input) {
			candidates = append(candidates, ch)
		}
	}
	switch len(candidates) {
	case 0:
		return "", fmt.Errorf("no child matches %q", input)
	case 1:
		return candidates[0].ChildID, nil
	default:
		var matches []string
		for _, ch := range candidates {
			label := ch.Name
			if label == "" { label = ch.ChildID }
			matches = append(matches, label)
		}
		return "", fmt.Errorf("ambiguous identifier %q matches: %s",
			input, strings.Join(matches, ", "))
	}
}

// Resolve looks up a childId from the user's input (childId, name, or
// unambiguous prefix). Uses the Client's own List method.
func (c *Client) Resolve(ctx context.Context, input string) (string, error) {
	return ResolveWith(ctx, c, input)
}

// List is the convenience wrapper for the Lister interface; calls
// ctrl_list and returns just the children slice.
func (c *Client) List(ctx context.Context, filter protocol.ListRequestFilter) ([]protocol.ChildSummary, error) {
	resp, err := c.Request(ctx, protocol.ListRequest{
		Type:   protocol.TypeCtrlList,
		Filter: filter,
	})
	if err != nil { return nil, err }
	if !resp.Success {
		return nil, fmt.Errorf("ctrl_list failed: %s", responseErr(resp))
	}
	var data protocol.ListResponseData
	if err := json.Unmarshal(resp.Data, &data); err != nil {
		return nil, fmt.Errorf("decode list data: %w", err)
	}
	return data.Children, nil
}

func responseErr(resp *protocol.Response) string {
	if resp.Error != nil {
		return fmt.Sprintf("%s: %s", resp.Error.Code, resp.Error.Message)
	}
	return "unknown error"
}
```

Add `import "encoding/json"` if not already imported.

- [ ] **Step 4: Tests pass.**

```bash
go test -race ./internal/client/...
```

- [ ] **Step 5: Commit.**

```bash
git add internal/client/resolve.go internal/client/resolve_test.go
git commit -m "client: identifier resolution (childId, name, unambiguous prefix)"
```

---

### Task 5: Output formatting (JSON, table, color)

**Files:**
- Create: `cmd/pi-ctl/output.go`
- Create: `cmd/pi-ctl/output_test.go`

Shared output helpers. Used by every subcommand.

- [ ] **Step 1: Write failing tests.**

```go
package main

import (
	"bytes"
	"strings"
	"testing"

	"graveland.dev/pi-controller/internal/protocol"
)

func TestRenderList_Table(t *testing.T) {
	var buf bytes.Buffer
	children := []protocol.ChildSummary{
		{ChildID: "c_01HXABC", Name: "afk-impl", Status: "streaming", Model: "anthropic/claude-sonnet-4", StartedAt: 1716636789},
	}
	if err := renderList(&buf, children, outputTable, false); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{"c_01HXABC", "afk-impl", "streaming", "claude-sonnet-4"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q:\n%s", want, out)
		}
	}
}

func TestRenderList_JSON(t *testing.T) {
	var buf bytes.Buffer
	children := []protocol.ChildSummary{{ChildID: "c_1", Name: "x"}}
	if err := renderList(&buf, children, outputJSON, false); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, `"childId":"c_1"`) {
		t.Fatalf("JSON output: %s", out)
	}
}

func TestColorEnabled_AlwaysFlag(t *testing.T) {
	if !colorEnabled("always", false) {
		t.Fatal("always should be true")
	}
	if colorEnabled("never", true) {
		t.Fatal("never should be false")
	}
}
```

- [ ] **Step 2: Run tests — fails.**

- [ ] **Step 3: Implement `output.go`.**

```go
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"golang.org/x/term"

	"graveland.dev/pi-controller/internal/protocol"
)

type outputMode string

const (
	outputAuto  outputMode = "auto"
	outputJSON  outputMode = "json"
	outputTable outputMode = "table"
)

// resolveOutputMode resolves "auto" by checking if stdout is a TTY:
// TTY → table, otherwise → json.
func resolveOutputMode(flag string, isTTY bool) outputMode {
	switch outputMode(flag) {
	case outputJSON:
		return outputJSON
	case outputTable:
		return outputTable
	default:
		if isTTY {
			return outputTable
		}
		return outputJSON
	}
}

// colorEnabled decides whether to emit ANSI color codes.
// flag: always|never|auto. isTTY: whether stdout is a terminal.
// Honors NO_COLOR env var (always disables).
func colorEnabled(flag string, isTTY bool) bool {
	if os.Getenv("NO_COLOR") != "" {
		return false
	}
	switch flag {
	case "always":
		return true
	case "never":
		return false
	default:
		return isTTY
	}
}

// isStdoutTTY is a small wrapper for testability.
func isStdoutTTY() bool {
	return term.IsTerminal(int(os.Stdout.Fd()))
}

// renderList writes a list of ChildSummary either as JSON or as a table.
func renderList(w io.Writer, children []protocol.ChildSummary, mode outputMode, useColor bool) error {
	if mode == outputJSON {
		return writeJSON(w, map[string]any{"children": children})
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, headerLine("ID", "NAME", "STATUS", "MODEL", "STARTED", useColor))
	for _, ch := range children {
		started := "-"
		if ch.StartedAt > 0 {
			started = time.Unix(ch.StartedAt, 0).Format("2006-01-02 15:04")
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n",
			ch.ChildID,
			defaultDash(ch.Name),
			colorStatus(ch.Status, useColor),
			defaultDash(ch.Model),
			started)
	}
	return tw.Flush()
}

func defaultDash(s string) string {
	if s == "" { return "-" }
	return s
}

func writeJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func headerLine(cols ...any) string {
	useColor, _ := cols[len(cols)-1].(bool)
	cols = cols[:len(cols)-1]
	parts := make([]string, len(cols))
	for i, c := range cols {
		s := fmt.Sprint(c)
		if useColor { s = dim(s) }
		parts[i] = s
	}
	return strings.Join(parts, "\t")
}

func colorStatus(status string, useColor bool) string {
	if !useColor { return status }
	switch status {
	case "idle":
		return cyan(status)
	case "streaming", "tool_running", "compacting":
		return green(status)
	case "exited":
		return red(status)
	case "shutting_down":
		return yellow(status)
	case "blocked_ui":
		return magenta(status)
	default:
		return status
	}
}

// ANSI color helpers — simple, no external dep.
func dim(s string) string     { return "\x1b[2m" + s + "\x1b[0m" }
func red(s string) string     { return "\x1b[31m" + s + "\x1b[0m" }
func green(s string) string   { return "\x1b[32m" + s + "\x1b[0m" }
func yellow(s string) string  { return "\x1b[33m" + s + "\x1b[0m" }
func cyan(s string) string    { return "\x1b[36m" + s + "\x1b[0m" }
func magenta(s string) string { return "\x1b[35m" + s + "\x1b[0m" }

// Suppress unused-import warnings until tail uses them in Task 11.
var _ = term.IsTerminal
```

- [ ] **Step 4: Tests pass.**

```bash
go test ./cmd/pi-ctl/...
```

- [ ] **Step 5: Commit.**

```bash
git add cmd/pi-ctl/output.go cmd/pi-ctl/output_test.go
git commit -m "pi-ctl: output formatting helpers (JSON/table/color)"
```

---

### Task 6: Read-only subcommands: list, get, status

**Files:**
- Create: `cmd/pi-ctl/cmd_list.go`
- Create: `cmd/pi-ctl/cmd_get.go`
- Create: `cmd/pi-ctl/cmd_status.go`
- Modify: `cmd/pi-ctl/main.go` (wire commands)
- Create: `cmd/pi-ctl/cli_helpers.go` (shared command helpers)

The shared pattern: parse flags, dial the client, send a request, render the result.

- [ ] **Step 1: Add a shared helper file `cli_helpers.go`.**

```go
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"graveland.dev/pi-controller/internal/client"
)

// mustDial connects to the daemon's UDS using the --socket flag value
// (default ~/.pi/run/controller.sock). Exits with code 2 on failure.
func mustDial(cmd *cobra.Command) *client.Client {
	socket, _ := cmd.Flags().GetString("socket")
	c, err := client.Dial(socket)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error: connect:", err)
		os.Exit(2)
	}
	return c
}

// cmdCtx returns the cobra command's context (with cancellation on
// SIGINT etc). Falls back to context.Background() if no context is set.
func cmdCtx(cmd *cobra.Command) context.Context {
	if ctx := cmd.Context(); ctx != nil { return ctx }
	return context.Background()
}

// outputOpts pulls --output and --color flags + computes effective values.
func outputOpts(cmd *cobra.Command) (outputMode, bool) {
	outFlag, _ := cmd.Flags().GetString("output")
	colorFlag, _ := cmd.Flags().GetString("color")
	tty := isStdoutTTY()
	return resolveOutputMode(outFlag, tty), colorEnabled(colorFlag, tty)
}
```

- [ ] **Step 2: Write `cmd_list.go`.**

```go
package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"graveland.dev/pi-controller/internal/protocol"
)

func newListCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List active and recently-exited children",
		Args:  cobra.NoArgs,
		RunE:  runList,
	}
	cmd.Flags().String("status", "", "Filter by status")
	cmd.Flags().String("name-contains", "", "Filter by substring in name")
	cmd.Flags().String("cwd-contains", "", "Filter by substring in cwd")
	return cmd
}

func runList(cmd *cobra.Command, _ []string) error {
	c := mustDial(cmd)
	defer c.Close()

	filter := protocol.ListRequestFilter{}
	if v, _ := cmd.Flags().GetString("status"); v != "" {
		filter.Status = v
	}
	if v, _ := cmd.Flags().GetString("name-contains"); v != "" {
		filter.NameContains = v
	}
	if v, _ := cmd.Flags().GetString("cwd-contains"); v != "" {
		filter.CwdContains = v
	}

	children, err := c.List(cmdCtx(cmd), filter)
	if err != nil {
		return fmt.Errorf("list: %w", err)
	}

	mode, useColor := outputOpts(cmd)
	if mode == outputJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(map[string]any{"children": children})
	}
	return renderList(os.Stdout, children, mode, useColor)
}
```

- [ ] **Step 3: Write `cmd_get.go`.**

```go
package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"graveland.dev/pi-controller/internal/protocol"
)

func newGetCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "get <id|name>",
		Short: "Show details for a single child",
		Args:  cobra.ExactArgs(1),
		RunE:  runGet,
	}
	return cmd
}

func runGet(cmd *cobra.Command, args []string) error {
	c := mustDial(cmd)
	defer c.Close()

	ctx := cmdCtx(cmd)
	childID, err := c.Resolve(ctx, args[0])
	if err != nil { return err }

	resp, err := c.Request(ctx, protocol.GetRequest{
		Type:    protocol.TypeCtrlGet,
		ChildID: childID,
	})
	if err != nil { return err }
	if !resp.Success {
		return fmt.Errorf("ctrl_get: %s", responseErr(resp))
	}

	var child protocol.ChildSummary
	if err := json.Unmarshal(resp.Data, &child); err != nil {
		return fmt.Errorf("decode: %w", err)
	}

	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(child)
}

// responseErr formats a *protocol.Response's error payload.
func responseErr(resp *protocol.Response) string {
	if resp == nil || resp.Error == nil { return "unknown error" }
	return fmt.Sprintf("%s: %s", resp.Error.Code, resp.Error.Message)
}
```

- [ ] **Step 4: Write `cmd_status.go`.**

```go
package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"graveland.dev/pi-controller/internal/protocol"
)

func newStatusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show daemon status",
		Args:  cobra.NoArgs,
		RunE:  runStatus,
	}
}

func runStatus(cmd *cobra.Command, _ []string) error {
	c := mustDial(cmd)
	defer c.Close()

	resp, err := c.Request(cmdCtx(cmd), protocol.StatusRequest{
		Type: protocol.TypeCtrlStatus,
	})
	if err != nil { return err }
	if !resp.Success {
		return fmt.Errorf("ctrl_status: %s", responseErr(resp))
	}

	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(json.RawMessage(resp.Data))
}
```

- [ ] **Step 5: Wire commands into root in `main.go`.**

```go
// In newRootCmd(), after PersistentFlags():
root.AddCommand(
	newListCmd(),
	newGetCmd(),
	newStatusCmd(),
)
```

- [ ] **Step 6: Resolve any duplicate `responseErr` definition.**

`internal/client/resolve.go` already exports `responseErr` as unexported. Reuse it from there, OR change pi-ctl's local definition to a different name (`fmtRespErr` works). Pick one and remove the duplicate.

Cleanest: export it from internal/client as `client.FormatError(*protocol.Response) string` and reuse. Modify `internal/client/resolve.go` to export.

- [ ] **Step 7: Verify by hand.**

```bash
# Boot the daemon (if not running)
./bin/pi-controller &

# Test:
./bin/pi-ctl status
./bin/pi-ctl list
./bin/pi-ctl list --output json
```

- [ ] **Step 8: Add unit tests if convenient (integration in Task 12).**

For now, the manual smoke test is enough. The integration test in Task 12 will exercise these end-to-end.

- [ ] **Step 9: Commit.**

```bash
git add cmd/pi-ctl/ internal/client/resolve.go
git commit -m "pi-ctl: list/get/status read-only subcommands"
```

---

### Task 7: Lifecycle subcommands: spawn, resume, kill

**Files:**
- Create: `cmd/pi-ctl/cmd_spawn.go`
- Create: `cmd/pi-ctl/cmd_resume.go`
- Create: `cmd/pi-ctl/cmd_kill.go`
- Modify: `cmd/pi-ctl/main.go`

- [ ] **Step 1: Write `cmd_spawn.go`.**

```go
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"graveland.dev/pi-controller/internal/protocol"
)

func newSpawnCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "spawn [name]",
		Short: "Spawn a new pi child",
		Args:  cobra.MaximumNArgs(1),
		RunE:  runSpawn,
	}
	cmd.Flags().String("cwd", "", "Working directory (required, must be absolute)")
	cmd.Flags().String("model", "", "Model (e.g. anthropic/claude-sonnet-4)")
	cmd.Flags().String("thinking", "", "Thinking level: off|minimal|low|medium|high|xhigh")
	cmd.Flags().Bool("no-session", false, "Run in ephemeral mode (no session file)")
	cmd.Flags().String("session", "", "Resume an existing session.jsonl by path")
	cmd.Flags().String("fork", "", "Fork from an existing session.jsonl by path")
	cmd.Flags().Bool("no-extensions", false, "Disable extension discovery")
	cmd.Flags().StringSlice("extension", nil, "Load an extension (repeatable)")
	cmd.Flags().Bool("verbose", false, "Verbose startup")
	cmd.Flags().StringSlice("extra-arg", nil, "Extra pi arg (repeatable)")
	return cmd
}

func runSpawn(cmd *cobra.Command, args []string) error {
	c := mustDial(cmd)
	defer c.Close()

	cwd, _ := cmd.Flags().GetString("cwd")
	if cwd == "" {
		return fmt.Errorf("--cwd is required")
	}
	if !strings.HasPrefix(cwd, "/") {
		return fmt.Errorf("--cwd must be absolute")
	}
	model, _ := cmd.Flags().GetString("model")
	thinking, _ := cmd.Flags().GetString("thinking")
	noSession, _ := cmd.Flags().GetBool("no-session")
	resume, _ := cmd.Flags().GetString("session")
	fork, _ := cmd.Flags().GetString("fork")
	noExt, _ := cmd.Flags().GetBool("no-extensions")
	exts, _ := cmd.Flags().GetStringSlice("extension")
	verbose, _ := cmd.Flags().GetBool("verbose")
	extraArgs, _ := cmd.Flags().GetStringSlice("extra-arg")

	name := ""
	if len(args) > 0 { name = args[0] }

	req := protocol.SpawnRequest{
		Type:          protocol.TypeCtrlSpawn,
		Name:          name,
		Cwd:           cwd,
		Model:         model,
		Thinking:      thinking,
		NoSession:     noSession,
		ResumeSession: resume,
		ForkSession:   fork,
		NoExtensions:  noExt,
		Extensions:    exts,
		Verbose:       verbose,
		ExtraArgs:     extraArgs,
	}

	resp, err := c.Request(cmdCtx(cmd), req)
	if err != nil { return err }
	if !resp.Success {
		return fmt.Errorf("ctrl_spawn: %s", responseErr(resp))
	}

	var data protocol.SpawnResponseData
	_ = json.Unmarshal(resp.Data, &data)
	if err := setActive(data.ChildID); err != nil {
		// Best effort — log to stderr but don't fail.
		fmt.Fprintln(os.Stderr, "warning: could not update active marker:", err)
	}

	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(data)
}
```

Note: `setActive(childID)` will be added in Task 10.

- [ ] **Step 2: Write `cmd_resume.go`.**

```go
package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"graveland.dev/pi-controller/internal/protocol"
)

func newResumeCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "resume <id|name>",
		Short: "Resume an exited child",
		Args:  cobra.ExactArgs(1),
		RunE:  runResume,
	}
	cmd.Flags().String("api-key", "", "Optional API key override for this resume")
	return cmd
}

func runResume(cmd *cobra.Command, args []string) error {
	c := mustDial(cmd)
	defer c.Close()

	ctx := cmdCtx(cmd)
	childID, err := c.Resolve(ctx, args[0])
	if err != nil { return err }

	apiKey, _ := cmd.Flags().GetString("api-key")

	resp, err := c.Request(ctx, protocol.ResumeRequest{
		Type:    protocol.TypeCtrlResume,
		ChildID: childID,
		APIKey:  apiKey,
	})
	if err != nil { return err }
	if !resp.Success {
		return fmt.Errorf("ctrl_resume: %s", responseErr(resp))
	}

	_ = setActive(childID)

	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(json.RawMessage(resp.Data))
}
```

- [ ] **Step 3: Write `cmd_kill.go`.**

```go
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"

	"graveland.dev/pi-controller/internal/protocol"
)

func newKillCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "kill <id|name>",
		Short: "Stop a running child gracefully",
		Args:  cobra.ExactArgs(1),
		RunE:  runKill,
	}
	cmd.Flags().Duration("shutdown-timeout", 0, "Override shutdown timeout (e.g. 180s)")
	cmd.Flags().Duration("kill-timeout", 0, "Override kill timeout (e.g. 30s)")
	return cmd
}

func runKill(cmd *cobra.Command, args []string) error {
	c := mustDial(cmd)
	defer c.Close()

	ctx := cmdCtx(cmd)
	childID, err := c.Resolve(ctx, args[0])
	if err != nil { return err }

	st, _ := cmd.Flags().GetDuration("shutdown-timeout")
	kt, _ := cmd.Flags().GetDuration("kill-timeout")

	req := protocol.KillRequest{
		Type:    protocol.TypeCtrlKill,
		ChildID: childID,
	}
	if st > 0 { req.ShutdownTimeoutMs = st.Milliseconds() }
	if kt > 0 { req.KillTimeoutMs = kt.Milliseconds() }

	// Allow longer than the default 30s context if the user requested it.
	if st > 30*time.Second {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, st+5*time.Second)
		defer cancel()
	}

	resp, err := c.Request(ctx, req)
	if err != nil { return err }
	if !resp.Success {
		return fmt.Errorf("ctrl_kill: %s", responseErr(resp))
	}

	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(json.RawMessage(resp.Data))
}
```

Add `import "context"` to `cmd_kill.go` if needed (Cobra context may or may not need it — verify build).

- [ ] **Step 4: Wire into root.**

In `main.go`:

```go
root.AddCommand(
	newListCmd(),
	newGetCmd(),
	newStatusCmd(),
	newSpawnCmd(),
	newResumeCmd(),
	newKillCmd(),
)
```

- [ ] **Step 5: Verify the build compiles. `setActive` won't exist yet — stub it.**

In `cli_helpers.go`, add a stub:

```go
// setActive is a stub until Task 10 implements the active file.
func setActive(childID string) error {
	return nil
}
```

```bash
make build-ctl
```

- [ ] **Step 6: Manual smoke.**

```bash
./bin/pi-controller &
./bin/pi-ctl spawn test-smoke --cwd /tmp --no-session --no-extensions --model fake/dummy --extra-arg "--api-key=" 2>&1 | head -5
./bin/pi-ctl list
./bin/pi-ctl kill test-smoke
```

The spawn may fail without a real pi binary; that's fine, the goal is to confirm the request hits the daemon and gets a typed error response.

- [ ] **Step 7: Commit.**

```bash
git add cmd/pi-ctl/
git commit -m "pi-ctl: spawn/resume/kill lifecycle subcommands"
```

---

### Task 8: Forget subcommand

**Files:**
- Create: `cmd/pi-ctl/cmd_forget.go`
- Modify: `cmd/pi-ctl/main.go`

- [ ] **Step 1: Write `cmd_forget.go`.**

```go
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"

	"graveland.dev/pi-controller/internal/protocol"
)

func newForgetCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "forget [id|name]",
		Short: "Drop an exited child from the controller",
		Long: `Drop an exited child from the controller's in-memory store.
Disk artifacts (logs, state record) are NOT removed.

With --all-exited, forgets every exited child (optionally filtered by --older-than).`,
		Args: cobra.MaximumNArgs(1),
		RunE: runForget,
	}
	cmd.Flags().Bool("all-exited", false, "Forget all exited children")
	cmd.Flags().Duration("older-than", 0, "Only forget exited children older than this")
	return cmd
}

func runForget(cmd *cobra.Command, args []string) error {
	c := mustDial(cmd)
	defer c.Close()

	ctx := cmdCtx(cmd)
	allExited, _ := cmd.Flags().GetBool("all-exited")

	if allExited {
		olderThan, _ := cmd.Flags().GetDuration("older-than")
		req := protocol.ForgetAllExitedRequest{
			Type: protocol.TypeCtrlForgetAllExited,
		}
		if olderThan > 0 {
			req.OlderThanMs = olderThan.Milliseconds()
		}
		resp, err := c.Request(ctx, req)
		if err != nil { return err }
		if !resp.Success {
			return fmt.Errorf("ctrl_forget_all_exited: %s", responseErr(resp))
		}
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(json.RawMessage(resp.Data))
	}

	if len(args) == 0 {
		return fmt.Errorf("forget requires <id|name> or --all-exited")
	}

	childID, err := c.Resolve(ctx, args[0])
	if err != nil { return err }

	resp, err := c.Request(ctx, protocol.ForgetRequest{
		Type:    protocol.TypeCtrlForget,
		ChildID: childID,
	})
	if err != nil { return err }
	if !resp.Success {
		return fmt.Errorf("ctrl_forget: %s", responseErr(resp))
	}
	fmt.Fprintln(os.Stderr, "forgot", childID)
	return nil
}

// Unused import guard
var _ = time.Second
```

- [ ] **Step 2: Wire into root.**

```go
root.AddCommand(
	// ... existing ...
	newForgetCmd(),
)
```

- [ ] **Step 3: Build + smoke.**

```bash
make build-ctl
./bin/pi-ctl forget --help
```

- [ ] **Step 4: Commit.**

```bash
git add cmd/pi-ctl/cmd_forget.go cmd/pi-ctl/main.go
git commit -m "pi-ctl: forget subcommand"
```

---

### Task 9: Recent, search, send subcommands

**Files:**
- Create: `cmd/pi-ctl/cmd_recent.go`
- Create: `cmd/pi-ctl/cmd_search.go`
- Create: `cmd/pi-ctl/cmd_send.go`
- Modify: `cmd/pi-ctl/main.go`

- [ ] **Step 1: Write `cmd_recent.go`.**

```go
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"

	"graveland.dev/pi-controller/internal/protocol"
)

func newRecentCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "recent <id|name>",
		Short: "Show recent events from a child's ring buffer",
		Args:  cobra.ExactArgs(1),
		RunE:  runRecent,
	}
	cmd.Flags().Int("limit", 100, "Maximum number of events")
	cmd.Flags().Duration("since", 0, "Only events newer than this (e.g. 5m)")
	cmd.Flags().StringSlice("include", nil, "Include only these event types (repeatable)")
	cmd.Flags().StringSlice("exclude", nil, "Exclude these event types (repeatable)")
	return cmd
}

func runRecent(cmd *cobra.Command, args []string) error {
	c := mustDial(cmd)
	defer c.Close()

	ctx := cmdCtx(cmd)
	childID, err := c.Resolve(ctx, args[0])
	if err != nil { return err }

	limit, _ := cmd.Flags().GetInt("limit")
	since, _ := cmd.Flags().GetDuration("since")
	include, _ := cmd.Flags().GetStringSlice("include")
	exclude, _ := cmd.Flags().GetStringSlice("exclude")

	req := protocol.GetRecentRequest{
		Type:    protocol.TypeCtrlGetRecent,
		ChildID: childID,
		Limit:   limit,
		Include: include,
		Exclude: exclude,
	}
	if since > 0 {
		req.Since = time.Now().Add(-since).UnixMilli()
	}

	resp, err := c.Request(ctx, req)
	if err != nil { return err }
	if !resp.Success {
		return fmt.Errorf("ctrl_get_recent: %s", responseErr(resp))
	}

	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(json.RawMessage(resp.Data))
}
```

- [ ] **Step 2: Write `cmd_search.go`.**

```go
package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"graveland.dev/pi-controller/internal/protocol"
)

func newSearchCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "search <query>",
		Short: "Search across live children's events",
		Args:  cobra.ExactArgs(1),
		RunE:  runSearch,
	}
	cmd.Flags().Bool("regex", false, "Treat query as a regular expression")
	cmd.Flags().Int("limit", 50, "Maximum hits to return")
	cmd.Flags().Int("context", 2, "Context lines around each hit")
	cmd.Flags().String("cwd-contains", "", "Restrict to children whose cwd contains this")
	cmd.Flags().String("name-contains", "", "Restrict to children whose name contains this")
	return cmd
}

func runSearch(cmd *cobra.Command, args []string) error {
	c := mustDial(cmd)
	defer c.Close()

	regex, _ := cmd.Flags().GetBool("regex")
	limit, _ := cmd.Flags().GetInt("limit")
	context, _ := cmd.Flags().GetInt("context")
	cwd, _ := cmd.Flags().GetString("cwd-contains")
	name, _ := cmd.Flags().GetString("name-contains")

	req := protocol.SearchRequest{
		Type:    protocol.TypeCtrlSearch,
		Query:   args[0],
		Regex:   regex,
		Limit:   limit,
		Context: context,
	}
	if cwd != "" || name != "" {
		req.SessionFilter = protocol.ListRequestFilter{
			CwdContains:  cwd,
			NameContains: name,
		}
	}

	resp, err := c.Request(cmdCtx(cmd), req)
	if err != nil { return err }
	if !resp.Success {
		return fmt.Errorf("ctrl_search: %s", responseErr(resp))
	}

	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(json.RawMessage(resp.Data))
}
```

- [ ] **Step 3: Write `cmd_send.go`.**

```go
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/spf13/cobra"

	"graveland.dev/pi-controller/internal/protocol"
)

func newSendCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "send <id|name> [frame-json]",
		Short: "Send a raw pi-RPC frame to a child",
		Long: `Send a raw pi-RPC frame (debugging or scripting).

If frame-json is omitted, read the frame from stdin.

Example:
  pi-ctl send afk-impl '{"type":"prompt","message":"Hello!"}'`,
		Args: cobra.RangeArgs(1, 2),
		RunE: runSend,
	}
	return cmd
}

func runSend(cmd *cobra.Command, args []string) error {
	c := mustDial(cmd)
	defer c.Close()

	ctx := cmdCtx(cmd)
	childID, err := c.Resolve(ctx, args[0])
	if err != nil { return err }

	var frame json.RawMessage
	if len(args) == 2 {
		frame = json.RawMessage(args[1])
	} else {
		b, err := io.ReadAll(os.Stdin)
		if err != nil { return fmt.Errorf("read stdin: %w", err) }
		frame = json.RawMessage(b)
	}

	// Validate it parses.
	var probe map[string]any
	if err := json.Unmarshal(frame, &probe); err != nil {
		return fmt.Errorf("frame is not valid JSON: %w", err)
	}

	resp, err := c.Request(ctx, protocol.SendRequest{
		Type:    protocol.TypeCtrlSend,
		ChildID: childID,
		Frame:   frame,
	})
	if err != nil { return err }
	if !resp.Success {
		return fmt.Errorf("ctrl_send: %s", responseErr(resp))
	}
	return nil
}
```

- [ ] **Step 4: Wire into root.**

- [ ] **Step 5: Build + smoke.**

```bash
make build-ctl
./bin/pi-ctl --help  # check all subcommands listed
```

- [ ] **Step 6: Commit.**

```bash
git add cmd/pi-ctl/
git commit -m "pi-ctl: recent/search/send subcommands"
```

---

### Task 10: Active file + tab completion

**Files:**
- Create: `cmd/pi-ctl/active.go`
- Modify: `cmd/pi-ctl/cli_helpers.go` (replace stub)
- Modify: `cmd/pi-ctl/cmd_*.go` (add tab completion to subcommands that take `<id|name>`)

- [ ] **Step 1: Write `active.go`.**

```go
package main

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

func activeFilePath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".pi", "run", "active")
}

// setActive writes childID to the active file (best-effort).
func setActive(childID string) error {
	path := activeFilePath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(childID+"\n"), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// getActive reads the active childID, returning "" if absent.
func getActive() string {
	b, err := os.ReadFile(activeFilePath())
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) { return "" }
		return ""
	}
	return strings.TrimSpace(string(b))
}
```

- [ ] **Step 2: Replace the stub in `cli_helpers.go`.**

Remove the stub `setActive`. The real one in `active.go` is now in scope.

Also: add a helper that resolves "id-or-name-or-active" for subcommands that want to fall back to the active marker:

```go
// resolveTarget returns the resolved childID. If input is empty, falls
// back to the active marker; if that's also empty, returns an error.
func resolveTarget(ctx context.Context, c *client.Client, input string) (string, error) {
	if input == "" {
		input = getActive()
		if input == "" {
			return "", fmt.Errorf("no child specified and no active marker; run `pi-ctl list` to see options")
		}
	}
	return c.Resolve(ctx, input)
}
```

- [ ] **Step 3: Make `tail` and `recent` accept a no-argument form that uses the active marker.**

(They currently require `cobra.ExactArgs(1)`. Change those to `cobra.MaximumNArgs(1)` and adjust the body to call `resolveTarget`.)

- [ ] **Step 4: Add Cobra tab completion to subcommands that take child identifiers.**

For each `cobra.Command` that takes `<id|name>` (get, resume, kill, send, recent, tail, logs, forget):

```go
cmd.ValidArgsFunction = func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
	if len(args) > 0 { return nil, cobra.ShellCompDirectiveNoFileComp }
	return completeChildren(cmd, toComplete), cobra.ShellCompDirectiveNoFileComp
}
```

Add `completeChildren` to `cli_helpers.go`:

```go
// completeChildren returns childIds and names that match toComplete.
// Used by Cobra dynamic-completion handlers. Best-effort; returns nil
// on any error (so tab completion gracefully no-ops if the daemon is down).
func completeChildren(cmd *cobra.Command, toComplete string) []string {
	c, err := client.Dial(socketFromCmd(cmd))
	if err != nil { return nil }
	defer c.Close()
	children, err := c.List(cmdCtx(cmd), protocol.ListRequestFilter{})
	if err != nil { return nil }

	var out []string
	for _, ch := range children {
		if strings.HasPrefix(ch.ChildID, toComplete) { out = append(out, ch.ChildID) }
		if ch.Name != "" && strings.HasPrefix(ch.Name, toComplete) { out = append(out, ch.Name) }
	}
	return out
}

func socketFromCmd(cmd *cobra.Command) string {
	s, _ := cmd.Flags().GetString("socket")
	return s
}
```

- [ ] **Step 5: Wire shell completion subcommand.**

Cobra has a built-in `completion` subcommand:

```go
// In newRootCmd, after AddCommand calls:
root.AddCommand(newCompletionCmd())
```

```go
// cmd_completion.go (small)
package main

import (
	"os"
	"github.com/spf13/cobra"
)

func newCompletionCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "completion [bash|zsh|fish|powershell]",
		Short: "Generate shell completion script",
		Long:  `Generate a shell completion script. To enable: source <(pi-ctl completion bash)`,
		DisableFlagsInUseLine: true,
		ValidArgs: []string{"bash", "zsh", "fish", "powershell"},
		Args:      cobra.MatchAll(cobra.ExactArgs(1), cobra.OnlyValidArgs),
		Run: func(cmd *cobra.Command, args []string) {
			switch args[0] {
			case "bash":       _ = cmd.Root().GenBashCompletion(os.Stdout)
			case "zsh":        _ = cmd.Root().GenZshCompletion(os.Stdout)
			case "fish":       _ = cmd.Root().GenFishCompletion(os.Stdout, true)
			case "powershell": _ = cmd.Root().GenPowerShellCompletionWithDesc(os.Stdout)
			}
		},
	}
	return cmd
}
```

- [ ] **Step 6: Build + smoke.**

```bash
make build-ctl
./bin/pi-ctl spawn afk --cwd /tmp --no-session --model fake/x
./bin/pi-ctl get  # without arg, uses active
./bin/pi-ctl recent --limit 5
```

- [ ] **Step 7: Commit.**

```bash
git add cmd/pi-ctl/
git commit -m "pi-ctl: active marker + tab completion"
```

---

### Task 11: Tail subcommand + event renderer

**Files:**
- Create: `cmd/pi-ctl/cmd_tail.go`
- Create: `cmd/pi-ctl/render_tail.go`
- Create: `cmd/pi-ctl/render_tail_test.go`
- Modify: `cmd/pi-ctl/main.go`

This is the biggest CLI task. Subscribes to events, renders them as the controller streams.

- [ ] **Step 1: Write `cmd_tail.go` — subscribe + render loop.**

```go
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"graveland.dev/pi-controller/internal/protocol"
)

func newTailCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "tail [id|name]",
		Short: "Stream events from a child",
		Args:  cobra.MaximumNArgs(1),
		RunE:  runTail,
	}
	cmd.Flags().String("profile", "", "Subscription profile: firehose|results|coarse|lifecycle")
	cmd.Flags().StringSlice("include", nil, "Add event types to subscription (repeatable)")
	cmd.Flags().StringSlice("exclude", nil, "Exclude event types from subscription (repeatable)")
	cmd.Flags().Bool("no-deltas", true, "Suppress token-by-token message_update deltas (default true)")
	return cmd
}

func runTail(cmd *cobra.Command, args []string) error {
	c := mustDial(cmd)
	defer c.Close()

	ctx, cancel := context.WithCancel(cmdCtx(cmd))
	defer cancel()

	// SIGINT/SIGTERM cancel the context.
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		select {
		case <-sigs:
			cancel()
		case <-ctx.Done():
		}
	}()

	target := ""
	if len(args) > 0 { target = args[0] }
	childID, err := resolveTarget(ctx, c, target)
	if err != nil { return err }
	_ = setActive(childID)

	events, cancelSub, err := c.Subscribe()
	if err != nil { return err }
	defer cancelSub()

	profile, _ := cmd.Flags().GetString("profile")
	include, _ := cmd.Flags().GetStringSlice("include")
	exclude, _ := cmd.Flags().GetStringSlice("exclude")
	noDeltas, _ := cmd.Flags().GetBool("no-deltas")
	if noDeltas {
		exclude = append(exclude, "message_update")
	}

	subReq := protocol.SubscribeRequest{
		Type:    protocol.TypeCtrlSubscribe,
		ChildID: childID,
	}
	if profile != "" || len(include) > 0 || len(exclude) > 0 {
		subReq.Filter = &protocol.SubscribeFilter{
			Profile: profile,
			Include: include,
			Exclude: exclude,
		}
	}

	resp, err := c.Request(ctx, subReq)
	if err != nil { return err }
	if !resp.Success {
		return fmt.Errorf("ctrl_subscribe: %s", responseErr(resp))
	}

	mode, useColor := outputOpts(cmd)
	renderer := newTailRenderer(os.Stdout, useColor, mode)

	for {
		select {
		case frame, ok := <-events:
			if !ok { return nil }
			if err := renderer.render(frame); err != nil {
				fmt.Fprintln(os.Stderr, "render error:", err)
			}
		case <-ctx.Done():
			return nil
		}
	}
}

// Unused import guard
var _ = json.RawMessage(nil)
```

- [ ] **Step 2: Write `render_tail.go` — the event renderer.**

```go
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"graveland.dev/pi-controller/internal/protocol"
)

// tailRenderer formats incoming event frames (raw bytes) onto w.
// Handles the ctrl_event wrapper plus ctrl_child_* lifecycle events.
type tailRenderer struct {
	w         io.Writer
	useColor  bool
	mode      outputMode
}

func newTailRenderer(w io.Writer, useColor bool, mode outputMode) *tailRenderer {
	return &tailRenderer{w: w, useColor: useColor, mode: mode}
}

// render writes a human-readable representation of frame to w.
// In JSON mode, just pretty-prints the frame.
func (r *tailRenderer) render(frame []byte) error {
	if r.mode == outputJSON {
		var v any
		if err := json.Unmarshal(frame, &v); err != nil {
			fmt.Fprintln(r.w, string(frame))
			return nil
		}
		b, _ := json.Marshal(v)
		fmt.Fprintln(r.w, string(b))
		return nil
	}

	var hdr struct {
		Type    string          `json:"type"`
		ChildID string          `json:"childId"`
		Event   json.RawMessage `json:"event,omitempty"`

		// ctrl_child_* fields
		Status   string `json:"status,omitempty"`
		Previous string `json:"previous,omitempty"`
		ExitCode *int   `json:"exitCode,omitempty"`
		Signal   string `json:"signal,omitempty"`
		Name     string `json:"name,omitempty"`
	}
	if err := json.Unmarshal(frame, &hdr); err != nil {
		// Malformed; print raw.
		fmt.Fprintln(r.w, string(frame))
		return nil
	}

	switch hdr.Type {
	case protocol.TypeCtrlEvent:
		return r.renderPiEvent(hdr.Event)
	case protocol.TypeCtrlChildStatus:
		r.printDim(fmt.Sprintf("[%s] status: %s → %s",
			time.Now().Format("15:04:05"), hdr.Previous, hdr.Status))
	case protocol.TypeCtrlChildExited:
		code := "?"
		if hdr.ExitCode != nil { code = fmt.Sprintf("%d", *hdr.ExitCode) }
		divider := "─── child exited"
		if hdr.Signal != "" { divider += fmt.Sprintf(" (signal %s)", hdr.Signal) }
		divider += fmt.Sprintf(" (code %s) ───", code)
		r.printRed(divider)
	case protocol.TypeCtrlChildSpawned:
		r.printDim(fmt.Sprintf("─── child spawned (%s) ───", hdr.ChildID))
	case protocol.TypeCtrlChildRenamed:
		r.printDim(fmt.Sprintf("[rename] %s → %s", hdr.Previous, hdr.Name))
	default:
		fmt.Fprintln(r.w, string(frame))
	}
	return nil
}

func (r *tailRenderer) renderPiEvent(event json.RawMessage) error {
	var hdr struct {
		Type    string          `json:"type"`
		Reason  string          `json:"reason,omitempty"`
		Message json.RawMessage `json:"message,omitempty"`
		ToolResults json.RawMessage `json:"toolResults,omitempty"`

		// tool_execution_*
		ToolName string `json:"toolName,omitempty"`
		ToolCallID string `json:"toolCallId,omitempty"`
		IsError  bool   `json:"isError,omitempty"`
	}
	if err := json.Unmarshal(event, &hdr); err != nil {
		fmt.Fprintln(r.w, string(event))
		return nil
	}

	switch hdr.Type {
	case "agent_start":
		r.printDim("─── agent_start ───")
	case "agent_end":
		fmt.Fprintln(r.w, "")
		r.printDim("─── agent_end ───")
	case "turn_end":
		r.renderTurnEnd(hdr.Message, hdr.ToolResults)
	case "tool_execution_start":
		fmt.Fprintf(r.w, "  %s %s\n", r.cyan("↻"), hdr.ToolName)
	case "tool_execution_end":
		mark := "✓"
		fn := r.green
		if hdr.IsError { mark = "✗"; fn = r.red }
		fmt.Fprintf(r.w, "  %s %s\n", fn(mark), hdr.ToolName)
	case "extension_ui_request":
		var ui struct {
			Method  string `json:"method"`
			Title   string `json:"title"`
			Message string `json:"message"`
		}
		_ = json.Unmarshal(event, &ui)
		r.printYellow(fmt.Sprintf("❓ %s: %s", ui.Method, ui.Title))
		if ui.Message != "" {
			fmt.Fprintln(r.w, "  "+ui.Message)
		}
	case "compaction_start":
		r.printDim("─── compaction_start ───")
	case "compaction_end":
		r.printDim("─── compaction_end ───")
	case "auto_retry_start":
		r.printDim("[auto-retry]")
	default:
		// Fallback: print event type + raw JSON (compact)
		var compact bytes
		_ = json.Compact(&compact, event)
		r.printDim(fmt.Sprintf("[%s] %s", hdr.Type, compact.String()))
	}
	return nil
}

func (r *tailRenderer) renderTurnEnd(messageRaw, toolResultsRaw json.RawMessage) {
	if len(messageRaw) == 0 { return }
	var msg struct {
		Role    string `json:"role"`
		Content json.RawMessage `json:"content,omitempty"`
	}
	if err := json.Unmarshal(messageRaw, &msg); err != nil { return }

	// Extract text content blocks.
	var content []map[string]any
	if err := json.Unmarshal(msg.Content, &content); err == nil {
		var text strings.Builder
		for _, block := range content {
			if t, _ := block["type"].(string); t == "text" {
				if s, _ := block["text"].(string); s != "" {
					text.WriteString(s)
				}
			}
		}
		if text.Len() > 0 {
			fmt.Fprintln(r.w, text.String())
		}
	} else if msg.Content != nil {
		// content may be a string (legacy / partial)
		var s string
		if err := json.Unmarshal(msg.Content, &s); err == nil && s != "" {
			fmt.Fprintln(r.w, s)
		}
	}
}

// Color helpers — use the existing ones from output.go.
func (r *tailRenderer) printDim(s string)     { if r.useColor { s = dim(s) }; fmt.Fprintln(r.w, s) }
func (r *tailRenderer) printRed(s string)     { if r.useColor { s = red(s) }; fmt.Fprintln(r.w, s) }
func (r *tailRenderer) printYellow(s string)  { if r.useColor { s = yellow(s) }; fmt.Fprintln(r.w, s) }
func (r *tailRenderer) red(s string) string   { if r.useColor { return red(s) }; return s }
func (r *tailRenderer) green(s string) string { if r.useColor { return green(s) }; return s }
func (r *tailRenderer) cyan(s string) string  { if r.useColor { return cyan(s) }; return s }

// Add `import "bytes"` if missing (used in default case).
type bytes = strings.Builder
```

Note: that last `type bytes = strings.Builder` is wrong — replace with proper `import "bytes"` and use `bytes.Buffer`. Cleanup:

```go
import "bytes"

// in renderPiEvent default case:
var compact bytes.Buffer
_ = json.Compact(&compact, event)
r.printDim(fmt.Sprintf("[%s] %s", hdr.Type, compact.String()))
```

- [ ] **Step 3: Write `render_tail_test.go`.**

```go
package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestRender_AgentStartEnd(t *testing.T) {
	var buf bytes.Buffer
	r := newTailRenderer(&buf, false, outputTable)

	r.render([]byte(`{"type":"ctrl_event","childId":"c_x","event":{"type":"agent_start"}}`))
	r.render([]byte(`{"type":"ctrl_event","childId":"c_x","event":{"type":"agent_end"}}`))

	out := buf.String()
	if !strings.Contains(out, "agent_start") { t.Fatalf("missing agent_start: %s", out) }
	if !strings.Contains(out, "agent_end") { t.Fatalf("missing agent_end: %s", out) }
}

func TestRender_ToolExecution(t *testing.T) {
	var buf bytes.Buffer
	r := newTailRenderer(&buf, false, outputTable)
	r.render([]byte(`{"type":"ctrl_event","childId":"c_x","event":{"type":"tool_execution_start","toolName":"bash"}}`))
	r.render([]byte(`{"type":"ctrl_event","childId":"c_x","event":{"type":"tool_execution_end","toolName":"bash","isError":false}}`))
	out := buf.String()
	if !strings.Contains(out, "bash") {
		t.Fatalf("missing tool name: %s", out)
	}
}

func TestRender_ChildExited(t *testing.T) {
	var buf bytes.Buffer
	r := newTailRenderer(&buf, false, outputTable)
	r.render([]byte(`{"type":"ctrl_child_exited","childId":"c_x","exitCode":0}`))
	out := buf.String()
	if !strings.Contains(out, "child exited") {
		t.Fatalf("missing 'child exited': %s", out)
	}
}

func TestRender_JSONMode_PassThrough(t *testing.T) {
	var buf bytes.Buffer
	r := newTailRenderer(&buf, false, outputJSON)
	r.render([]byte(`{"type":"ctrl_event","childId":"c_x","event":{"type":"agent_start"}}`))
	out := buf.String()
	if !strings.Contains(out, `"agent_start"`) {
		t.Fatalf("JSON not passed through: %s", out)
	}
}
```

- [ ] **Step 4: Wire into root, build, smoke.**

```bash
make build-ctl
./bin/pi-ctl tail --help
```

- [ ] **Step 5: Manual smoke against a real daemon.**

```bash
./bin/pi-controller &
./bin/pi-ctl spawn smoke --cwd /tmp --no-session --model fake/x
./bin/pi-ctl tail smoke &
# In another terminal:
./bin/pi-ctl send smoke '{"type":"prompt","message":"Hello"}'
# Observe events streaming in the tail.
./bin/pi-ctl kill smoke
```

- [ ] **Step 6: Commit.**

```bash
git add cmd/pi-ctl/
git commit -m "pi-ctl: tail subcommand with event-stream renderer"
```

---

### Task 12: Logs subcommand + integration tests

**Files:**
- Create: `cmd/pi-ctl/cmd_logs.go`
- Create: `test/integration/cli_integration_test.go`
- Modify: `cmd/pi-ctl/main.go`

- [ ] **Step 1: Write `cmd_logs.go`.**

```go
package main

import (
	"compress/gzip"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
)

func newLogsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "logs <id|name>",
		Short: "Show the on-disk log location for a child",
		Args:  cobra.MaximumNArgs(1),
		RunE:  runLogs,
	}
	cmd.Flags().Bool("cat", false, "Print the contents of out.jsonl.gz to stdout")
	cmd.Flags().Bool("in", false, "Cat in.jsonl.gz instead of out.jsonl.gz")
	cmd.Flags().Bool("err", false, "Cat err.log.gz instead of out.jsonl.gz")
	return cmd
}

func runLogs(cmd *cobra.Command, args []string) error {
	c := mustDial(cmd)
	defer c.Close()

	target := ""
	if len(args) > 0 { target = args[0] }
	childID, err := resolveTarget(cmdCtx(cmd), c, target)
	if err != nil { return err }

	home, _ := os.UserHomeDir()
	logsDir := filepath.Join(home, ".pi", "run", "logs", childID)

	if _, err := os.Stat(logsDir); os.IsNotExist(err) {
		return fmt.Errorf("no logs at %s (child still alive, or persistence mode is `never`)", logsDir)
	}

	wantIn, _ := cmd.Flags().GetBool("in")
	wantErr, _ := cmd.Flags().GetBool("err")
	wantCat, _ := cmd.Flags().GetBool("cat")

	if !wantCat {
		fmt.Println(logsDir)
		return nil
	}

	file := "out.jsonl.gz"
	if wantIn { file = "in.jsonl.gz" }
	if wantErr { file = "err.log.gz" }

	path := filepath.Join(logsDir, file)
	f, err := os.Open(path)
	if err != nil { return err }
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil { return err }
	defer gz.Close()
	_, err = io.Copy(os.Stdout, gz)
	return err
}
```

- [ ] **Step 2: Wire into root.**

```go
root.AddCommand(
	// ... existing ...
	newLogsCmd(),
)
```

- [ ] **Step 3: Write integration tests in `test/integration/cli_integration_test.go`.**

```go
package integration_test

import (
	"bytes"
	"encoding/json"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// piCtlBin returns the absolute path to the built pi-ctl binary.
// Assumes `make build` has run; TestMain in this package handles it.
func piCtlBin(t *testing.T) string {
	t.Helper()
	repoRoot := findRepoRoot(t)
	bin := filepath.Join(repoRoot, "bin", "pi-ctl")
	return bin
}

func TestCLI_Status(t *testing.T) {
	d := bootDaemon(t)
	defer d.Stop()

	cmd := exec.Command(piCtlBin(t), "--socket", d.socket, "status")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("status failed: %v\noutput: %s", err, out)
	}
	if !strings.Contains(string(out), `"version"`) {
		t.Fatalf("status output missing version: %s", out)
	}
}

func TestCLI_SpawnListKillForget(t *testing.T) {
	d := bootDaemon(t)
	defer d.Stop()

	// spawn
	spawn := exec.Command(piCtlBin(t),
		"--socket", d.socket,
		"--output", "json",
		"spawn", "smoke",
		"--cwd", "/tmp",
		"--no-session",
		"--model", "fake/dummy",
		"--no-extensions",
	)
	out, err := spawn.CombinedOutput()
	if err != nil { t.Fatalf("spawn failed: %v\n%s", err, out) }

	var spawnResp struct{ ChildID string `json:"childId"` }
	if err := json.Unmarshal(out, &spawnResp); err != nil {
		t.Fatalf("decode spawn response: %v\n%s", err, out)
	}
	childID := spawnResp.ChildID

	// list — child should be present
	list := exec.Command(piCtlBin(t), "--socket", d.socket, "--output", "json", "list")
	out, err = list.CombinedOutput()
	if err != nil { t.Fatalf("list failed: %v\n%s", err, out) }
	if !strings.Contains(string(out), childID) {
		t.Fatalf("list missing %s: %s", childID, out)
	}

	// kill
	kill := exec.Command(piCtlBin(t), "--socket", d.socket, "kill", "smoke")
	out, err = kill.CombinedOutput()
	if err != nil { t.Fatalf("kill failed: %v\n%s", err, out) }

	// Wait for status=exited (best-effort polling)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		get := exec.Command(piCtlBin(t), "--socket", d.socket, "get", "smoke")
		out, _ = get.CombinedOutput()
		if strings.Contains(string(out), `"status":"exited"`) { break }
		time.Sleep(100 * time.Millisecond)
	}

	// forget
	forget := exec.Command(piCtlBin(t), "--socket", d.socket, "forget", "smoke")
	out, err = forget.CombinedOutput()
	if err != nil { t.Fatalf("forget failed: %v\n%s", err, out) }
}

func TestCLI_ResolveByPrefix(t *testing.T) {
	d := bootDaemon(t)
	defer d.Stop()

	spawn := exec.Command(piCtlBin(t),
		"--socket", d.socket, "--output", "json",
		"spawn", "afk-impl",
		"--cwd", "/tmp",
		"--no-session",
		"--no-extensions",
		"--model", "fake/dummy",
	)
	if out, err := spawn.CombinedOutput(); err != nil {
		t.Fatalf("spawn: %v\n%s", err, out)
	}

	// resolve by prefix "afk"
	get := exec.Command(piCtlBin(t), "--socket", d.socket, "get", "afk")
	out, err := get.CombinedOutput()
	if err != nil { t.Fatalf("get with prefix: %v\n%s", err, out) }
	if !strings.Contains(string(out), "afk-impl") {
		t.Fatalf("expected afk-impl in get output: %s", out)
	}

	// cleanup
	kill := exec.Command(piCtlBin(t), "--socket", d.socket, "kill", "afk-impl")
	_, _ = kill.CombinedOutput()
}
```

Note: `bootDaemon`, `d.socket`, `d.Stop`, `findRepoRoot` are reused from the existing daemon integration test. If they're in `integration_test.go`, extract to a shared helper file or inline as needed.

- [ ] **Step 4: TestMain to build the binary.**

If not already present, add to `integration_test.go`:

```go
var binDir string

func TestMain(m *testing.M) {
	tmpDir, err := os.MkdirTemp("", "pi-bin")
	if err != nil { panic(err) }
	binDir = tmpDir
	defer os.RemoveAll(tmpDir)

	repoRoot := /* find repo root */

	// Build both binaries.
	for _, cmd := range []string{"pi-controller", "pi-ctl"} {
		buildCmd := exec.Command("go", "build", "-o",
			filepath.Join(binDir, cmd),
			"./cmd/"+cmd)
		buildCmd.Dir = repoRoot
		if out, err := buildCmd.CombinedOutput(); err != nil {
			panic(fmt.Sprintf("build %s: %v\n%s", cmd, err, out))
		}
	}
	os.Exit(m.Run())
}
```

The existing `integration_test.go` already does this for `pi-controller`; just add the `pi-ctl` build.

- [ ] **Step 5: Run integration tests.**

```bash
go test -race ./test/integration/... -count=1
```

All tests should pass.

- [ ] **Step 6: Commit.**

```bash
git add cmd/pi-ctl/cmd_logs.go test/integration/cli_integration_test.go test/integration/integration_test.go
git commit -m "pi-ctl: logs subcommand + end-to-end CLI integration tests"
```

---

## Verification before declaring done

- [ ] **All tests pass with `-race`.**

```bash
make test-race
```

- [ ] **`go vet ./...` is clean.**

- [ ] **`make build` produces both binaries cleanly.**

```bash
make build
ls -la bin/
```

Both `bin/pi-controller` and `bin/pi-ctl` exist.

- [ ] **Manual end-to-end smoke against real pi.**

```bash
./bin/pi-controller &
./bin/pi-ctl status
./bin/pi-ctl spawn smoke --cwd /tmp --no-extensions --model anthropic/claude-haiku-4-5
./bin/pi-ctl list
./bin/pi-ctl tail smoke &
./bin/pi-ctl send smoke '{"type":"prompt","message":"Say hi"}'
sleep 10
./bin/pi-ctl kill smoke
./bin/pi-ctl logs smoke --cat | head -5
./bin/pi-ctl forget smoke
kill %1
```

Expect: spawn returns a childId; list shows the child; tail prints the agent's response when prompt is sent; kill drains; logs print the captured pi events.

- [ ] **Commit any final touch-ups.**

---

## Self-review notes

Items deliberately *not* covered by this plan:

- **`pi-attach` (the thin-TUI client)** — separate document, not v1.
- **Multi-host UDS forwarding** — out of scope; use SSH if needed.
- **Profile expansion for tail** — daemon-side limitation (profile names are accepted but not yet expanded to event sets per the daemon's v1 limitations doc); the CLI passes them through unchanged.
- **Rich markdown rendering in tail** — minimum-viable text output only; full Markdown is `pi-attach` territory.
- **Output as YAML** — JSON+table is enough for v1.

Items that should be tracked but are intentionally deferred from this v1:

- Tab completion via `Cobra`'s autocomplete framework currently does one ctrl_list per Tab press; for many children this is slow. A short-lived cache (~1s TTL) in pi-ctl would help.
- `pi-ctl recent` doesn't pretty-print — it dumps raw JSON. Could route through the same tail renderer.
- `pi-ctl spawn`'s flag set is incomplete relative to the full §6.3 spawn schema (no `--skill`, no `--theme`, etc.); add as needed.
- No `pi-ctl wait` command for "block until child status == X" — useful for scripting, easy to add.
