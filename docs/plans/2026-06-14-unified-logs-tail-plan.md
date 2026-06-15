# Unified `logs`/`tail` + history backfill — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make `pic logs` and `pic tail` two presets of one history engine (follow is the only real difference), give both rendered-by-default output with a `--raw` escape hatch and `-n` history backfill, serve logs while a child is still running, and show prior transcript scrollback when attaching — all without forking pi.

**Architecture:** A child's event history already lives in one place: the daemon's per-child ring buffer (`internal/ring`) while alive, `ExitedRing`/on-disk dump after exit, exposed by the existing `ctrl_get_recent` RPC for the `out` event stream. We add one small RPC (`ctrl_get_streams`) for the live `in`/`err` raw streams, a shared `runHistory` engine in `cmd/pic` that both `tail` and `logs` delegate to, and a scrollback-priming step in pic-attach's own bun runtime (`attach/src`) that replays retained events through its existing event-handling path before the TUI renders.

**Tech Stack:** Go (daemon + `pic` CLI, cobra, JSONL-over-UDS), TypeScript/Bun (`pic-attach` TUI client). Tests: Go `testing`, Bun `bun test`.

---

## File Structure

**Go — daemon (`internal/`, `cmd/pi-controller/`)**
- `protocol/types.go` — add `GetStreamsRequest`, `GetStreamsResponseData`, `TypeCtrlGetStreams` const.
- `internal/server/dispatch.go` — add `GetStreams` to the `Controller` interface, a `getStreams` handler, and a switch case.
- `cmd/pi-controller/controller.go` — implement `Controller.GetStreams` (live: in-memory snapshots; exited: returns "not live" so the CLI falls back to disk).

**Go — `pic` CLI (`cmd/pic/`)**
- `cmd/pic/history.go` (new) — shared `runHistory` engine + `dedupeBoundary` pure helper.
- `cmd/pic/cmd_tail.go` — becomes a thin preset (follow on, `-n 20`) delegating to `runHistory`.
- `cmd/pic/cmd_logs.go` — becomes a thin preset (follow off, `-n all`, rendered default) delegating to `runHistory`; keeps `--raw`/`--in`/`--err`/`--all`/`--path` and serves live children.
- Tests: `cmd/pic/history_test.go` (new), extend `cmd/pic/cmd_tail_test.go`.

**Bun — `pic-attach` (`attach/src/`)**
- `attach/src/client.ts` — add `getRecent(childId, limit)`.
- `attach/src/session.ts` — register the event iterator in the constructor without looping; add `primeHistory(limit)` and `start()`.
- `attach/src/runtime.ts` — in `connect()`, after `ctrl_subscribe`: `primeHistory` then `start`.
- `cmd/pic/cmd_attach.go` — add `-n/--tail` flag, pass depth to `pic-attach` via `PIC_ATTACH_TAIL` env.
- `cmd/pic/cli_helpers.go` — `execPicAttach` passes `PIC_ATTACH_TAIL` through.
- Tests: extend `attach/src/session.test.ts`.

**`-n` semantics (used everywhere):** `-1` = all available, `0` = none (pure live), `N>0` = last N. Maps to `ctrl_get_recent`'s `Limit` field: `-1 → 0` (ring "no limit"), `N>0 → N`, `0 → skip the fetch entirely`.

---

## Task 1: Daemon `ctrl_get_streams` RPC

Serves the live in-memory `in`/`err` raw streams so `pic logs --in/--err` works while a child runs. `out` is already covered by `ctrl_get_recent`; exited children keep using the on-disk path in the CLI.

**Files:**
- Modify: `protocol/types.go` (add request/response types + type const near line 31 / 293 / 414)
- Modify: `internal/server/dispatch.go` (interface line 65-115, switch line 147-186, new handler near line 328)
- Modify: `cmd/pi-controller/controller.go` (new method after `GetRecent`, line ~227)
- Test: `internal/server/dispatch_test.go`

- [ ] **Step 1: Add protocol types**

In `protocol/types.go`, add the type constant alongside the other `TypeCtrl*` consts (after line 31, `TypeCtrlGetRecent`):

```go
	TypeCtrlGetStreams        = "ctrl_get_streams"
```

After `GetRecentResponseData` (line 414), add:

```go
// GetStreamsRequest queries a live child's in-memory stdin/stderr capture.
// Which selects the streams: "in", "err", or "all".
type GetStreamsRequest struct {
	Type    string `json:"type"`
	ID      string `json:"id,omitempty"`
	ChildID string `json:"childId"`
	Which   string `json:"which,omitempty"` // "in" | "err" | "all"; default "all"
}

// GetStreamsResponseData carries raw, uncompressed stream bytes for a live
// child. In holds stdin frames (one []byte per frame, no trailing newline);
// Err holds raw stderr bytes. Alive is false when the child has already
// exited, signalling the caller to fall back to the on-disk dump.
type GetStreamsResponseData struct {
	Alive bool     `json:"alive"`
	In    [][]byte `json:"in,omitempty"`
	Err   []byte   `json:"err,omitempty"`
}
```

- [ ] **Step 2: Write the failing dispatch test**

In `internal/server/dispatch_test.go`, find the existing fake controller (search for the type implementing `Controller`) and add a `GetStreams` method plus a configurable result field. First add to the fake's struct and methods:

```go
// (add field to the fake controller struct used in dispatch_test.go)
getStreamsResult server.GetStreamsResult
getStreamsErr    error

func (f *fakeController) GetStreams(childID string, which string) (server.GetStreamsResult, error) {
	if f.getStreamsErr != nil {
		return server.GetStreamsResult{}, f.getStreamsErr
	}
	return f.getStreamsResult, nil
}
```

Then add the test:

```go
func TestDispatchGetStreams(t *testing.T) {
	fc := &fakeController{getStreamsResult: server.GetStreamsResult{
		Alive: true,
		In:    [][]byte{[]byte(`{"type":"user_input"}`)},
		Err:   []byte("boom\n"),
	}}
	d := server.NewDispatch(fc)
	req := []byte(`{"type":"ctrl_get_streams","id":"x1","childId":"child-1","which":"all"}`)
	resp := d.HandleFrame(nil, req)

	var got protocol.Response
	if err := json.Unmarshal(resp, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !got.Success {
		t.Fatalf("expected success, got error: %+v", got.Error)
	}
	var data protocol.GetStreamsResponseData
	if err := json.Unmarshal(got.Data, &data); err != nil {
		t.Fatalf("unmarshal data: %v", err)
	}
	if !data.Alive || len(data.In) != 1 || string(data.Err) != "boom\n" {
		t.Fatalf("unexpected data: %+v", data)
	}
}
```

- [ ] **Step 3: Run the test to verify it fails**

Run: `cd ~/home/pi-controller && go test ./internal/server/ -run TestDispatchGetStreams`
Expected: FAIL — `GetStreams` not in `Controller` interface / undefined `server.GetStreamsResult`.

- [ ] **Step 4: Add the interface method, result alias, handler, and switch case**

In `internal/server/dispatch.go`, add the result alias near the other aliases (after line 56):

```go
// GetStreamsResult is returned by Controller.GetStreams.
type GetStreamsResult = protocol.GetStreamsResponseData
```

Add to the `Controller` interface (in the read-only queries block, after `GetRecent`, line 69):

```go
	// GetStreams returns a live child's in-memory stdin/stderr capture.
	// Returns Alive:false (no error) when the child has exited.
	GetStreams(childID string, which string) (GetStreamsResult, error)
```

Add the switch case (after the `TypeCtrlGetRecent` case, line 163):

```go
	case protocol.TypeCtrlGetStreams:
		return d.getStreams(frame, hdr.ID)
```

Add the handler (after `getRecent`, near line 328):

```go
func (d *dispatcher) getStreams(frame []byte, id string) []byte {
	var req protocol.GetStreamsRequest
	if err := json.Unmarshal(frame, &req); err != nil {
		return errResponse(protocol.TypeCtrlGetStreams, id, protocol.ErrInvalidArgs, "malformed request")
	}
	if req.ChildID == "" {
		return errResponse(protocol.TypeCtrlGetStreams, id, protocol.ErrInvalidArgs, "childId required")
	}
	result, err := d.c.GetStreams(req.ChildID, req.Which)
	if err != nil {
		return mapErr(protocol.TypeCtrlGetStreams, id, err, protocol.ErrChildNotFound)
	}
	return okResponse(protocol.TypeCtrlGetStreams, id, result)
}
```

- [ ] **Step 5: Implement `Controller.GetStreams`**

In `cmd/pi-controller/controller.go`, after `GetRecent` (line ~227), add:

```go
func (c *Controller) GetStreams(childID string, which string) (server.GetStreamsResult, error) {
	if _, ok := c.st.Get(childID); !ok {
		return server.GetStreamsResult{}, &server.ControllerError{
			Code:    protocol.ErrChildNotFound,
			Message: "child not found: " + childID,
		}
	}
	ch, alive := c.cm.Get(childID)
	if !alive {
		// Exited: the CLI falls back to the on-disk dump.
		return server.GetStreamsResult{Alive: false}, nil
	}
	res := server.GetStreamsResult{Alive: true}
	if which == "" || which == "all" || which == "in" {
		res.In = ch.InSnapshot()
	}
	if which == "" || which == "all" || which == "err" {
		res.Err = ch.StderrSnapshot()
	}
	return res, nil
}
```

Note: `StderrSnapshot`'s doc-comment says "must only be called after Done()" due to a race with `readStderr`. Confirm during implementation whether reading it on a live child is safe; if `errBuf` writes are not mutex-guarded, guard the read or copy under the child's lock. If unsafe and not easily fixed, scope this RPC to `in` only and have `--err` on a live child print a clear "stderr available after exit" notice. Update the test accordingly.

- [ ] **Step 6: Run the tests to verify they pass**

Run: `cd ~/home/pi-controller && go test ./internal/server/ ./cmd/pi-controller/ -run 'GetStreams|GetRecent'`
Expected: PASS.

- [ ] **Step 7: Vet and commit**

Run: `cd ~/home/pi-controller && go vet ./...`
Expected: no output.

```bash
cd ~/home/pi-controller
git add protocol/types.go internal/server/dispatch.go internal/server/dispatch_test.go cmd/pi-controller/controller.go
git commit -m "daemon: add ctrl_get_streams RPC for live stdin/stderr capture"
```

---

## Task 2: History engine core (`runHistory`) + shape-correct rendering & dedup

The shared engine both `tail` and `logs` will delegate to. Wiring into the cobra commands is Tasks 3–4.

> **Frame-shape reality (verified against the code — do not skip):**
> - The ring (`internal/child/child.go:402-409`) stores the **raw inner child frame**; `ctrl_get_recent` returns those raw inner frames verbatim. The **bus** (live subscribe) delivers them wrapped in a **`ctrl_event` envelope** (`{type:"ctrl_event", childId, event:{...}}`).
> - `tailRenderer.render()` routes by the **envelope** type — `ctrl_event` → `renderPiEvent(hdr.Event)` (render_tail.go:109-110). A raw inner frame fed to `render()` falls through to the `default` case and prints unrendered. **Therefore backfill frames must be rendered with `renderPiEvent(frame)` directly; live frames go through `render(frame)`.**
> - Dedup must compare **inner-to-inner**: unwrap the live envelope's `event` and compare to the raw backfill bytes.
> - **Provider caveat:** for **pi** children the raw inner frame *is* the normalized pi event (`PiProvider.BusFrames` is the identity), so backfill renders identically to live. For **claude** children the ring holds raw stream-json and the pi-normalized view is produced by a **stateful** translator (`provider_claude_state.go`) that only runs live — it cannot be replayed over the ring. So backfilled claude frames render raw (their `type` names don't match `renderPiEvent`'s switch). Acceptable for v1; see the caveat section.

**Files:**
- Create: `cmd/pic/history.go`
- Test: `cmd/pic/history_test.go`

- [ ] **Step 1: Write the failing test for `innerEvent`**

Create `cmd/pic/history_test.go`:

```go
package main

import "testing"

func TestInnerEvent(t *testing.T) {
	// A ctrl_event envelope (live shape) → returns the inner event bytes verbatim.
	env := []byte(`{"type":"ctrl_event","childId":"c1","event":{"type":"message_end","x":1}}`)
	if got, want := string(innerEvent(env)), `{"type":"message_end","x":1}`; got != want {
		t.Fatalf("innerEvent(envelope) = %s, want %s", got, want)
	}
	// A raw inner frame (backfill shape) → returned unchanged.
	raw := []byte(`{"type":"message_end","x":1}`)
	if got := string(innerEvent(raw)); got != string(raw) {
		t.Fatalf("innerEvent(raw) = %s, want unchanged", got)
	}
	// Non-ctrl_event envelope (e.g. lifecycle) → returned unchanged.
	life := []byte(`{"type":"ctrl_child_exited","childId":"c1","exitCode":0}`)
	if got := string(innerEvent(life)); got != string(life) {
		t.Fatalf("innerEvent(lifecycle) = %s, want unchanged", got)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `cd ~/home/pi-controller && go test ./cmd/pic/ -run TestInnerEvent`
Expected: FAIL — `innerEvent` undefined.

- [ ] **Step 3: Implement `history.go`**

Create `cmd/pic/history.go`:

```go
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"git.graveland.dev/brent/pi-controller/client"
	"git.graveland.dev/brent/pi-controller/protocol"
)

// historyOpts configures runHistoryOut. Defaults differ by frontend:
// tail sets follow=true,tailN=20; logs sets follow=false,tailN=-1.
type historyOpts struct {
	follow   bool
	tailN    int // -1 = all, 0 = none, >0 = last N
	raw      bool
	profile  string
	include  []string
	exclude  []string
	verbose  bool
	mode     outputMode
	useColor bool
}

// innerEvent returns the inner pi-event bytes of a frame. Live frames are
// ctrl_event envelopes carrying the event under "event"; backfill frames from
// ctrl_get_recent are already the raw inner event. Returns the frame unchanged
// when it is not a ctrl_event envelope (e.g. lifecycle frames).
func innerEvent(frame []byte) []byte {
	var hdr struct {
		Type  string          `json:"type"`
		Event json.RawMessage `json:"event,omitempty"`
	}
	if err := json.Unmarshal(frame, &hdr); err != nil {
		return frame
	}
	if hdr.Type == protocol.TypeCtrlEvent && len(hdr.Event) > 0 {
		return hdr.Event
	}
	return frame
}

// fetchBackfill returns the last-N (per opts.tailN) out-stream event frames for
// childID via ctrl_get_recent. Frames are raw inner pi events. Returns nil when
// tailN == 0.
func fetchBackfill(ctx context.Context, c *client.Client, childID string, opts historyOpts) ([][]byte, error) {
	if opts.tailN == 0 {
		return nil, nil
	}
	limit := 0 // 0 = no limit (all) on the daemon side
	if opts.tailN > 0 {
		limit = opts.tailN
	}
	req := protocol.GetRecentRequest{
		Type:    protocol.TypeCtrlGetRecent,
		ChildID: childID,
		Limit:   limit,
		Include: opts.include,
		Exclude: opts.exclude,
	}
	resp, err := c.Request(ctx, req)
	if err != nil {
		return nil, err
	}
	if !resp.Success {
		return nil, fmt.Errorf("ctrl_get_recent: %s", client.FormatError(resp))
	}
	var data protocol.GetRecentResponseData
	if err := json.Unmarshal(resp.Data, &data); err != nil {
		return nil, err
	}
	out := make([][]byte, len(data.Events))
	for i, e := range data.Events {
		out[i] = []byte(e)
	}
	return out, nil
}

// runHistoryOut handles the default `out` event stream: optional backfill,
// then optional live follow. Used by both `tail` and `logs`.
//
// Backfill frames are raw inner pi events → rendered via renderPiEvent (or
// printed verbatim with --raw). Live frames are ctrl_event envelopes → rendered
// via render(). Dedup compares the inner bytes: any live frame whose inner event
// duplicates a backfilled frame (during the brief subscribe↔fetch overlap) is
// dropped, and the first non-duplicate closes the dedup window.
func runHistoryOut(ctx context.Context, c *client.Client, childID string, opts historyOpts) error {
	var events <-chan []byte
	var cancelSub func()
	if opts.follow {
		var err error
		events, cancelSub, err = c.Subscribe()
		if err != nil {
			return err
		}
		defer cancelSub()
		subReq := protocol.SubscribeRequest{Type: protocol.TypeCtrlSubscribe, ChildID: childID}
		if opts.profile != "" || len(opts.include) > 0 || len(opts.exclude) > 0 {
			subReq.Filter = &protocol.SubscribeFilter{Profile: opts.profile, Include: opts.include, Exclude: opts.exclude}
		}
		resp, err := c.Request(ctx, subReq)
		if err != nil {
			return err
		}
		if !resp.Success {
			return fmt.Errorf("ctrl_subscribe: %s", client.FormatError(resp))
		}
	}

	backfill, err := fetchBackfill(ctx, c, childID, opts)
	if err != nil {
		return err
	}

	renderer := newTailRenderer(os.Stdout, opts.useColor, opts.mode, opts.verbose)

	// Render backfill (raw inner pi events).
	seen := make(map[string]bool, len(backfill))
	for _, f := range backfill {
		seen[string(f)] = true
		if opts.raw {
			fmt.Fprintln(os.Stdout, string(f))
			continue
		}
		if err := renderer.renderPiEvent(f); err != nil && !errors.Is(err, errDaemonShutdown) {
			fmt.Fprintln(os.Stderr, "render error:", err)
		}
	}

	if !opts.follow {
		return nil
	}

	dedupWindow := len(seen) > 0
	for {
		select {
		case frame, ok := <-events:
			if !ok {
				return nil
			}
			if dedupWindow {
				inner := innerEvent(frame)
				if seen[string(inner)] {
					delete(seen, string(inner))
					continue
				}
				dedupWindow = false // first non-duplicate closes the window
			}
			if opts.raw {
				fmt.Fprintln(os.Stdout, string(innerEvent(frame)))
			} else if err := renderer.render(frame); err != nil {
				if errors.Is(err, errDaemonShutdown) {
					return nil
				}
				fmt.Fprintln(os.Stderr, "render error:", err)
			}
			if isChildExited(frame, childID) {
				return nil
			}
		case <-ctx.Done():
			return nil
		}
	}
}
```

Note: `renderPiEvent` is an unexported method on `*tailRenderer` in the same package (`render_tail.go:174`), so `history.go` can call it directly. It handles `message_start`/`message_end`/`agent_*`/tool frames; lifecycle frames never appear in backfill (the ring stores stdout pi events only).

- [ ] **Step 4: Run the tests to verify they pass**

Run: `cd ~/home/pi-controller && go test ./cmd/pic/ -run TestInnerEvent`
Expected: PASS.

- [ ] **Step 5: Vet and commit**

Run: `cd ~/home/pi-controller && go vet ./cmd/pic/`
Expected: no output (note: `runHistoryOut`/`fetchBackfill` may be flagged unused until Task 3 wires them — if `go vet` errors on unused, proceed; the build is exercised in Task 3. If the linter blocks the commit, combine Steps 5 here with Task 3.)

```bash
cd ~/home/pi-controller
git add cmd/pic/history.go cmd/pic/history_test.go
git commit -m "pic: add shared history engine with boundary dedup"
```

---

## Task 3: Rewire `pic tail` onto the history engine (adds backfill)

`tail` becomes the follow=on, `-n 20` preset. Existing flags preserved.

**Files:**
- Modify: `cmd/pic/cmd_tail.go` (flags + `runTailChild` body)
- Test: `cmd/pic/cmd_tail_test.go`

- [ ] **Step 1: Add the `-n/--tail` flag**

In `cmd/pic/cmd_tail.go` `newTailCmd`, after the existing flag block (line ~39), add:

```go
	cmd.Flags().IntP("tail", "n", 20, "Backfill the last N events before following (-1 = all in buffer, 0 = none)")
	cmd.Flags().Bool("raw", false, "Emit raw event frames (JSONL) instead of the rendered view")
```

- [ ] **Step 2: Delegate per-child mode to the engine**

The engine now owns the subscription for per-child mode, so two things change in `cmd/pic/cmd_tail.go`:

(a) In `runTail`, read the two new flags and stop opening a subscription for per-child mode. The `events, cancelSub, err := c.Subscribe()` block (lines 116-120) is only needed by label mode — move it inside the `if labelFiltered` branch (before the `runTailLabeled` call). Add near the other flag reads (line ~108):

```go
	tailN, _ := cmd.Flags().GetInt("tail")
	raw, _ := cmd.Flags().GetBool("raw")
```

Then the dispatch at the end of `runTail` becomes:

```go
	if labelFiltered {
		events, cancelSub, err := c.Subscribe()
		if err != nil {
			return err
		}
		defer cancelSub()
		return runTailLabeled(ctx, c, events, labels, hasLabels, profile, include, exclude, mode, useColor, verbose)
	}
	return runTailChild(ctx, c, args, tailN, raw, profile, include, exclude, mode, useColor, verbose)
```

(b) Replace `runTailChild` (lines 192-254) with a thin delegate. New signature drops the `events` param and adds `tailN int, raw bool`:

```go
func runTailChild(
	ctx context.Context,
	c *client.Client,
	args []string,
	tailN int, raw bool,
	profile string, include, exclude []string,
	mode outputMode, useColor, verbose bool,
) error {
	target := ""
	if len(args) > 0 {
		target = args[0]
	}
	childID, err := resolveTarget(ctx, c, target)
	if err != nil {
		return err
	}
	_ = setActive(childID)
	return runHistoryOut(ctx, c, childID, historyOpts{
		follow:   true,
		tailN:    tailN,
		raw:      raw,
		profile:  profile,
		include:  include,
		exclude:  exclude,
		verbose:  verbose,
		mode:     mode,
		useColor: useColor,
	})
}
```

`isChildExited` (lines 256-267) is now used by `history.go`; it stays in `cmd_tail.go` (same package) — no move needed.

- [ ] **Step 3: Write the failing backfill test**

In `cmd/pic/cmd_tail_test.go`, add a test that drives `runHistoryOut` against a stub daemon. Use the existing test harness in this file if one exists (search for how `cmd_tail_test.go` starts a fake server / client); otherwise add a focused test of the emit+watermark path via a small in-process fake `*client.Client` is not feasible — instead test the observable behaviour through `shouldDropLive` (already covered) plus an integration check that `--tail 0` issues no `ctrl_get_recent`. Concretely, assert flag wiring:

```go
func TestTailFlagsDefaults(t *testing.T) {
	cmd := newTailCmd()
	n, _ := cmd.Flags().GetInt("tail")
	if n != 20 {
		t.Fatalf("tail default = %d, want 20", n)
	}
	if _, err := cmd.Flags().GetBool("raw"); err != nil {
		t.Fatalf("raw flag missing: %v", err)
	}
}
```

(If `cmd_tail_test.go` already has an integration harness with a fake daemon, prefer adding a backfill-ordering test there: seed the ring with 3 events, run with `--tail 2 --no-deltas`, assert the last 2 render before live, and a live event duplicating the newest is dropped.)

- [ ] **Step 4: Run the tests**

Run: `cd ~/home/pi-controller && go test ./cmd/pic/ -run 'Tail'`
Expected: PASS.

- [ ] **Step 5: Vet and commit**

Run: `cd ~/home/pi-controller && go vet ./cmd/pic/`
Expected: no output.

```bash
cd ~/home/pi-controller
git add cmd/pic/cmd_tail.go cmd/pic/cmd_tail_test.go
git commit -m "pic tail: backfill last N events via shared history engine"
```

---

## Task 4: Rewire `pic logs` onto the history engine (rendered default, live serving)

`logs` becomes the follow=off, `-n all`, rendered-by-default preset. `-f` makes it follow (≡ `tail`). `--raw` restores raw JSONL. `--in/--err/--all` are raw stream selectors that serve live children via `ctrl_get_streams` and exited children via the existing on-disk zcat.

**Files:**
- Modify: `cmd/pic/cmd_logs.go`
- Test: `cmd/pic/history_test.go` (stream selector mapping)

- [ ] **Step 1: Rewrite the command flags**

In `cmd/pic/cmd_logs.go` `newLogsCmd`, keep `--in/--err/--all/--path` and add:

```go
	cmd.Flags().IntP("tail", "n", -1, "Show the last N events (-1 = all, 0 = none)")
	cmd.Flags().BoolP("follow", "f", false, "Keep streaming new output after catching up (≡ pic tail)")
	cmd.Flags().Bool("raw", false, "Emit raw stream bytes/JSONL instead of the rendered view")
	cmd.Flags().Bool("no-deltas", true, "Suppress token-by-token message_update deltas in the rendered view (default true)")
	cmd.Flags().BoolP("verbose", "v", false, "Include internal RPC/lifecycle frames")
```

`--in/--err/--all` continue to be mutually exclusive with `--path`; they imply raw. Document on the command Long string: "logs == tail; -f follows. Rendered by default; --raw for verbatim bytes. Only the out stream can be followed; --in/--err are snapshots."

- [ ] **Step 2: Implement the new `runLogs`**

Replace `runLogs` (lines 45-93) so it routes by stream selector:

```go
func runLogs(cmd *cobra.Command, args []string) error {
	c := mustDial(cmd)
	defer c.Close()
	ctx := cmdCtx(cmd)

	target := ""
	if len(args) > 0 {
		target = args[0]
	}
	childID, err := resolveTarget(ctx, c, target)
	if err != nil {
		return err
	}

	wantPath, _ := cmd.Flags().GetBool("path")
	if wantPath {
		home, _ := os.UserHomeDir()
		fmt.Println(filepath.Join(home, ".pi", "run", "logs", childID))
		return nil
	}

	wantIn, _ := cmd.Flags().GetBool("in")
	wantErr, _ := cmd.Flags().GetBool("err")
	wantAll, _ := cmd.Flags().GetBool("all")
	tailN, _ := cmd.Flags().GetInt("tail")
	follow, _ := cmd.Flags().GetBool("follow")
	raw, _ := cmd.Flags().GetBool("raw")
	noDeltas, _ := cmd.Flags().GetBool("no-deltas")
	verbose, _ := cmd.Flags().GetBool("verbose")
	mode, useColor := outputOpts(cmd)

	// in/err/all → raw stream dump (snapshot; no follow).
	if wantIn || wantErr || wantAll {
		return dumpRawStreams(ctx, c, childID, wantIn, wantErr, wantAll)
	}

	// Suppress token deltas in the rendered view (raw shows everything).
	var exclude []string
	if noDeltas && !raw {
		exclude = []string{"message_update"}
	}

	// Default: the out event stream through the shared engine.
	return runHistoryOut(ctx, c, childID, historyOpts{
		follow:   follow,
		tailN:    tailN,
		raw:      raw,
		exclude:  exclude,
		verbose:  verbose,
		mode:     mode,
		useColor: useColor,
	})
}
```

- [ ] **Step 3: Implement `dumpRawStreams` (live via RPC, exited via disk)**

Add to `cmd/pic/cmd_logs.go`:

```go
// dumpRawStreams prints the in/err (and out, for --all) raw streams. Live
// children are served from the daemon's in-memory capture; exited children
// fall back to the on-disk gzip dump.
func dumpRawStreams(ctx context.Context, c *client.Client, childID string, wantIn, wantErr, wantAll bool) error {
	which := "all"
	switch {
	case wantIn && !wantErr && !wantAll:
		which = "in"
	case wantErr && !wantIn && !wantAll:
		which = "err"
	}

	resp, err := c.Request(ctx, protocol.GetStreamsRequest{
		Type:    protocol.TypeCtrlGetStreams,
		ChildID: childID,
		Which:   which,
	})
	if err != nil {
		return err
	}
	if resp.Success {
		var data protocol.GetStreamsResponseData
		if err := json.Unmarshal(resp.Data, &data); err != nil {
			return err
		}
		if data.Alive {
			if wantAll {
				fmt.Println("=== in.jsonl ===")
			}
			if wantIn || wantAll {
				for _, line := range data.In {
					fmt.Fprintln(os.Stdout, string(line))
				}
			}
			if wantAll {
				fmt.Println("=== out.jsonl ===")
				// out comes from the ring via ctrl_get_recent
				bf, err := fetchBackfill(ctx, c, childID, historyOpts{tailN: -1})
				if err != nil {
					return err
				}
				for _, f := range bf {
					fmt.Fprintln(os.Stdout, string(f))
				}
				fmt.Println("=== err.log ===")
			}
			if wantErr || wantAll {
				os.Stdout.Write(data.Err)
			}
			return nil
		}
	}
	// Exited (or RPC unsupported): fall back to the on-disk dump.
	return dumpDiskStreams(childID, wantIn, wantErr, wantAll)
}
```

- [ ] **Step 4: Preserve the on-disk path as `dumpDiskStreams`**

Move the original disk-reading logic (old lines 58-92, the `logsDir`/`zcatTo`/`--all` loop) into a helper. `zcatTo` already exists — keep it. Add:

```go
func dumpDiskStreams(childID string, wantIn, wantErr, wantAll bool) error {
	home, _ := os.UserHomeDir()
	logsDir := filepath.Join(home, ".pi", "run", "logs", childID)
	if _, err := os.Stat(logsDir); errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("no logs at %s (child alive but capture unavailable, or persistence mode is `never`)", logsDir)
	}
	if wantAll {
		for _, name := range []string{"in.jsonl.gz", "out.jsonl.gz", "err.log.gz"} {
			fmt.Printf("=== %s ===\n", name)
			if err := zcatTo(os.Stdout, filepath.Join(logsDir, name)); err != nil {
				fmt.Fprintln(os.Stderr, "warning:", err)
			}
		}
		return nil
	}
	file := "out.jsonl.gz"
	if wantIn {
		file = "in.jsonl.gz"
	}
	if wantErr {
		file = "err.log.gz"
	}
	return zcatTo(os.Stdout, filepath.Join(logsDir, file))
}
```

Add imports as needed (`context`, `git.graveland.dev/brent/pi-controller/client`, `git.graveland.dev/brent/pi-controller/protocol`, `encoding/json`); keep `compress/gzip`, `errors`, `io`, `io/fs`, `os`, `path/filepath`.

- [ ] **Step 5: Write the failing stream-selector test**

In `cmd/pic/history_test.go`, add:

```go
func TestLogsFlagDefaults(t *testing.T) {
	cmd := newLogsCmd()
	n, _ := cmd.Flags().GetInt("tail")
	if n != -1 {
		t.Fatalf("logs tail default = %d, want -1 (all)", n)
	}
	f, _ := cmd.Flags().GetBool("follow")
	if f {
		t.Fatalf("logs follow default = true, want false")
	}
}
```

- [ ] **Step 6: Run tests + vet**

Run: `cd ~/home/pi-controller && go test ./cmd/pic/ -run 'Logs|Tail|Dedupe' && go vet ./cmd/pic/`
Expected: PASS, no vet output.

- [ ] **Step 7: Manual smoke (live child)**

Run (with the daemon up and a live child `foo`):
```bash
cd ~/home/pi-controller && make build   # rebuild pic + daemon per Makefile
./pic logs foo            # rendered, full history, exits
./pic logs foo -f -n 5    # last 5 rendered, then follows
./pic logs foo --raw      # raw JSONL
./pic logs foo --err      # live stderr snapshot
```
Expected: all return content while `foo` is still running; previously `logs` errored until exit.

- [ ] **Step 8: Commit**

```bash
cd ~/home/pi-controller
git add cmd/pic/cmd_logs.go cmd/pic/history_test.go
git commit -m "pic logs: rendered default, live serving, -f follow (logs == tail)"
```

---

## Task 5: pic-attach scrollback (prime `_messages` before the TUI renders)

Replay the daemon's retained events through pic-attach's existing event-handling path so the TUI shows prior transcript on attach. No pi changes.

**Files:**
- Modify: `attach/src/client.ts` (add `getRecent`)
- Modify: `attach/src/session.ts` (defer consume loop; add `primeHistory` + `start`)
- Modify: `attach/src/runtime.ts` (`connect`: prime then start)
- Modify: `cmd/pic/cmd_attach.go` + `cmd/pic/cli_helpers.go` (`-n` passthrough via env)
- Test: `attach/src/session.test.ts`

- [ ] **Step 1: Add `getRecent` to the bun client**

In `attach/src/client.ts`, add a public method (after `subscribe`, line ~248):

```ts
    /**
     * Fetch the last `limit` retained event frames for a child via
     * ctrl_get_recent. limit <= 0 means "all in buffer". Returns the inner pi
     * event frames (already unwrapped — the ring stores raw child stdout).
     */
    async getRecent(childId: string, limit: number): Promise<Record<string, unknown>[]> {
        const req: Record<string, unknown> = { type: "ctrl_get_recent", childId };
        if (limit > 0) req["limit"] = limit;
        const resp = await this.request(req);
        if (!resp.success) {
            throw new Error(`ctrl_get_recent failed: ${resp.error?.code ?? "unknown"}`);
        }
        const data = resp.data as { events?: Record<string, unknown>[] } | undefined;
        return data?.events ?? [];
    }
```

- [ ] **Step 2: Write the failing session test**

In `attach/src/session.test.ts`, add (mirroring the existing test setup — reuse its fake client / session factory):

```ts
test("primeHistory rebuilds messages from retained agent_end", async () => {
  const recorded: Record<string, unknown>[] = [
    { type: "agent_start" },
    {
      type: "agent_end",
      messages: [
        { role: "user", content: "hello" },
        { role: "assistant", content: "hi there" },
      ],
      willRetry: false,
    },
  ];
  // Build a RemoteAgentSession wired to a fake client whose getRecent
  // returns `recorded`. (Use the file's existing makeSession/fakeClient helper;
  // add a getRecent stub returning recorded.)
  const session = makeTestSession({ getRecent: async () => recorded });
  await session.primeHistory(-1);
  expect(session.messages.map((m: any) => m.content)).toEqual(["hello", "hi there"]);
});

test("primeHistory ignores message_update deltas", async () => {
  const session = makeTestSession({
    getRecent: async () => [{ type: "message_update", delta: "partial" }],
  });
  await session.primeHistory(-1);
  expect(session.messages).toEqual([]);
});
```

- [ ] **Step 3: Run the test to verify it fails**

Run: `cd ~/home/pi-controller/attach && bun test src/session.test.ts -t primeHistory`
Expected: FAIL — `primeHistory` is not a function.

- [ ] **Step 4: Defer the consume loop and add `primeHistory`/`start`**

In `attach/src/session.ts`:

Change the constructor's tail (line 415) from starting the loop to only registering the iterator:

```ts
        // Register the event iterator now so frames the daemon pushes after
        // ctrl_subscribe are buffered (AsyncQueue, 256) — but do NOT begin
        // consuming until start(), so primeHistory() can seed _messages first
        // without racing live updates.
        this._eventIter = this._client.subscribe();
```

Add two methods (near `consumeEvents`, line 420):

```ts
    /**
     * Seed _messages from the daemon's retained history so the TUI shows prior
     * transcript on attach. Replays each retained event through the same
     * translate→updateCacheFromEvent path the live loop uses. message_update
     * deltas (transient streaming state) are skipped. limit <= 0 = all.
     * Faithful for pi children; claude frames may not reconstruct cleanly
     * (see provider caveat in the plan). Best-effort: a fetch failure leaves
     * _messages empty (blank scrollback).
     */
    async primeHistory(limit: number): Promise<void> {
        if (limit === 0) return;
        let frames: Record<string, unknown>[];
        try {
            frames = await this._client.getRecent(this._childId, limit);
        } catch (err) {
            if (process.env.PIC_ATTACH_DEBUG === "1") {
                console.error("[pic-attach] primeHistory failed:", err);
            }
            return;
        }
        for (const inner of frames) {
            if (inner["type"] === "message_update") continue;
            const ev = this.translate(inner);
            if (ev === null) continue;
            this.updateCacheFromEvent(ev);
        }
        // History replay can leave _isStreaming true if the last retained event
        // was an agent_start; the first live event corrects it, but reset here
        // so the initial paint isn't stuck in a streaming state.
        this._isStreaming = false;
        this._streamingMessage = undefined;
    }

    /** Begin consuming live events. Call after primeHistory(). */
    start(): void {
        void this.consumeEvents();
    }
```

`consumeEvents` already uses `this._eventIter` if set — verify it does **not** re-call `this._client.subscribe()` (it currently does at line 421). Change line 421 from `this._eventIter = this._client.subscribe();` to use the already-registered iterator:

```ts
        // _eventIter was registered in the constructor.
        if (!this._eventIter) this._eventIter = this._client.subscribe();
```

- [ ] **Step 5: Wire prime+start into `connect`**

In `attach/src/runtime.ts` `connect()`, after the `ctrl_subscribe` block (line 140) and before `return runtime`:

```ts
        // Seed prior transcript, then begin consuming live events.
        const tail = Number(process.env["PIC_ATTACH_TAIL"] ?? "-1");
        await session.primeHistory(Number.isFinite(tail) ? tail : -1);
        session.start();
```

(`session` is in scope from line 98; if `connect` only holds `runtime`, expose the session via `runtime.session` or capture the local `session` const — it is already a local at line 98.)

- [ ] **Step 6: Run the bun tests**

Run: `cd ~/home/pi-controller/attach && bun test src/session.test.ts`
Expected: PASS, including the two new tests.

- [ ] **Step 7: Add `-n` passthrough on `pic attach`**

In `cmd/pic/cmd_attach.go` `newAttachCmd`, add a flag (after line 23):

```go
	cmd.Flags().IntP("tail", "n", -1, "Scrollback: replay the last N retained events into the TUI (-1 = all, 0 = none)")
```

In `runAttach` (line 45), read it and pass to the attach launcher. Thread it through `attachAndDecide` → `execPicAttach`. Simplest: read in `runAttach` and set the env before exec. In `cmd/pic/cli_helpers.go` `execPicAttach` (line 130), set the env on the child command:

```go
	// caller passes scrollback depth via PIC_ATTACH_TAIL
```

Concretely, in `runAttach`:

```go
	tailN, _ := cmd.Flags().GetInt("tail")
	os.Setenv("PIC_ATTACH_TAIL", strconv.Itoa(tailN))
```

(Add `os` and `strconv` imports to `cmd_attach.go`. Since `pic` execs `pic-attach` with inherited env, `os.Setenv` is sufficient; no `execPicAttach` change needed if it inherits `os.Environ()`. Verify `execPicAttach` uses inherited env — it spawns with stdio inherited, so env is inherited by default.)

- [ ] **Step 8: Build and manual smoke**

Run:
```bash
cd ~/home/pi-controller && make build && make build-attach
./pic attach foo          # should show prior conversation scrollback, then live
./pic attach foo -n 0     # blank scrollback (old behaviour), then live
```
Expected: scrollback appears on attach where it was blank before.

- [ ] **Step 9: Commit**

```bash
cd ~/home/pi-controller
git add attach/src/client.ts attach/src/session.ts attach/src/runtime.ts attach/src/session.test.ts cmd/pic/cmd_attach.go cmd/pic/cli_helpers.go
git commit -m "pic-attach: prime transcript scrollback from retained history on attach"
```

---

## Task 6: Final verification

- [ ] **Step 1: Full Go test + vet**

Run: `cd ~/home/pi-controller && go test ./... && go vet ./...`
Expected: all pass, no vet output.

- [ ] **Step 2: Full bun test**

Run: `cd ~/home/pi-controller/attach && bun test`
Expected: all pass.

- [ ] **Step 3: Rebuild artifacts and confirm the deployed binaries (not source)**

Per project gotchas, verify the rebuilt `pic`/`pic-attach`/daemon are the ones on PATH after `make update`, and restart the daemon:
```bash
cd ~/home/pi-controller && make update   # build + pi-install
# restart the daemon per project convention, then re-run the Task 4/5 smokes
```

- [ ] **Step 4: Update README/help if needed**

If `attach/README.md` or `pic` help text documents `logs`/`tail`/`attach` behaviour, update it to state: logs == tail (follow differs), rendered by default, `--raw` escape hatch, `-n` backfill, attach shows scrollback. Commit any doc changes.

---

## Provider caveat: pi vs claude backfill fidelity

Rendered backfill/scrollback is **fully faithful for pi children** (the identity
provider — raw ring frame == normalized pi event). For **claude children** the
ring holds raw stream-json and the pi-normalized view is produced by a stateful
per-child translator (`internal/child/provider_claude_state.go`) that runs only
on the live stream and cannot be replayed over the ring. Consequences:

- `pic tail`/`logs` backfill of a claude child: frames whose `type` doesn't match
  `renderPiEvent`'s switch print raw-ish rather than as the polished conversation
  view. Live (post-catch-up) rendering is unaffected.
- `pic attach` scrollback of a claude child: `primeHistory`'s `translate` passes
  unrecognized claude frames through verbatim, so `_messages` may not reconstruct
  cleanly; live events after attach render normally.

This is a known v1 limitation, not a bug to fix here. Full claude fidelity would
require persisting normalized bus frames (or a replayable translator) — out of
scope. Surface it in help text where reasonable; do not block on it.

## Spec coverage check

- "logs == tail, follow the only difference" → Tasks 3–4 (both delegate to `runHistoryOut`; `logs -f` ≡ `tail`).
- "rendered default + `--raw` escape hatch" → Task 2 (`renderPiEvent`/`render` vs verbatim), Tasks 3–4 flags.
- "`-n` backfill, default tail=20 / logs=all" → Task 3 (`-n 20`), Task 4 (`-n -1`).
- "logs works while running" → Task 1 (`ctrl_get_streams`) + Task 4 (`runHistoryOut` uses ring for out; `dumpRawStreams` for in/err).
- "same content before/after exit" → Task 4 live-RPC / disk fallback symmetry.
- "attach scrollback, no pi fork" → Task 5 (priming in pic-attach's own runtime).
- "shape-correct render + dedup" → Task 2 `innerEvent` + inner-bytes dedup window.
- Caveat "only out can be followed; in/err snapshot-only" → Task 4 Long text + `dumpRawStreams` (no follow path).
- Caveat "pi vs claude backfill fidelity" → provider caveat section above.

## Open items (resolve during implementation)

- **`StderrSnapshot` on a live child** (Task 1 Step 5): confirm thread-safety; scope to `in` only if unsafe.
- **Attach seed source** (Task 5): ring via `getRecent` is the default; if a long session exceeds the ring window and full scrollback is wanted, switch `primeHistory` to read pi's session file via the already-built `sessionManager` (which also sidesteps the claude caveat, since the session file is the canonical transcript).
- **Dedup window across shapes**: inner-bytes equality is exact for pi (raw inner == live `event` inner). For claude the shapes differ, so duplicate suppression may not fire in the narrow subscribe↔fetch overlap — at worst a couple of duplicated events at the boundary. Acceptable; revisit only if noticeable.
