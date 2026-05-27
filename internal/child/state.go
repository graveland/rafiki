// Package child implements per-child state for pi child processes. The state
// machine and metadata fields are not internally synchronized; callers must
// hold the owning Child's c.metaMu before invoking any StateMachine method or
// reading the metadata fields (handleFrame and Status/Metadata both do this).
package child

import "graveland.dev/pi-controller/protocol"

// PiUIRequestMeta carries the minimum fields needed from an
// extension_ui_request event to make the dialog/fire-and-forget decision.
type PiUIRequestMeta struct {
	ID     string
	Method string
}

// Counters holds informational statistics updated by pi events.
// None of the counter updates cause a state transition.
type Counters struct {
	ExtensionErrors int
	AutoRetries     int
	LastRetryError  string
	LastRetryFinal  string
}

// dialogMethods are the extension_ui methods that block the agent loop and
// therefore push the current state onto the modal stack.
var dialogMethods = map[string]bool{
	"select":  true,
	"confirm": true,
	"input":   true,
	"editor":  true,
}

// pendingUICapacity is the maximum number of in-flight dialog request IDs
// that the state machine tracks. Entries beyond this cap are silently dropped;
// the cap prevents unbounded memory growth from a misbehaving extension.
const pendingUICapacity = 64

// StateMachine tracks the status of one pi child process and enforces the
// transition rules defined in §10 of the pi-controller protocol spec.
//
// Callers must serialize all method calls by holding the owning Child's
// c.metaMu before invoking any SM method (this is what handleFrame and
// Status() do). The SM does not provide internal synchronization.
type StateMachine struct {
	current     protocol.Status
	stack       []protocol.Status // modal stack for compacting / blocked_ui
	activeTools int               // outstanding tool_execution_start calls
	counters    Counters
	pendingUI   map[string]struct{} // dialog request IDs awaiting a response
}

// NewStateMachine creates a fresh state machine in StatusSpawning.
//
// Lifecycle note: when a child is resumed via ctrl_resume (per protocol
// §10.1 row 18), the supervise loop creates a new StateMachine — the
// state machine is intentionally not designed to transition out of
// StatusExited. This keeps the lifetime of an SM instance tied 1:1 to
// the lifetime of an underlying pi process, which simplifies the modal
// stack and counter invariants.
func NewStateMachine() *StateMachine {
	return &StateMachine{
		current:   protocol.StatusSpawning,
		pendingUI: make(map[string]struct{}),
	}
}

// Current returns the current status.
func (sm *StateMachine) Current() protocol.Status {
	return sm.current
}

// Counters returns a copy of the informational counters.
func (sm *StateMachine) Counters() Counters {
	return sm.counters
}

// OnFirstResponse transitions from spawning to idle when pi's first response
// arrives on stdout. This is a separate method (rather than an OnPiEvent case)
// because the trigger is process I/O, not a pi-RPC event type.
func (sm *StateMachine) OnFirstResponse() (changed bool, prev protocol.Status) {
	if sm.current == protocol.StatusSpawning {
		prev = sm.current
		sm.current = protocol.StatusIdle
		return true, prev
	}
	return false, sm.current
}

// OnPiEvent processes a pi-RPC event. eventType is the "type" field from the
// event JSON. meta must be non-nil only for extension_ui_request events where
// the state machine needs payload data to classify the request.
func (sm *StateMachine) OnPiEvent(eventType string, meta *PiUIRequestMeta) (changed bool, prev protocol.Status) {
	prev = sm.current
	switch eventType {
	case "agent_start":
		sm.transition(protocol.StatusStreaming)

	case "agent_end":
		// Defensively reset the tool counter in case any tool_execution_end
		// events were missed — this prevents the counter from leaking across turns.
		sm.activeTools = 0
		sm.transition(protocol.StatusIdle)

	case "tool_execution_start":
		sm.activeTools++
		// Only the first tool causes a transition; subsequent increments while
		// already in tool_running are counted but do not re-transition.
		if sm.activeTools == 1 && sm.current == protocol.StatusStreaming {
			sm.transition(protocol.StatusToolRunning)
		}

	case "tool_execution_end":
		if sm.activeTools > 0 {
			sm.activeTools--
		}
		if sm.activeTools == 0 && sm.current == protocol.StatusToolRunning {
			sm.transition(protocol.StatusStreaming)
		}

	case "compaction_start":
		sm.push(protocol.StatusCompacting)

	case "compaction_end":
		sm.pop()

	case "extension_ui_request":
		if meta != nil && dialogMethods[meta.Method] {
			// Over-cap dialog requests are silently dropped: not tracked,
			// not pushed. Pi will eventually time out the dialog.
			if len(sm.pendingUI) >= pendingUICapacity {
				break
			}
			sm.pendingUI[meta.ID] = struct{}{}
			// Only push once when entering blocked_ui — additional concurrent
			// dialogs increment the pending set but don't stack-push.
			// The corresponding pop happens only when pendingUI empties.
			if sm.current != protocol.StatusBlockedUI {
				sm.push(protocol.StatusBlockedUI)
			}
		}
		// Fire-and-forget methods (notify, setStatus, etc.) are intentionally
		// not handled here — they do not cause a state transition.

	case "extension_error":
		sm.counters.ExtensionErrors++
	}

	return sm.current != prev, prev
}

// OnExtensionUIResponse is called when the controller forwards a UI response
// back to the child. If this was the last pending dialog request, the
// blocked_ui state is popped off the stack.
func (sm *StateMachine) OnExtensionUIResponse(id string) (changed bool, prev protocol.Status) {
	if _, ok := sm.pendingUI[id]; !ok {
		return false, sm.current
	}
	delete(sm.pendingUI, id)
	if sm.current == protocol.StatusBlockedUI && len(sm.pendingUI) == 0 {
		prev = sm.current
		sm.pop()
		return sm.current != prev, prev
	}
	return false, sm.current
}

// OnAutoRetryStart records the start of an auto-retry attempt. This does
// not change the state — the agent loop is still "streaming" while
// waiting on the retry timer. Informational only.
func (sm *StateMachine) OnAutoRetryStart(errorMessage string) {
	sm.counters.AutoRetries++
	sm.counters.LastRetryError = errorMessage
}

// OnAutoRetryFinalFailure records the error string from a non-success
// auto_retry_end event. No transition occurs.
func (sm *StateMachine) OnAutoRetryFinalFailure(finalError string) {
	sm.counters.LastRetryFinal = finalError
}

// OnShutdownStart transitions to shutting_down. It clears the modal stack
// because shutdown overrides any in-progress compaction or UI block.
// Called by Child when it initiates a graceful shutdown (ctrl_kill or
// process-exit interception).
func (sm *StateMachine) OnShutdownStart() (changed bool, prev protocol.Status) {
	if sm.current == protocol.StatusShuttingDown || sm.current == protocol.StatusExited {
		return false, sm.current
	}
	prev = sm.current
	sm.current = protocol.StatusShuttingDown
	sm.stack = nil
	return true, prev
}

// OnProcessExit is the terminal transition. It always moves to exited unless
// the machine is already there.
func (sm *StateMachine) OnProcessExit() (changed bool, prev protocol.Status) {
	if sm.current == protocol.StatusExited {
		return false, sm.current
	}
	prev = sm.current
	sm.current = protocol.StatusExited
	sm.activeTools = 0
	sm.stack = nil
	return true, prev
}

// transition sets the current state directly. Used for non-modal transitions
// where there is no need to remember the prior state.
func (sm *StateMachine) transition(to protocol.Status) {
	sm.current = to
}

// push saves the current state on the modal stack and enters the new state.
func (sm *StateMachine) push(to protocol.Status) {
	sm.stack = append(sm.stack, sm.current)
	sm.current = to
}

// pop restores the state from the top of the modal stack.
// A pop on an empty stack is a no-op (defensive).
func (sm *StateMachine) pop() {
	if len(sm.stack) == 0 {
		return
	}
	sm.current = sm.stack[len(sm.stack)-1]
	sm.stack = sm.stack[:len(sm.stack)-1]
}
