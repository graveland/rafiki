package execpool

import (
	"fmt"
	"log/slog"
	"sync/atomic"
)

// inFlightWarnAt is where the in-flight warning fires: below the stream
// limit, so hitting it is visible before calls start waiting.
const inFlightWarnAt = 200

// inFlight counts calls open on one executor connection and warns once
// per upward crossing of inFlightWarnAt.
//
// It measures the long-lived and tool RPCs that can pile up on the single
// connection — Execute, StartJob, JobOutput, KillJob, Ping and the workspace
// project/skill calls. The short-lived Health, Provision and Release RPCs are
// deliberately not counted: they complete promptly and their contribution to
// stream pressure is negligible.
type inFlight struct {
	n      atomic.Int64
	warned atomic.Bool
	id     string // executor id, for the log line
}

// enter counts a call in; the returned func counts it out. Call it as
// `defer f.enter()()`.
func (f *inFlight) enter() func() {
	if f == nil {
		return func() {}
	}
	v := f.n.Add(1)
	if v >= inFlightWarnAt && f.warned.CompareAndSwap(false, true) {
		slog.Warn(fmt.Sprintf("executor: in-flight calls past the warning threshold; the stream limit is %d", maxExecutorStreams),
			"executor", f.id, "inFlight", v, "warnAt", inFlightWarnAt)
	}
	return func() {
		v := f.n.Add(-1)
		if v < inFlightWarnAt/2 {
			// Re-armed with hysteresis: only falling well below the threshold
			// clears the flag, so a level hovering at the warn point cannot
			// flood the log.
			f.warned.Store(false)
		}
	}
}
