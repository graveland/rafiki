package fundi

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"

	"go.graveland.dev/rafiki/pkg/llm"
	"go.graveland.dev/rafiki/pkg/llm/llmtest"
	"go.graveland.dev/rafiki/pkg/msg"
	msganthropic "go.graveland.dev/rafiki/pkg/msg/anthropic"

	"github.com/multigres/testkit/assert"
)

// capturingSender is a scripted llm.Sender (same replay-in-order contract as
// fakeSender/scriptedSender) that additionally records the msg.Request of every
// call it serves. It is the seam that lets a test assert on the OUTGOING
// REQUEST SHAPE rather than merely observing that a scripted call "succeeded" —
// fakeSender ignores the request entirely (see faketurns.go), so a successful
// Continue proves nothing about whether the request it sent was well-formed.
// The shape the real Anthropic API actually enforces — a tool_use block always
// followed by a matching tool_result — is exactly what
// assertToolResultFollowsToolUse checks against a captured request's Messages.
type capturingSender struct {
	mu       sync.Mutex
	next     int
	turns    []*msg.Response
	captured []*msg.Request
}

// newCapturingSender scripts bodies (raw Anthropic response JSON) to be
// replayed in call order, mirroring scriptedSender/LoadFakeSender.
func newCapturingSender(t *testing.T, bodies ...string) *capturingSender {
	t.Helper()
	s := &capturingSender{}
	for i, b := range bodies {
		resp, err := msganthropic.DecodeResponse([]byte(b))
		assert.NewAborting(t).NoError(err, "capturingSender: decode scripted body %d", i)
		s.turns = append(s.turns, resp)
	}
	return s
}

// Send records req, then returns the next scripted response.
//
// It honours ctx BEFORE recording, for the same reason fakeSender.Send does
// (see faketurns.go) and one more besides: a real HTTP sender never issues the
// request at all on a cancelled context, so an aborted iteration must leave no
// trace in captured. That is what lets a test assert "the aborted turn made no
// further API call" by counting captured requests.
func (s *capturingSender) Send(ctx context.Context, req *msg.Request, _ msg.EncodeOptions) (*msg.Response, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.captured = append(s.captured, req)
	if s.next >= len(s.turns) {
		return nil, errors.New("agent: capturingSender: scripted turns exhausted")
	}
	resp := s.turns[s.next]
	s.next++
	return resp, nil
}

func (s *capturingSender) Stream(ctx context.Context, req *msg.Request, opts msg.EncodeOptions) (msg.StreamDecoder, error) {
	resp, err := s.Send(ctx, req, opts)
	if err != nil {
		return nil, err
	}
	return llmtest.NewDecoder(llmtest.Events(resp)), nil
}

// callCount reports how many requests this sender actually served. A request
// that never went out (cancelled context) is not counted — see New.
func (s *capturingSender) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.captured)
}

// lastParams returns the request of the most recent call, failing the test if
// the sender was never called.
func (s *capturingSender) lastParams(t *testing.T) *msg.Request {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	assert.NewAborting(t).NotEmpty(s.captured, "capturingSender: never called")
	return s.captured[len(s.captured)-1]
}

// assertToolResultFollowsToolUse asserts that msgs (a captured request's
// Messages) contains an assistant message with a tool_use block matching
// toolUseID, IMMEDIATELY followed by a user message carrying a tool_result
// block for that same id. This is the exact shape the real Anthropic API
// validates and rejects a request for lacking — the invariant RepairOrphans
// exists to restore.
func assertToolResultFollowsToolUse(t *testing.T, msgs []msg.Message, toolUseID string) {
	t.Helper()
	c := assert.NewAborting(t)
	for i, m := range msgs {
		if m.Role != msg.Assistant {
			continue
		}
		var hasToolUse bool
		for _, block := range m.Content {
			if tu, ok := block.(msg.ToolUse); ok && tu.ID == toolUseID {
				hasToolUse = true
				break
			}
		}
		if !hasToolUse {
			continue
		}
		c.Less(len(msgs), i+1, "request has %d messages; the assistant message with tool_use %q is the last one — no follow-up tool_result", len(msgs), toolUseID)
		next := msgs[i+1]
		c.Eq(msg.User, next.Role, "message after tool_use %q has role %v, want user", toolUseID, next.Role)
		for _, block := range next.Content {
			if tr, ok := block.(msg.ToolResult); ok && tr.ToolUseID == toolUseID {
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
	c := assert.NewAborting(t)
	client, err := llm.NewClient(
		llm.WithProviderSender("anthropic", scriptedSender(t, bodies...)),
		llm.WithDefaultModel("claude-x"))
	c.NoError(err)
	conv, err := client.Conversation(context.Background(), llm.NewConversation("", "agent"))
	c.NoError(err)
	return conv
}

func TestRepairOrphansOnEmptyConversation(t *testing.T) {
	c := assert.NewAborting(t)
	conv := testConversation(t, sampleEndTurn)
	n, err := RepairOrphans(context.Background(), conv)
	c.NoError(err)
	c.Eq(0, n, "RepairOrphans on an empty conversation synthesized")
}

func TestRepairOrphansNoOpWhenTrailingMessageIsUser(t *testing.T) {
	c := assert.NewAborting(t)
	conv := testConversation(t, sampleEndTurn)
	ctx := context.Background()
	c.NoError(conv.SeedHistory(ctx, []msg.Message{
		{Role: msg.User, Content: llm.UserText("go")},
	}))
	n, err := RepairOrphans(ctx, conv)
	c.NoError(err)
	c.Eq(0, n, "RepairOrphans with a trailing user row synthesized")
}

func TestRepairOrphansNoOpWhenAllToolUseResolved(t *testing.T) {
	c := assert.NewAborting(t)
	conv := testConversation(t, sampleEndTurn)
	ctx := context.Background()
	c.NoError(conv.SeedHistory(ctx, []msg.Message{
		{Role: msg.User, Content: llm.UserText("go")},
		{Role: msg.Assistant, Content: []msg.Block{
			msg.ToolUse{ID: "tu_1", Name: "bash", Input: json.RawMessage(`{}`)},
		}},
		{Role: msg.User, Content: []msg.Block{
			msg.ToolResult{ToolUseID: "tu_1", Content: []msg.Block{msg.Text{Text: "file.txt"}}},
		}},
	}))
	n, err := RepairOrphans(ctx, conv)
	c.NoError(err)
	c.Eq(0, n, "RepairOrphans with every tool_use already resolved synthesized")
	history, err := conv.History(ctx)
	c.NoError(err)
	c.Len(history, 3, "history grew to %d rows, want 3 (no-op must not append anything)", len(history))
}

// TestRepairOrphansSynthesizesOnlyUnresolvedToolUse covers the partial-batch
// case: a trailing assistant message with two tool_use blocks where only one
// was resolved before the abort landed. RepairOrphans must synthesize a
// result for the unresolved id ONLY — a synthetic result for an already
// resolved id would itself violate the API shape (two tool_result blocks for
// one tool_use id).
func TestRepairOrphansSynthesizesOnlyUnresolvedToolUse(t *testing.T) {
	c := assert.NewAborting(t)
	conv := testConversation(t, sampleEndTurn)
	ctx := context.Background()
	c.NoError(conv.SeedHistory(ctx, []msg.Message{
		{Role: msg.User, Content: llm.UserText("go")},
		{Role: msg.Assistant, Content: []msg.Block{
			msg.ToolUse{ID: "tu_1", Name: "bash", Input: json.RawMessage(`{}`)},
			msg.ToolUse{ID: "tu_2", Name: "bash", Input: json.RawMessage(`{}`)},
		}},
		{Role: msg.User, Content: []msg.Block{
			msg.ToolResult{ToolUseID: "tu_1", Content: []msg.Block{msg.Text{Text: "file.txt"}}},
		}},
	}))

	n, err := RepairOrphans(ctx, conv)
	c.NoError(err)
	c.Eq(1, n, "RepairOrphans synthesized")

	history, err := conv.History(ctx)
	c.NoError(err)
	c.Len(history, 4, "history has %d rows, want 4 (one synthetic row appended)", len(history))
	last := history[len(history)-1]
	c.Eq(msg.User, last.Msg.Role, "synthesized row role")
	c.Len(last.Msg.Content, 1, "synthesized row has %d blocks, want 1 (tu_1 must not be re-synthesized)", len(last.Msg.Content))
	tr, ok := nodeToolResult(last)
	if !ok || tr.ToolUseID != "tu_2" {
		t.Fatalf("synthesized block = %+v, want a tool_result for tu_2", last.Msg.Content[0])
	}
	c.True(tr.IsError, "synthesized tool_result is not marked IsError")
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
	c := assert.NewAborting(t)
	sender := newCapturingSender(t, sampleResp, sampleEndTurn)
	client, err := llm.NewClient(
		llm.WithProviderSender("anthropic", sender),
		llm.WithDefaultModel("claude-x"))
	c.NoError(err)
	conv, err := client.Conversation(context.Background(), llm.NewConversation("", "agent"))
	c.NoError(err)
	ctx := context.Background()

	// sampleResp is a tool_use response (id tu_1); the tool it names never
	// runs, so its result is never appended — the orphan.
	if _, err := conv.Send(ctx, llm.UserText("go")); err != nil {
		t.Fatal(err)
	}

	n, err := RepairOrphans(ctx, conv)
	c.NoError(err)
	c.Eq(1, n, "RepairOrphans synthesized")

	history, err := conv.History(ctx)
	c.NoError(err)
	last := history[len(history)-1]
	c.Eq(msg.User, last.Msg.Role, "history's trailing row role")
	tr, ok := nodeToolResult(last)
	if !ok || tr.ToolUseID != "tu_1" {
		t.Fatalf("trailing row's block = %+v, want a tool_result for tu_1", last.Msg.Content[0])
	}

	if _, err := conv.Continue(ctx); err != nil {
		t.Fatalf("Continue after RepairOrphans failed: %v", err)
	}

	// The real proof: assert on the shape of the request Continue actually
	// sent, not just that the scripted call returned successfully.
	assertToolResultFollowsToolUse(t, sender.lastParams(t).Messages, "tu_1")
}
