package child

import (
	"encoding/json"
	"testing"
)

// decodeFrames unmarshals raw bus frames to maps for inspection.
func decodeFrames(t *testing.T, raws [][]byte) []map[string]any {
	t.Helper()
	out := make([]map[string]any, 0, len(raws))
	for _, raw := range raws {
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatalf("frame not valid JSON: %v (%s)", err, raw)
		}
		out = append(out, m)
	}
	return out
}

// A claude child never echoes the user's own prompt on stdout, so the daemon
// must synthesize the user message_start/message_end when it forwards a prompt —
// otherwise the TUI (which renders a user bubble only from message_start
// role:user) shows nothing. This pins that contract.
func TestClaudeOutboundEchoEmitsUserMessage(t *testing.T) {
	prov := (ClaudeProvider{}).Fresh()

	frames := decodeFrames(t, prov.OutboundEcho([]byte(`{"type":"prompt","message":"read /etc/hostname"}`), 990))
	if len(frames) != 2 {
		t.Fatalf("want 2 echo frames (message_start, message_end), got %d: %v", len(frames), frames)
	}

	if frames[0]["type"] != "message_start" || frames[1]["type"] != "message_end" {
		t.Fatalf("want [message_start, message_end], got [%v, %v]", frames[0]["type"], frames[1]["type"])
	}
	for _, f := range frames {
		msg, ok := f["message"].(map[string]any)
		if !ok {
			t.Fatalf("frame missing message object: %v", f)
		}
		if msg["role"] != "user" {
			t.Fatalf("echo message role = %v, want user", msg["role"])
		}
		if msg["content"] != "read /etc/hostname" {
			t.Fatalf("echo message content = %v, want the prompt text", msg["content"])
		}
		if msg["id"] == nil || msg["id"] == "" {
			t.Fatalf("echo message must carry an id so the consumer's id-dedup keeps distinct user turns; got %v", msg["id"])
		}
	}

	// The recorded user message must survive into the turn's agent_end so a
	// post-turn cache rebuild (which replaces _messages with agent_end.messages)
	// does not drop the user turn.
	end := decodeFrames(t, prov.BusFrames([]byte(`{"type":"result"}`), 1100))
	if len(end) != 1 || end[0]["type"] != "agent_end" {
		t.Fatalf("want a single agent_end, got %v", end)
	}
	msgs, ok := end[0]["messages"].([]any)
	if !ok || len(msgs) == 0 {
		t.Fatalf("agent_end.messages missing or empty: %v", end[0]["messages"])
	}
	first := msgs[0].(map[string]any)
	if first["role"] != "user" || first["content"] != "read /etc/hostname" {
		t.Fatalf("agent_end.messages[0] should be the echoed user turn, got %v", first)
	}
}

func TestClaudeOutboundEchoIgnoresNonPromptFrames(t *testing.T) {
	prov := (ClaudeProvider{}).Fresh()
	for _, frame := range []string{
		`{"type":"abort"}`,
		`{"type":"set_session_name","name":"x"}`,
		`{"type":"prompt","message":""}`,
		`not json`,
	} {
		if got := prov.OutboundEcho([]byte(frame), 1); got != nil {
			t.Fatalf("OutboundEcho(%s) = %v, want nil", frame, got)
		}
	}
}

func TestPiOutboundEchoIsNil(t *testing.T) {
	// pi echoes the user message_start on its own stdout, so synthesizing one
	// would double-render.
	if got := (PiProvider{}).OutboundEcho([]byte(`{"type":"prompt","message":"hi"}`), 1); got != nil {
		t.Fatalf("PiProvider.OutboundEcho = %v, want nil", got)
	}
}
