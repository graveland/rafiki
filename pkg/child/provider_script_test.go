// SPDX-License-Identifier: Apache-2.0

package child

import (
	"bytes"
	"testing"

	"go.graveland.dev/rafiki/pkg/protocol"

	"github.com/multigres/testkit/assert"
)

// TestScriptProviderThroughStateMachine drives the real state machine with
// the provider's parse results and asserts the transition sequence a script
// child produces: spawning → idle (first output; the moment
// activateLiveChild's idle wait unblocks) → streaming (so the rail renders a
// running script as WORKING and the busy checks treat it as mid-flight).
// Repeated lines are no-ops — every Parse call reports the same result, and
// the machine must treat repeats as such.
func TestScriptProviderThroughStateMachine(t *testing.T) {
	c := assert.NewAborting(t)
	sm := NewStateMachine()

	res := ScriptProvider{}.Parse([]byte("working"))
	c.True(res.FirstResponse, "FirstResponse must be set for every stdout line of a script child")
	c.False(len(res.Events) != 1 || res.Events[0].Type != "agent_start", "events = %+v, want exactly one agent_start", res.Events)

	changed, prev := sm.OnFirstResponse()
	c.False(!changed || prev != protocol.StatusSpawning, "first OnFirstResponse: changed=%v prev=%s, want true/spawning", changed, prev)
	c.Eq(protocol.StatusIdle, sm.Current(), "status after first response")
	changed, prev = sm.OnPiEvent("agent_start", nil)
	c.False(!changed || prev != protocol.StatusIdle, "agent_start: changed=%v prev=%s, want true/idle", changed, prev)
	c.Eq(protocol.StatusStreaming, sm.Current(), "status after agent_start")

	// Repeated lines change nothing: still streaming, no new transition.
	for _, line := range []string{"still working", `{"type":"agent_start"}`} {
		res := ScriptProvider{}.Parse([]byte(line))
		c.True(res.FirstResponse, "line %q: FirstResponse unset", line)
		changed, _ = sm.OnFirstResponse()
		c.False(changed, "line %q: OnFirstResponse must not transition out of spawning twice", line)
		changed, _ = sm.OnPiEvent("agent_start", nil)
		c.False(changed, "line %q: a repeated agent_start must not record a transition", line)
	}
	c.Eq(protocol.StatusStreaming, sm.Current(), "status after repeated lines")
}

// TestScriptProviderNeverTakesStdinInput pins the stdin posture: no
// bootstrap frame, nothing encoded outbound, no echo — a script child's
// messages arrive through its inbox and its Receive stream, never through
// stdin, so any frame the daemon would write must be dropped.
func TestScriptProviderNeverTakesStdinInput(t *testing.T) {
	c := assert.NewAborting(t)
	p := ScriptProvider{}
	c.Nil(p.BootstrapFrame(), "BootstrapFrame must be nil: nothing is written to a script's stdin")
	c.Nil(p.EncodeOutbound([]byte(`{"type":"prompt","message":"hi"}`)), "EncodeOutbound must drop every frame, got")
	c.Nil(p.OutboundEcho([]byte("anything"), 0), "OutboundEcho must be nil, got")
	c.False(p.Normalizes(), "Normalizes must be false: the line stream is verbatim text")
	if got := p.BusFrames([]byte("raw text"), 0); len(got) != 1 || !bytes.Equal(got[0], []byte("raw text")) {
		t.Fatalf("BusFrames must return the raw line verbatim, got %v", got)
	}
}
