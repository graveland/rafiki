package protocol

// DisplayState is the operator-facing state of a child: one derived value
// answering "what is this child doing right now", independent of the
// recovery-oriented Status. It is computed from displayFacts by the daemon
// and carried on ChildSummary and the operator_state event.
//
// The zero value DisplayState("") is invalid: it means "no display state was
// computed" and must never be rendered as a state. computeDisplayState always
// returns one of the five constants below, and a consumer that cannot derive
// one must omit the field rather than emit the empty string.
type DisplayState string

const (
	// DisplayWorking means a turn is in flight (or the child is mid-startup /
	// mid-shutdown): the child is doing something, no operator attention is
	// implied.
	DisplayWorking DisplayState = "working"
	// DisplayWaiting means the child is idle but blocked on something; WaitingOn
	// names what.
	DisplayWaiting DisplayState = "waiting"
	// DisplayFailed means the child's last turn ended in an error.
	DisplayFailed DisplayState = "failed"
	// DisplayIdle means the child is idle with nothing outstanding.
	DisplayIdle DisplayState = "idle"
	// DisplayExited means the child's process has exited.
	DisplayExited DisplayState = "exited"
)

// WaitingOn names what an idle child is blocked on, when its DisplayState is
// DisplayWaiting. It is meaningless for any other state.
//
// The zero value WaitingOn("") means "not waiting"; displayMessage and every
// consumer must treat it as absent and never render it. A waiting child always
// carries a non-empty WaitingOn.
type WaitingOn string

const (
	// WaitingOnOperator means a top-level child is blocked on the human: it has
	// an outstanding question or has halted over budget.
	WaitingOnOperator WaitingOn = "operator"
	// WaitingOnSchedule means the child has an open schedule due to fire it.
	WaitingOnSchedule WaitingOn = "schedule"
	// WaitingOnJob means the child has a background job still running.
	WaitingOnJob WaitingOn = "job"
	// WaitingOnChildren means the child has live descendants.
	WaitingOnChildren WaitingOn = "children"
	// WaitingOnEvents means untriaged news is queued in the child's event buffer.
	WaitingOnEvents WaitingOn = "events"
)
