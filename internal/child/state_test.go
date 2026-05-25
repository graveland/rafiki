package child_test

import (
	"testing"

	"graveland.dev/pi-controller/internal/child"
	"graveland.dev/pi-controller/internal/protocol"
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
