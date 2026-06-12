package child

import (
	"bufio"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestClaudeProvider_Bootstrap_IsNil(t *testing.T) {
	if got := (ClaudeProvider{}).BootstrapFrame(); got != nil {
		t.Fatalf("claude needs no bootstrap, got %q", got)
	}
}

func TestClaudeProvider_Parse_SystemInitIsFirstResponse(t *testing.T) {
	line := []byte(`{"type":"system","subtype":"init","session_id":"sess-123","model":"claude-opus-4-8","cwd":"/tmp"}`)
	res := ClaudeProvider{}.Parse(line)
	if !res.FirstResponse {
		t.Fatal("system/init should be FirstResponse")
	}
	if !res.HasMeta || res.Meta.SessionID != "sess-123" || res.Meta.Model != "claude-opus-4-8" {
		t.Fatalf("meta = %+v hasMeta=%v", res.Meta, res.HasMeta)
	}
	if len(res.Events) != 0 {
		t.Fatalf("system/init should emit no SM events, got %+v", res.Events)
	}
}

func TestClaudeProvider_Parse_SessionStartHookIsFirstResponse(t *testing.T) {
	// The real `claude -p --input-format stream-json` does NOT emit system/init
	// un-prompted; on startup it emits only the SessionStart hook lifecycle and
	// then waits for input. Those frames must signal readiness, or a freshly-
	// spawned, un-prompted child never reaches idle and subagent_send (idle-gated)
	// is rejected forever.
	for _, line := range []string{
		`{"type":"system","subtype":"hook_started","hook_name":"SessionStart:startup","session_id":"sess-h"}`,
		`{"type":"system","subtype":"hook_response","session_id":"sess-h"}`,
	} {
		res := ClaudeProvider{}.Parse([]byte(line))
		if !res.FirstResponse {
			t.Fatalf("system frame must signal FirstResponse (readiness): %s", line)
		}
		if len(res.Events) != 0 {
			t.Fatalf("system frame should emit no SM events, got %+v", res.Events)
		}
	}
	// The session id is captured from the hook frame so the spawn result / resume
	// token is populated before the first turn's init arrives.
	res := ClaudeProvider{}.Parse([]byte(`{"type":"system","subtype":"hook_started","session_id":"sess-h"}`))
	if !res.HasMeta || res.Meta.SessionID != "sess-h" {
		t.Fatalf("hook frame should capture session id, meta=%+v hasMeta=%v", res.Meta, res.HasMeta)
	}
}

func TestClaudeProvider_Parse_AssistantTextIsAgentStart(t *testing.T) {
	line := []byte(`{"type":"assistant","session_id":"sess-123","message":{"content":[{"type":"text","text":"hi"}]}}`)
	res := ClaudeProvider{}.Parse(line)
	if res.FirstResponse {
		t.Fatal("assistant must not be FirstResponse")
	}
	if len(res.Events) != 1 || res.Events[0].Type != "agent_start" {
		t.Fatalf("events = %+v", res.Events)
	}
}

func TestClaudeProvider_Parse_AssistantToolUseStartsTool(t *testing.T) {
	line := []byte(`{"type":"assistant","session_id":"s","message":{"content":[{"type":"text","text":"running"},{"type":"tool_use","id":"t1","name":"bash"}]}}`)
	res := ClaudeProvider{}.Parse(line)
	// First agent_start (assistant content present), then one tool_execution_start
	// per tool_use block.
	if len(res.Events) != 2 || res.Events[0].Type != "agent_start" || res.Events[1].Type != "tool_execution_start" {
		t.Fatalf("events = %+v", res.Events)
	}
}

func TestClaudeProvider_Parse_ToolResultEndsTool(t *testing.T) {
	line := []byte(`{"type":"user","session_id":"s","message":{"content":[{"type":"tool_result","tool_use_id":"t1","content":"ok"}]}}`)
	res := ClaudeProvider{}.Parse(line)
	if len(res.Events) != 1 || res.Events[0].Type != "tool_execution_end" {
		t.Fatalf("events = %+v", res.Events)
	}
}

func TestClaudeProvider_Parse_ResultIsAgentEnd(t *testing.T) {
	line := []byte(`{"type":"result","subtype":"success","session_id":"sess-123","total_cost_usd":0.01}`)
	res := ClaudeProvider{}.Parse(line)
	if len(res.Events) != 1 || res.Events[0].Type != "agent_end" {
		t.Fatalf("events = %+v", res.Events)
	}
	if !res.HasMeta || res.Meta.SessionID != "sess-123" {
		t.Fatalf("result should carry session id, meta=%+v", res.Meta)
	}
}

func TestClaudeProvider_Parse_Garbage(t *testing.T) {
	res := ClaudeProvider{}.Parse([]byte(`not json`))
	if res.FirstResponse || res.HasMeta || len(res.Events) != 0 {
		t.Fatalf("garbage should be a no-op, got %+v", res)
	}
}

// TestClaudeProvider_GoldenTranscripts replays the captured transcripts through
// Parse and asserts the invariants the daemon relies on for readiness:
//   - readiness (FirstResponse) is signaled, and ONLY by `system` frames;
//   - it fires on the FIRST system frame (the SessionStart hook lifecycle), not
//     deferred to a later system/init — an un-prompted child must reach idle from
//     startup alone (downstream idleOnce/OnFirstResponse enforce once-ness, so
//     Parse re-reporting FirstResponse on later system frames is harmless);
//   - the terminal result yields an agent_end.
func TestClaudeProvider_GoldenTranscripts(t *testing.T) {
	for _, name := range []string{"startup_and_turn.jsonl", "turn_with_tool.jsonl"} {
		t.Run(name, func(t *testing.T) {
			f, err := os.Open("testdata/claude/" + name)
			if err != nil {
				t.Fatalf("open fixture: %v", err)
			}
			defer func() { _ = f.Close() }()

			firstResponses, agentEnds := 0, 0
			firstFRIdx, firstSystemIdx := -1, -1
			idx := 0
			sc := bufio.NewScanner(f)
			sc.Buffer(make([]byte, 0, 1<<20), 16<<20)
			for sc.Scan() {
				ln := strings.TrimSpace(sc.Text())
				if ln == "" {
					continue
				}
				var probe struct {
					Type string `json:"type"`
				}
				_ = json.Unmarshal([]byte(ln), &probe)
				if probe.Type == "system" && firstSystemIdx == -1 {
					firstSystemIdx = idx
				}
				res := ClaudeProvider{}.Parse([]byte(ln))
				if res.FirstResponse {
					firstResponses++
					if firstFRIdx == -1 {
						firstFRIdx = idx
					}
					if probe.Type != "system" {
						t.Fatalf("FirstResponse fired on a non-system frame (type=%q) at line %d", probe.Type, idx)
					}
				}
				for _, e := range res.Events {
					if e.Type == "agent_end" {
						agentEnds++
					}
				}
				idx++
			}
			if err := sc.Err(); err != nil {
				t.Fatalf("scan: %v", err)
			}
			if firstResponses < 1 {
				t.Fatalf("want >=1 FirstResponse (readiness signaled), got %d", firstResponses)
			}
			if firstFRIdx != firstSystemIdx {
				t.Fatalf("readiness must fire on the first system frame (idx %d), but first fired at idx %d", firstSystemIdx, firstFRIdx)
			}
			if agentEnds < 1 {
				t.Fatalf("want >=1 agent_end (terminal result), got %d", agentEnds)
			}
		})
	}
}

func TestClaudeProvider_EncodePrompt(t *testing.T) {
	got := ClaudeProvider{}.EncodeOutbound([]byte(`{"type":"prompt","message":"hello there"}`))
	if got == nil {
		t.Fatal("prompt must encode to a non-nil frame")
	}
	var env struct {
		Type    string `json:"type"`
		Message struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal(got, &env); err != nil {
		t.Fatalf("encoded frame is not valid JSON: %v (%s)", err, got)
	}
	if env.Type != "user" || env.Message.Role != "user" || env.Message.Content != "hello there" {
		t.Fatalf("bad envelope: %s", got)
	}
}

func TestClaudeProvider_EncodeSteerSameAsPrompt(t *testing.T) {
	got := ClaudeProvider{}.EncodeOutbound([]byte(`{"type":"steer","message":"keep going"}`))
	if got == nil || !json.Valid(got) {
		t.Fatalf("steer should encode like a prompt, got %q", got)
	}
}

func TestClaudeProvider_EncodeUnknownDropped(t *testing.T) {
	if got := (ClaudeProvider{}).EncodeOutbound([]byte(`{"type":"set_session_name","name":"x"}`)); got != nil {
		t.Fatalf("unsupported frame should be dropped (nil), got %q", got)
	}
}

func TestClaudeProvider_EncodeGarbageDropped(t *testing.T) {
	if got := (ClaudeProvider{}).EncodeOutbound([]byte(`not json`)); got != nil {
		t.Fatalf("garbage should be dropped (nil), got %q", got)
	}
}
