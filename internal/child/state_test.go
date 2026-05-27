package child_test

import (
	"fmt"
	"testing"

	"git.graveland.dev/brent/pi-controller/internal/child"
	"git.graveland.dev/brent/pi-controller/protocol"
)

func TestStateMachine_BasicLifecycle(t *testing.T) {
	sm := child.NewStateMachine()
	// Initial state assumed by callers: post-construction the SM sits in
	// "spawning". The supervise loop transitions to idle on first response.
	if sm.Current() != protocol.StatusSpawning {
		t.Fatalf("initial: %v", sm.Current())
	}

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
}

func TestStateMachine_ParallelTools(t *testing.T) {
	sm := child.NewStateMachine()
	sm.OnFirstResponse()
	sm.OnPiEvent("agent_start", nil)

	// Three tools start: state goes streaming→tool_running on first only.
	sm.OnPiEvent("tool_execution_start", nil)
	if sm.Current() != protocol.StatusToolRunning {
		t.Fatalf("first tool: %v", sm.Current())
	}
	sm.OnPiEvent("tool_execution_start", nil)
	sm.OnPiEvent("tool_execution_start", nil)
	if sm.Current() != protocol.StatusToolRunning {
		t.Fatalf("3rd tool started: %v", sm.Current())
	}

	// First two end: still tool_running.
	sm.OnPiEvent("tool_execution_end", nil)
	sm.OnPiEvent("tool_execution_end", nil)
	if sm.Current() != protocol.StatusToolRunning {
		t.Fatalf("2 of 3 ended: %v", sm.Current())
	}

	// Last end: back to streaming.
	changed, prev := sm.OnPiEvent("tool_execution_end", nil)
	if !changed || prev != protocol.StatusToolRunning || sm.Current() != protocol.StatusStreaming {
		t.Fatalf("all tools ended: %v→%v", prev, sm.Current())
	}
}

func TestStateMachine_ModalStack_Compaction(t *testing.T) {
	sm := child.NewStateMachine()
	sm.OnFirstResponse()
	sm.OnPiEvent("agent_start", nil)
	// streaming → compacting (push), then compaction_end → streaming (pop)
	sm.OnPiEvent("compaction_start", nil)
	if sm.Current() != protocol.StatusCompacting {
		t.Fatalf("compaction_start: %v", sm.Current())
	}
	sm.OnPiEvent("compaction_end", nil)
	if sm.Current() != protocol.StatusStreaming {
		t.Fatalf("compaction_end did not restore: %v", sm.Current())
	}
}

func TestStateMachine_DialogUI_Push_OnlyForDialogMethods(t *testing.T) {
	sm := child.NewStateMachine()
	sm.OnFirstResponse()
	sm.OnPiEvent("agent_start", nil)

	// fire-and-forget: no transition.
	sm.OnPiEvent("extension_ui_request", &child.PiUIRequestMeta{
		ID: "u1", Method: "notify",
	})
	if sm.Current() != protocol.StatusStreaming {
		t.Fatalf("notify must not block: %v", sm.Current())
	}

	// dialog: push.
	sm.OnPiEvent("extension_ui_request", &child.PiUIRequestMeta{
		ID: "u2", Method: "confirm",
	})
	if sm.Current() != protocol.StatusBlockedUI {
		t.Fatalf("confirm must block: %v", sm.Current())
	}

	// Response: pop.
	sm.OnExtensionUIResponse("u2")
	if sm.Current() != protocol.StatusStreaming {
		t.Fatalf("response did not pop: %v", sm.Current())
	}
}

func TestStateMachine_ExtensionError_Counter_NoTransition(t *testing.T) {
	sm := child.NewStateMachine()
	sm.OnFirstResponse()
	sm.OnPiEvent("agent_start", nil)
	before := sm.Current()
	sm.OnPiEvent("extension_error", nil)
	if sm.Current() != before {
		t.Fatalf("extension_error changed state: %v→%v", before, sm.Current())
	}
	if sm.Counters().ExtensionErrors != 1 {
		t.Fatalf("counter not incremented")
	}
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
	sm := child.NewStateMachine()
	sm.OnFirstResponse()
	sm.OnPiEvent("agent_start", nil)
	before := sm.Current()

	sm.OnAutoRetryStart("529 overloaded_error: Overloaded")

	if sm.Current() != before {
		t.Fatalf("auto_retry_start changed state: %v → %v", before, sm.Current())
	}
	c := sm.Counters()
	if c.AutoRetries != 1 {
		t.Fatalf("AutoRetries: got %d, want 1", c.AutoRetries)
	}
	if c.LastRetryError != "529 overloaded_error: Overloaded" {
		t.Fatalf("LastRetryError: got %q", c.LastRetryError)
	}
}

func TestStateMachine_MultipleConcurrentDialogs_ResolveCleanly(t *testing.T) {
	sm := child.NewStateMachine()
	sm.OnFirstResponse()
	sm.OnPiEvent("agent_start", nil)
	// Now in streaming.

	sm.OnPiEvent("extension_ui_request", &child.PiUIRequestMeta{
		ID: "u1", Method: "confirm",
	})
	if sm.Current() != protocol.StatusBlockedUI {
		t.Fatalf("after u1: %v", sm.Current())
	}

	// Second concurrent dialog while still in blocked_ui.
	sm.OnPiEvent("extension_ui_request", &child.PiUIRequestMeta{
		ID: "u2", Method: "confirm",
	})
	if sm.Current() != protocol.StatusBlockedUI {
		t.Fatalf("after u2: %v", sm.Current())
	}

	// Resolve in arbitrary order.
	sm.OnExtensionUIResponse("u2")
	if sm.Current() != protocol.StatusBlockedUI {
		t.Fatalf("after u2 resp (u1 still pending): %v", sm.Current())
	}
	sm.OnExtensionUIResponse("u1")
	// Now both resolved; should be back to streaming.
	if sm.Current() != protocol.StatusStreaming {
		t.Fatalf("after all resolved: %v, want streaming", sm.Current())
	}
}

func TestStateMachine_DialogOverCap_SilentlyDropped(t *testing.T) {
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
	if sm.Current() != protocol.StatusBlockedUI {
		t.Fatalf("after 64 dialogs: %v", sm.Current())
	}

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
	if sm.Current() != protocol.StatusStreaming {
		t.Fatalf("after resolving all tracked: %v, want streaming (overflow dialog must not have pushed)", sm.Current())
	}

	// The overflow response should be a no-op.
	sm.OnExtensionUIResponse("overflow")
	if sm.Current() != protocol.StatusStreaming {
		t.Fatalf("overflow response changed state: %v", sm.Current())
	}
}

func TestStateMachine_DefensivePopOnEmptyStack(t *testing.T) {
	// compaction_end with nothing on the stack must be a no-op.
	sm := child.NewStateMachine()
	sm.OnFirstResponse()
	before := sm.Current()
	sm.OnPiEvent("compaction_end", nil)
	if sm.Current() != before {
		t.Fatalf("defensive pop changed state")
	}
}
