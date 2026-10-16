// SPDX-License-Identifier: Apache-2.0

package tui

import (
	"strings"
	"testing"
	"unicode/utf8"

	uv "github.com/charmbracelet/ultraviolet"
	"github.com/multigres/testkit/assert"

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
// supports OSC 7501 from a reply.
func supportedEmitter() *osc7501Emitter {
	e := newOSC7501Emitter(env(nil))
	e.observe(osc7501ReplyFrame())
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
