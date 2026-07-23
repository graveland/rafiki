package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/jackc/pgx/v5/pgxpool"

	"git.graveland.dev/brent/rafiki/agentloop"
	"git.graveland.dev/brent/rafiki/llm"
	"git.graveland.dev/brent/rafiki/store"
)

// dbTestPool mirrors rafiki's own scratch-database pattern (see
// llm/conversation_test.go's convTestPool and agentloop/agentloop_test.go's
// testPool): connect an admin pool from RAFIKI_TEST_DSN, CREATE DATABASE a
// uniquely-named scratch db, register cleanup to drop it, reconnect scoped to
// that database, and migrate it. Never touches the DSN's own database.
func dbTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("RAFIKI_TEST_DSN")
	if dsn == "" {
		if os.Getenv("RAFIKI_REQUIRE_DB") != "" {
			t.Fatal("RAFIKI_TEST_DSN not set but RAFIKI_REQUIRE_DB is — the integration job must provide it")
		}
		t.Skip("RAFIKI_TEST_DSN not set; skipping integration test")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect admin pool: %v", err)
	}
	t.Cleanup(admin.Close)

	name := fmt.Sprintf("fundi_agent_orphans_%d", time.Now().UnixNano())
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatalf("create scratch db %s: %v", name, err)
	}
	t.Cleanup(func() {
		if _, err := admin.Exec(context.Background(), "DROP DATABASE "+name+" WITH (FORCE)"); err != nil {
			t.Logf("drop scratch db %s: %v", name, err)
		}
	})

	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse DSN: %v", err)
	}
	cfg.ConnConfig.Database = name
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("connect scratch db %s: %v", name, err)
	}
	t.Cleanup(pool.Close)

	if err := store.Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate scratch db %s: %v", name, err)
	}
	return pool
}

// cancelOnExecuteTools is an agentloop.ToolSet with one tool ("bash",
// matching sampleResp's tool_use block) that cancels the turn's own context
// the moment it runs, then returns a normal (non-error) result. This is the
// mechanism for producing a GENUINE orphaned tool_use against a DB-backed
// conversation: agentloop.drive persists the tool's result via
// conv.AppendUser(ctx, ...) using that same outer ctx, so by the time the
// persist runs, pgx sees an already-cancelled context and the write fails —
// leaving the just-persisted assistant tool_use message with no matching
// tool_result. A store-less conversation can't exhibit this: its
// loadHistory/appendMessage path ignores ctx entirely (see package doc in
// orphans.go and the DESIGN note this test exists to cover).
type cancelOnExecuteTools struct {
	cancel context.CancelFunc
}

func (cancelOnExecuteTools) Definitions() []anthropic.ToolUnionParam {
	return []anthropic.ToolUnionParam{{OfTool: &anthropic.ToolParam{
		Name:        "bash",
		InputSchema: anthropic.ToolInputSchemaParam{Type: "object"},
	}}}
}

func (c cancelOnExecuteTools) Execute(_ context.Context, name string, _ json.RawMessage) (string, error) {
	c.cancel()
	return "ok", nil
}

// TestRepairOrphansDBBackedGenuineOrphan closes the gap every other orphan
// test leaves: it drives a DB-backed conversation (pgx honors ctx, unlike the
// store-less path) through a real agentloop turn whose tool cancels the
// turn's own context mid-execution, so the subsequent persist of the tool's
// result genuinely fails and leaves a real dangling tool_use — not one
// pre-seeded by the test. It then proves RepairOrphans fixes the API-shape
// invariant by asserting on the OUTGOING request shape of the follow-up
// Continue, not merely that the scripted call succeeded (see
// capturingSender's doc comment in orphans_test.go — the fake transport has
// no capacity to reject a malformed request the way the real API would).
func TestRepairOrphansDBBackedGenuineOrphan(t *testing.T) {
	pool := dbTestPool(t)
	background := context.Background()

	// sampleResp (tool_use, id tu_1, tool "bash") drives the first Continue;
	// sampleEndTurn drives the follow-up Continue after repair. capturingSender
	// records every request's params so the post-repair assertion can check
	// the actual shape sent, not just that the call returned successfully.
	sender := newCapturingSender(t, sampleResp, sampleEndTurn)
	client, err := llm.NewClient(
		llm.WithUpstream(llm.UpstreamAnthropic, sender),
		llm.WithStore(pool),
		llm.WithDefaultModel("claude-x"),
	)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	conv, err := client.Conversation(background, llm.NewConversation("fundi", "agent"))
	if err != nil {
		t.Fatalf("Conversation: %v", err)
	}

	turnCtx, cancel := context.WithCancel(background)
	defer cancel() // no-op once the tool has already cancelled; guards early-return paths
	tools := cancelOnExecuteTools{cancel: cancel}

	_, runErr := agentloop.Run(turnCtx, conv, tools, nil, llm.UserText("go"))
	if runErr == nil {
		t.Fatal("agentloop.Run succeeded, want an error from the cancelled-context persist " +
			"(the tool cancels turnCtx before the tool_result gets persisted)")
	}
	t.Logf("agentloop.Run failed as expected: %v", runErr)

	// Prove the premise: the pre-repair history genuinely has a dangling
	// tool_use, not an artificially seeded one. Use a fresh, uncancelled
	// context to read it back.
	before, err := conv.History(background)
	if err != nil {
		t.Fatalf("History (pre-repair): %v", err)
	}
	if len(before) != 2 {
		t.Fatalf("pre-repair history has %d rows, want 2 (user + assistant tool_use); rows: %+v",
			len(before), before)
	}
	assistant := before[1]
	if assistant.Param.Role != anthropic.MessageParamRoleAssistant {
		t.Fatalf("row 1 role = %v, want assistant", assistant.Param.Role)
	}
	var sawToolUse bool
	for _, block := range assistant.Param.Content {
		if tu := block.OfToolUse; tu != nil {
			sawToolUse = true
			if tu.ID != "tu_1" {
				t.Fatalf("assistant tool_use id = %q, want tu_1", tu.ID)
			}
		}
	}
	if !sawToolUse {
		t.Fatal("assistant message has no tool_use block; the orphan never formed")
	}
	if len(assistant.ToolUseIDs) != 1 || assistant.ToolUseIDs[0] != "tu_1" {
		t.Fatalf("assistant.ToolUseIDs = %v, want [tu_1]", assistant.ToolUseIDs)
	}
	// No trailing row at all means tu_1 has no tool_result following it —
	// exactly the shape the real Anthropic API rejects on the next request.
	t.Log("confirmed: pre-repair history ends on an unresolved assistant tool_use (genuine orphan)")

	n, err := RepairOrphans(background, conv)
	if err != nil {
		t.Fatalf("RepairOrphans: %v", err)
	}
	if n != 1 {
		t.Fatalf("RepairOrphans synthesized %d results, want 1 (tu_1)", n)
	}

	after, err := conv.History(background)
	if err != nil {
		t.Fatalf("History (post-repair): %v", err)
	}
	if len(after) != 3 {
		t.Fatalf("post-repair history has %d rows, want 3 (user, assistant, synthetic result); rows: %+v",
			len(after), after)
	}
	repairRow := after[2]
	if repairRow.Param.Role != anthropic.MessageParamRoleUser {
		t.Fatalf("repair row role = %v, want user", repairRow.Param.Role)
	}
	if len(repairRow.Param.Content) != 1 {
		t.Fatalf("repair row has %d blocks, want 1", len(repairRow.Param.Content))
	}
	tr := repairRow.Param.Content[0].OfToolResult
	if tr == nil || tr.ToolUseID != "tu_1" {
		t.Fatalf("repair row block = %+v, want a tool_result for tu_1", repairRow.Param.Content[0])
	}
	if !tr.IsError.Value {
		t.Fatal("synthesized tool_result is not marked IsError")
	}

	resp, err := conv.Continue(background)
	if err != nil {
		t.Fatalf("Continue after RepairOrphans failed: %v", err)
	}
	if resp.StopReason != "end_turn" {
		t.Fatalf("post-repair Continue stop_reason = %q, want end_turn", resp.StopReason)
	}

	// The real proof of the API-shape invariant: assert on the shape of the
	// request this Continue actually sent. Without repair, the assistant
	// message carrying tu_1 would have no follow-up tool_result — exactly
	// what the real Anthropic API rejects. With repair applied, it does.
	assertToolResultFollowsToolUse(t, sender.lastParams(t).Messages, "tu_1")
}
