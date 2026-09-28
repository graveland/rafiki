// SPDX-License-Identifier: Apache-2.0

package child

import (
	"bytes"
	"testing"

	"go.graveland.dev/rafiki/pkg/protocol"

	"github.com/multigres/testkit/assert"
)

// TestScriptParseEmitsOutputNotAgentStart pins the provider's parse result:
// every stdout line is liveness (FirstResponse) and exactly one script_output
// event carrying the raw line — never a synthesized agent_start, which would
// drive idle→streaming and make output move a script child's status.
func TestScriptParseEmitsOutputNotAgentStart(t *testing.T) {
	c := assert.NewAborting(t)
	p := ScriptProvider{}
	for _, line := range []string{"working", `{"type":"agent_start"}`, "", "some stderr-shaped text"} {
		res := p.Parse([]byte(line))
		c.True(res.FirstResponse, "line %q: FirstResponse must stay set — it is the liveness signal that closes Idle()", line)
		c.False(len(res.Events) != 1 || res.Events[0].Type != "script_output", "line %q: events = %+v, want exactly one script_output (no agent_start)", line, res.Events)
		c.Eq(line, res.Events[0].Text, "line %q: the script_output event must carry the raw line as its text", line)
	}
}

// TestScriptStatusRunningUntilExit pins the script child's status machine: a
// machine configured with ForScript sits at running from creation (set once
// at spawn), output events never transition it, the first-response signal is
// liveness only, and the process exit is the one transition (running→exited).
// An output event after the exit must not revive a working status.
func TestScriptStatusRunningUntilExit(t *testing.T) {
	c := assert.NewAborting(t)
	sm := NewStateMachine()
	sm.ForScript()
	c.Eq(protocol.StatusRunning, sm.Current(), "a script child's status is running the moment the machine is created, before any output")

	// Many output events: none transitions anything.
	for i := range 50 {
		changed, prev := sm.OnPiEvent("script_output", nil)
		c.False(changed, "output %d must not transition (changed=%v prev=%s)", i, changed, prev)
	}
	c.Eq(protocol.StatusRunning, sm.Current(), "status after 50 output events")

	// The first-response signal is liveness only; output after it changes nothing.
	changed, _ := sm.OnFirstResponse()
	c.False(changed, "OnFirstResponse must not move a script child out of running")
	changed, _ = sm.OnPiEvent("script_output", nil)
	c.False(changed, "output after the first response must not transition")
	c.Eq(protocol.StatusRunning, sm.Current(), "status after first response and more output")

	// Exit is the only transition.
	changed, prev := sm.OnProcessExit()
	c.False(!changed || prev != protocol.StatusRunning, "exit: changed=%v prev=%s, want true/running", changed, prev)
	c.Eq(protocol.StatusExited, sm.Current(), "status after exit")

	// Output after exit does not revive a working status.
	changed, _ = sm.OnPiEvent("script_output", nil)
	c.False(changed, "output after exit must not revive the status")
	c.Eq(protocol.StatusExited, sm.Current(), "status after output following exit")
}

// TestScriptProviderThroughStateMachine drives the real state machine with
// the provider's parse results and asserts the transition sequence a script
// child produces: the machine is running from the spawn (Child.Spawn calls
// ForScript and records spawning→running for activateLiveChild to drain), and
// then NOTHING — every stdout line is liveness plus a script_output event, and
// neither moves the status — until the process exit, the child's one settle,
// replaces running with exited.
func TestScriptProviderThroughStateMachine(t *testing.T) {
	c := assert.NewAborting(t)
	sm := NewStateMachine()
	sm.ForScript()
	c.Eq(protocol.StatusRunning, sm.Current(), "ForScript moves the machine to running at spawn")

	res := ScriptProvider{}.Parse([]byte("working"))
	c.True(res.FirstResponse, "FirstResponse must be set for every stdout line of a script child")
	changed, _ := sm.OnFirstResponse()
	c.False(changed, "the first response must not transition: the status was set at spawn")
	changed, _ = sm.OnPiEvent(res.Events[0].Type, nil)
	c.False(changed, "script_output must not transition")

	// Repeated lines are no-ops: every Parse call reports the same result, and
	// the machine must treat repeats as such.
	for _, line := range []string{"still working", `{"type":"agent_start"}`, ""} {
		res := ScriptProvider{}.Parse([]byte(line))
		c.True(res.FirstResponse, "line %q: FirstResponse unset", line)
		changed, _ = sm.OnFirstResponse()
		c.False(changed, "line %q: OnFirstResponse must be inert for a script child", line)
		changed, _ = sm.OnPiEvent(res.Events[0].Type, nil)
		c.False(changed, "line %q: a script_output event must not record a transition", line)
	}
	c.Eq(protocol.StatusRunning, sm.Current(), "status after repeated output lines")

	changed, prev := sm.OnProcessExit()
	c.False(!changed || prev != protocol.StatusRunning, "exit: changed=%v prev=%s, want true/running", changed, prev)
	c.Eq(protocol.StatusExited, sm.Current(), "the exit is a script child's settle: status is exited")
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
