// SPDX-License-Identifier: Apache-2.0

package tui

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/multigres/testkit/assert"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/tui/rail"
)

// testChildID is a realistic top-level child id: "c_" plus a 26-character
// ULID, which is 28 bytes and fits one OSC 7501 id segment.
const testChildID = "c_01M235RH46KEAJHCFK1SBTQM39"

// osc7501ReplyFrame is the support reply a terminal that understands the
// protocol sends to the "?" probe: an OSC whose command number is 7501.
func osc7501ReplyFrame() uv.UnknownOscEvent {
	return uv.UnknownOscEvent("\x1b]7501;?;states=working,blocked,error,done,idle,clear;kinds=question\x1b\\")
}

// supportedEmitter is an emitter that has already learned the terminal
// supports OSC 7501 from a reply. The production done-settle delay is disabled
// so these tests pin the raw edge-trigger mapping; the settle has its own
// tests below (settlingEmitter).
func supportedEmitter() *osc7501Emitter {
	e := newOSC7501Emitter(env(nil))
	e.observe(osc7501ReplyFrame())
	e.doneDelay = 0
	return e
}

// settlingEmitter is a supported emitter carrying the real done settle delay
// and a controllable clock. Hold the clock still to keep a held done pending,
// or advance it past osc7501DoneDelay to let the done go out -- no real timer.
func settlingEmitter(now func() time.Time) *osc7501Emitter {
	e := newOSC7501Emitter(env(nil))
	e.observe(osc7501ReplyFrame())
	e.now = now
	return e
}

// dsp builds one top-level display named "alice" carrying msg.
func dsp(state protocol.DisplayState, waiting protocol.WaitingOn, msg string) rail.TopLevelDisplay {
	return rail.TopLevelDisplay{
		ChildID:      testChildID,
		Name:         "alice",
		DisplayState: string(state),
		WaitingOn:    string(waiting),
		DisplayMsg:   msg,
	}
}

// TestOSC7501SequenceGolden pins the exact bytes of one record per state:
// working, blocked(question, with msg), error, done, idle and clear.
func TestOSC7501SequenceGolden(t *testing.T) {
	ck := assert.NewCollecting(t)
	id := testChildID

	working := supportedEmitter()
	ck.Eq("\x1b]7501;state=working:id="+id+":app=rafiki\x1b\\",
		working.Sync([]rail.TopLevelDisplay{dsp(protocol.DisplayWorking, "", "")}),
		"working record")

	blocked := supportedEmitter()
	ck.Eq("\x1b]7501;state=blocked:id="+id+":app=rafiki:kind=question:msg=aGk=:title=YWxpY2U=\x1b\\",
		blocked.Sync([]rail.TopLevelDisplay{dsp(protocol.DisplayWaiting, protocol.WaitingOnOperator, "hi")}),
		"blocked/question record carries the base64 msg and title")

	failed := supportedEmitter()
	ck.Eq("\x1b]7501;state=error:id="+id+":app=rafiki:msg=Ym9vbQ==:title=YWxpY2U=\x1b\\",
		failed.Sync([]rail.TopLevelDisplay{dsp(protocol.DisplayFailed, "", "boom")}),
		"error record carries the base64 msg and title")

	done := supportedEmitter()
	ck.NotEmpty(done.Sync([]rail.TopLevelDisplay{dsp(protocol.DisplayWorking, "", "")}), "work first")
	ck.Eq("\x1b]7501;state=done:id="+id+":app=rafiki\x1b\\",
		done.Sync([]rail.TopLevelDisplay{dsp(protocol.DisplayIdle, "", "")}),
		"done record after work")

	idle := supportedEmitter()
	ck.NotEmpty(idle.Sync([]rail.TopLevelDisplay{dsp(protocol.DisplayWorking, "", "")}), "work first")
	ck.Eq("\x1b]7501;state=idle:id="+id+":app=rafiki\x1b\\",
		idle.Sync([]rail.TopLevelDisplay{dsp(protocol.DisplayWaiting, protocol.WaitingOnEvents, "")}),
		"a non-operator wait drops a prior working with idle")

	clear := supportedEmitter()
	ck.NotEmpty(clear.Sync([]rail.TopLevelDisplay{dsp(protocol.DisplayWorking, "", "")}), "something emitted first")
	ck.Eq("\x1b]7501;state=clear:id="+id+":app=rafiki\x1b\\",
		clear.Sync([]rail.TopLevelDisplay{dsp(protocol.DisplayExited, "", "")}),
		"clear record on exit")
}

// TestOSC7501SanitisesMsg pins the message sanitiser: control characters
// become spaces, runs collapse, and truncation never splits a rune.
func TestOSC7501SanitisesMsg(t *testing.T) {
	ck := assert.NewCollecting(t)

	ck.Eq("a b c", osc7501Sanitise("a\nb\tc", 100), "newline and tab become spaces")
	ck.Eq("a b", osc7501Sanitise("a\u0085b", 100), "a C1 control becomes a space")
	ck.Eq("a b", osc7501Sanitise("a \x00\x00 b", 100), "runs of control-derived spaces collapse")
	ck.Eq("hi there", osc7501Sanitise("  hi   there  ", 100), "trims and collapses")
	ck.Eq("", osc7501Sanitise(" \n\t\u007f ", 100), "an all-control message trims to empty")

	// 3000 multi-byte runes: 6000 bytes, twice the limit. Truncation must land
	// on a rune boundary, so the result is valid UTF-8 and exactly at the cap.
	multi := osc7501Sanitise(strings.Repeat("é", 3000), osc7501MaxMsg)
	ck.True(len(multi) <= osc7501MaxMsg, "multi-byte truncation respects the byte limit")
	ck.True(utf8.ValidString(multi), "multi-byte truncation does not split a rune")
	ck.Eq(osc7501MaxMsg, len(multi), "multi-byte truncation fills the limit without splitting a rune")

	// A 3000-byte ASCII message ends at the byte limit.
	ascii := osc7501Sanitise(strings.Repeat("x", 3000), osc7501MaxMsg)
	ck.Eq(osc7501MaxMsg, len(ascii), "a 3000-byte message ends at 2048 decoded bytes")
}

// TestOSC7501DiscardsOverlongSequences pins two whole-record discards: an id
// the grammar cannot express emits nothing for that child, and a sequence that
// would exceed the record limit is dropped rather than shipped.
func TestOSC7501DiscardsOverlongSequences(t *testing.T) {
	ck := assert.NewCollecting(t)

	e := supportedEmitter()
	bad := []string{
		"c_has a space",
		"c_has:colon",
		"c_" + strings.Repeat("x", 33), // one segment over 32 bytes
		strings.Repeat("x", 33) + "/ok",
		"",
	}
	for _, id := range bad {
		d := dsp(protocol.DisplayWorking, "", "")
		d.ChildID = id
		ck.Eq("", e.Sync([]rail.TopLevelDisplay{d}), "an id that fails the segment pattern emits nothing: %q", id)
	}

	// The whole-sequence cap: a value that would push the record past 4096
	// bytes voids it. (The decoded msg/title caps keep a real record well
	// under this, so the guard is a safety net, but it must still fire.)
	ck.Eq("", osc7501(osc7501Working, testChildID, kv{"msg", strings.Repeat("A", osc7501MaxSeq)}),
		"an over-long sequence is discarded whole")
}

// TestOSC7501EdgeTriggered pins that an unchanged state emits once.
func TestOSC7501EdgeTriggered(t *testing.T) {
	ck := assert.NewCollecting(t)
	e := supportedEmitter()
	d := []rail.TopLevelDisplay{dsp(protocol.DisplayWorking, "", "")}

	ck.NotEmpty(e.Sync(d), "the first working emits")
	ck.Eq("", e.Sync(d), "the same state twice emits once")
}

// TestOSC7501NeverIdleOverDone pins that a later idle never erases a done the
// operator has not seen: each report replaces its record completely.
func TestOSC7501NeverIdleOverDone(t *testing.T) {
	ck := assert.NewCollecting(t)
	e := supportedEmitter()

	ck.NotEmpty(e.Sync([]rail.TopLevelDisplay{dsp(protocol.DisplayWorking, "", "")}), "work")
	ck.NotEmpty(e.Sync([]rail.TopLevelDisplay{dsp(protocol.DisplayIdle, "", "")}), "idle after work emits done")
	ck.Eq("", e.Sync([]rail.TopLevelDisplay{dsp(protocol.DisplayIdle, "", "")}), "another idle emits nothing")
	ck.Eq("", e.Sync([]rail.TopLevelDisplay{dsp(protocol.DisplayWaiting, protocol.WaitingOnEvents, "")}),
		"a later idle must not overwrite the done")
}

// TestOSC7501WaitingMapsToIdleOnlyAfterWorkingOrBlocked pins the one rule that
// needs the prior state: a non-operator wait drops a spinner or badge, and
// otherwise says nothing.
func TestOSC7501WaitingMapsToIdleOnlyAfterWorkingOrBlocked(t *testing.T) {
	ck := assert.NewCollecting(t)

	e := supportedEmitter()
	ck.Eq("", e.Sync([]rail.TopLevelDisplay{dsp(protocol.DisplayWaiting, protocol.WaitingOnEvents, "")}),
		"a wait with nothing prior emits nothing")
	ck.NotEmpty(e.Sync([]rail.TopLevelDisplay{dsp(protocol.DisplayWorking, "", "")}), "work")
	ck.Eq(osc7501(osc7501Idle, testChildID),
		e.Sync([]rail.TopLevelDisplay{dsp(protocol.DisplayWaiting, protocol.WaitingOnEvents, "")}),
		"working then a wait emits idle")
	ck.Eq("", e.Sync([]rail.TopLevelDisplay{dsp(protocol.DisplayWaiting, protocol.WaitingOnJob, "")}),
		"a second wait, with last idle, emits nothing")

	b := supportedEmitter()
	ck.NotEmpty(b.Sync([]rail.TopLevelDisplay{dsp(protocol.DisplayWaiting, protocol.WaitingOnOperator, "hi")}), "blocked")
	ck.Eq(osc7501(osc7501Idle, testChildID),
		b.Sync([]rail.TopLevelDisplay{dsp(protocol.DisplayWaiting, protocol.WaitingOnChildren, "")}),
		"blocked then a wait emits idle")
}

// TestOSC7501IdleWithoutWorkEmitsNothing pins that a child that never worked
// produces no done record.
func TestOSC7501IdleWithoutWorkEmitsNothing(t *testing.T) {
	ck := assert.NewCollecting(t)
	e := supportedEmitter()
	ck.Eq("", e.Sync([]rail.TopLevelDisplay{dsp(protocol.DisplayIdle, "", "")}),
		"an idle child that never worked emits nothing")
}

// TestOSC7501OffUntilSupported pins detection by reply: the emitter is off
// until the terminal answers, and the probe goes out regardless.
func TestOSC7501OffUntilSupported(t *testing.T) {
	ck := assert.NewCollecting(t)
	e := newOSC7501Emitter(env(nil))
	d := []rail.TopLevelDisplay{dsp(protocol.DisplayWorking, "", "")}

	ck.Eq("", e.Sync(d), "off until support is established")
	ck.Eq(osc7501Query, e.query(), "the support probe is still sent")

	e.observe(uv.UnknownOscEvent("<not an osc>"))
	ck.Eq("", e.Sync(d), "a non-OSC message establishes nothing")

	e.observe(uv.UnknownOscEvent("\x1b]52;c;aGk=\x1b\\"))
	ck.Eq("", e.Sync(d), "an unrelated OSC establishes nothing")

	e.observe(osc7501ReplyFrame())
	ck.NotEmpty(e.Sync(d), "the 7501 reply turns the emitter on")
}

// TestOSC7501OffUnderTmux pins that $TMUX disables the emitter entirely, even
// after a reply arrived.
func TestOSC7501OffUnderTmux(t *testing.T) {
	ck := assert.NewCollecting(t)
	e := newOSC7501Emitter(env(map[string]string{"TMUX": "/tmp/tmux-1000/default,123,0"}))

	ck.Eq("", e.query(), "no probe is sent under tmux")
	e.observe(osc7501ReplyFrame())
	ck.Eq("", e.Sync([]rail.TopLevelDisplay{dsp(protocol.DisplayWorking, "", "")}),
		"off under tmux even after a reply")
}

// TestOSC7501QuitEmitsNothing pins that there is no teardown emission: on quit
// the cockpit simply stops syncing, and the terminal drops working/blocked
// records itself while keeping done/error.
func TestOSC7501QuitEmitsNothing(t *testing.T) {
	ck := assert.NewCollecting(t)
	e := supportedEmitter()
	d := []rail.TopLevelDisplay{dsp(protocol.DisplayWorking, "", "")}

	ck.NotEmpty(e.Sync(d), "work")
	ck.Eq("", e.Sync(d), "a repeated sync is silent")
	ck.Eq("", e.Sync(nil), "an empty display set emits nothing, not a clear")
}

// TestOSC7501ClearOnExit pins that an exiting child clears its record, and
// that a child with no record emits nothing on exit.
func TestOSC7501ClearOnExit(t *testing.T) {
	ck := assert.NewCollecting(t)

	e := supportedEmitter()
	ck.NotEmpty(e.Sync([]rail.TopLevelDisplay{dsp(protocol.DisplayWorking, "", "")}), "work")
	ck.Eq(osc7501(osc7501Clear, testChildID),
		e.Sync([]rail.TopLevelDisplay{dsp(protocol.DisplayExited, "", "")}),
		"exit clears a record that was emitted")

	e2 := supportedEmitter()
	ck.Eq("", e2.Sync([]rail.TopLevelDisplay{dsp(protocol.DisplayExited, "", "")}),
		"exit with no record emits nothing")
}

// TestOSC7501CockpitWiresSupport pins the cockpit wiring: the support reply
// reaches the emitter through Update.
func TestOSC7501CockpitWiresSupport(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := NewCockpit(Options{})
	ck.False(c.osc7501.supported, "off before any reply")
	c.Update(osc7501ReplyFrame())
	ck.True(c.osc7501.supported, "the reply reaches the emitter through Update")
}

// TestOSC7501DoneHeldForSettleDelay pins F1: an idle after work does NOT emit
// done immediately -- it is held for the settle delay, and only goes out once
// the delay elapses with the child still idle. A coordinator whose pending fact
// just flushed would otherwise flash done before its status flips to working.
func TestOSC7501DoneHeldForSettleDelay(t *testing.T) {
	ck := assert.NewCollecting(t)
	now := time.Unix(1_000, 0)
	e := settlingEmitter(func() time.Time { return now })

	ck.NotEmpty(e.Sync([]rail.TopLevelDisplay{dsp(protocol.DisplayWorking, "", "")}),
		"work emits immediately")
	ck.Eq("", e.Sync([]rail.TopLevelDisplay{dsp(protocol.DisplayIdle, "", "")}),
		"idle after work does NOT emit done immediately")

	now = now.Add(osc7501DoneDelay - time.Millisecond)
	ck.Eq("", e.Sync([]rail.TopLevelDisplay{dsp(protocol.DisplayIdle, "", "")}),
		"still inside the settle delay emits nothing")

	now = now.Add(2 * time.Millisecond)
	ck.Eq(osc7501(osc7501Done, testChildID),
		e.Sync([]rail.TopLevelDisplay{dsp(protocol.DisplayIdle, "", "")}),
		"the held done is emitted once the settle delay elapses")
}

// TestOSC7501NewStateCancelsHeldDone pins that a non-idle state arriving inside
// the settle delay cancels the held done and is processed normally, so no done
// is ever emitted for the transient idle -- the F1 wake-up case.
func TestOSC7501NewStateCancelsHeldDone(t *testing.T) {
	ck := assert.NewCollecting(t)

	cases := []struct {
		name string
		next rail.TopLevelDisplay
		want string
	}{
		{
			name: "working is edge-equal to the last emitted working",
			next: dsp(protocol.DisplayWorking, "", ""),
			want: "",
		},
		{
			name: "blocked replaces the held done",
			next: dsp(protocol.DisplayWaiting, protocol.WaitingOnOperator, "q"),
			want: osc7501(osc7501Blocked, testChildID,
				kv{"kind", "question"},
				kv{"msg", osc7501Value("q", osc7501MaxMsg)},
				kv{"title", osc7501Value("alice", osc7501MaxTitle)}),
		},
		{
			name: "error replaces the held done",
			next: dsp(protocol.DisplayFailed, "", "boom"),
			want: osc7501(osc7501Error, testChildID,
				kv{"msg", osc7501Value("boom", osc7501MaxMsg)},
				kv{"title", osc7501Value("alice", osc7501MaxTitle)}),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Unix(3_000, 0)
			e := settlingEmitter(func() time.Time { return now })

			ck.NotEmpty(e.Sync([]rail.TopLevelDisplay{dsp(protocol.DisplayWorking, "", "")}), "work")
			ck.Eq("", e.Sync([]rail.TopLevelDisplay{dsp(protocol.DisplayIdle, "", "")}), "idle holds a done")
			ck.Eq(tc.want, e.Sync([]rail.TopLevelDisplay{tc.next}),
				"a non-idle state inside the delay cancels the held done and is processed")

			// Even long after the delay, no done is emitted for the first idle.
			now = now.Add(10 * time.Second)
			ck.Eq("", e.Sync([]rail.TopLevelDisplay{tc.next}),
				"no done ever follows the cancelled hold")
		})
	}
}

// cmdRaws runs a tea.Cmd and concatenates every RawMsg payload it produces,
// flattening one level of tea.BatchMsg. Tests only pass commands that are pure
// Raw writes: a settle tick is never invoked this way (it would sleep), so a
// command still holding only a tick is asserted on by inspecting the emitter.
func cmdRaws(cmd tea.Cmd) string {
	if cmd == nil {
		return ""
	}
	var out strings.Builder
	switch m := cmd().(type) {
	case tea.RawMsg:
		if s, ok := m.Msg.(string); ok {
			out.WriteString(s)
		}
	case tea.BatchMsg:
		for _, sub := range m {
			if sub == nil {
				continue
			}
			if rm, ok := sub().(tea.RawMsg); ok {
				if s, ok := rm.Msg.(string); ok {
					out.WriteString(s)
				}
			}
		}
	}
	return out.String()
}

// TestOSC7501RemovedChildPrunesHeldDone pins F1-R1: a child removed from the
// display set while its done is held must not leave a stale pending entry,
// whose expired deadline pendingDelay clamps to 0 and which would re-arm a
// zero-delay settle tick on every sync -- an unbounded Update loop.
func TestOSC7501RemovedChildPrunesHeldDone(t *testing.T) {
	ck := assert.NewCollecting(t)
	now := time.Unix(6_000, 0)
	e := settlingEmitter(func() time.Time { return now })

	ck.NotEmpty(e.Sync([]rail.TopLevelDisplay{dsp(protocol.DisplayWorking, "", "")}), "work")
	ck.Eq("", e.Sync([]rail.TopLevelDisplay{dsp(protocol.DisplayIdle, "", "")}), "idle holds a done")
	d, ok := e.pendingDelay()
	ck.True(ok && d > 0, "the held done has a positive remaining delay")

	// The child is removed from the rail: it is gone from the display set.
	ck.Eq("", e.Sync(nil), "a vanished child emits nothing")

	_, held := e.pendingDelay()
	ck.False(held, "the held done is pruned, so no settle tick is armed")
	ck.Eq(0, len(e.pending), "pending is empty after the prune")
	ck.Eq(0, len(e.lastEmitted), "lastEmitted is pruned too")
	ck.Eq(0, len(e.worked), "worked is pruned too")
}

// TestOSC7501CockpitSettleTickEmitsHeldDone exercises the real cockpit path:
// syncOSC arming the settle tick while a done is held, the osc7501SettleMsg
// handler emitting the held done once the delay elapses, and a new state
// arriving inside the delay cancelling it. The emitter's clock is injected, so
// nothing sleeps the real 1.5s.
func TestOSC7501CockpitSettleTickEmitsHeldDone(t *testing.T) {
	ck := assert.NewCollecting(t)

	now := time.Unix(7_000, 0)
	c := NewCockpit(Options{BaseURL: "http://127.0.0.1:1", ChildID: testChildID})
	c.osc7501.now = func() time.Time { return now }
	c.osc7501.supported = true
	seed := func(state, waiting, msg string) {
		c.rail.Seed([]*rafikiv1.ChildSummary{{
			ChildId:      testChildID,
			Name:         "alice",
			Status:       "idle",
			DisplayState: state,
			WaitingOn:    waiting,
			DisplayMsg:   msg,
			Labels:       map[string]string{},
		}})
	}

	// Work emits immediately and holds nothing.
	seed(string(protocol.DisplayWorking), "", "")
	ck.Eq(osc7501(osc7501Working, testChildID), cmdRaws(c.syncOSC()), "working emits immediately")
	_, held := c.osc7501.pendingDelay()
	ck.False(held, "nothing is held after working")

	// Idle after work holds the done and arms the settle tick.
	seed(string(protocol.DisplayIdle), "", "")
	ck.NotNil(c.syncOSC(), "a settle tick is armed for the held done")
	d, held := c.osc7501.pendingDelay()
	ck.True(held && d > 0, "the done is held with a positive remaining delay")
	ck.Eq(osc7501(osc7501Working, testChildID), c.osc7501.lastEmitted[testChildID],
		"the held done has not replaced working yet")

	// A settle tick inside the delay emits nothing and keeps holding.
	_, tickCmd := c.Update(osc7501SettleMsg{})
	ck.NotNil(tickCmd, "the tick re-arms while the done is still held")
	_, held = c.osc7501.pendingDelay()
	ck.True(held, "still held inside the delay")
	ck.Eq(osc7501(osc7501Working, testChildID), c.osc7501.lastEmitted[testChildID],
		"no done was emitted inside the delay")

	// Past the delay the settle handler emits the held done.
	now = now.Add(osc7501DoneDelay + time.Second)
	_, emitCmd := c.Update(osc7501SettleMsg{})
	ck.Eq(osc7501(osc7501Done, testChildID), cmdRaws(emitCmd),
		"the settle tick after the delay emits the held done")
	_, held = c.osc7501.pendingDelay()
	ck.False(held, "the hold is cleared once emitted")

	// Cancellation: work, hold a done, then blocked arrives inside the delay.
	seed(string(protocol.DisplayWorking), "", "")
	c.syncOSC()
	seed(string(protocol.DisplayIdle), "", "")
	c.syncOSC()
	seed(string(protocol.DisplayWaiting), string(protocol.WaitingOnOperator), "q")
	ck.Eq(osc7501(osc7501Blocked, testChildID,
		kv{"kind", "question"},
		kv{"msg", osc7501Value("q", osc7501MaxMsg)},
		kv{"title", osc7501Value("alice", osc7501MaxTitle)}),
		cmdRaws(c.syncOSC()),
		"a blocked inside the delay cancels the held done and is emitted")
	_, held = c.osc7501.pendingDelay()
	ck.False(held, "the held done was cancelled")

	// Long after the delay, no done is emitted for the first idle.
	now = now.Add(10 * time.Second)
	_, lateCmd := c.Update(osc7501SettleMsg{})
	ck.Eq("", cmdRaws(lateCmd), "no done ever follows the cancelled hold")
}
