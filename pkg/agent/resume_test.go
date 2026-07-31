package agent

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/anthropics/anthropic-sdk-go"

	"go.graveland.dev/rafiki/pkg/agentloop"
	"go.graveland.dev/rafiki/pkg/llm"
)

// TestResumeBootTimeOrphanRepair is Task 15's Requirement 1: a
// DB-backed conversation reattached via the same external ref across a
// process restart (ctrl_resume re-execs the agent with the same
// PI_CONTROLLER_CHILD_ID, and --ref defaults to it) can carry a dangling
// tool_use left by a PREVIOUS process that crashed or was killed mid-turn.
// Config.BuildEngine must repair it once, at boot, before the reattached
// engine ever executes a turn - distinct from runTurn's own abort-path
// repair (engine.go), which only ever cleans up a turn cancelled WITHIN
// this process.
//
// "Process 1" builds its dangling orphan the same way
// TestRepairOrphansDBBackedGenuineOrphan does (orphans_db_test.go):
// cancelOnExecuteTools cancels the turn's own context from inside the tool,
// so the tool_result persist itself fails against pgx's ctx-aware store,
// genuinely leaving the trailing assistant message's tool_use unresolved.
// It deliberately does NOT go through Engine at all - Engine's own runTurn
// cancelled-branch would repair the orphan itself, inside this same
// process, proving nothing about the boot-time path this test exists to
// cover. A process that is killed outright never runs its own abort
// handling either, so bypassing Engine is the more faithful crash
// simulation, not a shortcut.
//
// "Process 2" is a second, independent Config.BuildEngine call against the
// SAME database with the SAME ref - simulating ctrl_resume's re-exec -
// which must reattach to the very same conversation and run the boot-time
// repair before returning.
func TestResumeBootTimeOrphanRepair(t *testing.T) {
	silenceSlog(t)
	pool, _ := dbTestPool(t)
	ctx := context.Background()
	const ref = "resume-test-ref"

	// --- process 1: create a genuine dangling tool_use, bypassing Engine ---
	sender1 := newCapturingSender(t, sampleResp)
	client1, err := llm.NewClient(
		llm.WithUpstream(llm.UpstreamAnthropic, sender1),
		llm.WithStore(pool),
		llm.WithDefaultModel("claude-x"),
	)
	if err != nil {
		t.Fatalf("NewClient (process 1): %v", err)
	}
	conv1, err := client1.Conversation(ctx, llm.Entrypoint("agent"), llm.ByExternalRef(ref))
	if err != nil {
		t.Fatalf("Conversation (process 1): %v", err)
	}

	turnCtx, cancel := context.WithCancel(ctx)
	defer cancel() // no-op once the tool has already cancelled; guards early-return paths
	tools := cancelOnExecuteTools{cancel: cancel}
	if _, runErr := agentloop.Run(turnCtx, conv1, tools, nil, llm.UserText("go")); runErr == nil {
		t.Fatal("agentloop.Run (process 1) succeeded, want an error from the cancelled-context persist " +
			"(the tool cancels turnCtx before the tool_result gets persisted)")
	}

	// Prove the premise: process 1 genuinely left a dangling tool_use, not
	// one artificially seeded by the test.
	before, err := conv1.History(ctx)
	if err != nil {
		t.Fatalf("History (pre-repair): %v", err)
	}
	if len(before) != 2 {
		t.Fatalf("pre-repair history has %d rows, want 2 (user + assistant tool_use); rows: %+v", len(before), before)
	}
	assistant := before[1]
	if assistant.Param.Role != anthropic.MessageParamRoleAssistant {
		t.Fatalf("row 1 role = %v, want assistant", assistant.Param.Role)
	}
	if len(assistant.ToolUseIDs) != 1 || assistant.ToolUseIDs[0] != "tu_1" {
		t.Fatalf("assistant.ToolUseIDs = %v, want [tu_1] (the orphan never formed)", assistant.ToolUseIDs)
	}
	t.Log("confirmed: process 1 left a genuine dangling tool_use (tu_1), unrepaired")

	// --- process 2: BuildEngine again with the same ref against the same
	// database, simulating ctrl_resume's re-exec ---
	fe := NewFrontend(strings.NewReader(""), &syncBuffer{}, nil)
	cfg := Config{
		Model:     "anthropic/claude-x",
		Ref:       ref,
		Pool:      pool,
		FakeTurns: writeFakeTurns(t, sampleEndTurn),
		Tools:     fakeToolSet{},
	}
	eng2, shutdown2, err := cfg.BuildEngine(ctx, fe)
	if err != nil {
		t.Fatalf("BuildEngine (process 2): %v", err)
	}
	defer shutdown2()
	defer eng2.Close()

	if eng2.conv.ID != conv1.ID {
		t.Fatalf("process 2 conversation id = %s, want it to reattach to process 1's conversation %s", eng2.conv.ID, conv1.ID)
	}

	// The boot-time repair must have run synchronously inside BuildEngine,
	// before it returned to us - assert directly on the persisted history.
	after, err := eng2.conv.History(ctx)
	if err != nil {
		t.Fatalf("History (post-repair): %v", err)
	}
	if len(after) != 3 {
		t.Fatalf("post-repair history has %d rows, want 3 (user, assistant, synthetic result); rows: %+v", len(after), after)
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

	// Per correction (B): a scripted Continue succeeding proves nothing - the
	// fake sender ignores request contents and llm.Continue does no
	// client-side tool_use/tool_result cross-check (see capturingSender's doc
	// comment in orphans_test.go). Assert on the ACTUAL OUTGOING REQUEST
	// SHAPE instead, via an independent capturing sender reattached to the
	// same persisted conversation (same store, same ref) - it sees exactly
	// what any real client would send next.
	sender3 := newCapturingSender(t, sampleEndTurn)
	client3, err := llm.NewClient(
		llm.WithUpstream(llm.UpstreamAnthropic, sender3),
		llm.WithStore(pool),
		llm.WithDefaultModel("claude-x"),
	)
	if err != nil {
		t.Fatalf("NewClient (process 3, capturing): %v", err)
	}
	conv3, err := client3.Conversation(ctx, llm.Entrypoint("agent"), llm.ByExternalRef(ref))
	if err != nil {
		t.Fatalf("Conversation (process 3, capturing): %v", err)
	}
	if conv3.ID != conv1.ID {
		t.Fatalf("process 3 conversation id = %s, want it to reattach to %s too", conv3.ID, conv1.ID)
	}
	if _, err := conv3.Continue(ctx); err != nil {
		t.Fatalf("Continue after boot-time repair failed: %v", err)
	}
	assertToolResultFollowsToolUse(t, sender3.lastParams(t).Messages, "tu_1")
}

// TestResumeReportsConversationIDAsSessionID is Requirement 2:
// NewEngine already threads conv.ID into StateData.SessionID (engine.go),
// and Frontend's get_state handler reports it verbatim as the "sessionId"
// field the daemon sniffs and persists (frontend.go). This test pins that
// down specifically for DB mode, where conv.ID must be the real persisted
// conversation UUID - not the in-memory "mem-..." placeholder llm.Client
// mints when there is no store - since a later resume can only find the
// conversation again via a real id.
func TestResumeReportsConversationIDAsSessionID(t *testing.T) {
	silenceSlog(t)
	pool, _ := dbTestPool(t)
	ctx := context.Background()

	inR, inW := io.Pipe()
	out := &syncBuffer{}
	fe := NewFrontend(inR, out, nil)
	cfg := Config{
		Model:     "anthropic/claude-x",
		Ref:       "resume-test-get-state",
		Pool:      pool,
		FakeTurns: writeFakeTurns(t, sampleEndTurn),
		Tools:     fakeToolSet{},
	}
	eng, shutdown, err := cfg.BuildEngine(ctx, fe)
	if err != nil {
		t.Fatalf("BuildEngine: %v", err)
	}
	defer shutdown()

	if strings.HasPrefix(eng.conv.ID, "mem-") {
		t.Fatalf("conversation id = %q, want a real DB-backed id, not the in-memory placeholder", eng.conv.ID)
	}

	runDone := make(chan error, 1)
	go func() { runDone <- fe.Run() }()

	writeFrame(t, inW, map[string]any{"type": "get_state", "id": "1"})
	if err := inW.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case runErr := <-runDone:
		if runErr != nil {
			t.Fatalf("Frontend.Run: %v", runErr)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Frontend.Run did not return after stdin closed")
	}

	eng.Wait()
	eng.Close()

	var resp struct {
		Data struct {
			SessionID string `json:"sessionId"`
		} `json:"data"`
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) == 0 || lines[0] == "" {
		t.Fatal("Frontend wrote no frames; expected a get_state response")
	}
	if err := json.Unmarshal([]byte(lines[0]), &resp); err != nil {
		t.Fatalf("parse get_state response %q: %v", lines[0], err)
	}
	if resp.Data.SessionID != eng.conv.ID {
		t.Fatalf("get_state sessionId = %q, want %q (the DB-backed conversation id)", resp.Data.SessionID, eng.conv.ID)
	}
}
