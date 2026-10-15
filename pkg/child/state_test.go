package child_test

import (
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
	changed, prev = sm.OnPiEvent("agent_start")
	if !changed || prev != protocol.StatusIdle || sm.Current() != protocol.StatusStreaming {
		t.Fatalf("agent_start: %v %v", prev, sm.Current())
	}

	// agent_end → idle.
	changed, _ = sm.OnPiEvent("agent_end")
	if !changed || sm.Current() != protocol.StatusIdle {
		t.Fatalf("agent_end: %v", sm.Current())
	}

	// agent_settled after agent_end is a no-op (already idle).
	changed, _ = sm.OnPiEvent("agent_settled")
	if changed || sm.Current() != protocol.StatusIdle {
		t.Fatalf("agent_settled after agent_end: changed=%v cur=%v", changed, sm.Current())
	}

	// agent_settled alone also idles a streaming agent (pi ≥0.80.x true-idle
	// event; agent_end may be skipped observationally under retry races).
	sm.OnPiEvent("agent_start")
	changed, _ = sm.OnPiEvent("agent_settled")
	if !changed || sm.Current() != protocol.StatusIdle {
		t.Fatalf("agent_settled: %v", sm.Current())
	}
}

func TestStateMachine_ParallelTools(t *testing.T) {
	c := assert.NewAborting(t)
	sm := child.NewStateMachine()
	sm.OnFirstResponse()
	sm.OnPiEvent("agent_start")

	// Three tools start: state goes streaming→tool_running on first only.
	sm.OnPiEvent("tool_execution_start")
	c.Eq(protocol.StatusToolRunning, sm.Current(), "first tool")
	sm.OnPiEvent("tool_execution_start")
	sm.OnPiEvent("tool_execution_start")
	c.Eq(protocol.StatusToolRunning, sm.Current(), "3rd tool started")

	// First two end: still tool_running.
	sm.OnPiEvent("tool_execution_end")
	sm.OnPiEvent("tool_execution_end")
	c.Eq(protocol.StatusToolRunning, sm.Current(), "2 of 3 ended")

	// Last end: back to streaming.
	changed, prev := sm.OnPiEvent("tool_execution_end")
	if !changed || prev != protocol.StatusToolRunning || sm.Current() != protocol.StatusStreaming {
		t.Fatalf("all tools ended: %v→%v", prev, sm.Current())
	}
}

func TestStateMachine_ModalStack_Compaction(t *testing.T) {
	c := assert.NewAborting(t)
	sm := child.NewStateMachine()
	sm.OnFirstResponse()
	sm.OnPiEvent("agent_start")
	// streaming → compacting (push), then compaction_end → streaming (pop)
	sm.OnPiEvent("compaction_start")
	c.Eq(protocol.StatusCompacting, sm.Current(), "compaction_start")
	sm.OnPiEvent("compaction_end")
	c.Eq(protocol.StatusStreaming, sm.Current(), "compaction_end did not restore")
}

// TestStateMachineBatchWaitPushPop pins the batch_wait modal pair: the
// parked state is pushed on batch_wait_start and popped back to the
// previous (streaming) state on batch_wait_end, exactly the way compaction
// is. Deleting either switch case in OnPiEvent fails this test.
func TestStateMachineBatchWaitPushPop(t *testing.T) {
	sm := child.NewStateMachine()
	sm.OnFirstResponse()
	sm.OnPiEvent("agent_start")

	changed, _ := sm.OnPiEvent("batch_wait_start")
	if !changed || sm.Current() != protocol.StatusBatchWait {
		t.Fatalf("batch_wait_start: changed=%v current=%v, want push to batch_wait", changed, sm.Current())
	}

	changed, prev := sm.OnPiEvent("batch_wait_end")
	if !changed || prev != protocol.StatusBatchWait || sm.Current() != protocol.StatusStreaming {
		t.Fatalf("batch_wait_end: changed=%v prev=%v current=%v, want pop back to streaming", changed, prev, sm.Current())
	}
}

func TestStateMachine_ExtensionError_Counter_NoTransition(t *testing.T) {
	c := assert.NewAborting(t)
	sm := child.NewStateMachine()
	sm.OnFirstResponse()
	sm.OnPiEvent("agent_start")
	before := sm.Current()
	sm.OnPiEvent("extension_error")
	c.Eq(before, sm.Current(), "extension_error changed state")
	c.Eq(1, sm.Counters().ExtensionErrors, "counter not incremented")
}

func TestStateMachine_ShuttingDown(t *testing.T) {
	sm := child.NewStateMachine()
	sm.OnFirstResponse()
	sm.OnPiEvent("agent_start")
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
	sm.OnPiEvent("agent_start")
	before := sm.Current()

	sm.OnAutoRetryStart("529 overloaded_error: Overloaded")

	ck.Eq(before, sm.Current(), "auto_retry_start changed state")
	c := sm.Counters()
	ck.Eq(1, c.AutoRetries, "AutoRetries: got")
	ck.Eq("529 overloaded_error: Overloaded", c.LastRetryError, "LastRetryError: got")
}

func TestStateMachine_DefensivePopOnEmptyStack(t *testing.T) {
	// compaction_end with nothing on the stack must be a no-op.
	sm := child.NewStateMachine()
	sm.OnFirstResponse()
	before := sm.Current()
	sm.OnPiEvent("compaction_end")
	assert.NewAborting(t).Eq(before, sm.Current(), "defensive pop changed state")
}
