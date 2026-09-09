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

// A user frame carries only tool_result blocks and must not open a turn. Each
// tool_result emits its ToolExecutionEnd FIRST, then a UserMessage carrying the
// flattened output — the exact shape fundi's publishToolResult uses. Without the
// second event the cockpit's reducer never sets HasResult and a claude child
// renders "⋯ no result" forever.
func TestNativeUserEmitsToolEndAndResultMessage(t *testing.T) {
	p := newClaudeProvider()
	line := []byte(`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"tu_1","content":"ok"}]}}`)

	evs := p.BusFramesNative(line, 1000)

	assertTypes(t, nativeTypeNames(evs), []string{"tool_execution_end", "user_message"})

	end := evs[0].GetToolExecutionEnd()
	if end == nil {
		t.Fatal("first event is not a ToolExecutionEnd")
	}
	if got := end.GetToolUseId(); got != "tu_1" {
		t.Fatalf("tool_use_id = %q, want %q", got, "tu_1")
	}

	um := evs[1].GetUserMessage()
	if um == nil {
		t.Fatal("second event is not a UserMessage")
	}
	if len(um.GetContent()) != 1 {
		t.Fatalf("content blocks = %d, want 1", len(um.GetContent()))
	}
	tr := um.GetContent()[0].GetToolResult()
	if tr == nil {
		t.Fatal("UserMessage block is not a ToolResult")
	}
	if got := tr.GetToolUseId(); got != "tu_1" {
		t.Fatalf("tool_result tool_use_id = %q, want %q", got, "tu_1")
	}
	if got := tr.GetIsError(); got {
		t.Fatalf("tool_result is_error = %v, want false", got)
	}
	if got := tr.GetContent()[0].GetText().GetText(); got != "ok" {
		t.Fatalf("tool_result text = %q, want %q", got, "ok")
	}
}

// Two tool_result blocks in one frame pair each end with its own result
// message, in per-block order: End, then the UserMessage (fundi's ToolEnd
// order). ToolUseIds must not cross.
func TestNativeUserPairsEachResultWithItsOwnMessage(t *testing.T) {
	p := newClaudeProvider()
	line := []byte(`{"type":"user","message":{"role":"user","content":[` +
		`{"type":"tool_result","tool_use_id":"tu_1","content":"first output"},` +
		`{"type":"tool_result","tool_use_id":"tu_2","content":"second output","is_error":true}]}}`)

	evs := p.BusFramesNative(line, 1000)

	assertTypes(t, nativeTypeNames(evs),
		[]string{"tool_execution_end", "user_message", "tool_execution_end", "user_message"})

	want := []struct {
		id      string
		isError bool
		text    string
	}{{"tu_1", false, "first output"}, {"tu_2", true, "second output"}}
	for i, w := range want {
		end := evs[i*2].GetToolExecutionEnd()
		if end == nil || end.GetToolUseId() != w.id || end.GetIsError() != w.isError {
			t.Fatalf("event %d: end = %+v, want tool_use_id=%q is_error=%v", i*2, evs[i*2], w.id, w.isError)
		}
		tr := evs[i*2+1].GetUserMessage().GetContent()[0].GetToolResult()
		if tr == nil || tr.GetToolUseId() != w.id {
			t.Fatalf("event %d: result message not paired with %q", i*2+1, w.id)
		}
		if tr.GetIsError() != w.isError {
			t.Fatalf("event %d: is_error = %v, want %v", i*2+1, tr.GetIsError(), w.isError)
		}
		if got := tr.GetContent()[0].GetText().GetText(); got != w.text {
			t.Fatalf("event %d: text = %q, want %q", i*2+1, got, w.text)
		}
	}
}

// An empty-content tool_result still emits the result message with an empty
// text block: a call that ran and returned nothing is a completed call, and the
// reducer's HasResult must be true rather than "⋯ no result".
func TestNativeUserEmitsResultMessageForEmptyContent(t *testing.T) {
	p := newClaudeProvider()
	line := []byte(`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"tu_1","content":""}]}}`)

	evs := p.BusFramesNative(line, 1000)

	assertTypes(t, nativeTypeNames(evs), []string{"tool_execution_end", "user_message"})

	tr := evs[1].GetUserMessage().GetContent()[0].GetToolResult()
	if tr == nil {
		t.Fatal("UserMessage block is not a ToolResult")
	}
	if len(tr.GetContent()) != 1 {
		t.Fatalf("tool_result content blocks = %d, want 1", len(tr.GetContent()))
	}
	if got := tr.GetContent()[0].GetText().GetText(); got != "" {
		t.Fatalf("tool_result text = %q, want empty", got)
	}
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

// The user's prompt must reach the native stream. claude never echoes it on
// stdout, so without this the cockpit shows replies with no prompts.
func TestOutboundEchoNativeEmitsUserMessage(t *testing.T) {
	p := newClaudeProvider()
	frame := []byte(`{"type":"prompt","message":"do the thing"}`)

	evs := p.OutboundEchoNative(frame, 1000)

	assertTypes(t, nativeTypeNames(evs), []string{"user_message"})

	um := evs[0].GetUserMessage()
	if um == nil {
		t.Fatal("event is not a UserMessage")
	}
	if len(um.GetContent()) != 1 {
		t.Fatalf("content blocks = %d, want 1", len(um.GetContent()))
	}
	if got := um.GetContent()[0].GetText().GetText(); got != "do the thing" {
		t.Fatalf("text = %q, want %q", got, "do the thing")
	}
}

// Frames carrying no user-authored text produce nothing. claudeUserEcho already
// owns this judgment (it rejects abort, set_session_name and empty messages);
// re-deciding it here would be a second copy that drifts.
func TestOutboundEchoNativeIgnoresNonPromptFrames(t *testing.T) {
	p := newClaudeProvider()
	for _, frame := range []string{
		`{"type":"abort"}`,
		`{"type":"set_session_name","name":"x"}`,
		`{"type":"prompt","message":""}`,
		`not json at all`,
	} {
		if evs := p.OutboundEchoNative([]byte(frame), 1000); len(evs) != 0 {
			t.Fatalf("frame %s produced %d events, want 0", frame, len(evs))
		}
	}
}

// OutboundEchoNative type-asserts Content to string and drops the echo when it
// is not one. This pins the invariant that assertion rests on.
//
// A test asserting the assertion's own fallback would be vacuous — nothing
// reachable through claudeUserEcho produces a non-string today. This instead
// fails the moment that stops being true, and says what to go fix: otherwise
// the change lands and every claude prompt silently stops reaching the cockpit.
func TestClaudeUserEchoContentIsAString(t *testing.T) {
	msg, _, ok := claudeUserEcho([]byte(`{"type":"prompt","message":"hi"}`), 1000)
	if !ok {
		t.Fatal("claudeUserEcho rejected a valid prompt frame")
	}
	if _, isString := msg.Content.(string); !isString {
		t.Fatalf("PiUserMessage.Content is %T, want string — OutboundEchoNative's "+
			"type assertion now drops every prompt silently; teach it the new shape",
			msg.Content)
	}
}
