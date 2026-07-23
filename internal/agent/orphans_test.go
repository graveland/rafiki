package agent

import (
	"context"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"

	"git.graveland.dev/brent/rafiki/llm"
)

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
// RepairOrphans, then prove the API-shape invariant end to end: a follow-up
// Continue that would previously have sent a malformed request (a tool_use
// with no matching tool_result) now succeeds.
func TestRepairOrphansRoundTrip(t *testing.T) {
	conv := testConversation(t, sampleResp, sampleEndTurn)
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

	// The real proof: without repair this Continue would carry a dangling
	// tool_use and the (real) API would reject it. Our scripted sender can't
	// enforce that itself, so what we're actually proving is that the
	// conversation's stored shape is once again request-ready and the
	// scripted end_turn call is reachable — the request that repair unblocks.
	if _, err := conv.Continue(ctx); err != nil {
		t.Fatalf("Continue after RepairOrphans failed: %v", err)
	}
}
