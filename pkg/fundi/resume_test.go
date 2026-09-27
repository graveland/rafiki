package fundi

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
	"go.graveland.dev/rafiki/pkg/providers"

	"github.com/multigres/testkit/assert"
)

// TestResumeBootTimeOrphanRepair is Task 15's Requirement 1: a
// DB-backed conversation reattached via the same external ref across a
// process restart (the daemon's resume path re-execs the agent with the same
// RAFIKI_CHILD_ID, and --ref defaults to it) can carry a dangling
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
// SAME database with the SAME ref - simulating the daemon's resume re-exec -
// which must reattach to the very same conversation and run the boot-time
// repair before returning.
func TestResumeBootTimeOrphanRepair(t *testing.T) {
	c := assert.NewAborting(t)
	silenceSlog(t)
	pool, _ := dbTestPool(t)
	ctx := context.Background()
	const ref = "resume-test-ref"

	// --- process 1: create a genuine dangling tool_use, bypassing Engine ---
	sender1 := newCapturingSender(t, sampleResp)
	client1, err := llm.NewClient(
		llm.WithProviderSender("anthropic", sender1),
		llm.WithStore(pool),
		llm.WithDefaultModel("claude-x"),
	)
	c.NoError(err, "NewClient (process 1)")
	conv1, err := client1.Conversation(ctx, llm.Entrypoint("agent"), llm.ByExternalRef(ref))
	c.NoError(err, "Conversation (process 1)")

	turnCtx, cancel := context.WithCancel(ctx)
	defer cancel() // no-op once the tool has already cancelled; guards early-return paths
	tools := cancelOnExecuteTools{cancel: cancel}
	_, runErr := agentloop.Run(turnCtx, conv1, tools, nil, llm.UserText("go"))
	c.Error(runErr, "agentloop.Run (process 1) succeeded, want an error from the cancelled-context persist "+
		"(the tool cancels turnCtx before the tool_result gets persisted)")

	// Prove the premise: process 1 genuinely left a dangling tool_use, not
	// one artificially seeded by the test.
	before, err := conv1.History(ctx)
	c.NoError(err, "History (pre-repair)")
	c.Len(before, 2, "pre-repair history has %d rows, want 2 (user + assistant tool_use); rows", len(before))
	assistant := before[1]
	c.Eq(anthropic.MessageParamRoleAssistant, assistant.Param.Role, "row 1 role")
	c.False(len(assistant.ToolUseIDs) != 1 || assistant.ToolUseIDs[0] != "tu_1", "assistant.ToolUseIDs = %v, want [tu_1] (the orphan never formed)", assistant.ToolUseIDs)
	t.Log("confirmed: process 1 left a genuine dangling tool_use (tu_1), unrepaired")

	// --- process 2: BuildEngine again with the same ref against the same
	// database, simulating the daemon's resume re-exec ---
	fe := NewFrontend(strings.NewReader(""), &syncBuffer{}, nil)
	cfg := Config{
		Model:     "anthropic/claude-x",
		Ref:       ref,
		Pool:      pool,
		FakeTurns: writeFakeTurns(t, sampleEndTurn),
		Tools:     fakeToolSet{},
		Providers: providers.Default(),
	}
	eng2, shutdown2, err := cfg.BuildEngine(ctx, fe)
	c.NoError(err, "BuildEngine (process 2)")
	defer shutdown2()
	defer eng2.Close()

	c.Eq(conv1.ID, eng2.conv.ID, "process 2 conversation id")

	// The boot-time repair must have run synchronously inside BuildEngine,
	// before it returned to us - assert directly on the persisted history.
	after, err := eng2.conv.History(ctx)
	c.NoError(err, "History (post-repair)")
	c.Len(after, 3, "post-repair history has %d rows, want 3 (user, assistant, synthetic result); rows", len(after))
	repairRow := after[2]
	c.Eq(anthropic.MessageParamRoleUser, repairRow.Param.Role, "repair row role")
	c.Len(repairRow.Param.Content, 1, "repair row has %d blocks, want 1", len(repairRow.Param.Content))
	tr := repairRow.Param.Content[0].OfToolResult
	if tr == nil || tr.ToolUseID != "tu_1" {
		t.Fatalf("repair row block = %+v, want a tool_result for tu_1", repairRow.Param.Content[0])
	}
	c.True(tr.IsError.Value, "synthesized tool_result is not marked IsError")

	// Per correction (B): a scripted Continue succeeding proves nothing - the
	// fake sender ignores request contents and llm.Continue does no
	// client-side tool_use/tool_result cross-check (see capturingSender's doc
	// comment in orphans_test.go). Assert on the ACTUAL OUTGOING REQUEST
	// SHAPE instead, via an independent capturing sender reattached to the
	// same persisted conversation (same store, same ref) - it sees exactly
	// what any real client would send next.
	sender3 := newCapturingSender(t, sampleEndTurn)
	client3, err := llm.NewClient(
		llm.WithProviderSender("anthropic", sender3),
		llm.WithStore(pool),
		llm.WithDefaultModel("claude-x"),
	)
	c.NoError(err, "NewClient (process 3, capturing)")
	conv3, err := client3.Conversation(ctx, llm.Entrypoint("agent"), llm.ByExternalRef(ref))
	c.NoError(err, "Conversation (process 3, capturing)")
	c.Eq(conv1.ID, conv3.ID, "process 3 conversation id")
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
	c := assert.NewAborting(t)
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
		Providers: providers.Default(),
	}
	eng, shutdown, err := cfg.BuildEngine(ctx, fe)
	c.NoError(err, "BuildEngine")
	defer shutdown()

	if strings.HasPrefix(eng.conv.ID, "mem-") {
		t.Fatalf("conversation id = %q, want a real DB-backed id, not the in-memory placeholder", eng.conv.ID)
	}

	runDone := make(chan error, 1)
	go func() { runDone <- fe.Run() }()

	writeFrame(t, inW, map[string]any{"type": "get_state", "id": "1"})
	c.NoError(inW.Close())
	select {
	case runErr := <-runDone:
		c.NoError(runErr, "Frontend.Run")
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
	c.False(len(lines) == 0 || lines[0] == "", "Frontend wrote no frames; expected a get_state response")
	c.NoError(json.Unmarshal([]byte(lines[0]), &resp), "parse get_state response %q", lines[0])
	c.Eq(eng.conv.ID, resp.Data.SessionID, "get_state sessionId")
}
