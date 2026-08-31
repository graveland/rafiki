package child

import (
	"testing"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
)

// nativeTypeNames reduces an event slice to its payload type names, which is
// what these tests assert on: the ORDER and SET of events is the contract the
// cockpit's session reducer depends on.
func nativeTypeNames(evs []*rafikiv1.Event) []string {
	out := make([]string, 0, len(evs))
	for _, ev := range evs {
		switch ev.Payload.(type) {
		case *rafikiv1.Event_TurnStart:
			out = append(out, "turn_start")
		case *rafikiv1.Event_TurnEnd:
			out = append(out, "turn_end")
		case *rafikiv1.Event_AssistantMessage:
			out = append(out, "assistant_message")
		case *rafikiv1.Event_UserMessage:
			out = append(out, "user_message")
		case *rafikiv1.Event_ContentBlockDelta:
			out = append(out, "content_block_delta")
		case *rafikiv1.Event_ToolExecutionStart:
			out = append(out, "tool_execution_start")
		case *rafikiv1.Event_ToolExecutionEnd:
			out = append(out, "tool_execution_end")
		default:
			out = append(out, "unknown")
		}
	}
	return out
}

func assertTypes(t *testing.T, got []string, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("event types = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("event types = %v, want %v", got, want)
		}
	}
}

// A text-only assistant frame produces exactly one assistant_message. No
// TurnStart (the pi path's openTurn owns the shared turnActive flag and runs
// first, so a native TurnStart never fires in production) and no delta (claude
// frames are complete messages, so a delta would duplicate the message and, on
// a later turn, append its text to the PREVIOUS turn's finalized block).
func TestNativeAssistantTextEmitsMessageOnly(t *testing.T) {
	p := newClaudeProvider()
	line := []byte(`{"type":"assistant","message":{"model":"claude-opus-5","content":[{"type":"text","text":"hello"}]}}`)

	evs := p.BusFramesNative(line, 1000)

	assertTypes(t, nativeTypeNames(evs), []string{"assistant_message"})

	am := evs[0].GetAssistantMessage()
	if am == nil {
		t.Fatal("first event is not an AssistantMessage")
	}
	if len(am.GetContent()) != 1 {
		t.Fatalf("content blocks = %d, want 1", len(am.GetContent()))
	}
	if got := am.GetContent()[0].GetText().GetText(); got != "hello" {
		t.Fatalf("text = %q, want %q", got, "hello")
	}
}

// A tool_use frame emits the assistant message FIRST, then the execution start.
// The cockpit's applyToolStart looks up the call by id on the last assistant
// block and only marks it running; if the start arrives first there is no block
// to find and it appends a duplicate.
func TestNativeAssistantEmitsMessageBeforeToolStart(t *testing.T) {
	p := newClaudeProvider()
	line := []byte(`{"type":"assistant","message":{"model":"claude-opus-5","content":[{"type":"tool_use","id":"tu_1","name":"bash","input":{"command":"ls"}}]}}`)

	evs := p.BusFramesNative(line, 1000)

	assertTypes(t, nativeTypeNames(evs), []string{"assistant_message", "tool_execution_start"})

	if got := evs[1].GetToolExecutionStart().GetToolUseId(); got != "tu_1" {
		t.Fatalf("tool_use_id = %q, want %q", got, "tu_1")
	}
}

// A user frame carries only tool_result blocks and must not open a turn.
func TestNativeUserEmitsToolEndOnly(t *testing.T) {
	p := newClaudeProvider()
	line := []byte(`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"tu_1","content":"ok"}]}}`)

	evs := p.BusFramesNative(line, 1000)

	assertTypes(t, nativeTypeNames(evs), []string{"tool_execution_end"})
}

// Guard the whole rule in one place: the native claude vocabulary is fundi's
// vocabulary. Nothing in it may be a TurnStart or a ContentBlockDelta.
func TestNativeVocabularyExcludesTurnStartAndDeltas(t *testing.T) {
	lines := [][]byte{
		[]byte(`{"type":"assistant","message":{"model":"claude-opus-5","content":[{"type":"text","text":"a"}]}}`),
		[]byte(`{"type":"assistant","message":{"model":"claude-opus-5","content":[{"type":"thinking","thinking":"t"}]}}`),
		[]byte(`{"type":"assistant","message":{"model":"claude-opus-5","content":[{"type":"tool_use","id":"tu_1","name":"bash","input":{}}]}}`),
		[]byte(`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"tu_1","content":"ok"}]}}`),
	}
	p := newClaudeProvider()
	for _, line := range lines {
		for _, name := range nativeTypeNames(p.BusFramesNative(line, 1000)) {
			if name == "turn_start" || name == "content_block_delta" {
				t.Fatalf("native path emitted %q; the claude vocabulary must match fundi's", name)
			}
		}
	}
}
