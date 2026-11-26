// SPDX-License-Identifier: Apache-2.0

package session_test

import (
	"testing"
	"time"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/tui/session"

	"github.com/multigres/testkit/assert"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func at(ev *rafikiv1.Event, t time.Time) *rafikiv1.Event {
	ev.Ts = timestamppb.New(t)
	return ev
}

// A block is stamped with when the event HAPPENED. A replayed history or a
// reattach delivers old events now, so stamping with the local clock would
// show every message as having just arrived.
func TestBlocksCarryTheEventsOwnTime(t *testing.T) {
	ck := assert.NewCollecting(t)
	s := session.New("c1")
	then := time.Date(2026, 11, 20, 9, 30, 0, 0, time.UTC)

	s.ApplyHistory(at(textEvent("c1", "old question"), then))
	s.Apply(at(assistantEvent("c1", "old answer"), then.Add(40*time.Second)))

	ck.Require().Len(s.Blocks, 2, "blocks")
	ck.True(s.Blocks[0].At.Equal(then), "user block At = %v, want %v", s.Blocks[0].At, then)
	ck.True(s.Blocks[1].At.Equal(then.Add(40*time.Second)), "assistant block At = %v", s.Blocks[1].At)
}

// An event with no ts, or the epoch an old row decodes to, falls back to the
// local clock rather than stamping 1970.
func TestBlockWithoutAnEventTimeFallsBackToNow(t *testing.T) {
	ck := assert.NewCollecting(t)
	s := session.New("c1")
	before := time.Now()

	s.Apply(textEvent("c1", "no ts"))
	s.Apply(at(textEvent("c1", "epoch"), time.Unix(0, 0)))

	ck.Require().Len(s.Blocks, 2, "blocks")
	for i, b := range s.Blocks {
		ck.False(b.At.Before(before), "block %d At = %v, want the local clock", i, b.At)
	}
}

// One event's time must not leak into the next event that carries none.
func TestEventTimeDoesNotLeakIntoALaterEvent(t *testing.T) {
	ck := assert.NewCollecting(t)
	s := session.New("c1")
	then := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

	s.Apply(at(textEvent("c1", "stamped"), then))
	s.Apply(textEvent("c1", "unstamped"))

	ck.Require().Len(s.Blocks, 2, "blocks")
	ck.False(s.Blocks[1].At.Equal(then), "an unstamped event inherited the previous event's time")
}

func TestTurnDurationIsTurnStartToTurnEnd(t *testing.T) {
	ck := assert.NewCollecting(t)
	s := session.New("c1")
	start := time.Date(2026, 11, 20, 9, 30, 0, 0, time.UTC)

	s.Apply(at(turnStartEvent("c1"), start))
	s.Apply(at(assistantEvent("c1", "answer"), start.Add(10*time.Second)))
	s.Apply(at(turnEndEvent("c1"), start.Add(12400*time.Millisecond)))

	ck.Require().Len(s.Blocks, 1, "blocks")
	ck.Eq(12400*time.Millisecond, s.Blocks[0].TurnDuration, "turn duration")
}

// A replayed history has messages but no turn events, so no duration can be
// known; drawing a wrong one would be worse than none.
func TestReplayedTurnHasNoDuration(t *testing.T) {
	ck := assert.NewCollecting(t)
	s := session.New("c1")
	then := time.Date(2026, 11, 20, 9, 30, 0, 0, time.UTC)

	s.ApplyHistory(at(textEvent("c1", "q"), then))
	s.ApplyHistory(at(assistantEvent("c1", "a"), then.Add(time.Minute)))

	for i, b := range s.Blocks {
		ck.Eq(time.Duration(0), b.TurnDuration, "block %d duration", i)
	}
}

// A turn end with no turn start (a stream joined mid-turn) or no ts on either
// end records nothing, and one turn's start never carries into the next.
func TestTurnDurationNeedsBothEndsAndDoesNotCarryOver(t *testing.T) {
	ck := assert.NewCollecting(t)
	s := session.New("c1")
	start := time.Date(2026, 11, 20, 9, 30, 0, 0, time.UTC)

	s.Apply(at(assistantEvent("c1", "first"), start))
	s.Apply(at(turnEndEvent("c1"), start.Add(5*time.Second)))
	ck.Eq(time.Duration(0), s.Blocks[0].TurnDuration, "turn end without a start")

	s.Apply(at(turnStartEvent("c1"), start.Add(time.Minute)))
	s.Apply(at(assistantEvent("c1", "second"), start.Add(61*time.Second)))
	s.Apply(turnEndEvent("c1"))
	ck.Require().Len(s.Blocks, 2, "blocks")
	ck.Eq(time.Duration(0), s.Blocks[1].TurnDuration, "turn end without a ts")

	s.Apply(at(assistantEvent("c1", "third"), start.Add(2*time.Minute)))
	s.Apply(at(turnEndEvent("c1"), start.Add(3*time.Minute)))
	ck.Require().Len(s.Blocks, 3, "blocks")
	ck.Eq(time.Duration(0), s.Blocks[2].TurnDuration, "a finished turn's start leaked into the next")
}

// The duration changes what is drawn without changing the text, so it must
// move the fingerprint the renderer's live-tail cache keys on.
func TestFingerprintChangesWithTurnDuration(t *testing.T) {
	a := session.Block{Kind: session.KindAssistant, Text: "x", Final: true}
	b := a
	b.TurnDuration = time.Second
	assert.NewCollecting(t).NotEq(a.Fingerprint(), b.Fingerprint(), "fingerprint")
}
