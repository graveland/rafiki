// SPDX-License-Identifier: Apache-2.0

package child

import (
	"bytes"
	"testing"

	"go.graveland.dev/rafiki/pkg/protocol"
)

// TestScriptProviderThroughStateMachine drives the real state machine with
// the provider's parse results and asserts the transition sequence a script
// child produces: spawning → idle (first output; the moment
// activateLiveChild's idle wait unblocks) → streaming (so the rail renders a
// running script as WORKING and the busy checks treat it as mid-flight).
// Repeated lines are no-ops — every Parse call reports the same result, and
// the machine must treat repeats as such.
func TestScriptProviderThroughStateMachine(t *testing.T) {
	sm := NewStateMachine()

	res := ScriptProvider{}.Parse([]byte("working"))
	if !res.FirstResponse {
		t.Fatal("FirstResponse must be set for every stdout line of a script child")
	}
	if len(res.Events) != 1 || res.Events[0].Type != "agent_start" {
		t.Fatalf("events = %+v, want exactly one agent_start", res.Events)
	}

	changed, prev := sm.OnFirstResponse()
	if !changed || prev != protocol.StatusSpawning {
		t.Fatalf("first OnFirstResponse: changed=%v prev=%s, want true/spawning", changed, prev)
	}
	if sm.Current() != protocol.StatusIdle {
		t.Fatalf("status after first response = %s, want idle", sm.Current())
	}
	changed, prev = sm.OnPiEvent("agent_start", nil)
	if !changed || prev != protocol.StatusIdle {
		t.Fatalf("agent_start: changed=%v prev=%s, want true/idle", changed, prev)
	}
	if sm.Current() != protocol.StatusStreaming {
		t.Fatalf("status after agent_start = %s, want streaming", sm.Current())
	}

	// Repeated lines change nothing: still streaming, no new transition.
	for _, line := range []string{"still working", `{"type":"agent_start"}`} {
		res := ScriptProvider{}.Parse([]byte(line))
		if !res.FirstResponse {
			t.Fatalf("line %q: FirstResponse unset", line)
		}
		changed, _ = sm.OnFirstResponse()
		if changed {
			t.Fatalf("line %q: OnFirstResponse must not transition out of spawning twice", line)
		}
		changed, _ = sm.OnPiEvent("agent_start", nil)
		if changed {
			t.Fatalf("line %q: a repeated agent_start must not record a transition", line)
		}
	}
	if sm.Current() != protocol.StatusStreaming {
		t.Fatalf("status after repeated lines = %s, want streaming", sm.Current())
	}
}

// TestScriptProviderNeverTakesStdinInput pins the stdin posture: no
// bootstrap frame, nothing encoded outbound, no echo — a script child's
// messages arrive through its inbox and its Receive stream, never through
// stdin, so any frame the daemon would write must be dropped.
func TestScriptProviderNeverTakesStdinInput(t *testing.T) {
	p := ScriptProvider{}
	if p.BootstrapFrame() != nil {
		t.Fatal("BootstrapFrame must be nil: nothing is written to a script's stdin")
	}
	if got := p.EncodeOutbound([]byte(`{"type":"prompt","message":"hi"}`)); got != nil {
		t.Fatalf("EncodeOutbound must drop every frame, got %q", got)
	}
	if got := p.OutboundEcho([]byte("anything"), 0); got != nil {
		t.Fatalf("OutboundEcho must be nil, got %v", got)
	}
	if p.Normalizes() {
		t.Fatal("Normalizes must be false: the line stream is verbatim text")
	}
	if got := p.BusFrames([]byte("raw text"), 0); len(got) != 1 || !bytes.Equal(got[0], []byte("raw text")) {
		t.Fatalf("BusFrames must return the raw line verbatim, got %v", got)
	}
}
