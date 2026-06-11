package child

import (
	"bufio"
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
// Parse and asserts the high-level invariants that the daemon relies on:
// exactly one FirstResponse, and an agent_end for the terminal result.
func TestClaudeProvider_GoldenTranscripts(t *testing.T) {
	for _, name := range []string{"startup_and_turn.jsonl", "turn_with_tool.jsonl"} {
		t.Run(name, func(t *testing.T) {
			f, err := os.Open("testdata/claude/" + name)
			if err != nil {
				t.Fatalf("open fixture: %v", err)
			}
			defer f.Close()

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
				t.Fatalf("want exactly 1 FirstResponse, got %d (check system/init shape vs NOTES.md)", firstResponses)
			}
			if agentEnds < 1 {
				t.Fatalf("want >=1 agent_end (terminal result), got %d", agentEnds)
			}
		})
	}
}
