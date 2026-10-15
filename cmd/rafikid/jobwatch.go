package main

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"go.graveland.dev/rafiki/pkg/execpool"
	"go.graveland.dev/rafiki/pkg/protocol"
)

// jobEventSource is the event-buffer source name for background-job news.
// One source per concern keeps a coordinator's injected frame readable: the
// buffer coalesces per (child, source), so job exits land in their own batch
// rather than interleaved with subagent settles or budget warnings.
const jobEventSource = "jobs"

// jobAckTimeout bounds one AckJobExits call. An ack is idempotent on the
// executor and an unacked exit is replayed on the next subscribe, so a lost
// ack costs a duplicate event, not a lost notification.
const jobAckTimeout = 5 * time.Second

// jobRef is what the daemon remembers about a job it believes is running: who
// started it (to route the news) and what command it ran (to name it in the
// gone fragment — a handle alone reads as nothing).
type jobRef struct {
	owner   string
	command string
}

// jobAckSeam delivers a job-exit acknowledgement to the executor that emitted
// it. It is the narrow interface the daemon needs from the executor pool for
// the ack half of the push contract — *execpool.Pool satisfies it — kept
// separate from executorPool so the ack is testable with a recording stub and
// does not bloat every fake pool with a method only this path calls.
type jobAckSeam interface {
	AckJobExits(ctx context.Context, executorID string, handles []string) error
}

// handleJobEvent is the daemon's job-event handler, wired once at startup via
// execPool.SetJobEventHandler. It runs on the pool's per-connection goroutine,
// so events for one executor arrive in order and are processed synchronously.
//
// It replaces the poll loop wholesale: the executor PUSHES running/exited
// events and a ReplayComplete marker, and the daemon keeps just enough state —
// which handles it believes are running — to tell a job that finished from one
// an executor restart lost. No timer, no JobOutput poll, no per-job goroutine.
//
// Replay phase is tracked explicitly because the same Running/Exited kinds
// arrive BOTH during a subscription's replay and live afterwards. Only events
// seen while replaying belong in the replay's seen-set; a live event that leaked
// in would make its handle look "seen" at the NEXT replay, and a job the
// executor had since lost would never be reported gone.
func (c *Controller) handleJobEvent(ev execpool.JobEvent) {
	switch ev.Kind {
	case execpool.JobEventReplayStart:
		// A fresh subscription is open and the executor is about to replay what
		// it already holds. Enter the replay phase and clear the seen-set: from
		// here until ReplayComplete, Running/Exited are replayed, not live.
		c.jobRunningMu.Lock()
		c.setJobReplayingLocked(ev.ExecutorID, true)
		delete(c.jobReplay, ev.ExecutorID)
		c.jobRunningMu.Unlock()

	case execpool.JobEventRunning:
		// A running job is recorded, and — only while replaying — marked as seen
		// THIS replay, so the ReplayComplete that closes it cannot call it gone.
		c.jobRunningMu.Lock()
		c.jobRunningForLocked(ev.ExecutorID)[ev.Handle] = jobRef{owner: ev.Owner, command: ev.Command}
		if c.isJobReplayingLocked(ev.ExecutorID) {
			c.jobReplayForLocked(ev.ExecutorID)[ev.Handle] = true
		}
		c.jobRunningMu.Unlock()

	case execpool.JobEventExited:
		c.jobRunningMu.Lock()
		if running := c.jobRunning[ev.ExecutorID]; running != nil {
			delete(running, ev.Handle)
		}
		if c.isJobReplayingLocked(ev.ExecutorID) {
			c.jobReplayForLocked(ev.ExecutorID)[ev.Handle] = true
		}
		c.jobRunningMu.Unlock()

		// Push BEFORE ack — the no-loss rule, never the reverse. A repeated
		// exit (the executor replayed an unacked one while our ack was in
		// flight) collapses on the (child, source, handle) key while the first
		// fragment is still pending, so the child sees it once.
		if c.jobDeliverable(ev.Owner) {
			c.evbuf.Push(ev.Owner, jobEventSource, ev.Handle, fmt.Sprintf(
				"background job %s (%s) finished with exit code %d. Read what it printed with bash_output {\"handle\":%q}.",
				ev.Handle, ev.Command, ev.ExitCode, ev.Handle))
		}
		c.ackJobExit(ev.ExecutorID, ev.Handle)

	case execpool.JobEventReplayComplete:
		// Every handle still believed running that was NOT seen during THIS
		// replay is gone: the executor that held it no longer knows it, and
		// neither its output nor its exit survives. The seen-set was filled only
		// by replayed events (see JobEventReplayStart), so a LIVE-started job
		// missing from this replay is correctly reported gone, while a handle
		// the executor just replayed is not. The first subscription after a
		// daemon start has an empty jobRunning, so nothing is reported gone.
		c.jobRunningMu.Lock()
		running := c.jobRunning[ev.ExecutorID]
		seen := c.jobReplay[ev.ExecutorID]
		type goneJob struct {
			handle string
			ref    jobRef
		}
		var gone []goneJob
		for handle, ref := range running {
			if seen[handle] {
				continue
			}
			gone = append(gone, goneJob{handle: handle, ref: ref})
			delete(running, handle)
		}
		delete(c.jobReplay, ev.ExecutorID)
		c.setJobReplayingLocked(ev.ExecutorID, false)
		c.jobRunningMu.Unlock()

		for _, g := range gone {
			if !c.jobDeliverable(g.ref.owner) {
				continue
			}
			c.evbuf.Push(g.ref.owner, jobEventSource, g.handle, fmt.Sprintf(
				"background job %s (%s) is no longer on its executor: neither what it printed nor "+
					"how it ended can be recovered. Re-run the command if you still need the result.",
				g.handle, g.ref.command))
		}
	}
}

// jobRunningForLocked returns the executor's running-jobs map, creating it if
// needed. Caller holds c.jobRunningMu.
func (c *Controller) jobRunningForLocked(executorID string) map[string]jobRef {
	if c.jobRunning == nil {
		c.jobRunning = make(map[string]map[string]jobRef)
	}
	m := c.jobRunning[executorID]
	if m == nil {
		m = make(map[string]jobRef)
		c.jobRunning[executorID] = m
	}
	return m
}

// jobReplayForLocked returns the executor's current-replay handle set,
// creating it if needed. Caller holds c.jobRunningMu.
func (c *Controller) jobReplayForLocked(executorID string) map[string]bool {
	if c.jobReplay == nil {
		c.jobReplay = make(map[string]map[string]bool)
	}
	s := c.jobReplay[executorID]
	if s == nil {
		s = make(map[string]bool)
		c.jobReplay[executorID] = s
	}
	return s
}

// isJobReplayingLocked reports whether executorID is currently inside a
// subscription replay (between a ReplayStart and its ReplayComplete). Caller
// holds c.jobRunningMu.
func (c *Controller) isJobReplayingLocked(executorID string) bool {
	return c.jobReplaying[executorID]
}

// setJobReplayingLocked enters or leaves the replay phase for executorID.
// Caller holds c.jobRunningMu.
func (c *Controller) setJobReplayingLocked(executorID string, on bool) {
	if c.jobReplaying == nil {
		c.jobReplaying = make(map[string]bool)
	}
	if on {
		c.jobReplaying[executorID] = true
		return
	}
	delete(c.jobReplaying, executorID)
}

// forgetRunningJob drops a handle from the running set for an executor. The
// agent's bash_kill resolved the job on purpose, so a later ReplayComplete must
// never report it gone — gone means "lost to an executor restart", not "you
// killed it". Nil-safe: a Controller with no executor plane has an empty map.
func (c *Controller) forgetRunningJob(executorID, handle string) {
	c.jobRunningMu.Lock()
	defer c.jobRunningMu.Unlock()
	if m := c.jobRunning[executorID]; m != nil {
		delete(m, handle)
	}
}

// jobDeliverable reports whether owner can still receive an injected frame:
// the same rule the settle path uses. A child that exited (its workspace — and
// its jobs — were released with it) or was closed ends its watches silently:
// there is nobody to tell.
func (c *Controller) jobDeliverable(owner string) bool {
	if c.evbuf == nil || c.st == nil || owner == "" {
		return false
	}
	snap, ok := c.st.Get(owner)
	return ok && snap.Status != protocol.StatusExited
}

// ackJobExit tells the executor a job exit is delivered, logging a failure at
// Warn. Never fatal: the next subscribe replays the unacked exit, and the
// duplicate collapses on the key while the first fragment is still pending.
func (c *Controller) ackJobExit(executorID, handle string) {
	if c.jobAcks == nil {
		return
	}
	ctx, cancel := context.WithTimeout(c.controlContext(), jobAckTimeout)
	defer cancel()
	if err := c.jobAcks.AckJobExits(ctx, executorID, []string{handle}); err != nil {
		slog.Warn("could not ack a job exit; the executor will replay it on the next subscribe",
			"executor", executorID, "handle", handle, "error", err)
	}
}

// noteBoundExecutor retains the child's boundExecutor, so a per-call consumer
// (and the teardown that drops it) has a handle on the child's current binding.
// agentRuntimeOptions creates it per child and otherwise hands it only to the
// fundi runtime; without this retention the binding would be unreachable. A
// resume replaces the entry (recovery rebuilds the binding).
func (c *Controller) noteBoundExecutor(childID string, be *boundExecutor) {
	c.boundMu.Lock()
	defer c.boundMu.Unlock()
	if c.bound == nil {
		c.bound = make(map[string]*boundExecutor)
	}
	c.bound[childID] = be
}

func (c *Controller) forgetBoundExecutor(childID string) {
	c.boundMu.Lock()
	defer c.boundMu.Unlock()
	delete(c.bound, childID)
}

func (c *Controller) boundExecutorFor(childID string) *boundExecutor {
	c.boundMu.Lock()
	defer c.boundMu.Unlock()
	return c.bound[childID]
}

// controlContext is the daemon's base context, or Background when a
// hand-built Controller left it nil.
func (c *Controller) controlContext() context.Context {
	if c.baseCtx != nil {
		return c.baseCtx
	}
	return context.Background()
}
