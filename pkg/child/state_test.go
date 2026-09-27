package child_test

import (
	"fmt"
	"testing"

	"go.graveland.dev/rafiki/pkg/child"
	"go.graveland.dev/rafiki/pkg/protocol"

	"github.com/multigres/testkit/assert"
)

func TestStateMachine_BasicLifecycle(t *testing.T) {
	sm := child.NewStateMachine()
	// Initial state assumed by callers: post-construction the SM sits in
	// "spawning". The supervise loop transitions to idle on first response.
	assert.NewAborting(t).Eq(protocol.StatusSpawning, sm.Current(), "initial")

	// First response → idle.
	changed, prev := sm.OnFirstResponse()
	if !changed || prev != protocol.StatusSpawning || sm.Current() != protocol.StatusIdle {
		t.Fatalf("first response: changed=%v prev=%v cur=%v", changed, prev, sm.Current())
	}

	// agent_start → streaming.
	changed, prev = sm.OnPiEvent("agent_start", nil)
	if !changed || prev != protocol.StatusIdle || sm.Current() != protocol.StatusStreaming {
		t.Fatalf("agent_start: %v %v", prev, sm.Current())
	}

	// agent_end → idle.
	changed, _ = sm.OnPiEvent("agent_end", nil)
	if !changed || sm.Current() != protocol.StatusIdle {
		t.Fatalf("agent_end: %v", sm.Current())
	}

	// agent_settled after agent_end is a no-op (already idle).
	changed, _ = sm.OnPiEvent("agent_settled", nil)
	if changed || sm.Current() != protocol.StatusIdle {
		t.Fatalf("agent_settled after agent_end: changed=%v cur=%v", changed, sm.Current())
	}

	// agent_settled alone also idles a streaming agent (pi ≥0.80.x true-idle
	// event; agent_end may be skipped observationally under retry races).
	sm.OnPiEvent("agent_start", nil)
	changed, _ = sm.OnPiEvent("agent_settled", nil)
	if !changed || sm.Current() != protocol.StatusIdle {
		t.Fatalf("agent_settled: %v", sm.Current())
	}
}

func TestStateMachine_ParallelTools(t *testing.T) {
	c := assert.NewAborting(t)
	sm := child.NewStateMachine()
	sm.OnFirstResponse()
	sm.OnPiEvent("agent_start", nil)

	// Three tools start: state goes streaming→tool_running on first only.
	sm.OnPiEvent("tool_execution_start", nil)
	c.Eq(protocol.StatusToolRunning, sm.Current(), "first tool")
	sm.OnPiEvent("tool_execution_start", nil)
	sm.OnPiEvent("tool_execution_start", nil)
	c.Eq(protocol.StatusToolRunning, sm.Current(), "3rd tool started")

	// First two end: still tool_running.
	sm.OnPiEvent("tool_execution_end", nil)
	sm.OnPiEvent("tool_execution_end", nil)
	c.Eq(protocol.StatusToolRunning, sm.Current(), "2 of 3 ended")

	// Last end: back to streaming.
	changed, prev := sm.OnPiEvent("tool_execution_end", nil)
	if !changed || prev != protocol.StatusToolRunning || sm.Current() != protocol.StatusStreaming {
		t.Fatalf("all tools ended: %v→%v", prev, sm.Current())
	}
}

func TestStateMachine_ModalStack_Compaction(t *testing.T) {
	c := assert.NewAborting(t)
	sm := child.NewStateMachine()
	sm.OnFirstResponse()
	sm.OnPiEvent("agent_start", nil)
	// streaming → compacting (push), then compaction_end → streaming (pop)
	sm.OnPiEvent("compaction_start", nil)
	c.Eq(protocol.StatusCompacting, sm.Current(), "compaction_start")
	sm.OnPiEvent("compaction_end", nil)
	c.Eq(protocol.StatusStreaming, sm.Current(), "compaction_end did not restore")
}

// TestStateMachineBatchWaitPushPop pins the batch_wait modal pair: the
// parked state is pushed on batch_wait_start and popped back to the
// previous (streaming) state on batch_wait_end, exactly the way compaction
// is. Deleting either switch case in OnPiEvent fails this test.
func TestStateMachineBatchWaitPushPop(t *testing.T) {
	sm := child.NewStateMachine()
	sm.OnFirstResponse()
	sm.OnPiEvent("agent_start", nil)

	changed, _ := sm.OnPiEvent("batch_wait_start", nil)
	if !changed || sm.Current() != protocol.StatusBatchWait {
		t.Fatalf("batch_wait_start: changed=%v current=%v, want push to batch_wait", changed, sm.Current())
	}

	changed, prev := sm.OnPiEvent("batch_wait_end", nil)
	if !changed || prev != protocol.StatusBatchWait || sm.Current() != protocol.StatusStreaming {
		t.Fatalf("batch_wait_end: changed=%v prev=%v current=%v, want pop back to streaming", changed, prev, sm.Current())
	}
}

func TestStateMachine_DialogUI_Push_OnlyForDialogMethods(t *testing.T) {
	c := assert.NewAborting(t)
	sm := child.NewStateMachine()
	sm.OnFirstResponse()
	sm.OnPiEvent("agent_start", nil)

	// fire-and-forget: no transition.
	sm.OnPiEvent("extension_ui_request", &child.PiUIRequestMeta{
		ID: "u1", Method: "notify",
	})
	c.Eq(protocol.StatusStreaming, sm.Current(), "notify must not block")

	// dialog: push.
	sm.OnPiEvent("extension_ui_request", &child.PiUIRequestMeta{
		ID: "u2", Method: "confirm",
	})
	c.Eq(protocol.StatusBlockedUI, sm.Current(), "confirm must block")

	// Response: pop.
	sm.OnExtensionUIResponse("u2")
	c.Eq(protocol.StatusStreaming, sm.Current(), "response did not pop")
}

func TestStateMachine_ExtensionError_Counter_NoTransition(t *testing.T) {
	c := assert.NewAborting(t)
	sm := child.NewStateMachine()
	sm.OnFirstResponse()
	sm.OnPiEvent("agent_start", nil)
	before := sm.Current()
	sm.OnPiEvent("extension_error", nil)
	c.Eq(before, sm.Current(), "extension_error changed state")
	c.Eq(1, sm.Counters().ExtensionErrors, "counter not incremented")
}

func TestStateMachine_ShuttingDown(t *testing.T) {
	sm := child.NewStateMachine()
	sm.OnFirstResponse()
	sm.OnPiEvent("agent_start", nil)
	changed, prev := sm.OnShutdownStart()
	if !changed || prev != protocol.StatusStreaming || sm.Current() != protocol.StatusShuttingDown {
		t.Fatalf("shutdown start: %v→%v", prev, sm.Current())
	}
	changed, _ = sm.OnProcessExit()
	if !changed || sm.Current() != protocol.StatusExited {
		t.Fatalf("process exit: %v", sm.Current())
	}
}

func TestStateMachine_AutoRetryStart_SetsCountersAndError(t *testing.T) {
	ck := assert.NewAborting(t)
	sm := child.NewStateMachine()
	sm.OnFirstResponse()
	sm.OnPiEvent("agent_start", nil)
	before := sm.Current()

	sm.OnAutoRetryStart("529 overloaded_error: Overloaded")

	ck.Eq(before, sm.Current(), "auto_retry_start changed state")
	c := sm.Counters()
	ck.Eq(1, c.AutoRetries, "AutoRetries: got")
	ck.Eq("529 overloaded_error: Overloaded", c.LastRetryError, "LastRetryError: got")
}

func TestStateMachine_MultipleConcurrentDialogs_ResolveCleanly(t *testing.T) {
	c := assert.NewAborting(t)
	sm := child.NewStateMachine()
	sm.OnFirstResponse()
	sm.OnPiEvent("agent_start", nil)
	// Now in streaming.

	sm.OnPiEvent("extension_ui_request", &child.PiUIRequestMeta{
		ID: "u1", Method: "confirm",
	})
	c.Eq(protocol.StatusBlockedUI, sm.Current(), "after u1")

	// Second concurrent dialog while still in blocked_ui.
	sm.OnPiEvent("extension_ui_request", &child.PiUIRequestMeta{
		ID: "u2", Method: "confirm",
	})
	c.Eq(protocol.StatusBlockedUI, sm.Current(), "after u2")

	// Resolve in arbitrary order.
	sm.OnExtensionUIResponse("u2")
	c.Eq(protocol.StatusBlockedUI, sm.Current(), "after u2 resp (u1 still pending)")
	sm.OnExtensionUIResponse("u1")
	// Now both resolved; should be back to streaming.
	c.Eq(protocol.StatusStreaming, sm.Current(), "after all resolved")
}

func TestStateMachine_DialogOverCap_SilentlyDropped(t *testing.T) {
	c := assert.NewAborting(t)
	sm := child.NewStateMachine()
	sm.OnFirstResponse()
	sm.OnPiEvent("agent_start", nil)

	// Fill the pendingUI cap with 64 dialog requests.
	for i := 0; i < 64; i++ {
		sm.OnPiEvent("extension_ui_request", &child.PiUIRequestMeta{
			ID:     fmt.Sprintf("u%d", i),
			Method: "confirm",
		})
	}
	c.Eq(protocol.StatusBlockedUI, sm.Current(), "after 64 dialogs")

	// The 65th request — over cap. Should NOT push.
	// We can't directly observe "didn't push", but we can verify by
	// resolving the 64 tracked dialogs and confirming we return to streaming
	// (rather than getting stuck in blocked_ui with an orphan push).
	sm.OnPiEvent("extension_ui_request", &child.PiUIRequestMeta{
		ID: "overflow", Method: "confirm",
	})

	for i := 0; i < 64; i++ {
		sm.OnExtensionUIResponse(fmt.Sprintf("u%d", i))
	}
	// After resolving all 64 tracked, should be back to streaming.
	// If the overflow had pushed, we'd be stuck in blocked_ui.
	c.Eq(protocol.StatusStreaming, sm.Current(), "after resolving all tracked")

	// The overflow response should be a no-op.
	sm.OnExtensionUIResponse("overflow")
	c.Eq(protocol.StatusStreaming, sm.Current(), "overflow response changed state")
}

func TestStateMachine_DefensivePopOnEmptyStack(t *testing.T) {
	// compaction_end with nothing on the stack must be a no-op.
	sm := child.NewStateMachine()
	sm.OnFirstResponse()
	before := sm.Current()
	sm.OnPiEvent("compaction_end", nil)
	assert.NewAborting(t).Eq(before, sm.Current(), "defensive pop changed state")
}
