package child

import (
	"testing"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"

	"github.com/multigres/testkit/assert"
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
		case *rafikiv1.Event_CompactionBoundary:
			out = append(out, "compaction_boundary")
		default:
			out = append(out, "unknown")
		}
	}
	return out
}

func assertTypes(t *testing.T, got []string, want []string) {
	t.Helper()
	c := assert.NewAborting(t)
	c.Len(got, len(want), "event types = %v, want %v", got, want)
	for i := range want {
		c.Eq(want[i], got[i], "event types = %v, want %v", got, want)
	}
}

// A text-only assistant frame produces exactly one assistant_message. No
// TurnStart (the pi path's openTurn owns the shared turnActive flag and runs
// first, so a native TurnStart never fires in production) and no delta (claude
// frames are complete messages, so a delta would duplicate the message and, on
// a later turn, append its text to the PREVIOUS turn's finalized block).
func TestNativeAssistantTextEmitsMessageOnly(t *testing.T) {
	c := assert.NewAborting(t)
	p := newClaudeProvider()
	line := []byte(`{"type":"assistant","message":{"model":"claude-opus-5","content":[{"type":"text","text":"hello"}]}}`)

	evs := p.BusFramesNative(line, 1000)

	assertTypes(t, nativeTypeNames(evs), []string{"assistant_message"})

	am := evs[0].GetAssistantMessage()
	c.NotNil(am, "first event is not an AssistantMessage")
	c.Len(am.GetContent(), 1, "content blocks = %d, want 1", len(am.GetContent()))
	c.Eq("hello", am.GetContent()[0].GetText().GetText(), "text")
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

	assert.NewAborting(t).Eq("tu_1", evs[1].GetToolExecutionStart().GetToolUseId(), "tool_use_id")
}

// A user frame carries only tool_result blocks and must not open a turn. Each
// tool_result emits its ToolExecutionEnd FIRST, then a UserMessage carrying the
// flattened output — the exact shape fundi's publishToolResult uses. Without the
// second event the cockpit's reducer never sets HasResult and a claude child
// renders "⋯ no result" forever.
func TestNativeUserEmitsToolEndAndResultMessage(t *testing.T) {
	c := assert.NewAborting(t)
	p := newClaudeProvider()
	line := []byte(`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"tu_1","content":"ok"}]}}`)

	evs := p.BusFramesNative(line, 1000)

	assertTypes(t, nativeTypeNames(evs), []string{"tool_execution_end", "user_message"})

	end := evs[0].GetToolExecutionEnd()
	c.NotNil(end, "first event is not a ToolExecutionEnd")
	c.Eq("tu_1", end.GetToolUseId(), "tool_use_id")

	um := evs[1].GetUserMessage()
	c.NotNil(um, "second event is not a UserMessage")
	c.Len(um.GetContent(), 1, "content blocks = %d, want 1", len(um.GetContent()))
	tr := um.GetContent()[0].GetToolResult()
	c.NotNil(tr, "UserMessage block is not a ToolResult")
	c.Eq("tu_1", tr.GetToolUseId(), "tool_result tool_use_id")
	c.False(tr.GetIsError(), "tool_result is_error")
	c.Eq("ok", tr.GetContent()[0].GetText().GetText(), "tool_result text")
}

// Two tool_result blocks in one frame pair each end with its own result
// message, in per-block order: End, then the UserMessage (fundi's ToolEnd
// order). ToolUseIds must not cross.
func TestNativeUserPairsEachResultWithItsOwnMessage(t *testing.T) {
	c := assert.NewAborting(t)
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
		c.Eq(w.isError, tr.GetIsError(), "event %d: is_error = %v, want", i*2+1, tr.GetIsError())
		got := tr.GetContent()[0].GetText().GetText()
		c.Eq(w.text, got, "event %d: text = %q, want", i*2+1, got)
	}
}

// An empty-content tool_result still emits the result message with an empty
// text block: a call that ran and returned nothing is a completed call, and the
// reducer's HasResult must be true rather than "⋯ no result".
func TestNativeUserEmitsResultMessageForEmptyContent(t *testing.T) {
	c := assert.NewAborting(t)
	p := newClaudeProvider()
	line := []byte(`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"tu_1","content":""}]}}`)

	evs := p.BusFramesNative(line, 1000)

	assertTypes(t, nativeTypeNames(evs), []string{"tool_execution_end", "user_message"})

	tr := evs[1].GetUserMessage().GetContent()[0].GetToolResult()
	c.NotNil(tr, "UserMessage block is not a ToolResult")
	c.Len(tr.GetContent(), 1, "tool_result content blocks = %d, want 1", len(tr.GetContent()))
	c.Eq("", tr.GetContent()[0].GetText().GetText(), "tool_result text")
}

// A tool_result carrying an image (Read on a PNG, an MCP screenshot) keeps it:
// the flattened text block first, then each image in order, bytes decoded from
// claude's base64 source. Dropping it left the cockpit an empty result.
func TestNativeUserToolResultCarriesImages(t *testing.T) {
	c := assert.NewAborting(t)
	p := newClaudeProvider()
	line := []byte(`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"tu_1","content":[` +
		`{"type":"text","text":"read shot.png"},` +
		`{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AQID"}},` +
		`{"type":"image","source":{"type":"base64","media_type":"image/jpeg","data":"BAU="}}]}]}}`)

	evs := p.BusFramesNative(line, 1000)

	assertTypes(t, nativeTypeNames(evs), []string{"tool_execution_end", "user_message"})
	content := evs[1].GetUserMessage().GetContent()[0].GetToolResult().GetContent()
	c.Len(content, 3, "text then two images, got %d blocks", len(content))
	c.Eq("read shot.png", content[0].GetText().GetText(), "text first")
	c.Eq("image/png", content[1].GetImage().GetMediaType(), "first image media type")
	c.EqDeep([]byte{1, 2, 3}, content[1].GetImage().GetData(), "first image bytes")
	c.Eq("image/jpeg", content[2].GetImage().GetMediaType(), "second image media type")
	c.EqDeep([]byte{4, 5}, content[2].GetImage().GetData(), "second image bytes")
	for i, b := range content {
		c.Eq(int32(i), b.GetIndex(), "block %d index", i)
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
			assert.NewAborting(t).False(name == "turn_start" || name == "content_block_delta", "native path emitted %q; the claude vocabulary must match fundi's", name)
		}
	}
}

// The user's prompt must reach the native stream. claude never echoes it on
// stdout, so without this the cockpit shows replies with no prompts.
func TestOutboundEchoNativeEmitsUserMessage(t *testing.T) {
	c := assert.NewAborting(t)
	p := newClaudeProvider()
	frame := []byte(`{"type":"prompt","message":"do the thing"}`)

	evs := p.OutboundEchoNative(frame, 1000)

	assertTypes(t, nativeTypeNames(evs), []string{"user_message"})

	um := evs[0].GetUserMessage()
	c.NotNil(um, "event is not a UserMessage")
	c.Len(um.GetContent(), 1, "content blocks = %d, want 1", len(um.GetContent()))
	c.Eq("do the thing", um.GetContent()[0].GetText().GetText(), "text")
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
		evs := p.OutboundEchoNative([]byte(frame), 1000)
		assert.NewAborting(t).Empty(evs, "frame %s produced %d events, want 0", frame, len(evs))
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
	assert.NewAborting(t).True(ok, "claudeUserEcho rejected a valid prompt frame")
	if _, isString := msg.Content.(string); !isString {
		t.Fatalf("PiUserMessage.Content is %T, want string — OutboundEchoNative's "+
			"type assertion now drops every prompt silently; teach it the new shape",
			msg.Content)
	}
}

// Claude Code's own compaction (auto or /compact) announces itself on stdout
// with a system/compact_boundary frame. BusFramesNative is the durable event
// path, so this frame is the only live signal rafiki gets that the context was
// rewritten; without the branch the boundary is visible only on reattach.
func TestBusFramesNativeEmitsCompactionBoundary(t *testing.T) {
	c := assert.NewAborting(t)
	p := newClaudeProvider()
	line := []byte(`{"type":"system","subtype":"compact_boundary","compact_metadata":{"trigger":"auto","pre_tokens":182000,"post_tokens":45000}}`)

	evs := p.BusFramesNative(line, 1000)

	assertTypes(t, nativeTypeNames(evs), []string{"compaction_boundary"})
	cb := evs[0].GetCompactionBoundary()
	c.NotNil(cb, "event is not a CompactionBoundary")
	c.Eq("auto", cb.GetTrigger(), "trigger")
	// GetPreTokens/GetPostTokens return 0 on a nil pointer, so these assert
	// both presence and value — the pointers must be set, not bare zeroes.
	c.Eq(182000, cb.GetPreTokens(), "pre_tokens")
	c.Eq(45000, cb.GetPostTokens(), "post_tokens")
	c.Eq(int64(1000), evs[0].GetTs().AsTime().UnixMilli(), "ts")
}

// microcompact_boundary is a different subtype string entirely: it matches
// neither the init branch nor the compact_boundary branch and falls through to
// the default nil. No dedicated case is added for it.
func TestBusFramesNativeIgnoresMicrocompactBoundary(t *testing.T) {
	p := newClaudeProvider()
	line := []byte(`{"type":"system","subtype":"microcompact_boundary"}`)

	evs := p.BusFramesNative(line, 1000)
	assert.NewAborting(t).Empty(evs, "microcompact_boundary produced %d events, want 0", len(evs))
}

// The system/init frame carries the model into provider state and emits no
// events. Pin both halves: the compact_boundary branch must never turn an init
// frame into an event, and the early return must not have broken the model
// capture.
func TestBusFramesNativeSystemInitEmitsNothing(t *testing.T) {
	c := assert.NewAborting(t)
	p := newClaudeProvider()
	line := []byte(`{"type":"system","subtype":"init","session_id":"sess-1","model":"claude-opus-5","cwd":"/tmp"}`)

	evs := p.BusFramesNative(line, 1000)
	c.Empty(evs, "system/init produced %d events, want 0", len(evs))
	c.Eq("claude-opus-5", p.st.model, "st.model")
}
