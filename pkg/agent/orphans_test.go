package agent

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"

	"go.graveland.dev/rafiki/pkg/llm"
)

// capturingSender is a scripted llm.Sender (same replay-in-order contract as
// fakeSender/scriptedSender) that additionally records the
// anthropic.MessageNewParams of every New call it serves. It is the seam
// that lets a test assert on the OUTGOING REQUEST SHAPE rather than merely
// observing that a scripted call "succeeded" — fakeSender.New ignores params
// entirely (see faketurns.go), so a successful Continue proves nothing about
// whether the request it sent was well-formed. The shape the real Anthropic
// API actually enforces — a tool_use block always followed by a matching
// tool_result — is exactly what assertToolResultFollowsToolUse checks against
// a captured request's Messages.
type capturingSender struct {
	mu       sync.Mutex
	next     int
	turns    []*anthropic.Message
	captured []anthropic.MessageNewParams
}

// newCapturingSender scripts bodies (raw JSON anthropic.Message values) to be
// replayed in call order, mirroring scriptedSender/LoadFakeSender.
func newCapturingSender(t *testing.T, bodies ...string) *capturingSender {
	t.Helper()
	s := &capturingSender{}
	for i, b := range bodies {
		var msg anthropic.Message
		if err := json.Unmarshal([]byte(b), &msg); err != nil {
			t.Fatalf("capturingSender: unmarshal scripted body %d: %v", i, err)
		}
		s.turns = append(s.turns, &msg)
	}
	return s
}

// New records params, then returns the next scripted message.
//
// It honours ctx BEFORE recording, for the same reason fakeSender.New does
// (see faketurns.go) and one more besides: a real HTTP sender never issues the
// request at all on a cancelled context, so an aborted iteration must leave no
// trace in captured. That is what lets a test assert "the aborted turn made no
// further API call" by counting captured requests.
func (s *capturingSender) New(ctx context.Context, params anthropic.MessageNewParams) (*anthropic.Message, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.captured = append(s.captured, params)
	if s.next >= len(s.turns) {
		return nil, errors.New("agent: capturingSender: scripted turns exhausted")
	}
	msg := s.turns[s.next]
	s.next++
	return msg, nil
}

// callCount reports how many requests this sender actually served. A request
// that never went out (cancelled context) is not counted — see New.
func (s *capturingSender) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.captured)
}

// lastParams returns the params of the most recent New call, failing the
// test if New was never called.
func (s *capturingSender) lastParams(t *testing.T) anthropic.MessageNewParams {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.captured) == 0 {
		t.Fatal("capturingSender: New was never called")
	}
	return s.captured[len(s.captured)-1]
}

// assertToolResultFollowsToolUse asserts that msgs (a captured request's
// Messages) contains an assistant message with a tool_use block matching
// toolUseID, IMMEDIATELY followed by a user message carrying a tool_result
// block for that same id. This is the exact shape the real Anthropic API
// validates and rejects a request for lacking — the invariant RepairOrphans
// exists to restore.
func assertToolResultFollowsToolUse(t *testing.T, msgs []anthropic.MessageParam, toolUseID string) {
	t.Helper()
	for i, m := range msgs {
		if m.Role != anthropic.MessageParamRoleAssistant {
			continue
		}
		var hasToolUse bool
		for _, block := range m.Content {
			if tu := block.OfToolUse; tu != nil && tu.ID == toolUseID {
				hasToolUse = true
				break
			}
		}
		if !hasToolUse {
			continue
		}
		if i+1 >= len(msgs) {
			t.Fatalf("request has %d messages; the assistant message with tool_use %q is the last one — no follow-up tool_result",
				len(msgs), toolUseID)
		}
		next := msgs[i+1]
		if next.Role != anthropic.MessageParamRoleUser {
			t.Fatalf("message after tool_use %q has role %v, want user", toolUseID, next.Role)
		}
		for _, block := range next.Content {
			if tr := block.OfToolResult; tr != nil && tr.ToolUseID == toolUseID {
				return
			}
		}
		t.Fatalf("message after tool_use %q carries no matching tool_result block; content: %+v", toolUseID, next.Content)
	}
	t.Fatalf("request has no assistant message with a tool_use block id %q", toolUseID)
}

// testConversation builds a store-less (in-memory) rafiki conversation driven
// by a scripted sender — orphans_test.go's building block, mirroring
// newTestEngineWithSender's client wiring without the Engine/Frontend layer
// RepairOrphans doesn't touch.
func testConversation(t *testing.T, bodies ...string) *llm.Conversation {
	t.Helper()
	client, err := llm.NewClient(
		llm.WithUpstream(llm.UpstreamAnthropic, scriptedSender(t, bodies...)),
		llm.WithDefaultModel("claude-x"))
	if err != nil {
		t.Fatal(err)
	}
	conv, err := client.Conversation(context.Background(), llm.NewConversation("fundi", "agent"))
	if err != nil {
		t.Fatal(err)
	}
	return conv
}

func TestRepairOrphansOnEmptyConversation(t *testing.T) {
	conv := testConversation(t, sampleEndTurn)
	n, err := RepairOrphans(context.Background(), conv)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("RepairOrphans on an empty conversation synthesized %d results, want 0", n)
	}
}

func TestRepairOrphansNoOpWhenTrailingMessageIsUser(t *testing.T) {
	conv := testConversation(t, sampleEndTurn)
	ctx := context.Background()
	if err := conv.SeedHistory(ctx, []llm.Message{
		{Role: anthropic.MessageParamRoleUser, Content: llm.UserText("go")},
	}); err != nil {
		t.Fatal(err)
	}
	n, err := RepairOrphans(ctx, conv)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("RepairOrphans with a trailing user row synthesized %d results, want 0", n)
	}
}

func TestRepairOrphansNoOpWhenAllToolUseResolved(t *testing.T) {
	conv := testConversation(t, sampleEndTurn)
	ctx := context.Background()
	if err := conv.SeedHistory(ctx, []llm.Message{
		{Role: anthropic.MessageParamRoleUser, Content: llm.UserText("go")},
		{Role: anthropic.MessageParamRoleAssistant, Content: []anthropic.ContentBlockParamUnion{
			anthropic.NewToolUseBlock("tu_1", map[string]any{}, "bash"),
		}},
		{Role: anthropic.MessageParamRoleUser, Content: []anthropic.ContentBlockParamUnion{
			anthropic.NewToolResultBlock("tu_1", "file.txt", false),
		}},
	}); err != nil {
		t.Fatal(err)
	}
	n, err := RepairOrphans(ctx, conv)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("RepairOrphans with every tool_use already resolved synthesized %d results, want 0", n)
	}
	history, err := conv.History(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 3 {
		t.Fatalf("history grew to %d rows, want 3 (no-op must not append anything)", len(history))
	}
}

// TestRepairOrphansSynthesizesOnlyUnresolvedToolUse covers the partial-batch
// case: a trailing assistant message with two tool_use blocks where only one
// was resolved before the abort landed. RepairOrphans must synthesize a
// result for the unresolved id ONLY — a synthetic result for an already
// resolved id would itself violate the API shape (two tool_result blocks for
// one tool_use id).
func TestRepairOrphansSynthesizesOnlyUnresolvedToolUse(t *testing.T) {
	conv := testConversation(t, sampleEndTurn)
	ctx := context.Background()
	if err := conv.SeedHistory(ctx, []llm.Message{
		{Role: anthropic.MessageParamRoleUser, Content: llm.UserText("go")},
		{Role: anthropic.MessageParamRoleAssistant, Content: []anthropic.ContentBlockParamUnion{
			anthropic.NewToolUseBlock("tu_1", map[string]any{}, "bash"),
			anthropic.NewToolUseBlock("tu_2", map[string]any{}, "bash"),
		}},
		{Role: anthropic.MessageParamRoleUser, Content: []anthropic.ContentBlockParamUnion{
			anthropic.NewToolResultBlock("tu_1", "file.txt", false),
		}},
	}); err != nil {
		t.Fatal(err)
	}

	n, err := RepairOrphans(ctx, conv)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("RepairOrphans synthesized %d results, want 1 (only tu_2 is unresolved)", n)
	}

	history, err := conv.History(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 4 {
		t.Fatalf("history has %d rows, want 4 (one synthetic row appended)", len(history))
	}
	last := history[len(history)-1]
	if last.Param.Role != anthropic.MessageParamRoleUser {
		t.Fatalf("synthesized row role = %v, want user", last.Param.Role)
	}
	if len(last.Param.Content) != 1 {
		t.Fatalf("synthesized row has %d blocks, want 1 (tu_1 must not be re-synthesized)", len(last.Param.Content))
	}
	tr := last.Param.Content[0].OfToolResult
	if tr == nil || tr.ToolUseID != "tu_2" {
		t.Fatalf("synthesized block = %+v, want a tool_result for tu_2", last.Param.Content[0])
	}
	if !tr.IsError.Value {
		t.Fatal("synthesized tool_result is not marked IsError")
	}
}

// TestRepairOrphansRoundTrip is the brief's Step 1(a): seed a store-less
// conversation via a scripted sender that returns a tool_use response,
// "cancel before the tool completes" (i.e. never append a result for it —
// exactly the shape an aborted mid-tool-execution turn leaves behind), call
// RepairOrphans, then prove the API-shape invariant end to end by asserting
// on the OUTGOING request shape of the follow-up Continue: the assistant
// message carrying tool_use tu_1 must be immediately followed by a
// tool_result for tu_1. That is the shape the real Anthropic API validates;
// a scripted call merely succeeding proves nothing (see capturingSender's
// doc comment) since the fake transport can't reject a malformed request the
// way the real API would.
func TestRepairOrphansRoundTrip(t *testing.T) {
	sender := newCapturingSender(t, sampleResp, sampleEndTurn)
	client, err := llm.NewClient(
		llm.WithUpstream(llm.UpstreamAnthropic, sender),
		llm.WithDefaultModel("claude-x"))
	if err != nil {
		t.Fatal(err)
	}
	conv, err := client.Conversation(context.Background(), llm.NewConversation("fundi", "agent"))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	// sampleResp is a tool_use response (id tu_1); the tool it names never
	// runs, so its result is never appended — the orphan.
	if _, err := conv.Send(ctx, llm.UserText("go")); err != nil {
		t.Fatal(err)
	}

	n, err := RepairOrphans(ctx, conv)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("RepairOrphans synthesized %d results, want 1 (tu_1)", n)
	}

	history, err := conv.History(ctx)
	if err != nil {
		t.Fatal(err)
	}
	last := history[len(history)-1]
	if last.Param.Role != anthropic.MessageParamRoleUser {
		t.Fatalf("history's trailing row role = %v, want user", last.Param.Role)
	}
	tr := last.Param.Content[0].OfToolResult
	if tr == nil || tr.ToolUseID != "tu_1" {
		t.Fatalf("trailing row's block = %+v, want a tool_result for tu_1", last.Param.Content[0])
	}

	if _, err := conv.Continue(ctx); err != nil {
		t.Fatalf("Continue after RepairOrphans failed: %v", err)
	}

	// The real proof: assert on the shape of the request Continue actually
	// sent, not just that the scripted call returned successfully.
	assertToolResultFollowsToolUse(t, sender.lastParams(t).Messages, "tu_1")
}
