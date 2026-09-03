# Claude rendered backfill + slash_commands completion — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make `pic logs`/`pic tail`/`pic attach` render the conversation view for claude children (not raw JSON), and surface claude's `slash_commands` as TUI autocomplete — without modifying pi.

**Architecture:** A new provider capability `Normalizes()` gates claude-only behavior. For normalizing providers the Child captures the pi-vocabulary **bus** stream into a second "render-ring" (and to disk as `render.jsonl.gz`); `ctrl_get_recent` gains a `Rendered` selector that reads the render-ring for the rendered view while `--raw` keeps reading the raw ring. Separately, the daemon captures claude's `slash_commands` from its init frame into the child summary, which pic-attach feeds into its existing autocomplete provider.

**Tech Stack:** Go (daemon + `pic` CLI, cobra, JSONL/UDS, `internal/ring`), TypeScript/Bun (`pic-attach`). Tests: Go `testing`, `bun test`.

---

## File Structure

**Sub-project A — rendered backfill (render-ring):**
- `internal/child/provider.go` — add `Normalizes() bool` to the interface.
- `internal/child/provider_pi.go` — `Normalizes()` → false.
- `internal/child/provider_claude.go` — `Normalizes()` → true.
- `internal/child/child.go` — `renderRing` field, `publishBus` helper, route both publish sites through it, `RenderRingSnapshot`, `RenderRecent`, `Normalizes`.
- `internal/store/session.go` — `ExitedRenderRing` field on Session + Snapshot + copy.
- `internal/store/store.go` — `MarkExited` gains a render-ring param.
- `internal/persist/logs.go` — `Dump` writes `render.jsonl.gz`; add `ReadGzLines`.
- `cmd/pi-controller/controller.go` — `handleChildExit` captures render snapshot; `GetRecent` honors `Rendered`.
- `protocol/types.go` — `GetRecentRequest.Rendered`.
- `internal/server/dispatch.go` — `RecentQuery.Rendered`; handler copies it.
- `cmd/pic/history.go` — rendered path requests `Rendered:true`.
- `attach/src/client.ts` — `getRecent` sends `rendered:true`.

**Sub-project B — slash_commands completion:**
- `internal/child/sniff.go` — `SnifferMetadata.SlashCommands`.
- `internal/child/provider_claude.go` — `Parse` captures `slash_commands`.
- `internal/child/child.go` — store/expose slash commands via `Metadata()`.
- `cmd/pi-controller/controller.go` — `monitorChild` syncs slash commands to the store.
- `internal/store/session.go` — `SlashCommands` field on Session + Snapshot + copy.
- `protocol/types.go` — `ChildSummary.SlashCommands`.
- `internal/server/dispatch.go` — `snapshotToSummary` carries it.
- `cmd/pic/picembed/pic-helpers/index.ts` — `setupTuiAutocomplete` claude path.

**Build note:** verify with `go vet ./...` (NOT `go build`); bun from `attach/`. Conventions: `any` not `interface{}`, no `Co-Authored-By`, stage specific paths, no trivial comments.

---

# Sub-project A: claude rendered backfill

## Task 1: Provider `Normalizes()` capability

**Files:**
- Modify: `internal/child/provider.go`, `internal/child/provider_pi.go`, `internal/child/provider_claude.go`
- Test: `internal/child/provider_busframes_test.go`

- [ ] **Step 1: Write the failing test**

Add to `internal/child/provider_busframes_test.go`:

```go
func TestProviderNormalizes(t *testing.T) {
	if PiProvider{}.Normalizes() {
		t.Fatal("PiProvider.Normalizes() = true, want false (stdout is already pi-vocabulary)")
	}
	if !(ClaudeProvider{}.Normalizes()) {
		t.Fatal("ClaudeProvider.Normalizes() = false, want true (translates claude→pi)")
	}
	// The per-child instance must inherit it.
	if !ClaudeProvider{}.Fresh().Normalizes() {
		t.Fatal("claudeProvider (Fresh) Normalizes() = false, want true")
	}
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `cd ~/home/pi-controller && go test ./internal/child/ -run TestProviderNormalizes`
Expected: FAIL — `Normalizes` undefined.

- [ ] **Step 3: Add the interface method**

In `internal/child/provider.go`, add to the `ProtocolProvider` interface (after `OutboundEcho`):

```go
	// Normalizes reports whether the provider translates the child's raw stdout
	// into a different pi-vocabulary bus stream (claude), versus the raw stdout
	// already being the bus stream (pi, identity). When true the Child maintains
	// a render-ring capturing the bus output, since the raw ring alone is not
	// renderable.
	Normalizes() bool
```

- [ ] **Step 4: Implement on both providers**

In `internal/child/provider_pi.go`, add:

```go
// Normalizes is false: pi's stdout already IS the pi AgentSessionEvent stream,
// so the raw ring is renderable as-is.
func (PiProvider) Normalizes() bool { return false }
```

In `internal/child/provider_claude.go`, add (the per-child `*claudeProvider` embeds `ClaudeProvider`, so it inherits this):

```go
// Normalizes is true: claude's stdout is its native stream-json, translated to
// pi vocabulary on the bus by the per-child translator. The raw ring is not
// renderable, so the Child captures the bus output into a render-ring.
func (ClaudeProvider) Normalizes() bool { return true }
```

- [ ] **Step 5: Run it + vet**

Run: `cd ~/home/pi-controller && go test ./internal/child/ -run TestProviderNormalizes && go vet ./internal/child/`
Expected: PASS, no vet output.

- [ ] **Step 6: Commit**

```bash
cd ~/home/pi-controller
git add internal/child/provider.go internal/child/provider_pi.go internal/child/provider_claude.go internal/child/provider_busframes_test.go
git commit -m "child: add ProtocolProvider.Normalizes() capability"
```

---

## Task 2: Child render-ring + publishBus

**Files:**
- Modify: `internal/child/child.go`
- Test: `internal/child/child_test.go` (or a new `internal/child/render_ring_test.go`)

Read `internal/child/child.go` first: the struct is around line 110-128 (`bus`, `ring`, `in`, `provider`); `Spawn` constructs the Child around line 180-205 (`ring: ring.New(ring.Options{})`); `readStdout` publishes at lines 406-409; the supervise loop's `OutboundEcho` publishes at lines 374-376; `RingSnapshot` is at line 286.

- [ ] **Step 1: Write the failing test**

Create `internal/child/render_ring_test.go`:

```go
package child

import (
	"testing"

	"git.graveland.dev/brent/pi-controller/internal/ring"
)

func TestPublishBusCapturesToRenderRing(t *testing.T) {
	// A normalizing child captures bus frames into the render-ring.
	c := &Child{
		bus:        newTestBus(),
		renderRing: ring.New(ring.Options{}),
	}
	c.publishBus([]byte(`{"type":"message_start"}`), 1)
	c.publishBus([]byte(`{"type":"message_end"}`), 2)

	got := c.RenderRingSnapshot()
	if len(got) != 2 || string(got[0]) != `{"type":"message_start"}` || string(got[1]) != `{"type":"message_end"}` {
		t.Fatalf("render-ring = %v, want the two published frames in order", got)
	}
}

func TestRenderRingSnapshotNilWhenAbsent(t *testing.T) {
	// A non-normalizing child has no render-ring.
	c := &Child{bus: newTestBus()}
	c.publishBus([]byte(`{"type":"x"}`), 1)
	if c.RenderRingSnapshot() != nil {
		t.Fatal("RenderRingSnapshot() should be nil when renderRing is unset")
	}
}
```

If `newTestBus()` does not already exist in the package's tests, add this helper to the same file:

```go
func newTestBus() *busType { return newBus() }
```

INVESTIGATE the actual bus constructor/type first (the field is `bus *bus.Bus[[]byte]`). Use the real construction the package uses in other tests — e.g. `bus.New[[]byte](bus.Options{})`. Adjust the helper/imports to match; the test only needs a real bus so `Publish` doesn't panic. If a simpler existing test constructor exists, use it.

- [ ] **Step 2: Run it to verify it fails**

Run: `cd ~/home/pi-controller && go test ./internal/child/ -run 'RenderRing|PublishBus'`
Expected: FAIL — `renderRing` / `publishBus` / `RenderRingSnapshot` undefined.

- [ ] **Step 3: Add the field, helper, snapshot, and accessors**

In `internal/child/child.go`, add to the `Child` struct (next to `ring *ring.Ring`, ~line 113):

```go
	renderRing *ring.Ring // bus-frame capture for normalizing providers (claude); nil otherwise
```

In `Spawn`, after the Child value is constructed and `c.provider` is set (right after the struct literal that sets `provider: prov`), add:

```go
	if c.provider.Normalizes() {
		c.renderRing = ring.New(ring.Options{})
	}
```

Add the publish helper and accessors (near `RingSnapshot`, ~line 286):

```go
// publishBus appends a bus frame to the render-ring (when the provider
// normalizes) and publishes it. The render-ring captures the exact
// pi-vocabulary stream the bus carries — assistant turns and synthesized user
// turns — so backfill can render claude children. Safe for concurrent use:
// ring.Ring has its own mutex, and this is called from both the readStdout and
// supervise goroutines (matching the bus's own fan-out ordering).
func (c *Child) publishBus(f []byte, ts int64) {
	if c.renderRing != nil {
		c.renderRing.Append(f, ts)
	}
	c.bus.Publish(f)
}

// RenderRingSnapshot returns a copy of all frames in the render-ring, or nil
// when the provider does not normalize (no render-ring).
func (c *Child) RenderRingSnapshot() [][]byte {
	if c.renderRing == nil {
		return nil
	}
	events := c.renderRing.Recent(ring.Query{})
	out := make([][]byte, len(events))
	for i, e := range events {
		out[i] = e.Bytes
	}
	return out
}

// RenderRecent returns render-ring events matching q, or nil when there is no
// render-ring.
func (c *Child) RenderRecent(q ring.Query) []ring.Event {
	if c.renderRing == nil {
		return nil
	}
	return c.renderRing.Recent(q)
}

// Normalizes reports whether this child's provider translates stdout into a
// distinct bus stream (claude). When true, the render-ring is the renderable
// source; when false, the raw ring is already renderable.
func (c *Child) Normalizes() bool { return c.provider.Normalizes() }
```

- [ ] **Step 4: Route both publish sites through publishBus**

In `readStdout` (~line 407), change:

```go
		for _, f := range c.provider.BusFrames(line, ts) {
			c.publishBus(f, ts)
		}
```

In the supervise loop's OutboundEcho block (~line 374), capture the timestamp once and use it:

```go
				echoTS := time.Now().UnixMilli()
				for _, f := range c.provider.OutboundEcho(frame, echoTS) {
					c.publishBus(f, echoTS)
				}
```

(Replace the existing `time.Now().UnixMilli()` inline arg and `c.bus.Publish(f)` with the above.)

- [ ] **Step 5: Run tests + vet**

Run: `cd ~/home/pi-controller && go test ./internal/child/ -run 'RenderRing|PublishBus' && go vet ./internal/child/`
Expected: PASS, no vet output. Also run the full package: `go test ./internal/child/`.

- [ ] **Step 6: Commit**

```bash
cd ~/home/pi-controller
git add internal/child/child.go internal/child/render_ring_test.go
git commit -m "child: capture normalized bus stream into a render-ring"
```

---

## Task 3: Persist render-ring at exit (in-memory + disk)

**Files:**
- Modify: `internal/store/session.go`, `internal/store/store.go`, `internal/persist/logs.go`, `cmd/pi-controller/controller.go`
- Test: `internal/persist/logs_test.go`, `internal/store/store_test.go`

- [ ] **Step 1: Write the failing dump test**

In `internal/persist/logs_test.go`, add (mirror an existing Dump test in that file for setup style):

```go
func TestDumpWritesRenderJSONL(t *testing.T) {
	dir := t.TempDir()
	d := NewLogDumper(dir, ModeOnExit)
	render := [][]byte{[]byte(`{"type":"message_start"}`), []byte(`{"type":"message_end"}`)}
	err := d.Dump("c1", nil, [][]byte{[]byte(`{"type":"system"}`)}, render, nil, Meta{ChildID: "c1"}, ExitInfo{})
	if err != nil {
		t.Fatalf("Dump: %v", err)
	}
	got, err := ReadGzLines(filepath.Join(dir, "c1", "render.jsonl.gz"))
	if err != nil {
		t.Fatalf("ReadGzLines: %v", err)
	}
	if len(got) != 2 || string(got[0]) != `{"type":"message_start"}` {
		t.Fatalf("render.jsonl.gz = %v, want the 2 render frames", got)
	}
}

func TestDumpSkipsRenderWhenNil(t *testing.T) {
	dir := t.TempDir()
	d := NewLogDumper(dir, ModeOnExit)
	if err := d.Dump("c1", nil, nil, nil, nil, Meta{ChildID: "c1"}, ExitInfo{}); err != nil {
		t.Fatalf("Dump: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "c1", "render.jsonl.gz")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("render.jsonl.gz should not exist when render frames are nil")
	}
}
```

Add imports to the test file as needed: `path/filepath`, `os`, `errors`, `io/fs`.

- [ ] **Step 2: Run it to verify it fails**

Run: `cd ~/home/pi-controller && go test ./internal/persist/ -run 'RenderJSONL|SkipsRender'`
Expected: FAIL — `Dump` arity wrong / `ReadGzLines` undefined.

- [ ] **Step 3: Extend `Dump` + add `ReadGzLines`**

In `internal/persist/logs.go`, change the `Dump` signature and body to accept `render [][]byte` (between `out` and `errBytes`):

```go
func (d *LogDumper) Dump(
	childID string,
	in [][]byte,
	out [][]byte,
	render [][]byte,
	errBytes []byte,
	meta Meta,
	exit ExitInfo,
) error {
	if d.mode == ModeNever {
		return nil
	}
	if d.mode == ModeOnFailure &&
		exit.ExitCode == 0 &&
		exit.Signal == "" &&
		exit.LastStatus != "error" {
		return nil
	}

	childDir := filepath.Join(d.dir, childID)
	if err := os.MkdirAll(childDir, 0o700); err != nil {
		return err
	}

	if err := writeMeta(filepath.Join(childDir, "meta.json"), meta); err != nil {
		return err
	}
	if err := writeGzLines(filepath.Join(childDir, "in.jsonl.gz"), in); err != nil {
		return err
	}
	if err := writeGzLines(filepath.Join(childDir, "out.jsonl.gz"), out); err != nil {
		return err
	}
	if len(render) > 0 {
		if err := writeGzLines(filepath.Join(childDir, "render.jsonl.gz"), render); err != nil {
			return err
		}
	}
	return writeGzBytes(filepath.Join(childDir, "err.log.gz"), errBytes)
}
```

Add a reader at the end of `internal/persist/logs.go`:

```go
// ReadGzLines reads a gzip-compressed newline-delimited file into one []byte
// per line (trailing newline stripped). Returns an os.ErrNotExist-wrapped error
// when the file is absent so callers can distinguish "no dump" from a read
// failure.
func ReadGzLines(path string) ([][]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return nil, err
	}
	defer gz.Close()
	var out [][]byte
	sc := bufio.NewScanner(gz)
	sc.Buffer(make([]byte, 0, 64*1024), 16<<20)
	for sc.Scan() {
		line := sc.Bytes()
		cp := make([]byte, len(line))
		copy(cp, line)
		out = append(out, cp)
	}
	return out, sc.Err()
}
```

Add `"bufio"` to the imports.

- [ ] **Step 4: Add `ExitedRenderRing` to the store + `MarkExited` param**

In `internal/store/session.go`, add to `Session` (after `ExitedRing []ring.Event`):

```go
	// ExitedRenderRing snapshots the render-ring at exit (normalizing children
	// only), so the rendered ctrl_get_recent view survives the child's removal.
	ExitedRenderRing []ring.Event
```

Add the same field to `Snapshot` (after its `ExitedRing`):

```go
	ExitedRenderRing []ring.Event
```

In `Session.Snapshot()`, add to the returned struct literal (next to `ExitedRing: copyRingEvents(s.ExitedRing)`):

```go
		ExitedRenderRing: copyRingEvents(s.ExitedRenderRing),
```

In `internal/store/store.go`, change `MarkExited` to accept and set the render snapshot:

```go
func (s *Store) MarkExited(id string, exitedAt time.Time, exitCode int, exitSignal string, exitedRing []ring.Event, exitedRenderRing []ring.Event) bool {
```

and inside, where it sets `sess.ExitedRing = exitedRing`, add:

```go
	sess.ExitedRenderRing = exitedRenderRing
```

- [ ] **Step 5: Wire the exit path in the controller**

In `cmd/pi-controller/controller.go` `handleChildExit` (~line 1524), capture the render snapshot and thread it through both calls:

```go
	ringSnapshot := ch.Ring().Recent(ring.Query{})
	renderSnapshot := ch.RenderRingSnapshot()

	c.st.MarkExited(childID, now, res.ExitCode, res.Signal, ringSnapshot, renderRingEvents(renderSnapshot))
```

The `MarkExited` render param is `[]ring.Event`, but `RenderRingSnapshot` returns `[][]byte`. Add a small converter in `controller.go` (timestamps aren't preserved on the byte snapshot; rendered backfill doesn't use per-event timestamps for display, and Since-filtering on rendered exited frames is best-effort):

```go
// renderRingEvents wraps render-ring byte frames as ring.Events (timestamp 0;
// exact arrival times are not retained in the byte snapshot).
func renderRingEvents(frames [][]byte) []ring.Event {
	if len(frames) == 0 {
		return nil
	}
	out := make([]ring.Event, len(frames))
	for i, f := range frames {
		out[i] = ring.Event{Bytes: f}
	}
	return out
}
```

In the `dumper.Dump` call (~line 1556), pass the render snapshot (new `render` param, between out and stderr):

```go
		if err := c.dumper.Dump(childID, ch.InSnapshot(), ch.RingSnapshot(), ch.RenderRingSnapshot(), ch.StderrSnapshot(), meta, exitInfo); err != nil {
```

- [ ] **Step 6: Fix other `MarkExited` / `Dump` callers**

Run `cd ~/home/pi-controller && go vet ./...` and fix every compile error from the changed signatures. Known call sites: `handleChildExit` (done above). Search tests too: `grep -rn "MarkExited\|\.Dump(" --include=*.go cmd internal`. For test callers, pass `nil` for the new render arg. (e.g. `internal/store/store_test.go`, `internal/persist/logs_test.go` existing Dump calls — add the `nil` render param.)

- [ ] **Step 7: Run tests + vet**

Run: `cd ~/home/pi-controller && go test ./internal/persist/ ./internal/store/ ./cmd/pi-controller/ && go vet ./...`
Expected: PASS, no vet output.

- [ ] **Step 8: Commit**

```bash
cd ~/home/pi-controller
git add internal/store/session.go internal/store/store.go internal/persist/logs.go cmd/pi-controller/controller.go internal/persist/logs_test.go internal/store/store_test.go
git commit -m "daemon: persist render-ring at exit (ExitedRenderRing + render.jsonl.gz)"
```

---

## Task 4: `GetRecent` rendered selector

**Files:**
- Modify: `protocol/types.go`, `internal/server/dispatch.go`, `cmd/pi-controller/controller.go`
- Test: `cmd/pi-controller/controller_test.go` (or the existing GetRecent test file), `internal/server/dispatch_test.go`

- [ ] **Step 1: Add the protocol + query field**

In `protocol/types.go` `GetRecentRequest`, add:

```go
	Rendered bool `json:"rendered,omitempty"`
```

In `internal/server/dispatch.go` `RecentQuery`, add:

```go
	Rendered bool
```

In the `getRecent` handler, copy it into the query:

```go
	q := RecentQuery{
		Limit:    req.Limit,
		Since:    req.Since,
		Include:  req.Include,
		Exclude:  req.Exclude,
		Rendered: req.Rendered,
	}
```

- [ ] **Step 2: Write the failing controller test**

In the controller test file (e.g. `cmd/pi-controller/controller_streams_test.go` — reuse its store/manager setup, or the file where `GetRecent` is tested), add a test that a normalizing live child serves the render-ring when `Rendered:true` and the raw ring when `Rendered:false`. Use the existing test harness for constructing a Controller with a live child; if constructing a live `*child.Child` is impractical, test the exited path instead by inserting a store session with both `ExitedRing` and `ExitedRenderRing` set:

```go
func TestGetRecentRenderedExited(t *testing.T) {
	ctrl, _ := newTestController(t) // use the package's existing helper
	ctrl.st.Insert(&store.Session{
		ChildID:          "c1",
		Kind:             "claude",
		Status:           protocol.StatusExited,
		ExitedRing:       []ring.Event{{Bytes: []byte(`{"type":"system"}`)}},
		ExitedRenderRing: []ring.Event{{Bytes: []byte(`{"type":"message_end"}`)}},
	})

	raw, err := ctrl.GetRecent("c1", server.RecentQuery{Rendered: false})
	if err != nil {
		t.Fatal(err)
	}
	if len(raw.Events) != 1 || string(raw.Events[0]) != `{"type":"system"}` {
		t.Fatalf("raw events = %v, want the raw frame", raw.Events)
	}

	rendered, err := ctrl.GetRecent("c1", server.RecentQuery{Rendered: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(rendered.Events) != 1 || string(rendered.Events[0]) != `{"type":"message_end"}` {
		t.Fatalf("rendered events = %v, want the render frame", rendered.Events)
	}
}
```

(If `newTestController` doesn't exist, look at how `controller_streams_test.go` builds the controller/store and mirror it. The Controller's `st` field is the `*store.Store`.)

- [ ] **Step 3: Run it to verify it fails**

Run: `cd ~/home/pi-controller && go test ./cmd/pi-controller/ -run TestGetRecentRendered`
Expected: FAIL — rendered path returns raw frames (selector not implemented).

- [ ] **Step 4: Implement the rendered selector in `GetRecent`**

Replace the body of `Controller.GetRecent` (cmd/pi-controller/controller.go, ~line 174-227) so the source is chosen by `(q.Rendered, alive, normalizing)`, then the existing Since/Limit/filter logic runs on the chosen source:

```go
func (c *Controller) GetRecent(childID string, q server.RecentQuery) (server.RecentResult, error) {
	snap, ok := c.st.Get(childID)
	if !ok {
		return server.RecentResult{}, &server.ControllerError{
			Code:    protocol.ErrChildNotFound,
			Message: "child not found: " + childID,
		}
	}

	ch, alive := c.cm.Get(childID)

	// Select the source event slice based on raw-vs-rendered and liveness.
	var events []ring.Event
	var total int
	var oldestTS int64

	if alive {
		if q.Rendered && ch.Normalizes() {
			events = ch.RenderRecent(ring.Query{Limit: q.Limit, Since: q.Since})
			total = len(ch.RenderRecent(ring.Query{}))
		} else {
			r := ch.Ring()
			events = r.Recent(ring.Query{Limit: q.Limit, Since: q.Since})
			total, _, oldestTS = r.Stats()
		}
	} else {
		// Exited: pick the snapshot, falling back to the on-disk dump for
		// orphans reloaded after a restart (in-memory snapshots are lost then).
		var all []ring.Event
		switch {
		case q.Rendered && snap.Kind == "claude":
			all = snap.ExitedRenderRing
			if len(all) == 0 {
				all = c.readDiskEvents(childID, "render.jsonl.gz")
			}
		default:
			all = snap.ExitedRing
			if len(all) == 0 {
				all = c.readDiskEvents(childID, "out.jsonl.gz")
			}
		}
		total = len(all)
		if len(all) > 0 {
			oldestTS = all[0].Timestamp
		}
		if q.Since > 0 {
			i := 0
			// A zero timestamp means "unknown" (render frames sourced from the
			// on-disk render.jsonl.gz carry no timestamp) — keep those rather
			// than dropping the whole disk-sourced rendered backfill.
			for i < len(all) && all[i].Timestamp != 0 && all[i].Timestamp < q.Since {
				i++
			}
			all = all[i:]
		}
		if q.Limit > 0 && len(all) > q.Limit {
			all = all[len(all)-q.Limit:]
		}
		events = all
	}

	out := make([]json.RawMessage, 0, len(events))
	for _, ev := range events {
		if framePassesTypeFilter(ev.Bytes, q.Include, q.Exclude) {
			out = append(out, json.RawMessage(ev.Bytes))
		}
	}

	return server.RecentResult{
		Events:           out,
		TotalInBuffer:    total,
		OldestTimestamp:  oldestTS,
		TruncatedByLimit: q.Limit > 0 && len(out) == q.Limit,
	}, nil
}

// readDiskEvents reads a per-child on-disk dump file (out.jsonl.gz /
// render.jsonl.gz) into ring.Events with zero timestamps. Returns nil when the
// dump is absent or unreadable (best-effort backfill after a restart).
func (c *Controller) readDiskEvents(childID, name string) []ring.Event {
	if c.logsDir == "" {
		return nil
	}
	frames, err := persist.ReadGzLines(filepath.Join(c.logsDir, childID, name))
	if err != nil {
		return nil
	}
	out := make([]ring.Event, len(frames))
	for i, f := range frames {
		out[i] = ring.Event{Bytes: f}
	}
	return out
}
```

Confirm `framePassesTypeFilter` already exists in this package (it does — used by the old GetRecent). Confirm `filepath` and `persist` are imported (they are, used elsewhere in controller.go).

Note: the `default` raw branch also now falls back to `out.jsonl.gz` on disk for orphans — this fixes the previously-flagged "exited-after-restart shows nothing" gap for the raw path too.

- [ ] **Step 5: Add the dispatch test**

In `internal/server/dispatch_test.go`, extend the fake controller's `GetRecent` to assert the `Rendered` flag propagates (or add a case to an existing GetRecent dispatch test) — confirm a request with `"rendered":true` reaches `RecentQuery.Rendered == true`. Minimal:

```go
func TestDispatchGetRecentRenderedFlag(t *testing.T) {
	var gotRendered bool
	fc := &fakeController{getRecentFn: func(_ string, q server.RecentQuery) (server.RecentResult, error) {
		gotRendered = q.Rendered
		return server.RecentResult{Events: []json.RawMessage{}}, nil
	}}
	d := server.NewDispatch(fc)
	d.HandleFrame(nil, []byte(`{"type":"ctrl_get_recent","id":"1","childId":"c1","rendered":true}`))
	if !gotRendered {
		t.Fatal("Rendered flag did not propagate to RecentQuery")
	}
}
```

If the fake controller doesn't support a `getRecentFn` hook, adapt to its existing shape (e.g. a `recentQuery` capture field set inside its `GetRecent`). Match the file's established fake style.

- [ ] **Step 6: Run tests + vet**

Run: `cd ~/home/pi-controller && go test ./cmd/pi-controller/ ./internal/server/ && go vet ./...`
Expected: PASS, no vet output.

- [ ] **Step 7: Commit**

```bash
cd ~/home/pi-controller
git add protocol/types.go internal/server/dispatch.go internal/server/dispatch_test.go cmd/pi-controller/controller.go cmd/pi-controller/controller_streams_test.go
git commit -m "daemon: ctrl_get_recent rendered selector (render-ring + disk fallback)"
```

---

## Task 5: `pic` rendered path requests rendered frames

**Files:**
- Modify: `cmd/pic/history.go`
- Test: `cmd/pic/history_test.go`

- [ ] **Step 1: Write the failing test**

In `cmd/pic/history_test.go`, add a test asserting the request built by `fetchBackfill` sets `Rendered` to the inverse of `raw`. Since `fetchBackfill` issues a live request, factor the request construction into a pure helper and test that:

```go
func TestBackfillRequestRendered(t *testing.T) {
	if got := backfillRequest("c1", historyOpts{raw: false}).Rendered; !got {
		t.Fatal("non-raw backfill should request Rendered:true")
	}
	if got := backfillRequest("c1", historyOpts{raw: true}).Rendered; got {
		t.Fatal("--raw backfill should request Rendered:false")
	}
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `cd ~/home/pi-controller && go test ./cmd/pic/ -run TestBackfillRequestRendered`
Expected: FAIL — `backfillRequest` undefined.

- [ ] **Step 3: Extract `backfillRequest` and set `Rendered`**

In `cmd/pic/history.go`, factor the request build out of `fetchBackfill` into:

```go
// backfillRequest builds the ctrl_get_recent request for a backfill fetch.
// Rendered is the inverse of raw: the human/JSON views want the normalized
// (pi-vocabulary) stream; --raw wants the verbatim backend frames.
func backfillRequest(childID string, opts historyOpts) protocol.GetRecentRequest {
	limit := 0
	if opts.tailN > 0 {
		limit = opts.tailN
	}
	return protocol.GetRecentRequest{
		Type:     protocol.TypeCtrlGetRecent,
		ChildID:  childID,
		Limit:    limit,
		Include:  opts.include,
		Exclude:  opts.exclude,
		Rendered: !opts.raw,
	}
}
```

and change `fetchBackfill` to use it:

```go
	req := backfillRequest(childID, opts)
	resp, err := c.Request(ctx, req)
```

(Leave the rest of `fetchBackfill` — the `tailN == 0` short-circuit, response decode — unchanged.)

- [ ] **Step 4: Run tests + vet**

Run: `cd ~/home/pi-controller && go test ./cmd/pic/ && go vet ./cmd/pic/`
Expected: PASS, no vet output.

- [ ] **Step 5: Commit**

```bash
cd ~/home/pi-controller
git add cmd/pic/history.go cmd/pic/history_test.go
git commit -m "pic: request rendered frames for non-raw backfill"
```

---

## Task 6: pic-attach scrollback requests rendered frames

**Files:**
- Modify: `attach/src/client.ts`
- Test: `attach/src/client.test.ts`

- [ ] **Step 1: Write the failing test**

In `attach/src/client.test.ts`, add a test that `getRecent` sends `rendered:true` in the request frame. Reuse the file's existing fake-socket/Client harness (it already tests `request`); capture the written frame and assert `rendered === true`:

```ts
test("getRecent requests rendered frames", async () => {
  const { client, sent } = makeClientWithCapture(); // use the file's existing capture helper
  void client.getRecent("c1", 10);
  const frame = JSON.parse(sent.at(-1)!);
  expect(frame.type).toBe("ctrl_get_recent");
  expect(frame.rendered).toBe(true);
  expect(frame.limit).toBe(10);
});
```

If no capture helper exists, inspect how `client.test.ts` asserts on written frames and mirror it.

- [ ] **Step 2: Run it to verify it fails**

Run: `cd ~/home/pi-controller/attach && bun test src/client.test.ts -t "getRecent requests rendered"`
Expected: FAIL — `rendered` not in the frame.

- [ ] **Step 3: Add `rendered:true` to getRecent**

In `attach/src/client.ts` `getRecent`, add `rendered: true` to the request object:

```ts
    async getRecent(childId: string, limit: number): Promise<Record<string, unknown>[]> {
        const req: Record<string, unknown> = { type: "ctrl_get_recent", childId, rendered: true };
        if (limit > 0) req["limit"] = limit;
        const resp = await this.request(req);
        if (!resp.success) {
            throw new Error(`ctrl_get_recent failed: ${resp.error?.code ?? "unknown"}`);
        }
        const data = resp.data as { events?: Record<string, unknown>[] } | undefined;
        return data?.events ?? [];
    }
```

- [ ] **Step 4: Run tests**

Run: `cd ~/home/pi-controller/attach && bun test src/client.test.ts`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
cd ~/home/pi-controller
git add attach/src/client.ts attach/src/client.test.ts
git commit -m "pic-attach: request rendered frames for scrollback priming"
```

---

# Sub-project B: claude slash_commands → TUI completion

## Task 7: Capture slash_commands → ChildSummary

**Files:**
- Modify: `internal/child/sniff.go`, `internal/child/provider_claude.go`, `internal/child/child.go`, `cmd/pi-controller/controller.go`, `internal/store/session.go`, `protocol/types.go`, `internal/server/dispatch.go`
- Test: `internal/child/provider_claude_test.go`, `internal/server/dispatch_test.go`

- [ ] **Step 1: Write the failing parse test**

In `internal/child/provider_claude_test.go`, add:

```go
func TestParseInitCapturesSlashCommands(t *testing.T) {
	line := []byte(`{"type":"system","subtype":"init","session_id":"s1","model":"claude","slash_commands":["compact","review","init"]}`)
	res := ClaudeProvider{}.Parse(line)
	if !res.HasMeta {
		t.Fatal("init frame should produce metadata")
	}
	want := []string{"compact", "review", "init"}
	if len(res.Meta.SlashCommands) != 3 || res.Meta.SlashCommands[0] != "compact" {
		t.Fatalf("SlashCommands = %v, want %v", res.Meta.SlashCommands, want)
	}
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `cd ~/home/pi-controller && go test ./internal/child/ -run TestParseInitCapturesSlashCommands`
Expected: FAIL — `SlashCommands` field missing / not populated.

- [ ] **Step 3: Add the field + parse it**

In `internal/child/sniff.go` `SnifferMetadata`, add:

```go
	SlashCommands []string // claude init frame's slash_commands list (names only)
```

In `internal/child/provider_claude.go`, extend the `claudeFrame` struct used by `Parse` with the field:

```go
	SlashCommands []string `json:"slash_commands,omitempty"`
```

and in `Parse`'s `system`/`init` branch, populate it:

```go
		if f.Subtype == "init" {
			res.FirstResponse = true
			if f.SessionID != "" || f.Model != "" || len(f.SlashCommands) > 0 {
				res.Meta = SnifferMetadata{
					SessionID:     f.SessionID,
					Model:         f.Model,
					SlashCommands: f.SlashCommands,
				}
				res.HasMeta = true
			}
		}
```

- [ ] **Step 4: Store on the child + expose via Metadata**

In `internal/child/child.go` `handleFrame`, inside the `if res.HasMeta` block (after the Model handling, ~line 457), add:

```go
		if len(md.SlashCommands) > 0 {
			c.meta.SlashCommands = md.SlashCommands
		}
```

`Metadata()` already returns `c.meta` (which now includes `SlashCommands`), so no change there.

- [ ] **Step 5: Add the store field**

In `internal/store/session.go`, add to `Session` (near `Labels`):

```go
	// SlashCommands is the claude child's advertised slash-command list (names),
	// captured from its init frame. Empty for pi children.
	SlashCommands []string
```

Add the same to `Snapshot`, and in `Snapshot()` add to the literal:

```go
		SlashCommands: copyStrings(s.SlashCommands),
```

- [ ] **Step 6: Sync child → store in monitorChild**

In `cmd/pi-controller/controller.go` `monitorChild` (the polling block ~line 1360-1383 that reads `md := ch.Metadata()`), add a one-time sync after the session-meta block:

```go
				// Capture claude's advertised slash commands once they appear
				// (claude emits them in the init frame; static for the session).
				if len(md.SlashCommands) > 0 && !slashSynced {
					sc := md.SlashCommands
					_ = c.st.Update(childID, func(s *store.Session) { s.SlashCommands = sc })
					slashSynced = true
				}
```

Declare `slashSynced := false` alongside the other `lastKnown*` locals at the top of `monitorChild` (find where `lastKnownModel`/`lastKnownName` are declared and add it there).

Confirm `c.st.Update(id, func(*store.Session))` exists (it does — store.go:133). Confirm `store` is imported in controller.go (it is).

- [ ] **Step 7: Surface on ChildSummary**

In `protocol/types.go` `ChildSummary`, add:

```go
	SlashCommands []string `json:"slashCommands,omitempty"`
```

In `internal/server/dispatch.go` `snapshotToSummary`, set it (near the `Labels` handling):

```go
	if len(snap.SlashCommands) > 0 {
		cs.SlashCommands = snap.SlashCommands
	}
```

- [ ] **Step 8: Dispatch/summary test**

In `internal/server/dispatch_test.go`, add a case asserting a store session with `SlashCommands` surfaces them in the `ctrl_get` response. If the fake controller's `Get` returns a snapshot you control, set `SlashCommands` on it and assert the decoded `ChildSummary.SlashCommands`. Mirror the existing `ctrl_get` test in the file.

```go
func TestDispatchGetCarriesSlashCommands(t *testing.T) {
	fc := &fakeController{getSnap: store.Snapshot{ChildID: "c1", Status: protocol.StatusIdle, SlashCommands: []string{"compact", "review"}}}
	d := server.NewDispatch(fc)
	resp := d.HandleFrame(nil, []byte(`{"type":"ctrl_get","id":"1","childId":"c1"}`))
	var r protocol.Response
	_ = json.Unmarshal(resp, &r)
	var cs protocol.ChildSummary
	_ = json.Unmarshal(r.Data, &cs)
	if len(cs.SlashCommands) != 2 || cs.SlashCommands[0] != "compact" {
		t.Fatalf("SlashCommands = %v, want [compact review]", cs.SlashCommands)
	}
}
```

(Adapt `getSnap`/fake field names to the file's actual fake controller. If `Get` is hardwired, add a settable snapshot field.)

- [ ] **Step 9: Run tests + vet**

Run: `cd ~/home/pi-controller && go test ./internal/child/ ./internal/store/ ./internal/server/ ./cmd/pi-controller/ && go vet ./...`
Expected: PASS, no vet output.

- [ ] **Step 10: Commit**

```bash
cd ~/home/pi-controller
git add internal/child/sniff.go internal/child/provider_claude.go internal/child/provider_claude_test.go internal/child/child.go cmd/pi-controller/controller.go internal/store/session.go protocol/types.go internal/server/dispatch.go internal/server/dispatch_test.go
git commit -m "daemon: capture claude slash_commands into ChildSummary"
```

---

## Task 8: pic-attach autocomplete serves claude slash_commands

**Files:**
- Modify: `cmd/pic/picembed/pic-helpers/index.ts`
- Test: `attach/src/pic-helpers.test.ts`

Read `cmd/pic/picembed/pic-helpers/index.ts` first: `setupTuiAutocomplete` defines `refresh()` which calls `fetchChildKind` then, for `kind === "pi"`, `fetchCommandsFromDaemon`; otherwise sets `cachedCommands = []`. `fetchChildKind` already does a one-shot `ctrl_get` and reads `data.kind`.

- [ ] **Step 1: Write the failing test**

In `attach/src/pic-helpers.test.ts`, add a test for a new exported helper `slashCommandsToCommandInfo` that maps a string[] into the `CommandInfo[]` shape the provider consumes:

```ts
test("slashCommandsToCommandInfo maps names to CommandInfo", () => {
  const got = slashCommandsToCommandInfo(["compact", "review"]);
  expect(got).toEqual([
    { name: "compact", description: undefined },
    { name: "review", description: undefined },
  ]);
});
```

- [ ] **Step 2: Run it to verify it fails**

Run: `cd ~/home/pi-controller/attach && bun test src/pic-helpers.test.ts -t slashCommandsToCommandInfo`
Expected: FAIL — `slashCommandsToCommandInfo` is not exported.

- [ ] **Step 3: Add the helper + claude branch + fetch**

In `cmd/pic/picembed/pic-helpers/index.ts`, add the exported pure helper (near `filterCommandSuggestions`):

```ts
/**
 * Map claude's slash_commands (names only) into the CommandInfo shape the
 * autocomplete provider consumes. claude advertises no descriptions.
 */
export function slashCommandsToCommandInfo(names: string[]): CommandInfo[] {
    return names.map((name) => ({ name, description: undefined }));
}
```

Add a one-shot fetch that reads `slashCommands` off a `ctrl_get` response (model it on `fetchChildKind`, which already does the `ctrl_get` round-trip):

```ts
/**
 * Fetch a claude child's advertised slash commands via a one-shot ctrl_get,
 * read from the daemon's store (not forwarded to the child). Returns [] on any
 * failure.
 */
async function fetchSlashCommandsFromDaemon(socketPath: string, childId: string): Promise<string[]> {
    return new Promise<string[]>((resolve) => {
        const socket = net.createConnection(socketPath);
        let buf = "";
        let done = false;
        const finish = (cmds: string[]): void => {
            if (done) return;
            done = true;
            clearTimeout(timer);
            socket.destroy();
            resolve(cmds);
        };
        const timer = setTimeout(() => finish([]), 5_000);
        socket.on("connect", () => {
            socket.write(JSON.stringify({ type: "ctrl_get", childId }) + "\n");
        });
        socket.on("data", (chunk: Buffer) => {
            buf += chunk.toString("utf8");
            const lines = buf.split("\n");
            buf = lines.pop() ?? "";
            for (const line of lines) {
                if (!line.trim()) continue;
                let msg: Record<string, unknown>;
                try {
                    msg = JSON.parse(line) as Record<string, unknown>;
                } catch {
                    continue;
                }
                if (msg["type"] !== "ctrl_response") continue;
                const data = msg["data"] as { slashCommands?: string[] } | undefined;
                finish(Array.isArray(data?.slashCommands) ? data!.slashCommands! : []);
                return;
            }
        });
        socket.on("error", () => finish([]));
        socket.on("close", () => finish([]));
    });
}
```

In `refresh()`, replace the `kind !== "pi"` early-out with a claude branch:

```ts
        try {
            const kind = await fetchChildKind(socketPath, childId);
            if (kind === "pi") {
                cachedCommands = await fetchCommandsFromDaemon(socketPath, childId);
            } else if (kind === "claude") {
                cachedCommands = slashCommandsToCommandInfo(
                    await fetchSlashCommandsFromDaemon(socketPath, childId),
                );
            } else {
                cachedCommands = [];
            }
        } catch (err: unknown) {
            console.error(
                "[pic-helpers] failed to refresh daemon commands:",
                err instanceof Error ? err.message : String(err),
            );
        }
```

- [ ] **Step 4: Run tests + typecheck**

Run: `cd ~/home/pi-controller/attach && bun test src/pic-helpers.test.ts && bunx tsc --noEmit`
Expected: PASS, clean. (`pic-helpers.test.ts` imports from the relative `index.ts` — confirm the import path resolves; the file already imports `filterCommandSuggestions` from it.)

- [ ] **Step 5: Rebuild the embedded helper bump**

`pic-helpers` is embedded in `pic` and versioned via its `package.json`. Bump the version so `pic` reinstalls it on next run. Edit `cmd/pic/picembed/pic-helpers/package.json` and increment the `version` patch number.

- [ ] **Step 6: Commit**

```bash
cd ~/home/pi-controller
git add cmd/pic/picembed/pic-helpers/index.ts cmd/pic/picembed/pic-helpers/package.json attach/src/pic-helpers.test.ts
git commit -m "pic-attach: serve claude slash_commands as TUI autocomplete"
```

---

## Task 9: Final verification

- [ ] **Step 1: Full Go test + vet**

Run: `cd ~/home/pi-controller && go test ./... && go vet ./...`
Expected: all pass, no vet output.

- [ ] **Step 2: Full bun test + typecheck**

Run: `cd ~/home/pi-controller/attach && bunx tsc --noEmit` and run each test file (the full-file `session.test.ts` run truncates on a pre-existing `ctrl_daemon_shutdown` self-SIGTERM — run other files together and session groups individually): `bun test src/client.test.ts src/runtime.test.ts src/local-services.test.ts src/pic-helpers.test.ts src/claude-normalized.test.ts`, then `bun test src/session.test.ts -t primeHistory`.
Expected: all pass.

- [ ] **Step 3: Build all artifacts**

Run: `cd ~/home/pi-controller && make build`
Expected: builds `pi-controller`, `pic`, and `pic-attach` cleanly.

- [ ] **Step 4: Manual smoke (requires daemon restart — operator runs this)**

After `make update` + daemon restart, against a live **claude** child:
```bash
./bin/pic tail <claude-child>     # backfill renders the conversation, not raw JSON
./bin/pic logs <claude-child>     # full rendered transcript
./bin/pic logs <claude-child> --raw   # still raw claude stream-json
./bin/pic attach <claude-child>   # TUI shows prior transcript scrollback + slash-command autocomplete on "/"
```
Expected: rendered views for claude; `--raw` unchanged; `/`-autocomplete lists claude slash commands.

---

## Spec coverage check

- §1 provider `Normalizes()` → Task 1.
- §A1 render-ring capture (both publish sites) → Task 2.
- §A2 `ExitedRenderRing` + `render.jsonl.gz` → Task 3.
- §A3 `GetRecent` `Rendered` selector (live render-ring / exited snapshot / disk; pi → raw) → Task 4.
- §A4 CLI rendered path → Task 5; bun scrollback rendered → Task 6.
- §B1 capture slash_commands → ChildSummary → Task 7.
- §B2 pic-attach autocomplete claude path → Task 8.
- Testing → per-task tests + Task 9.

## Notes / deferred

- Rendered backfill is faithful within ring/disk retention bounds; very long claude sessions that evicted early frames degrade at the tail (documented limitation; same bound as the raw ring).
- Bonus from Task 4: the raw path now also falls back to `out.jsonl.gz` on disk for orphans, fixing the earlier "exited-after-restart shows nothing" gap for `--raw` too.
- Carry-over deferred items from the prior plan (backfill `-n` filter-after-limit, `--profile` not reaching backfill) are unchanged and out of scope here.
