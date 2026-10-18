package fundi_test

import (
	"bytes"
	"strings"
	"testing"

	"go.graveland.dev/rafiki/pkg/fundi"

	"github.com/multigres/testkit/assert"
)

// A fundi turn opens with the durable native TurnStart and closes with TurnEnd.
// The pair is the only turn boundary a non-TUI consumer gets: pkg/turntrace
// opens its invoke_agent span on TurnStart and ends it on TurnEnd, so a turn
// missing its start has no root span and its tool calls float unattached.
//
// AgentStart is the one point a fundi turn opens (Engine.runTurn and
// startupResume call it at the top of a turn and nowhere else), so exactly one
// TurnStart per turn is a property of that call site plus publishTurnStart.
func TestEmitterAgentStartPublishesTurnStart(t *testing.T) {
	c := assert.NewAborting(t)
	var out bytes.Buffer
	fe := fundi.NewFrontend(bytes.NewReader(nil), &out, nil)
	em := fundi.NewEmitter(fe, "anthropic", nil)
	sink := &capturingSink{}
	em.SetNativeSink(sink)

	em.AgentStart()
	// The pi frame still goes out first: an attached TUI renders the activity
	// indicator from it, and the native event must not displace it.
	c.True(strings.Contains(out.String(), "agent_start"), "the pi agent_start frame disappeared")

	em.AgentEnd()

	c.Len(sink.events, 2, "got %d native events, want turn_start then turn_end", len(sink.events))
	start := sink.events[0].GetTurnStart()
	c.Require().NotNil(start, "first native event is not a TurnStart")
	c.Nil(sink.events[0].GetTurnEnd(), "first native event is a TurnEnd, not a TurnStart")
	// Empty by design: a fundi turn opens before routing has chosen the model,
	// and a consumer falls back to the child's model metadata.
	c.Eq("", start.GetModel(), "turn_start model")
	c.Nil(sink.events[1].GetTurnStart(), "second native event is another TurnStart")
	c.Require().NotNil(sink.events[1].GetTurnEnd(), "second native event is not a TurnEnd")
}

// Each turn opens its own: a second AgentStart/AgentEnd pair publishes a second
// TurnStart, so a consumer keyed on the start never sees one turn swallow two.
func TestEmitterPublishesTurnStartPerTurn(t *testing.T) {
	c := assert.NewAborting(t)
	var out bytes.Buffer
	fe := fundi.NewFrontend(bytes.NewReader(nil), &out, nil)
	em := fundi.NewEmitter(fe, "anthropic", nil)
	sink := &capturingSink{}
	em.SetNativeSink(sink)

	for i := 0; i < 2; i++ {
		em.AgentStart()
		em.AgentEnd()
	}

	c.Len(sink.events, 4, "got %d native events, want two turn_start/turn_end pairs", len(sink.events))
	for i, ev := range sink.events {
		if i%2 == 0 {
			c.Require().NotNil(ev.GetTurnStart(), "event %d is not a TurnStart", i)
			continue
		}
		c.Require().NotNil(ev.GetTurnEnd(), "event %d is not a TurnEnd", i)
	}
}
