package child

import (
	"bufio"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/multigres/testkit/assert"
)

func TestClaudeProvider_Bootstrap_IsNil(t *testing.T) {
	assert.NewAborting(t).Nil((ClaudeProvider{}).BootstrapFrame(), "claude needs no bootstrap, got")
}

func TestClaudeProvider_Parse_SystemInitIsFirstResponse(t *testing.T) {
	c := assert.NewAborting(t)
	line := []byte(`{"type":"system","subtype":"init","session_id":"sess-123","model":"claude-opus-4-8","cwd":"/tmp"}`)
	res := ClaudeProvider{}.Parse(line)
	c.True(res.FirstResponse, "system/init should be FirstResponse")
	c.False(!res.HasMeta || res.Meta.SessionID != "sess-123" || res.Meta.Model != "claude-opus-4-8", "meta = %+v hasMeta=%v", res.Meta, res.HasMeta)
	c.Empty(res.Events, "system/init should emit no SM events, got")
}

func TestParseInitCapturesSlashCommands(t *testing.T) {
	c := assert.NewAborting(t)
	line := []byte(`{"type":"system","subtype":"init","session_id":"s1","model":"claude","slash_commands":["compact","review","init"]}`)
	res := ClaudeProvider{}.Parse(line)
	c.True(res.HasMeta, "init frame should produce metadata")
	c.False(len(res.Meta.SlashCommands) != 3 || res.Meta.SlashCommands[0] != "compact", "SlashCommands = %v, want [compact review init]", res.Meta.SlashCommands)
}

func TestClaudeProvider_ReadyOnSpawn(t *testing.T) {
	c := assert.NewAborting(t)
	// claude is silent on stdout until prompted, so readiness is process-up.
	c.True((ClaudeProvider{}).ReadyOnSpawn(), "claude must be ReadyOnSpawn (no stdout readiness signal exists)")
	// pi announces readiness via response.get_state, so it is NOT ready on spawn.
	c.False((IdentityProvider{}).ReadyOnSpawn(), "pi must NOT be ReadyOnSpawn (it waits for response.get_state)")
}

func TestClaudeProvider_Parse_NonInitSystemIsNoop(t *testing.T) {
	c := assert.NewAborting(t)
	// The SessionStart hook lifecycle (and any non-init system frame) must NOT be
	// treated as readiness or emit SM events — readiness is process-up, and these
	// frames only ever arrive once a turn is already running.
	for _, line := range []string{
		`{"type":"system","subtype":"hook_started","hook_name":"SessionStart:startup","session_id":"sess-h"}`,
		`{"type":"system","subtype":"hook_response","session_id":"sess-h"}`,
	} {
		res := ClaudeProvider{}.Parse([]byte(line))
		c.False(res.FirstResponse, "non-init system frame must not signal FirstResponse: %s", line)
		c.Empty(res.Events, "non-init system frame should emit no SM events, got")
	}
}

func TestClaudeProvider_Parse_AssistantTextIsAgentStart(t *testing.T) {
	c := assert.NewAborting(t)
	line := []byte(`{"type":"assistant","session_id":"sess-123","message":{"content":[{"type":"text","text":"hi"}]}}`)
	res := ClaudeProvider{}.Parse(line)
	c.False(res.FirstResponse, "assistant must not be FirstResponse")
	c.False(len(res.Events) != 1 || res.Events[0].Type != "agent_start", "events = %+v", res.Events)
}

func TestClaudeProvider_Parse_AssistantToolUseStartsTool(t *testing.T) {
	line := []byte(`{"type":"assistant","session_id":"s","message":{"content":[{"type":"text","text":"running"},{"type":"tool_use","id":"t1","name":"bash"}]}}`)
	res := ClaudeProvider{}.Parse(line)
	// First agent_start (assistant content present), then one tool_execution_start
	// per tool_use block.
	assert.NewAborting(t).False(len(res.Events) != 2 || res.Events[0].Type != "agent_start" || res.Events[1].Type != "tool_execution_start", "events = %+v", res.Events)
}

func TestClaudeProvider_Parse_ToolResultEndsTool(t *testing.T) {
	line := []byte(`{"type":"user","session_id":"s","message":{"content":[{"type":"tool_result","tool_use_id":"t1","content":"ok"}]}}`)
	res := ClaudeProvider{}.Parse(line)
	assert.NewAborting(t).False(len(res.Events) != 1 || res.Events[0].Type != "tool_execution_end", "events = %+v", res.Events)
}

func TestClaudeProvider_Parse_ResultIsAgentEnd(t *testing.T) {
	c := assert.NewAborting(t)
	line := []byte(`{"type":"result","subtype":"success","session_id":"sess-123","total_cost_usd":0.01}`)
	res := ClaudeProvider{}.Parse(line)
	c.False(len(res.Events) != 1 || res.Events[0].Type != "agent_end", "events = %+v", res.Events)
	c.False(!res.HasMeta || res.Meta.SessionID != "sess-123", "result should carry session id, meta=%+v", res.Meta)
}

func TestClaudeProvider_Parse_Garbage(t *testing.T) {
	res := ClaudeProvider{}.Parse([]byte(`not json`))
	assert.NewAborting(t).False(res.FirstResponse || res.HasMeta || len(res.Events) != 0, "garbage should be a no-op, got %+v", res)
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
			c := assert.NewAborting(t)
			f, err := os.Open("testdata/claude/" + name)
			c.NoError(err, "open fixture")
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
			c.NoError(sc.Err(), "scan")
			c.Eq(1, firstResponses, "want exactly 1 FirstResponse (one init line), got")
			c.GreaterOrEqual(1, agentEnds, "want >=1 agent_end (terminal result), got")
		})
	}
}

func TestClaudeProvider_EncodePrompt(t *testing.T) {
	c := assert.NewAborting(t)
	got := ClaudeProvider{}.EncodeOutbound([]byte(`{"type":"prompt","message":"hello there"}`))
	c.NotNil(got, "prompt must encode to a non-nil frame")
	var env struct {
		Type    string `json:"type"`
		Message struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"message"`
	}
	err := json.Unmarshal(got, &env)
	c.NoError(err, "encoded frame is not valid JSON: %v (%s)", err, got)
	c.False(env.Type != "user" || env.Message.Role != "user" || env.Message.Content != "hello there", "bad envelope: %s", got)
}

func TestClaudeProvider_EncodeSteerSameAsPrompt(t *testing.T) {
	got := ClaudeProvider{}.EncodeOutbound([]byte(`{"type":"steer","message":"keep going"}`))
	assert.NewAborting(t).False(got == nil || !json.Valid(got), "steer should encode like a prompt, got %q", got)
}

func TestClaudeProvider_EncodeUnknownDropped(t *testing.T) {
	assert.NewAborting(t).Nil((ClaudeProvider{}).EncodeOutbound([]byte(`{"type":"set_session_name","name":"x"}`)), "unsupported frame should be dropped (nil), got")
}

func TestClaudeProvider_EncodeGarbageDropped(t *testing.T) {
	assert.NewAborting(t).Nil((ClaudeProvider{}).EncodeOutbound([]byte(`not json`)), "garbage should be dropped (nil), got")
}

func TestClaudeProvider_EncodePromptCarriesAttachments(t *testing.T) {
	c := assert.NewAborting(t)
	frame := []byte(`{"type":"prompt","message":"what is this","attachments":[{"media_type":"image/png","data":"AQID"}]}`)
	got := ClaudeProvider{}.EncodeOutbound(frame)
	c.NotNil(got, "prompt with attachments must encode")
	var env struct {
		Message struct {
			Content []struct {
				Type   string `json:"type"`
				Text   string `json:"text"`
				Source struct {
					Type      string `json:"type"`
					MediaType string `json:"media_type"`
					Data      string `json:"data"`
				} `json:"source"`
			} `json:"content"`
		} `json:"message"`
	}
	c.NoError(json.Unmarshal(got, &env), "bad frame: %s", got)
	c.Eq(2, len(env.Message.Content), "image then text: %s", got)
	c.Eq("image", env.Message.Content[0].Type, "image must precede text")
	c.Eq("base64", env.Message.Content[0].Source.Type, "source type")
	c.Eq("image/png", env.Message.Content[0].Source.MediaType, "media type")
	c.Eq("AQID", env.Message.Content[0].Source.Data, "data must stay base64 of the raw bytes")
	c.Eq("text", env.Message.Content[1].Type, "text block")
	c.Eq("what is this", env.Message.Content[1].Text, "text")
}
