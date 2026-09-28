// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"google.golang.org/protobuf/proto"

	"go.graveland.dev/rafiki/pkg/child"
	"go.graveland.dev/rafiki/pkg/childstore"
	"go.graveland.dev/rafiki/pkg/eventlog"
	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/nativebus"
	"go.graveland.dev/rafiki/pkg/protocol"

	"github.com/multigres/testkit/assert"
)

// ─── test doubles ─────────────────────────────────────────────────────────────

// fakeScriptClock is a manual clock the test drives: it answers Now() and
// hands out fireable timers, so the 250 ms flush is deterministic instead of
// a real sleep.
type fakeScriptClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []*fakeScriptTimer
}

func newFakeScriptClock() *fakeScriptClock {
	return &fakeScriptClock{now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
}

func (c *fakeScriptClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeScriptClock) NewTimer(d time.Duration) scriptOutputTimer {
	t := &fakeScriptTimer{ch: make(chan time.Time, 1), deadline: c.Now().Add(d)}
	c.mu.Lock()
	c.timers = append(c.timers, t)
	c.mu.Unlock()
	return t
}

// pending returns the timers the loop has requested and not yet stopped —
// at most one at a time, since the loop stops each timer before making the
// next.
func (c *fakeScriptClock) pending() []*fakeScriptTimer {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []*fakeScriptTimer
	for _, t := range c.timers {
		if !t.stopped() {
			out = append(out, t)
		}
	}
	return out
}

// advance moves the clock and fires every timer whose deadline has passed.
func (c *fakeScriptClock) advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	var due []*fakeScriptTimer
	for _, t := range c.timers {
		if !t.stopped() && !t.deadline.After(c.now) {
			due = append(due, t)
		}
	}
	c.mu.Unlock()
	for _, t := range due {
		t.fire()
	}
}

type fakeScriptTimer struct {
	ch       chan time.Time
	deadline time.Time
	mu       sync.Mutex
	isStop   bool
}

func (t *fakeScriptTimer) C() <-chan time.Time { return t.ch }

func (t *fakeScriptTimer) Stop() {
	t.mu.Lock()
	t.isStop = true
	t.mu.Unlock()
}

func (t *fakeScriptTimer) stopped() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.isStop
}

func (t *fakeScriptTimer) fire() {
	select {
	case t.ch <- t.deadline:
	default:
	}
}

// newScriptOutputTestController builds the minimal Controller the coalescer
// publishes through: a native bus to subscribe to and a durable event log to
// assign ordinals from. Everything else is nil, exactly as publishEvent's
// nil-guards expect.
func newScriptOutputTestController() *Controller {
	return &Controller{
		native: nativebus.New(),
		evlog:  eventlog.NewMemory(),
	}
}

// newTestScriptCoalescer builds a coalescer on the given clock and starts its
// flush loop, without registering it (unit tests drive it directly).
func newTestScriptCoalescer(c *Controller, childID string, clock scriptOutputClock) *scriptOutputCoalescer {
	co := &scriptOutputCoalescer{
		c:       c,
		childID: childID,
		clock:   clock,
		streams: make(map[string]*scriptOutStream),
		wake:    make(chan struct{}, 1),
		done:    make(chan struct{}),
		stopped: make(chan struct{}),
	}
	go co.run()
	return co
}

// waitUntil polls cond until it holds or the deadline passes.
func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// scriptOutEvent is one ScriptOutput event as seen on the native bus or in
// the durable log.
type scriptOutEvent struct {
	stream  string
	text    string
	ordinal int32
}

// drainScriptBus drains everything currently queued on a native-bus
// subscription, decoding the ScriptOutput payloads.
func drainScriptBus(busCh <-chan *rafikiv1.Event) []scriptOutEvent {
	var out []scriptOutEvent
	for {
		select {
		case ev := <-busCh:
			so := ev.GetScriptOutput()
			e := scriptOutEvent{text: so.GetText(), stream: so.GetStream()}
			if ev.Ordinal != nil {
				e.ordinal = *ev.Ordinal
			}
			out = append(out, e)
		default:
			return out
		}
	}
}

// ─── the brief's tests ────────────────────────────────────────────────────────

// Four 1 KiB lines coalesce into ONE event once the buffer reaches the
// 4 KiB trigger — and nothing flushes before that, whatever the clock does.
func TestScriptOutputCoalescesBySize(t *testing.T) {
	ck := assert.NewCollecting(t)
	ctrl := newScriptOutputTestController()
	busCh, cancel := ctrl.native.Subscribe("c_1")
	defer cancel()

	co := newTestScriptCoalescer(ctrl, "c_1", newFakeScriptClock())

	line := strings.Repeat("a", 1024)
	for i := 0; i < 3; i++ {
		co.Add("stdout", fmt.Sprintf("%s-%d", line, i), true)
	}
	// Below the trigger, nothing is published — not on a wake, not on a
	// (never fired) deadline.
	time.Sleep(20 * time.Millisecond)
	ck.Eq(0, len(busCh), "an event was published before the 4 KiB trigger: %+v", drainScriptBus(busCh))

	// A5: the buffer seals BEFORE a line that would push it past the trigger,
	// so three lines flush as one size-triggered event and the fourth stays
	// open (its own append would exceed 4096).
	co.Add("stdout", line+"-3", true)
	waitUntil(t, "the size-triggered flush", func() bool { return len(busCh) > 0 })
	evs := drainScriptBus(busCh)
	ck.Require().Eq(1, len(evs), "got %d events for one 4 KiB flush", len(evs))
	ck.Eq("stdout", evs[0].stream, "Stream")
	want := ""
	for i := 0; i < 3; i++ {
		want += fmt.Sprintf("%s-%d\n", line, i)
	}
	ck.Eq(want, evs[0].text, "coalesced text (lines, newline-terminated)")
	ck.Eq(int32(0), evs[0].ordinal, "durable ordinal")

	// The buffer reopens: more output stays buffered until its own trigger.
	co.Add("stdout", "after", true)
	time.Sleep(20 * time.Millisecond)
	ck.Eq(0, len(busCh), "post-flush output published without a trigger")

	// Close flushes the remainder — the -3 line and "after" coalesce into it.
	co.Close()
	waitUntil(t, "the final flush", func() bool { return len(busCh) > 0 })
	evs = drainScriptBus(busCh)
	ck.Require().Eq(1, len(evs), "got %d events for the final flush", len(evs))
	ck.Eq(line+"-3\nafter\n", evs[0].text, "final flush text")
}

// The time trigger: 250 ms after a stream's first unflushed line, whatever is
// buffered flushes — driven here on a manual clock, no real sleeping.
func TestScriptOutputCoalescesByTime(t *testing.T) {
	ck := assert.NewCollecting(t)
	ctrl := newScriptOutputTestController()
	busCh, cancel := ctrl.native.Subscribe("c_1")
	defer cancel()

	clock := newFakeScriptClock()
	co := newTestScriptCoalescer(ctrl, "c_1", clock)

	co.Add("stdout", "slow-line", true)

	// The loop arms exactly one 250 ms timer anchored at the line's arrival.
	waitUntil(t, "the flush timer", func() bool { return len(clock.pending()) == 1 })
	pend := clock.pending()
	ck.Require().Eq(1, len(pend), "expected one outstanding timer")
	ck.Eq(newFakeScriptClock().now.Add(250*time.Millisecond).UnixNano(), pend[0].deadline.UnixNano(),
		"timer deadline is not first-line + 250 ms")
	ck.Eq(0, len(busCh), "published before the deadline")

	clock.advance(250 * time.Millisecond) // fires the deadline timer
	waitUntil(t, "the timed flush", func() bool { return len(busCh) > 0 })
	evs := drainScriptBus(busCh)
	ck.Require().Eq(1, len(evs), "got %d events for the timed flush", len(evs))
	ck.Eq("stdout", evs[0].stream, "Stream")
	ck.Eq("slow-line\n", evs[0].text, "timed flush text")

	// A line that arrives later anchors its own deadline.
	co.Add("stdout", "second", true)
	waitUntil(t, "the second flush timer", func() bool { return len(clock.pending()) == 1 })
	clock.advance(249 * time.Millisecond)
	time.Sleep(10 * time.Millisecond)
	ck.Eq(0, len(busCh), "flushed before the second line's 250 ms elapsed")
	clock.advance(1 * time.Millisecond)
	waitUntil(t, "the second timed flush", func() bool { return len(busCh) > 0 })
	evs = drainScriptBus(busCh)
	ck.Require().Eq(1, len(evs), "got %d events for the second timed flush", len(evs))
	ck.Eq("second\n", evs[0].text, "second flush text")

	co.Close()
}

// A single line longer than 4 KiB is split at 4 KiB, each piece its own
// event, never merged with any other line — and the concatenated events
// reproduce the raw stream byte for byte.
func TestScriptOutputSplitsLongLine(t *testing.T) {
	ck := assert.NewCollecting(t)
	ctrl := newScriptOutputTestController()
	busCh, cancel := ctrl.native.Subscribe("c_1")
	defer cancel()

	co := newTestScriptCoalescer(ctrl, "c_1", newFakeScriptClock())

	long := strings.Repeat("x", 10*1024)
	co.Add("stdout", long, true)

	// The two full 4 KiB pieces publish at once (size-triggered units); the
	// 2 KiB remainder stays in the open buffer for the final flush.
	waitUntil(t, "the split pieces", func() bool { return len(busCh) >= 2 })
	evs := drainScriptBus(busCh)
	ck.Require().Eq(2, len(evs), "got %d events, want the two 4 KiB pieces", len(evs))
	for i, ev := range evs {
		ck.Eq("stdout", ev.stream, "piece %d Stream", i)
		ck.Eq(4096, len(ev.text), "piece %d is not split at 4 KiB (len %d)", i, len(ev.text))
		ck.False(strings.Contains(ev.text, "\n"), "piece %d carries a newline mid-line", i)
	}

	// A following line joins the remainder — and the whole stream
	// concatenates back to the raw output.
	co.Add("stdout", "next", true)
	co.Close()
	waitUntil(t, "the remainder flush", func() bool { return len(busCh) >= 1 })
	evs = append(evs, drainScriptBus(busCh)...)
	ck.Require().Eq(3, len(evs), "got %d events total", len(evs))
	ck.Eq(long+"\nnext\n", evs[0].text+evs[1].text+evs[2].text,
		"concatenated events do not reproduce the raw stream")
	ck.Eq(2048+1+5, len(evs[2].text), "remainder event length")
}

// Close flushes both streams and stops the flush loop: nothing publishes
// after Close, the timer is stopped (it can never fire again), and a second
// Close is a no-op.
func TestScriptOutputCloseStopsTimer(t *testing.T) {
	ck := assert.NewCollecting(t)
	ctrl := newScriptOutputTestController()
	busCh, cancel := ctrl.native.Subscribe("c_1")
	defer cancel()

	clock := newFakeScriptClock()
	co := newTestScriptCoalescer(ctrl, "c_1", clock)

	co.Add("stdout", "out-1", true)
	co.Add("stderr", "err-1", true)
	waitUntil(t, "the flush timer", func() bool { return len(clock.pending()) == 1 })

	// Close flushes BOTH streams (the brief: output precedes child_exited).
	done := make(chan struct{})
	go func() { co.Close(); close(done) }()
	waitUntil(t, "Close to return", func() bool {
		select {
		case <-done:
			return true
		default:
			return false
		}
	})
	evs := drainScriptBus(busCh)
	ck.Require().Eq(2, len(evs), "got %d events for the final flush", len(evs))
	ck.Eq("stdout", evs[0].stream, "first flush stream (fixed order)")
	ck.Eq("out-1\n", evs[0].text, "stdout flush text")
	ck.Eq("stderr", evs[1].stream, "second flush stream")
	ck.Eq("err-1\n", evs[1].text, "stderr flush text")

	// The timer the loop had armed is stopped, not left to fire.
	pend := clock.pending()
	ck.Eq(0, len(pend), "a timer is still outstanding after Close")
	for _, tm := range clock.timers {
		ck.True(tm.stopped(), "a timer was never stopped")
	}

	// Nothing publishes after Close, whatever the clock does: an Add is
	// dropped, and firing the (stopped) timer emits nothing.
	co.Add("stdout", "too-late", true)
	clock.advance(time.Hour)
	time.Sleep(20 * time.Millisecond)
	ck.Eq(0, len(busCh), "an event was published after Close: %+v", drainScriptBus(busCh))

	// A second Close is a no-op, not a panic.
	co.Close()
}

// The ordering contract, end to end: a real script child through the
// controller's monitorChild — its coalesced output is flushed by
// handleChildExit BEFORE the child_exited event publishes, so the last output
// event's ordinal is below child_exited's.
func TestScriptOutputFlushesBeforeExit(t *testing.T) {
	ck := assert.NewCollecting(t)
	if _, err := exec.LookPath("/bin/sh"); err != nil {
		t.Skip("/bin/sh not available: the exit-ordering test needs a real process")
	}

	ctrl := &Controller{
		st:     childstore.New(),
		cm:     newChildManager(),
		native: nativebus.New(),
		evlog:  eventlog.NewMemory(),
	}
	childID := "c_script_exit"
	ctrl.st.Insert(&childstore.Session{
		ChildID:   childID,
		Kind:      protocol.KindScript,
		Status:    protocol.StatusSpawning,
		StartedAt: time.Now(),
	})

	spec := child.SpawnSpec{
		ChildID:  childID,
		Cwd:      t.TempDir(),
		PiBinary: "/bin/sh",
		Argv:     []string{"-c", "echo out-1; echo out-2; echo err-1 >&2"},
		Provider: child.ScriptProvider{},
	}
	spec.OnScriptOutput = ctrl.scriptOutputHook(childID)

	ch, err := child.Spawn(t.Context(), spec)
	ck.Require().NoError(err, "spawn script child")
	ctrl.cm.Add(childID, ch)
	go ctrl.monitorChild(childID, ch)

	// Wait for the exit to be fully handled: the child_exited record in the
	// durable log is what monitorChild's handleChildExit publishes last.
	waitUntil(t, "child_exited in the event log", func() bool {
		recs, err := ctrl.evlog.Read(t.Context(), childID, -1, 100)
		if err != nil {
			return false
		}
		for _, r := range recs {
			if r.Type == "child_exited" {
				return true
			}
		}
		return false
	})

	recs, err := ctrl.evlog.Read(t.Context(), childID, -1, 100)
	ck.Require().NoError(err, "read event log")

	var lastOutput *eventlog.Record
	var exit *eventlog.Record
	var outputText strings.Builder
	for i := range recs {
		switch recs[i].Type {
		case "script_output":
			lastOutput = &recs[i]
			outputText.WriteString(payloadText(t, recs[i].Payload))
		case "child_exited":
			ck.Nil(exit, "more than one child_exited record")
			exit = &recs[i]
		}
	}
	ck.Require().NotNil(lastOutput, "no script_output event reached the durable log")
	ck.Require().NotNil(exit, "no child_exited event in the durable log")
	// assert.Less(want, got): the last output event's ordinal must be BELOW
	// child_exited's.
	ck.Less(exit.Ordinal, lastOutput.Ordinal,
		"the last output event's ordinal (%d) must be below child_exited's (%d)",
		lastOutput.Ordinal, exit.Ordinal)
	ck.StrContains(outputText.String(), "out-1", "stdout did not reach the log")
	ck.StrContains(outputText.String(), "out-2", "stdout did not reach the log")
	ck.StrContains(outputText.String(), "err-1", "stderr did not reach the log")

	// And the coalescer is gone from the registry with the child.
	ck.Nil(ctrl.takeScriptOutputCoalescer(childID), "the coalescer survived the exit")
}

// payloadText decodes one event-log record's protojson payload and returns
// its ScriptOutput text (empty for any other type).
func payloadText(t *testing.T, payload []byte) string {
	t.Helper()
	var ev struct {
		ScriptOutput *struct {
			Stream string `json:"stream"`
			Text   string `json:"text"`
		} `json:"scriptOutput"`
	}
	if err := json.Unmarshal(payload, &ev); err != nil {
		t.Fatalf("decode event payload: %v", err)
	}
	if ev.ScriptOutput == nil {
		return ""
	}
	return ev.ScriptOutput.Text
}

// ─── fix-round tests (A1, A2, A4, A5, A3) ─────────────────────────────────────

// A1: an oversized line whose 4 KiB cut lands mid-rune must still marshal —
// the split steps back to the last rune boundary and every published event is
// valid proto3. Without the rune-boundary step the durable append fails and
// the unordinaled event kills every live subscriber stream.
func TestScriptOutputSplitsAtRuneBoundary(t *testing.T) {
	ck := assert.NewCollecting(t)
	ctrl := newScriptOutputTestController()
	busCh, cancel := ctrl.native.Subscribe("c_1")
	defer cancel()

	co := newTestScriptCoalescer(ctrl, "c_1", newFakeScriptClock())

	// "é" is two bytes; the second byte of the rune straddling byte 4096 sits
	// at position 4097, so a byte cut at 4096 would split it.
	body := strings.Repeat("a", 4095)
	line := body + "é" + strings.Repeat("b", 1024)
	ck.Eq(4097, len(body+"é"), "fixture: body+é must be 4097 bytes")
	co.Add("stdout", line, true)

	// The 4095-byte piece publishes immediately; the rune itself starts the
	// remainder, which sits in the open buffer until Close.
	waitUntil(t, "the rune-boundary split piece", func() bool { return len(busCh) >= 1 })
	time.Sleep(20 * time.Millisecond)
	evs := drainScriptBus(busCh)
	ck.Require().Eq(1, len(evs), "got %d events before Close", len(evs))
	co.Close()
	waitUntil(t, "the remainder flush", func() bool { return len(busCh) >= 1 })
	evs = append(evs, drainScriptBus(busCh)...)
	ck.Require().Eq(2, len(evs), "got %d events for the split line", len(evs))
	for i := range evs {
		// proto.Marshal refuses invalid UTF-8 in a proto3 string: marshalling
		// every published event IS the assertion.
		mev := &rafikiv1.Event{
			ChildId: "c_1",
			Payload: &rafikiv1.Event_ScriptOutput{ScriptOutput: &rafikiv1.ScriptOutput{
				Stream: evs[i].stream, Text: evs[i].text,
			}},
		}
		_, err := proto.Marshal(mev)
		ck.NoError(err, "event %d does not marshal (invalid UTF-8)", i)
	}
	// The first piece stopped before the split rune; the rune starts the next.
	ck.Eq(4095, len(evs[0].text), "piece 0 cut mid-rune (len %d)", len(evs[0].text))
	ck.True(strings.HasPrefix(evs[1].text, "\u00e9"), "the remainder does not start with the split rune")
}

// A1: any non-UTF-8 byte on stderr must not take down the durable append or
// the subscriber streams: the line is sanitized to valid UTF-8 at Add entry.
func TestScriptOutputSanitizesInvalidUTF8(t *testing.T) {
	ck := assert.NewCollecting(t)
	ctrl := newScriptOutputTestController()
	busCh, cancel := ctrl.native.Subscribe("c_1")
	defer cancel()

	co := newTestScriptCoalescer(ctrl, "c_1", newFakeScriptClock())
	co.Add("stderr", "bad \xff byte and \xc3 broken rune", true)
	co.Close()

	waitUntil(t, "the sanitized flush", func() bool { return len(busCh) > 0 })
	evs := drainScriptBus(busCh)
	ck.Require().Eq(1, len(evs), "got %d events", len(evs))
	ev := &rafikiv1.Event{
		ChildId: "c_1",
		Payload: &rafikiv1.Event_ScriptOutput{ScriptOutput: &rafikiv1.ScriptOutput{
			Stream: evs[0].stream, Text: evs[0].text,
		}},
	}
	_, err := proto.Marshal(ev)
	ck.NoError(err, "the sanitized event does not marshal")
	ck.True(utf8.ValidString(evs[0].text), "event text is still invalid UTF-8")
	ck.StrContains(evs[0].text, "\uFFFD", "invalid bytes were not replaced with U+FFFD")
}

// A2: a newline-free stderr blob is handed off in bounded fragments, so the
// child-side accumulator never exceeds the fragment bound and the fragments
// concatenate back to the input (modulo the documented UTF-8 sanitization).
func TestScriptOutputFragmentsBoundedPending(t *testing.T) {
	ck := assert.NewCollecting(t)
	if _, err := exec.LookPath("/bin/sh"); err != nil {
		t.Skip("/bin/sh not available: this test needs a real process")
	}

	var mu sync.Mutex
	var frags []string
	var maxPending atomic.Int64

	blob := strings.Repeat("x", 1<<20) // 1 MiB, no newline
	dir := t.TempDir()
	blobPath := filepath.Join(dir, "blob")
	ck.NoError(os.WriteFile(blobPath, []byte(blob), 0o600), "write blob fixture")
	c, err := child.Spawn(context.Background(), child.SpawnSpec{
		ChildID:  "c_script_fragbound",
		Cwd:      dir,
		PiBinary: "/bin/sh",
		Argv:     []string{"-c", "cat " + blobPath + " >&2"},
		Provider: child.ScriptProvider{},
		OnScriptOutput: func(stream, line string, terminated bool) {
			mu.Lock()
			frags = append(frags, line)
			maxPending.Store(max(maxPending.Load(), int64(len(line))))
			mu.Unlock()
		},
	})
	ck.Require().NoError(err, "Spawn")
	t.Cleanup(func() { _, _ = c.Shutdown(time.Second, time.Second) })

	var total int
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		total = len(strings.Join(frags, ""))
		n := len(frags)
		mu.Unlock()
		if n > 0 && total >= len(blob) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	ck.False(maxPending.Load() > 4096, "a hook chunk exceeded the fragment bound: %d", maxPending.Load())
	mu.Lock()
	joined := strings.Join(frags, "")
	mu.Unlock()
	ck.Eq(len(blob), len(joined), "fragment concatenation lost bytes")
	ck.Eq(blob, joined, "fragments do not concatenate back to the input")
}

// A2 (coalescer side): a fragment seals as its own unit with NO '\n' added, so
// terminated lines and fragments concatenate back to the raw stream.
func TestScriptOutputFragmentSealsAsItsOwnUnit(t *testing.T) {
	ck := assert.NewCollecting(t)
	ctrl := newScriptOutputTestController()
	busCh, cancel := ctrl.native.Subscribe("c_1")
	defer cancel()

	co := newTestScriptCoalescer(ctrl, "c_1", newFakeScriptClock())
	co.Add("stderr", "frag-one", false)
	co.Add("stderr", "frag-two", false)
	co.Add("stderr", "a full line", true)
	co.Close()

	waitUntil(t, "the fragment flushes", func() bool { return len(busCh) >= 3 })
	evs := drainScriptBus(busCh)
	ck.Require().Eq(3, len(evs), "got %d events, want each fragment as its own unit", len(evs))
	var joined string
	for i, ev := range evs {
		joined += ev.text
		if i < 2 {
			ck.False(strings.Contains(ev.text, "\n"), "fragment %d gained a newline", i)
		}
	}
	ck.Eq("frag-onefrag-twoa full line\n", joined, "fragments+line concatenation")
}

// A4: the 250 ms anchor resets when a size seal happens — lines arriving
// after a seal flush 250 ms after THEIR arrival, not the sealed content's.
func TestScriptOutputAnchorResetsAfterSeal(t *testing.T) {
	ck := assert.NewCollecting(t)
	ctrl := newScriptOutputTestController()
	busCh, cancel := ctrl.native.Subscribe("c_1")
	defer cancel()

	clock := newFakeScriptClock()
	co := newTestScriptCoalescer(ctrl, "c_1", clock)

	// Fill to exactly the trigger: the append seals.
	co.Add("stdout", strings.Repeat("a", 4095), true)
	waitUntil(t, "the size seal flush", func() bool { return len(busCh) > 0 })
	drainScriptBus(busCh)

	// One more line 10 ms later: its deadline must be ITS arrival + 250 ms
	// (t=460), not the sealed content's anchor (t=200).
	clock.advance(10 * time.Millisecond)
	co.Add("stdout", "after-seal", true)
	waitUntil(t, "the post-seal timer", func() bool { return len(clock.pending()) == 1 })
	pend := clock.pending()
	ck.Require().Eq(1, len(pend), "expected one outstanding timer")
	ck.Eq(newFakeScriptClock().now.Add(10*time.Millisecond+250*time.Millisecond).UnixNano(),
		pend[0].deadline.UnixNano(), "the post-seal anchor was not reset to the new line's arrival")

	// Before t=460 nothing flushes; at t=460 it does.
	clock.advance(249 * time.Millisecond)
	time.Sleep(20 * time.Millisecond)
	ck.Eq(0, len(busCh), "flushed before the post-seal line's own 250 ms elapsed")
	clock.advance(1 * time.Millisecond)
	waitUntil(t, "the post-seal timed flush", func() bool { return len(busCh) > 0 })
	evs := drainScriptBus(busCh)
	ck.Require().Eq(1, len(evs), "got %d events", len(evs))
	ck.Eq("after-seal\n", evs[0].text, "post-seal flush text")
	co.Close()
}

// A5: a size-triggered event stays ≤ 4 KiB + newline — the open buffer seals
// BEFORE a line that would push it past the trigger, and a remainder append
// that already meets the trigger seals too.
func TestScriptOutputSizeSealBoundsEvent(t *testing.T) {
	ck := assert.NewCollecting(t)
	ctrl := newScriptOutputTestController()
	busCh, cancel := ctrl.native.Subscribe("c_1")
	defer cancel()

	co := newTestScriptCoalescer(ctrl, "c_1", newFakeScriptClock())

	// 4095 + 1 (a line just over the trigger with this append) — the buffer
	// seals first, so no event exceeds 4097 bytes.
	co.Add("stdout", strings.Repeat("a", 4094), true)
	co.Add("stdout", strings.Repeat("b", 4095), true)
	co.Close()

	waitUntil(t, "the bounded flushes", func() bool { return len(busCh) >= 1 })
	var evs []scriptOutEvent
	evs = append(evs, drainScriptBus(busCh)...)
	for _, ev := range evs {
		ck.False(len(ev.text) > 4097, "an event is %d bytes, want ≤ 4097", len(ev.text))
	}
	joined := ""
	for _, ev := range evs {
		joined += ev.text
	}
	ck.Eq(strings.Repeat("a", 4094)+"\n"+strings.Repeat("b", 4095)+"\n", joined,
		"concatenation lost bytes across the pre-append seal")
}

// A3: a first line arriving AFTER the child's exit is dropped by the hook —
// no coalescer registration, no publish after child_exited, no leaked
// goroutine.
func TestScriptOutputHookDropsLinesAfterExit(t *testing.T) {
	ck := assert.NewCollecting(t)
	ctrl := newScriptOutputTestController()

	hook := ctrl.scriptOutputHook("c_late")
	hook("stdout", "in-time", true)

	// The exit path takes and closes the coalescer, marking the state exited
	// in the same critical section.
	co := ctrl.takeScriptOutputCoalescer("c_late")
	ck.NotNil(co, "the in-time line registered no coalescer")
	co.Close()

	// The late (abandon-path) line: dropped, never registered.
	busCh, cancel := ctrl.native.Subscribe("c_late")
	defer cancel()
	hook("stderr", "late-after-exit", true)
	hook("stdout", "also-late", false)
	time.Sleep(50 * time.Millisecond)
	ck.Eq(0, len(busCh), "a line was published after the exit: %+v", drainScriptBus(busCh))
	ck.Nil(ctrl.takeScriptOutputCoalescer("c_late"),
		"the late line registered a coalescer after the exit")
	// And the dropped registration left no registry state behind.
	ctrl.scriptOutputsMu.Lock()
	n := len(ctrl.scriptOutputState)
	ctrl.scriptOutputsMu.Unlock()
	ck.Eq(1, n, "unexpected scriptOutputState entries")
}
