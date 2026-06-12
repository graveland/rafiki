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

func TestClaudeProvider_ReadyOnSpawn(t *testing.T) {
	// claude is silent on stdout until prompted, so readiness is process-up.
	if !(ClaudeProvider{}).ReadyOnSpawn() {
		t.Fatal("claude must be ReadyOnSpawn (no stdout readiness signal exists)")
	}
	// pi announces readiness via response.get_state, so it is NOT ready on spawn.
	if (PiProvider{}).ReadyOnSpawn() {
		t.Fatal("pi must NOT be ReadyOnSpawn (it waits for response.get_state)")
	}
}

func TestClaudeProvider_Parse_NonInitSystemIsNoop(t *testing.T) {
	// The SessionStart hook lifecycle (and any non-init system frame) must NOT be
	// treated as readiness or emit SM events — readiness is process-up, and these
	// frames only ever arrive once a turn is already running.
	for _, line := range []string{
		`{"type":"system","subtype":"hook_started","hook_name":"SessionStart:startup","session_id":"sess-h"}`,
		`{"type":"system","subtype":"hook_response","session_id":"sess-h"}`,
	} {
		res := ClaudeProvider{}.Parse([]byte(line))
		if res.FirstResponse {
			t.Fatalf("non-init system frame must not signal FirstResponse: %s", line)
		}
		if len(res.Events) != 0 {
			t.Fatalf("non-init system frame should emit no SM events, got %+v", res.Events)
		}
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
// Parse and asserts the high-level invariants that the daemon relies on: exactly
// one FirstResponse (the single system/init line — note initial readiness itself
// is process-up via ReadyOnSpawn, this just guards the init-meta path), and an
// agent_end for the terminal result. The fixtures were captured with a piped
// prompt, so they DO contain init; a real un-prompted spawn emits nothing.
func TestClaudeProvider_GoldenTranscripts(t *testing.T) {
	for _, name := range []string{"startup_and_turn.jsonl", "turn_with_tool.jsonl"} {
		t.Run(name, func(t *testing.T) {
			f, err := os.Open("testdata/claude/" + name)
			if err != nil {
				t.Fatalf("open fixture: %v", err)
			}
			defer func() { _ = f.Close() }()

			firstResponses, agentEnds := 0, 0
			sc := bufio.NewScanner(f)
			sc.Buffer(make([]byte, 0, 1<<20), 16<<20)
			for sc.Scan() {
				ln := strings.TrimSpace(sc.Text())
				if ln == "" {
					continue
				}
				res := ClaudeProvider{}.Parse([]byte(ln))
				if res.FirstResponse {
					firstResponses++
				}
				for _, e := range res.Events {
					if e.Type == "agent_end" {
						agentEnds++
					}
				}
			}
			if err := sc.Err(); err != nil {
				t.Fatalf("scan: %v", err)
			}
			if firstResponses != 1 {
				t.Fatalf("want exactly 1 FirstResponse (one init line), got %d", firstResponses)
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
