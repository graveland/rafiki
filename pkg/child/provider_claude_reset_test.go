package child

import (
	"encoding/json"
	"testing"
)

// TestClaudeProviderResetStateClearsAccumulators proves ResetState actually
// clears every field claudeState carries, not just the messages a stale-state
// bug would leave behind. This is the piece that makes resetProviderIfDue's
// wiring correct: without it, a daraja Restart's boundary marker would fire
// but land on a provider that forgets nothing.
func TestClaudeProviderResetStateClearsAccumulators(t *testing.T) {
	p := newClaudeProvider()

	init := []byte(`{"type":"system","subtype":"init","session_id":"sess-1","model":"claude-opus-4-8"}`)
	p.BusFrames(init, 1)
	assistant := []byte(`{"type":"assistant","session_id":"sess-1","message":{"content":[{"type":"text","text":"hi"}]}}`)
	p.BusFrames(assistant, 2)

	if p.st.model == "" {
		t.Fatal("setup: model should be captured before reset")
	}
	if !p.st.turnActive {
		t.Fatal("setup: turnActive should be true mid-turn before reset")
	}
	if len(p.snapshotMessages()) == 0 {
		t.Fatal("setup: messages should be non-empty before reset")
	}

	p.ResetState()

	if p.st.model != "" {
		t.Errorf("model = %q after ResetState, want empty", p.st.model)
	}
	if p.st.provider != "" {
		t.Errorf("provider = %q after ResetState, want empty", p.st.provider)
	}
	if p.st.api != "" {
		t.Errorf("api = %q after ResetState, want empty", p.st.api)
	}
	if p.st.turnActive {
		t.Error("turnActive still true after ResetState")
	}
	if msgs := p.snapshotMessages(); len(msgs) != 0 {
		t.Errorf("messages = %v after ResetState, want empty", msgs)
	}
}

// TestClaudeProviderResetStateThenFreshInit proves a reset provider behaves
// exactly like a brand-new one for the replacement process's own system/init
// — the actual observable consequence of a stale turnActive: without a
// reset, openTurn's guard sees turnActive already true and never opens a new
// turn for the replacement process's first assistant frame.
func TestClaudeProviderResetStateThenFreshInit(t *testing.T) {
	p := newClaudeProvider()
	p.BusFrames([]byte(`{"type":"system","subtype":"init","session_id":"sess-1","model":"claude-opus-4-8"}`), 1)
	p.BusFrames([]byte(`{"type":"assistant","session_id":"sess-1","message":{"content":[{"type":"text","text":"hi"}]}}`), 2)

	p.ResetState()

	// The replacement process's own system/init, then its first assistant
	// frame: this must open a turn (agent_start) exactly as it would for a
	// genuinely fresh provider, proving turnActive did not survive the reset.
	p.BusFrames([]byte(`{"type":"system","subtype":"init","session_id":"sess-2","model":"claude-sonnet-5"}`), 3)
	frames := p.BusFrames([]byte(`{"type":"assistant","session_id":"sess-2","message":{"content":[{"type":"text","text":"hi again"}]}}`), 4)

	if len(frames) == 0 {
		t.Fatal("no bus frames from the replacement process's first assistant message")
	}
	var sawAgentStart bool
	for _, f := range frames {
		var hdr struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(f, &hdr); err != nil {
			t.Fatalf("frame not valid JSON: %v", err)
		}
		if hdr.Type == "agent_start" {
			sawAgentStart = true
		}
	}
	if !sawAgentStart {
		t.Fatal("no agent_start after reset — turnActive likely survived and the guard suppressed it")
	}
	if p.st.model != "claude-sonnet-5" {
		t.Errorf("model = %q, want the replacement process's own model", p.st.model)
	}
}
