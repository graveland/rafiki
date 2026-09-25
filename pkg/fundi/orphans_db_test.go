package fundi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/jackc/pgx/v5/pgxpool"

	"go.graveland.dev/rafiki/pkg/agentloop"
	"go.graveland.dev/rafiki/pkg/llm"
	"go.graveland.dev/rafiki/pkg/store"
)

// dbTestPool mirrors rafiki's own scratch-database pattern (see
// llm/conversation_test.go's convTestPool and agentloop/agentloop_test.go's
// testPool): connect an admin pool from RAFIKI_TEST_DSN, CREATE DATABASE a
// uniquely-named scratch db, register cleanup to drop it, reconnect scoped to
// that database, and migrate it. Never touches the DSN's own database.
//
// It also returns the scratch database's own DSN, kept for callers that need
// a connection string rather than an already-open pool (e.g. to open a second,
// independent pool against the same scratch database).
func dbTestPool(t *testing.T) (*pgxpool.Pool, string) {
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

	scratchDSN, err := withDatabase(dsn, name)
	if err != nil {
		t.Fatalf("build scratch db dsn: %v", err)
	}

	pool, err := pgxpool.New(ctx, scratchDSN)
	if err != nil {
		t.Fatalf("connect scratch db %s: %v", name, err)
	}
	t.Cleanup(pool.Close)

	if err := store.Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate scratch db %s: %v", name, err)
	}
	return pool, scratchDSN
}

// withDatabase returns dsn (a postgres:// URL) with its path replaced by
// dbName, so callers that need a connection STRING rather than an
// already-open pool can target the same scratch database dbTestPool just
// created.
func withDatabase(dsn, dbName string) (string, error) {
	u, err := url.Parse(dsn)
	if err != nil {
		return "", fmt.Errorf("parse dsn: %w", err)
	}
	u.Path = "/" + dbName
	return u.String(), nil
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
	pool, _ := dbTestPool(t)
	background := context.Background()

	// sampleResp (tool_use, id tu_1, tool "bash") drives the first Continue;
	// sampleEndTurn drives the follow-up Continue after repair. capturingSender
	// records every request's params so the post-repair assertion can check
	// the actual shape sent, not just that the call returned successfully.
	sender := newCapturingSender(t, sampleResp, sampleEndTurn)
	client, err := llm.NewClient(
		llm.WithProviderSender("anthropic", sender),
		llm.WithStore(pool),
		llm.WithDefaultModel("claude-x"),
	)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	conv, err := client.Conversation(background, llm.NewConversation("", "agent"))
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

// TestRepairOrphansSkipsPrefillIDs pins the boot-repair half of the
// pre-fill/boot-repair race fix: an interrupted pre-fill persists r0+r1 and
// dies before r2, and the restarted process's BuildEngine runs RepairOrphans
// against exactly that history. The unresolved tool_use ids are prefill_
// prefixed — completing them is runPrefill's prefillHasR1 job (re-execute
// r1's inputs, seed r2) — so repair must return 0 and append nothing. A
// non-prefill orphan in the same shape is still repaired, unchanged.
func TestRepairOrphansSkipsPrefillIDs(t *testing.T) {
	pool, _ := dbTestPool(t)
	ctx := context.Background()

	t.Run("prefill ids left untouched", func(t *testing.T) {
		ref := "repair-skips-prefill-ids"
		seedClient := prefillDBSeedClient(t, pool)
		conv, err := seedClient.Conversation(ctx, llm.Entrypoint("agent"), llm.ByExternalRef(ref))
		if err != nil {
			t.Fatalf("seed conversation: %v", err)
		}
		// r0+r1 with no r2: the state a dead process leaves mid-prefill,
		// and what boot-time RepairOrphans sees in BuildEngine.
		if err := conv.SeedHistory(ctx, prefillSeedRows("/tmp/a.txt")[:2]); err != nil {
			t.Fatalf("seed history: %v", err)
		}

		n, err := RepairOrphans(ctx, conv)
		if err != nil {
			t.Fatalf("RepairOrphans: %v", err)
		}
		if n != 0 {
			t.Fatalf("RepairOrphans synthesized %d results for prefill_ ids, want 0 — "+
				"completing an interrupted pre-fill is runPrefill's job, not the repair's", n)
		}
		hist, err := conv.History(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(hist) != 2 {
			t.Fatalf("history has %d rows, want 2 — repair must not append for a prefill_ id", len(hist))
		}
	})

	t.Run("non-prefill orphan still repaired", func(t *testing.T) {
		ref := "repair-still-fixes-real-orphan"
		seedClient := prefillDBSeedClient(t, pool)
		conv, err := seedClient.Conversation(ctx, llm.Entrypoint("agent"), llm.ByExternalRef(ref))
		if err != nil {
			t.Fatalf("seed conversation: %v", err)
		}
		// The same r0+r1-no-r2 shape, but the tool_use id is a real model id
		// — an ordinary orphan, which repair must still fabricate a result for.
		r0 := prefillSeedRows("/tmp/a.txt")[0]
		r1 := anthropic.MessageParam{
			Role: anthropic.MessageParamRoleAssistant,
			Content: []anthropic.ContentBlockParamUnion{
				anthropic.NewToolUseBlock("toolu_01ABC",
					json.RawMessage(`{"path":"/tmp/a.txt","offset":1,"limit":1000000}`), "read"),
			},
		}
		if err := conv.SeedHistory(ctx, []llm.Message{r0, r1}); err != nil {
			t.Fatalf("seed history: %v", err)
		}

		n, err := RepairOrphans(ctx, conv)
		if err != nil {
			t.Fatalf("RepairOrphans: %v", err)
		}
		if n != 1 {
			t.Fatalf("RepairOrphans synthesized %d results, want 1 (the non-prefill orphan)", n)
		}
		hist, err := conv.History(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(hist) != 3 {
			t.Fatalf("history has %d rows, want 3 (one synthetic row appended)", len(hist))
		}
		last := hist[2]
		if last.Param.Role != anthropic.MessageParamRoleUser {
			t.Fatalf("trailing row role = %v, want user", last.Param.Role)
		}
		tr := last.Param.Content[0].OfToolResult
		if tr == nil || tr.ToolUseID != "toolu_01ABC" {
			t.Fatalf("trailing block = %+v, want a tool_result for toolu_01ABC", last.Param.Content[0])
		}
		if !tr.IsError.Value || len(tr.Content) != 1 || tr.Content[0].OfText == nil ||
			tr.Content[0].OfText.Text != "Tool execution aborted by user." {
			t.Fatalf("synthesized result = %+v, want the standard abort text marked IsError", tr)
		}
	})
}
