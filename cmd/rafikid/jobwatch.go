package main

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"go.graveland.dev/rafiki/pkg/fundi/tools"
	"go.graveland.dev/rafiki/pkg/protocol"
)

// jobEventSource is the event-buffer source name for background-job news.
// One source per concern keeps a coordinator's injected frame readable: the
// buffer coalesces per (child, source), so job exits land in their own batch
// rather than interleaved with subagent settles or budget warnings.
const jobEventSource = "jobs"

const (
	// jobWatchInterval is how often the watcher asks the executor whether a
	// job has exited. The watcher exists so the AGENT never has to poll; a
	// few seconds of latency between a job's exit and the injected frame is
	// nothing next to a turn the agent would otherwise have spent re-reading
	// an unchanged 20k-char tail.
	jobWatchInterval = 5 * time.Second

	// jobPollTimeout bounds one JobOutput RPC. The watcher polls the CURRENT
	// binding only (see boundExecutor.peekJob), so this bounds a single call,
	// not a rebind ladder.
	jobPollTimeout = 10 * time.Second
)

// jobKey scopes a watch to the child that started the job. Handles are
// minted per executor workspace, so the childID half is what keeps two
// children's coincidentally equal handles apart.
type jobKey struct {
	childID string
	handle  string
}

// watchedJob is one in-flight watch.
type watchedJob struct {
	command string
	stop    chan struct{} // closed to end the loop with no notification
}

// jobWatcher polls the executor for a background job's exit and pushes a
// fragment into the starting child's event buffer, so the agent learns a job
// finished the way it learns a subagent settled: as an injected frame between
// turns, not by polling in a loop.
//
// It is the same division notifySubagentSettled rests on: the watcher says
// SOMETHING happened; what the job printed is read from the job's retained
// output with bash_output. A fragment that tried to carry the output would
// duplicate the retention budget the executor already enforces.
type jobWatcher struct {
	mu   sync.Mutex
	jobs map[jobKey]*watchedJob

	// poll reads a job's state. Overridable so tests script it; the
	// controller wiring resolves the child's boundExecutor and calls
	// peekJob — the CURRENT binding, deliberately never recover (a
	// re-provisioned workspace has no such job, so recovering for a poll
	// would buy a Found=false from the new workspace at the price of a
	// migration).
	poll func(ctx context.Context, childID, handle string) (tools.JobSnapshot, error)

	// push delivers a fragment; c.evbuf.Push, or nothing when the buffer is
	// disabled. Nil-safe like every other evbuf consumer.
	push func(childID, source, key, fragment string)

	// alive reports whether childID can still receive an injected frame. A
	// child that exited (its workspace — and its jobs — were released with
	// it) or was closed ends its watches silently: there is nobody to tell.
	alive func(childID string) bool

	interval time.Duration
	baseCtx  context.Context
}

func newJobWatcher() *jobWatcher {
	return &jobWatcher{
		jobs:     make(map[jobKey]*watchedJob),
		interval: jobWatchInterval,
		baseCtx:  context.Background(),
	}
}

// watch begins polling handle on childID's behalf. A repeat watch for the
// same (child, handle) replaces the first — defensive; StartJob mints a fresh
// handle every time.
func (w *jobWatcher) watch(childID, handle, command string) {
	if w == nil {
		return
	}
	k := jobKey{childID, handle}
	e := &watchedJob{command: command, stop: make(chan struct{})}

	w.mu.Lock()
	if prev, ok := w.jobs[k]; ok {
		close(prev.stop)
	}
	w.jobs[k] = e
	w.mu.Unlock()

	go w.loop(k, e)
}

// forget drops a watch WITHOUT notifying: the agent itself resolved the job
// (bash_kill), so a "finished" fragment would be news it already has. It is
// also the test's way to stop a loop that would otherwise tick forever.
func (w *jobWatcher) forget(childID, handle string) {
	if w == nil {
		return
	}
	w.mu.Lock()
	e, ok := w.jobs[jobKey{childID, handle}]
	delete(w.jobs, jobKey{childID, handle})
	w.mu.Unlock()
	if ok {
		close(e.stop)
	}
}

func (w *jobWatcher) loop(k jobKey, e *watchedJob) {
	// remove-if-still-ours, not forget: a watch replaced mid-poll must not
	// have its replacement's stop channel closed by a stale loop exiting.
	defer w.remove(k, e)
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()
	for {
		select {
		case <-e.stop:
			return
		case <-w.baseCtx.Done():
			return
		case <-ticker.C:
		}
		if !w.pollOnce(k, e) {
			return
		}
	}
}

// remove drops the map entry if it is still e. It never closes anything: the
// loop is the goroutine that reads e.stop, so it is the one place that may
// let it be garbage.
func (w *jobWatcher) remove(k jobKey, e *watchedJob) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.jobs[k] == e {
		delete(w.jobs, k)
	}
}

// pollOnce runs one poll and reports whether the watch continues. Only a
// definitive answer ends it — exited, gone, or a child that can no longer
// receive the news. An error keeps the watch: an executor blip must not
// silently drop a notification the agent is now waiting for (the same
// transient/terminal split refreshRow applies).
func (w *jobWatcher) pollOnce(k jobKey, e *watchedJob) bool {
	// BEFORE the poll, not only after a successful one. A child that can no
	// longer receive the news ends its watch on EVERY path, because the two
	// conditions arrive together: Close drops the binding pollJob resolves
	// through, so from that moment every poll errors and a liveness check
	// reachable only past a successful poll is never reached at all. That
	// left a closed child's loop ticking for the daemon's lifetime.
	if !w.alive(k.childID) {
		return false
	}

	ctx, cancel := context.WithTimeout(w.baseCtx, jobPollTimeout)
	snap, err := w.poll(ctx, k.childID, k.handle)
	cancel()
	if err != nil {
		// Shutdown is judged on baseCtx, NOT on ctx: cancel() above has
		// already made ctx.Err() non-nil by the time we get here, so reading
		// it would classify every transient error as shutdown and end the
		// watch on the first executor blip.
		if w.baseCtx.Err() != nil {
			return false // the daemon itself is shutting down
		}
		slog.Debug("job watch poll failed; will retry",
			"child", k.childID, "handle", k.handle, "error", err)
		return true
	}

	// Again after the poll: the RPC can take jobPollTimeout, and a child that
	// exited inside that window must not be pushed to.
	if !w.alive(k.childID) {
		return false
	}
	switch {
	case snap.Exited:
		w.push(k.childID, jobEventSource, k.handle, fmt.Sprintf(
			"background job %s (%s) finished with exit code %d. Read what it printed with bash_output {\"handle\":%q}.",
			k.handle, e.command, snap.ExitCode, k.handle))
		return false
	case !snap.Found:
		// The executor no longer knows the handle. Three things produce that
		// answer — the workspace was released, the executor restarted and lost
		// its in-memory registry, or the job finished and enforceBudget evicted
		// it (which deletes the registry entry, not just the output file) — and
		// a poll cannot tell them apart, so the fragment names none of them.
		// Whichever it was, the exit code and the output are unrecoverable and
		// the advice is the same.
		w.push(k.childID, jobEventSource, k.handle, fmt.Sprintf(
			"background job %s (%s) is no longer on its executor: neither what it printed nor "+
				"how it ended can be recovered. Re-run the command if you still need the result.",
			k.handle, e.command))
		return false
	}
	return true
}

// newControllerJobWatcher wires the watcher to this controller's store,
// buffer and per-child bindings. Split from newJobWatcher so the collision
// between "test constructs Controller{} literal" and "nil map deref" stays
// impossible: every controllerBinder entry point nil-checks c.jobs.
func (c *Controller) newControllerJobWatcher() *jobWatcher {
	w := newJobWatcher()
	if c.baseCtx != nil {
		w.baseCtx = c.baseCtx
	}
	w.poll = c.pollJob
	w.push = func(childID, source, key, fragment string) {
		if c.evbuf != nil {
			c.evbuf.Push(childID, source, key, fragment)
		}
	}
	w.alive = func(childID string) bool {
		snap, ok := c.st.Get(childID)
		// Exited is the only terminal status (pkg/protocol/types.go), and a
		// child's workspace release kills its jobs with it — the watch would
		// only ever discover a job that is already gone, to an agent that is
		// already gone.
		return ok && snap.Status != protocol.StatusExited
	}
	return w
}

// pollJob is the watcher's production poll: resolve the child's retained
// binding and read the job through it. A child with no retained binding has
// nothing to poll — the watch keeps ticking until the child goes terminal.
func (c *Controller) pollJob(ctx context.Context, childID, handle string) (tools.JobSnapshot, error) {
	be := c.boundExecutorFor(childID)
	if be == nil {
		return tools.JobSnapshot{}, fmt.Errorf("no executor bound for child %s", childID)
	}
	return be.peekJob(ctx, handle)
}

// noteBoundExecutor retains the child's boundExecutor for the job watcher.
// agentRuntimeOptions creates it per child and otherwise hands it only to the
// fundi runtime; without this retention the watcher would have no client to
// poll through. A resume replaces the entry (recovery rebuilds the binding).
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
