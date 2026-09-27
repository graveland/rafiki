package child

import (
	"encoding/json"
	"testing"

	"github.com/multigres/testkit/assert"
)

// TestClaudeProviderResetStateClearsAccumulators proves ResetState actually
// clears every field claudeState carries, not just the messages a stale-state
// bug would leave behind. This is the piece that makes resetProviderIfDue's
// wiring correct: without it, a daraja Restart's boundary marker would fire
// but land on a provider that forgets nothing.
func TestClaudeProviderResetStateClearsAccumulators(t *testing.T) {
	c := assert.NewCollecting(t)
	p := newClaudeProvider()

	init := []byte(`{"type":"system","subtype":"init","session_id":"sess-1","model":"claude-opus-4-8"}`)
	p.BusFrames(init, 1)
	assistant := []byte(`{"type":"assistant","session_id":"sess-1","message":{"content":[{"type":"text","text":"hi"}]}}`)
	p.BusFrames(assistant, 2)

	c.Require().NotEq("", p.st.model, "setup: model should be captured before reset")
	c.Require().True(p.st.turnActive, "setup: turnActive should be true mid-turn before reset")
	c.Require().NotEmpty(p.snapshotMessages(), "setup: messages should be non-empty before reset")

	p.ResetState()

	c.Eq("", p.st.model, "model")
	c.Eq("", p.st.provider, "provider")
	c.Eq("", p.st.api, "api")
	c.False(p.st.turnActive, "turnActive still true after ResetState")
	c.Empty(p.snapshotMessages(), "messages")
}

// TestClaudeProviderResetStateThenFreshInit proves a reset provider behaves
// exactly like a brand-new one for the replacement process's own system/init
// — the actual observable consequence of a stale turnActive: without a
// reset, openTurn's guard sees turnActive already true and never opens a new
// turn for the replacement process's first assistant frame.
func TestClaudeProviderResetStateThenFreshInit(t *testing.T) {
	c := assert.NewCollecting(t)
	p := newClaudeProvider()
	p.BusFrames([]byte(`{"type":"system","subtype":"init","session_id":"sess-1","model":"claude-opus-4-8"}`), 1)
	p.BusFrames([]byte(`{"type":"assistant","session_id":"sess-1","message":{"content":[{"type":"text","text":"hi"}]}}`), 2)

	p.ResetState()

	// The replacement process's own system/init, then its first assistant
	// frame: this must open a turn (agent_start) exactly as it would for a
	// genuinely fresh provider, proving turnActive did not survive the reset.
	p.BusFrames([]byte(`{"type":"system","subtype":"init","session_id":"sess-2","model":"claude-sonnet-5"}`), 3)
	frames := p.BusFrames([]byte(`{"type":"assistant","session_id":"sess-2","message":{"content":[{"type":"text","text":"hi again"}]}}`), 4)

	c.Require().NotEmpty(frames, "no bus frames from the replacement process's first assistant message")
	var sawAgentStart bool
	for _, f := range frames {
		var hdr struct {
			Type string `json:"type"`
		}
		c.Require().NoError(json.Unmarshal(f, &hdr), "frame not valid JSON")
		if hdr.Type == "agent_start" {
			sawAgentStart = true
		}
	}
	c.Require().True(sawAgentStart, "no agent_start after reset — turnActive likely survived and the guard suppressed it")
	c.Eq("claude-sonnet-5", p.st.model, "model")
}
