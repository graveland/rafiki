package main

import (
	"bytes"
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

func TestRender_MessageEnd_Suppressed(t *testing.T) {
	var buf bytes.Buffer
	r := newTailRenderer(&buf, false, outputTable, false)

	// Assistant message_end carries the full text; turn_end emits it for us.
	_ = r.render([]byte(`{"type":"ctrl_event","childId":"c_x","event":{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"hi"}]}}}`))

	if s := buf.String(); s != "" {
		t.Fatalf("message_end should be hidden by default; got: %q", s)
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
