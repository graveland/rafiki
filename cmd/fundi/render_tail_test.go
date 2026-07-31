package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"testing"
)

func TestRender_AgentStartEnd(t *testing.T) {
	var buf bytes.Buffer
	r := newTailRenderer(&buf, false, outputTable, false)

	_ = r.render([]byte(`{"type":"ctrl_event","childId":"c_x","event":{"type":"agent_start"}}`))
	_ = r.render([]byte(`{"type":"ctrl_event","childId":"c_x","event":{"type":"agent_end"}}`))

	out := buf.String()
	if !strings.Contains(out, "agent_start") {
		t.Fatalf("missing agent_start in output:\n%s", out)
	}
	if !strings.Contains(out, "agent_end") {
		t.Fatalf("missing agent_end in output:\n%s", out)
	}
}

func TestRender_ToolExecution(t *testing.T) {
	var buf bytes.Buffer
	r := newTailRenderer(&buf, false, outputTable, false)

	_ = r.render([]byte(`{"type":"ctrl_event","childId":"c_x","event":{"type":"tool_execution_start","toolName":"bash"}}`))
	_ = r.render([]byte(`{"type":"ctrl_event","childId":"c_x","event":{"type":"tool_execution_end","toolName":"bash","isError":false}}`))

	out := buf.String()
	if !strings.Contains(out, "bash") {
		t.Fatalf("missing tool name in output:\n%s", out)
	}
	if !strings.Contains(out, "↻") {
		t.Fatalf("missing start marker ↻ in output:\n%s", out)
	}
	if !strings.Contains(out, "✓") {
		t.Fatalf("missing success marker ✓ in output:\n%s", out)
	}
}

func TestRender_ToolDetail_ArgsAndResult(t *testing.T) {
	var buf bytes.Buffer
	r := newTailRenderer(&buf, false, outputTable, false)
	r.width = 200

	// Bash: args carry the command, result is an AgentToolResult content block.
	_ = r.render([]byte(`{"type":"ctrl_event","childId":"c_x","event":{"type":"tool_execution_start","toolName":"Bash","args":{"command":"echo hi"}}}`))
	_ = r.render([]byte(`{"type":"ctrl_event","childId":"c_x","event":{"type":"tool_execution_end","toolName":"Bash","isError":false,"result":{"content":[{"type":"text","text":"hi"}]}}}`))

	out := buf.String()
	if !strings.Contains(out, `"command": "echo hi"`) {
		t.Fatalf("args not rendered; got:\n%s", out)
	}
	// Result text block should be flattened to its text, not raw JSON.
	if !strings.Contains(out, "hi") || strings.Contains(out, `"content"`) {
		t.Fatalf("result content not flattened to text; got:\n%s", out)
	}
}

func TestRender_ToolDetail_HeadTailElision(t *testing.T) {
	var buf bytes.Buffer
	r := newTailRenderer(&buf, false, outputTable, false)
	r.width = 200

	var lines []string
	for i := 0; i < 30; i++ {
		lines = append(lines, "line")
	}
	text := strings.Join(lines, "\n")
	frame := `{"type":"ctrl_event","childId":"c_x","event":{"type":"tool_execution_end","toolName":"Bash","result":{"content":[{"type":"text","text":` +
		mustJSON(text) + `}]}}}`
	_ = r.render([]byte(frame))

	out := buf.String()
	if !strings.Contains(out, "… (20 more lines)") {
		t.Fatalf("expected elision marker for 30 lines; got:\n%s", out)
	}
	// 1 tool line + 5 head + 1 marker + 5 tail = 12 lines of output.
	if got := strings.Count(out, "\n"); got != 12 {
		t.Fatalf("expected 12 output lines, got %d:\n%s", got, out)
	}
}

func TestRender_ToolDetail_WidthClamp(t *testing.T) {
	var buf bytes.Buffer
	r := newTailRenderer(&buf, false, outputTable, false)
	r.width = 20 // avail = 20 - 4 indent = 16

	long := strings.Repeat("x", 100)
	frame := `{"type":"ctrl_event","childId":"c_x","event":{"type":"tool_execution_end","toolName":"Bash","result":{"content":[{"type":"text","text":` +
		mustJSON(long) + `}]}}}`
	_ = r.render([]byte(frame))

	out := buf.String()
	if !strings.Contains(out, "…") {
		t.Fatalf("expected width-truncation ellipsis; got:\n%s", out)
	}
	if strings.Contains(out, strings.Repeat("x", 100)) {
		t.Fatalf("long line was not clamped; got:\n%s", out)
	}
}

func TestRender_PiPerMessageUsage_Footer(t *testing.T) {
	var buf bytes.Buffer
	r := newTailRenderer(&buf, false, outputTable, false)

	// pi carries usage on each assistant message_end (not on agent_end).
	_ = r.render([]byte(`{"type":"ctrl_event","childId":"c_x","event":{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"hi"}],"usage":{"input":1200,"output":50,"cacheRead":0,"cacheWrite":0,"cost":{"total":0.012}}}}}`))

	out := buf.String()
	if !strings.Contains(out, "hi") {
		t.Fatalf("assistant text missing; got:\n%s", out)
	}
	if !strings.Contains(out, "1.2k in / 50 out") || !strings.Contains(out, "$0.0120") {
		t.Fatalf("per-message usage footer missing; got:\n%s", out)
	}
}

func TestRender_ZeroUsage_NoFooter(t *testing.T) {
	var buf bytes.Buffer
	r := newTailRenderer(&buf, false, outputTable, false)

	// claude emits a zero per-message usage (the real total rides on agent_end);
	// a zero usage must not render a spurious footer line.
	_ = r.render([]byte(`{"type":"ctrl_event","childId":"c_x","event":{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"hi"}],"usage":{"input":0,"output":0,"cacheRead":0,"cacheWrite":0,"cost":{"total":0}}}}}`))

	if out := buf.String(); strings.Contains(out, "·") {
		t.Fatalf("zero usage should not render a footer; got:\n%s", out)
	}
}

func TestRender_AgentEnd_UsageFooter(t *testing.T) {
	var buf bytes.Buffer
	r := newTailRenderer(&buf, false, outputTable, false)

	_ = r.render([]byte(`{"type":"ctrl_event","childId":"c_x","event":{"type":"agent_end","messages":[],"usage":{"input":8444,"output":79,"cacheRead":41125,"cacheWrite":13998,"cost":{"total":0.4095}}}}`))

	out := buf.String()
	if !strings.Contains(out, "8.4k in / 79 out") {
		t.Fatalf("usage tokens missing; got:\n%s", out)
	}
	if !strings.Contains(out, "cached") || !strings.Contains(out, "$0.4095") {
		t.Fatalf("usage cache/cost missing; got:\n%s", out)
	}
}

func TestRender_AgentEnd_NoUsage_PlainDivider(t *testing.T) {
	var buf bytes.Buffer
	r := newTailRenderer(&buf, false, outputTable, false)

	_ = r.render([]byte(`{"type":"ctrl_event","childId":"c_x","event":{"type":"agent_end","messages":[]}}`))

	out := buf.String()
	if !strings.Contains(out, "agent_end") || strings.Contains(out, "·") {
		t.Fatalf("expected plain agent_end divider; got:\n%s", out)
	}
}

func TestRender_SubAgentNesting_Indents(t *testing.T) {
	var buf bytes.Buffer
	r := newTailRenderer(&buf, false, outputTable, false)
	r.width = 200

	// A tool running inside a Task sub-agent carries parentToolUseId.
	_ = r.render([]byte(`{"type":"ctrl_event","childId":"c_x","event":{"type":"tool_execution_start","toolName":"Grep","parentToolUseId":"task_1","args":{"pattern":"foo"}}}`))
	// A top-level tool does not.
	_ = r.render([]byte(`{"type":"ctrl_event","childId":"c_x","event":{"type":"tool_execution_start","toolName":"Bash","args":{"command":"ls"}}}`))

	out := buf.String()
	if !strings.Contains(out, "      ↻ Grep") { // nest(4) + 2 = 6 spaces
		t.Fatalf("nested tool should be indented 6 spaces; got:\n%s", out)
	}
	if !strings.Contains(out, "\n  ↻ Bash") { // top-level = 2 spaces
		t.Fatalf("top-level tool should be indented 2 spaces; got:\n%s", out)
	}
}

func mustJSON(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	return string(b)
}

func TestRender_ChildExited(t *testing.T) {
	var buf bytes.Buffer
	r := newTailRenderer(&buf, false, outputTable, false)

	exitCode := 0
	_ = r.render([]byte(`{"type":"ctrl_child_exited","childId":"c_x","exitCode":0}`))

	out := buf.String()
	if !strings.Contains(out, "child exited") {
		t.Fatalf("missing 'child exited' in output:\n%s", out)
	}
	// Suppress unused variable warning.
	_ = exitCode
}

func TestRender_UserMessage_ShownAsBareLabel(t *testing.T) {
	var buf bytes.Buffer
	r := newTailRenderer(&buf, false, outputTable, false)

	_ = r.render([]byte(`{"type":"ctrl_event","childId":"c_x","event":{"type":"message_start","message":{"role":"user","content":[{"type":"text","text":"hello world"}]}}}`))

	out := buf.String()
	if !strings.Contains(out, "[user]") {
		t.Fatalf("user message should be labelled [user]; got:\n%s", out)
	}
	if !strings.Contains(out, "hello world") {
		t.Fatalf("user message text missing; got:\n%s", out)
	}
}

func TestRender_AssistantMessageStart_Suppressed(t *testing.T) {
	var buf bytes.Buffer
	r := newTailRenderer(&buf, false, outputTable, false)

	// Empty-content assistant message_start (the placeholder before streaming).
	_ = r.render([]byte(`{"type":"ctrl_event","childId":"c_x","event":{"type":"message_start","message":{"role":"assistant","content":[]}}}`))

	if s := buf.String(); s != "" {
		t.Fatalf("assistant message_start should be hidden by default; got: %q", s)
	}
}

func TestRender_AssistantReply_ShownOnMessageEnd(t *testing.T) {
	var buf bytes.Buffer
	r := newTailRenderer(&buf, false, outputTable, false)

	// claude emits no turn_end; the reply arrives in the assistant message_end.
	_ = r.render([]byte(`{"type":"ctrl_event","childId":"c_x","event":{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"hi there"}]}}}`))

	if s := buf.String(); !strings.Contains(s, "hi there") {
		t.Fatalf("assistant reply should be shown on message_end; got: %q", s)
	}
}

func TestRender_UserPrompt_StringContent(t *testing.T) {
	var buf bytes.Buffer
	r := newTailRenderer(&buf, false, outputTable, false)

	// claude's synthesized user echo carries content as a plain string, not a
	// block array — this previously failed to parse and showed nothing.
	_ = r.render([]byte(`{"type":"ctrl_event","childId":"c_x","event":{"type":"message_start","message":{"role":"user","content":"do the thing"}}}`))

	out := buf.String()
	if !strings.Contains(out, "[user]") || !strings.Contains(out, "do the thing") {
		t.Fatalf("user prompt with string content should render; got: %q", out)
	}
}

func TestRender_Thinking_ShownDimmedAndAbridged(t *testing.T) {
	var buf bytes.Buffer
	r := newTailRenderer(&buf, false, outputTable, false)
	r.width = 200

	_ = r.render([]byte(`{"type":"ctrl_event","childId":"c_x","event":{"type":"message_end","message":{"role":"assistant","content":[{"type":"thinking","thinking":"let me reason"},{"type":"text","text":"answer"}]}}}`))

	out := buf.String()
	if !strings.Contains(out, "[thinking]") || !strings.Contains(out, "let me reason") {
		t.Fatalf("thinking block should render; got: %q", out)
	}
	if !strings.Contains(out, "answer") {
		t.Fatalf("assistant text after thinking should render; got: %q", out)
	}
}

func TestRender_TurnStart_Suppressed(t *testing.T) {
	var buf bytes.Buffer
	r := newTailRenderer(&buf, false, outputTable, false)

	_ = r.render([]byte(`{"type":"ctrl_event","childId":"c_x","event":{"type":"turn_start"}}`))

	if s := buf.String(); s != "" {
		t.Fatalf("turn_start should be hidden by default; got: %q", s)
	}
}

func TestRender_Verbose_ShowsMessageEnd(t *testing.T) {
	var buf bytes.Buffer
	r := newTailRenderer(&buf, false, outputTable, true)

	_ = r.render([]byte(`{"type":"ctrl_event","childId":"c_x","event":{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"hi"}]}}}`))

	out := buf.String()
	if !strings.Contains(out, "message_end") {
		t.Fatalf("verbose mode should show message_end; got:\n%s", out)
	}
}

// Response frames arrive wrapped in ctrl_event envelopes (the daemon
// fans pi's whole stdout stream to per-child subscribers under that
// wrapper).  The inner event type is "response".
func TestRender_Response_HiddenByDefault(t *testing.T) {
	var buf bytes.Buffer
	r := newTailRenderer(&buf, false, outputTable, false)

	_ = r.render([]byte(`{"type":"ctrl_event","childId":"c_x","event":{"id":"gc-1","type":"response","command":"get_commands","success":true,"data":{"commands":[]}}}`))

	if s := buf.String(); s != "" {
		t.Fatalf("expected response frame to be hidden without --verbose; got: %q", s)
	}
}

func TestRender_Response_ShownWithVerbose(t *testing.T) {
	var buf bytes.Buffer
	r := newTailRenderer(&buf, false, outputTable, true)

	_ = r.render([]byte(`{"type":"ctrl_event","childId":"c_x","event":{"id":"gc-1","type":"response","command":"get_commands","success":true}}`))

	out := buf.String()
	if !strings.Contains(out, "[response]") {
		t.Fatalf("verbose mode should label response frames; got:\n%s", out)
	}
	if !strings.Contains(out, "get_commands") {
		t.Fatalf("verbose mode should include response payload; got:\n%s", out)
	}
	if !strings.Contains(out, "\n") {
		t.Fatalf("verbose response should be pretty-printed (multi-line); got:\n%s", out)
	}
}

func TestRender_DaemonShutdown(t *testing.T) {
	// Capture stderr to verify the message content.
	rp, wp, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	oldStderr := os.Stderr
	os.Stderr = wp

	var w bytes.Buffer
	r := newTailRenderer(&w, false, outputTable, false)
	renderErr := r.render([]byte(`{"type":"ctrl_daemon_shutdown","reason":"signal received: terminated"}`))

	// Restore stderr before reading the pipe to avoid a write-side block.
	wp.Close()
	os.Stderr = oldStderr

	var stderrBuf bytes.Buffer
	if _, err := io.Copy(&stderrBuf, rp); err != nil {
		t.Fatal(err)
	}
	rp.Close()

	if !errors.Is(renderErr, errDaemonShutdown) {
		t.Fatalf("expected errDaemonShutdown, got %v", renderErr)
	}
	stderrStr := stderrBuf.String()
	if !strings.Contains(stderrStr, "daemon shutting down") {
		t.Errorf("stderr missing 'daemon shutting down': %q", stderrStr)
	}
	if !strings.Contains(stderrStr, "signal received: terminated") {
		t.Errorf("stderr missing reason: %q", stderrStr)
	}
}

func TestRender_JSONMode_PassThrough(t *testing.T) {
	var buf bytes.Buffer
	r := newTailRenderer(&buf, false, outputJSON, false)

	_ = r.render([]byte(`{"type":"ctrl_event","childId":"c_x","event":{"type":"agent_start"}}`))

	out := buf.String()
	if !strings.Contains(out, `"agent_start"`) {
		t.Fatalf("JSON mode did not pass through event type; got:\n%s", out)
	}
	// JSON mode should also output the outer wrapper fields.
	if !strings.Contains(out, `"ctrl_event"`) {
		t.Fatalf("JSON mode did not pass through outer type; got:\n%s", out)
	}
}
