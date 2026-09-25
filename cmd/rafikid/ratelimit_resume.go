package main

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/inbox"
	"go.graveland.dev/rafiki/pkg/protocol"
)

// This file implements the claude rate-limit auto-resume: when a
// daemon-spawned claude child's turn dies because Anthropic rejected it with
// 429, the daemon schedules a resume for when the limit resets instead of
// leaving the task stalled until a human notices. The proxy side is
// pkg/server's RateLimitObserver — it sees the actual rejection and the reset
// headers, and calls the Controller methods below.
//
// Design doc: docs/plans/2026-09-25-claude-ratelimit-autoresume-design.md.

const (
	// rateLimitResumeBuffer is added to the upstream reset time: firing exactly
	// at reset races the window's own bookkeeping, and a just-passed reset that
	// 429s again simply re-arms (attempts allowing).
	rateLimitResumeBuffer = 30 * time.Second

	// rateLimitMinBackoff / rateLimitMaxBackoff bound the ladder used when the
	// 429 named no reset time (no unified headers, no Retry-After): 1m, 2m, 4m,
	// capped at 30m. A retry that is too early re-arms; one that is too late
	// only delays the resume.
	rateLimitMinBackoff = time.Minute
	rateLimitMaxBackoff = 30 * time.Minute

	// maxRateLimitResumes bounds consecutive scheduled resumes with no
	// intervening successful turn. Three covers "the window reset but the task
	// immediately burned through it again"; beyond that, auto-resuming is a
	// loop the operator needs to see rather than silence.
	maxRateLimitResumes = 3

	// rateLimitResumeTimeout bounds one fire's relaunch + queue work.
	rateLimitResumeTimeout = 30 * time.Second
)

// rateLimitResumeText is the prompt an auto-resume delivers to the child. The
// child keeps its full in-process context, so the instruction is a nudge, not
// a re-briefing; naming the cause keeps the model from narrating the gap as a
// mystery.
const rateLimitResumeText = "Your previous turn was interrupted by an Anthropic rate limit (HTTP 429), which has now reset. " +
	"Continue your work from exactly where it stopped — do not restart the task and do not repeat steps you have already completed."

// rateLimitState is one claude child's live rate-limit watch. Guarded by
// rateLimitWatch.mu; the timer callback re-validates everything it acts on.
type rateLimitState struct {
	// limitedAt is when the most recent 429 for this child's MAIN-thread
	// traffic was observed. Subagent threads share the child's
	// X-Rafiki-Session but never reach this file (the proxy gates them off).
	limitedAt time.Time
	// resetAt is the best-known reset time from the rejecting response. Zero
	// means unknown — the ladder applies.
	resetAt time.Time
	// lastOKAt is when the most recent clean main-thread completion was
	// observed. The schedule predicate is limitedAt.After(lastOKAt): the
	// upstream's latest verdict for this child is a rejection, so the turn
	// that just settled died of it. A 429 Claude Code retried past is always
	// post-dated by the retried request's success, because the retry is a new
	// proxied request issued only after the 429 was handled.
	lastOKAt time.Time
	// attempts counts scheduled resumes since the last successful turn.
	attempts int
	// timer is the pending resume, if any. At most one per child: a newer 429
	// supersedes it.
	timer *time.Timer
}

// rateLimitWatch is the per-child in-memory watch. State is deliberately NOT
// persisted: a daemon restart inside the wait window loses the pending resume
// and the child is stalled exactly as it was before this feature existed.
// Persisting would need a clear-on-success write per turn to keep stale marks
// from re-arming a recovered child after a restart; not worth it yet.
type rateLimitWatch struct {
	mu sync.Mutex
	m  map[string]*rateLimitState

	// How long a schedule waits past the upstream reset, and the bounds of the
	// ladder used when the 429 named no reset time. NewController sets the
	// production constants; tests shrink them so a schedule fires in
	// milliseconds. A zero field falls back to its constant, so a Controller
	// built without NewController (tests that literal-construct one) is safe.
	buffer     time.Duration
	minBackoff time.Duration
	maxBackoff time.Duration
}

// delay computes the wait before an auto-resume: the upstream reset time plus
// the buffer when the response named one (floored at the buffer — never in
// the past), else an exponential ladder from minBackoff, capped at maxBackoff.
// step is the zero-based backoff step (attempts-1 at the call site).
func (w *rateLimitWatch) delay(step int, resetAt time.Time) time.Duration {
	buffer, minB, maxB := w.buffer, w.minBackoff, w.maxBackoff
	if buffer == 0 {
		buffer = rateLimitResumeBuffer
	}
	if minB == 0 {
		minB = rateLimitMinBackoff
	}
	if maxB == 0 {
		maxB = rateLimitMaxBackoff
	}
	if !resetAt.IsZero() {
		d := time.Until(resetAt) + buffer
		if d < buffer {
			d = buffer
		}
		return d
	}
	if step > 20 {
		return maxB
	}
	d := minB << uint(step)
	if d > maxB || d <= 0 {
		return maxB
	}
	return d
}

func (w *rateLimitWatch) state(childID string, create bool) *rateLimitState {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.stateLocked(childID, create)
}

// stateLocked is state for callers already holding w.mu — the Controller's
// observer methods, which do their read-decide-write under one hold.
func (w *rateLimitWatch) stateLocked(childID string, create bool) *rateLimitState {
	if w.m == nil {
		w.m = make(map[string]*rateLimitState)
	}
	st, ok := w.m[childID]
	if !ok && create {
		st = &rateLimitState{}
		w.m[childID] = st
	}
	return st
}

// drop cancels childID's pending timer and forgets its watch state. Called
// when the child is forgotten, and on an operator-driven death — a Kill must
// not resurrect the child it deliberately stopped (ShutdownAllChildren uses
// dropAll for the same reason).
func (w *rateLimitWatch) drop(childID string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	st, ok := w.m[childID]
	if !ok {
		return
	}
	if st.timer != nil {
		st.timer.Stop()
	}
	delete(w.m, childID)
}

// dropAll cancels every pending timer and forgets every watch state: the
// daemon's own shutdown, where nothing will be around to act on a fire.
func (w *rateLimitWatch) dropAll() {
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, st := range w.m {
		if st.timer != nil {
			st.timer.Stop()
		}
	}
	w.m = make(map[string]*rateLimitState)
}

// RateLimited implements server.RateLimitObserver: a main-thread request of a
// supervised claude child was rejected with 429. session is the child id (the
// proxy only calls through for ids that resolved to a child — but this daemon
// may not be the child's daemon, and a hand-configured client's session never
// is, so the kind gate re-checks the store). resetAt is the response's
// best-known reset time; zero means unknown.
//
// Most 429s arrive while the child is mid-turn (Claude Code retrying): those
// only record. The turn's own ending — the idle transition, or this
// notification landing after it — is what schedules, via the same predicate in
// one place (maybeRateLimitResume).
func (c *Controller) RateLimited(session string, resetAt time.Time) {
	snap, ok := c.st.Get(session)
	if !ok || snap.Kind != protocol.KindClaude || snap.Native {
		return
	}
	c.rateWatch.mu.Lock()
	defer c.rateWatch.mu.Unlock()
	st := c.rateWatch.stateLocked(session, true)
	st.limitedAt = time.Now()
	st.resetAt = resetAt
	if st.timer != nil {
		// A pending resume built on an older reset time is superseded: this
		// rejection restarts the decision, and the idle hook (or the idle
		// check below) schedules against the newer reset.
		st.timer.Stop()
		st.timer = nil
	}
	// The failed turn may already have settled — the proxy's notification
	// races the result frame it caused. A stale header can also name a reset
	// already in the past; the watch's delay clamps that to the buffer floor.
	if snap.Status == protocol.StatusIdle && st.limitedAt.After(st.lastOKAt) {
		c.scheduleRateLimitResumeLocked(session, st)
	}
}

// TurnSucceeded implements server.RateLimitObserver: a clean main-thread
// completion means the model answered — cancel any pending resume and forget
// the consecutive-attempt count. Runs once per proxied request; everything
// under the lock is in-memory only.
func (c *Controller) TurnSucceeded(session string) {
	c.rateWatch.mu.Lock()
	defer c.rateWatch.mu.Unlock()
	st := c.rateWatch.stateLocked(session, false)
	if st == nil {
		return
	}
	st.lastOKAt = time.Now()
	if st.timer != nil {
		st.timer.Stop()
		st.timer = nil
		c.publishRateLimitNotice(session, false, st.attempts,
			"rate limit cleared; scheduled auto-resume canceled")
	}
	st.attempts = 0
}

// maybeRateLimitResume schedules a resume when the just-settled turn of a
// claude child died of a rate limit. Called on the idle transition (the turn's
// result frame) and from RateLimited (the 429 notification landing after it) —
// one predicate, two triggers, whichever arrives second wins.
func (c *Controller) maybeRateLimitResume(childID string) {
	c.rateWatch.mu.Lock()
	defer c.rateWatch.mu.Unlock()
	st := c.rateWatch.stateLocked(childID, false)
	if st == nil || st.limitedAt.IsZero() || !st.limitedAt.After(st.lastOKAt) {
		return // never limited here, or a success already post-dated the 429
	}
	if st.timer != nil {
		return // already scheduled; a newer 429 supersedes via RateLimited
	}
	c.scheduleRateLimitResumeLocked(childID, st)
}

// scheduleRateLimitResumeLocked arms the timer and publishes the TUI notice.
// rateLimitWatch.mu must be held.
func (c *Controller) scheduleRateLimitResumeLocked(childID string, st *rateLimitState) {
	if st.attempts >= maxRateLimitResumes {
		c.publishRateLimitNotice(childID, false, st.attempts, fmt.Sprintf(
			"auto-resume abandoned after %d attempts; resume manually with `rafiki resume`", maxRateLimitResumes))
		slog.Warn("claude child rate limited; auto-resume attempts exhausted",
			"childId", childID, "attempts", st.attempts, "resetAt", st.resetAt)
		return
	}
	st.attempts++
	delay := c.rateWatch.delay(st.attempts-1, st.resetAt)
	fireAt := time.Now().Add(delay)
	attempt := st.attempts
	st.timer = time.AfterFunc(delay, func() { c.fireRateLimitResume(childID, attempt) })
	c.publishRateLimitNotice(childID, true, attempt, fmt.Sprintf(
		"rate limited (HTTP 429); auto-resume scheduled for %s (attempt %d/%d)",
		fireAt.Format("15:04:05"), attempt, maxRateLimitResumes))
	slog.Warn("claude child rate limited; auto-resume scheduled",
		"childId", childID, "resume_at", fireAt.Format(time.RFC3339),
		"reset_at", st.resetAt.Format(time.RFC3339), "attempt", attempt, "max", maxRateLimitResumes)
}

// fireRateLimitResume re-validates the watch at fire time on the timer's own
// goroutine and delivers the resume. Every check is against state that could
// have changed while the timer sat pending: the child recovered on its own, a
// coordinator prompted it, or it was killed or forgotten.
func (c *Controller) fireRateLimitResume(childID string, attempt int) {
	c.rateWatch.mu.Lock()
	st := c.rateWatch.stateLocked(childID, false)
	if st != nil {
		st.timer = nil
	}
	c.rateWatch.mu.Unlock()
	if st == nil {
		return // dropped (forgotten, or a Kill) while the timer was pending
	}

	snap, ok := c.st.Get(childID)
	if !ok {
		c.rateWatch.drop(childID)
		return
	}
	if st.lastOKAt.After(st.limitedAt) {
		return // a success post-dated the 429; nothing to resume
	}
	switch snap.Status {
	case protocol.StatusIdle:
		// The failed turn settled and the child has been sitting on it.
		c.deliverRateLimitResume(childID, attempt)
	case protocol.StatusExited:
		// The process died while the resume was pending. deliverRateLimitResume
		// relaunches it via Resume — the same path a manual `rafiki resume`
		// takes — and then prompts it.
		c.deliverRateLimitResume(childID, attempt)
	default:
		// Mid-turn: the child recovered on its own (or a coordinator prompted
		// it). Its turn's own verdict drives the watch from here.
	}
}

// deliverRateLimitResume resumes a rate-limited claude child. An exited one is
// relaunched first via Resume — the same --resume <session> path a manual
// `rafiki resume` takes. The continuation prompt then goes through Send, the
// ordinary channel: on a daemon with an inbox it is a durable row (so if the
// child dies between the status check and the write, the pending row is
// exactly what the next resume replays — releaseInboxOnExit returns it to
// pending); on a DB-less daemon it degrades to the direct write Send itself
// takes.
func (c *Controller) deliverRateLimitResume(childID string, attempt int) {
	ctx, cancel := context.WithTimeout(context.Background(), rateLimitResumeTimeout)
	defer cancel()

	if snap, ok := c.st.Get(childID); ok && snap.Status == protocol.StatusExited {
		slog.Info("auto-resume relaunching exited claude child", "childId", childID, "attempt", attempt)
		if _, err := c.Resume(ctx, childID, ""); err != nil {
			// Not fatal: the prompt below is still accepted — on a daemon with
			// an inbox it queues pending and a later manual `rafiki resume`
			// replays it into the relaunched session.
			slog.Warn("auto-resume relaunch failed; prompt stays queued",
				"childId", childID, "error", err)
		}
	}

	frame, err := buildInjectionFrame(inbox.Batch{Mode: inbox.ModePrompt, Frags: []string{rateLimitResumeText}}, "")
	if err != nil {
		slog.Warn("auto-resume prompt build failed", "childId", childID, "error", err)
		return
	}
	if err := c.Send(childID, frame); err != nil {
		slog.Warn("auto-resume prompt delivery failed", "childId", childID, "error", err)
		return
	}
	c.publishRateLimitNotice(childID, false, attempt, fmt.Sprintf("auto-resume %d firing", attempt))
	slog.Info("rate-limited claude child auto-resumed", "childId", childID, "attempt", attempt)
}

// publishRateLimitNotice publishes the retry-family event the TUI renders:
// will_retry=true shows the rail's ⟳ and appends a system notice to the
// transcript with the scheduled time; will_retry=false clears the glyph.
// Event_Retry is durable-tier, so the notice replays on reattach. Durable and
// best-effort at once: publishEvent logs a warn on a failed log append and
// publishes anyway, the same contract every other caller relies on.
func (c *Controller) publishRateLimitNotice(childID string, willRetry bool, attempt int, reason string) {
	c.publishEvent(childID, &rafikiv1.Event{
		ChildId:  childID,
		TsUnixMs: time.Now().UnixMilli(),
		Payload: &rafikiv1.Event_Retry{Retry: &rafikiv1.Retry{
			Attempt:   int32(attempt),
			WillRetry: willRetry,
			Reason:    reason,
		}},
	})
}
