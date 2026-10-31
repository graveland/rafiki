package main

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"go.graveland.dev/rafiki/pkg/childstore"
	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/protocol"

	"google.golang.org/protobuf/types/known/timestamppb"
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

// rateLimitResumeLabel carries a PENDING auto-resume on the child's row, as the
// fire instant in RFC3339 (UTC). It is what makes the promise survive the
// daemon: the watch itself is in memory, so without this a restart inside the
// wait window dropped a resume the operator had already been told about. The
// label means "a resume is still owed" and nothing else — it is cleared the
// moment the resume is delivered, canceled by a clean completion, abandoned, or
// the child is killed or forgotten — so a restart never re-arms a watch that had
// already resolved. The daemon's own shutdown deliberately LEAVES it in place
// (rateWatch.dropAll), because that is exactly the resume the next boot owes.
const rateLimitResumeLabel = "rafiki/rate-limit-resume-at"

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
	// attempts counts resumes DELIVERED since the last successful turn — not
	// schedules. Only a resume that actually went out spends the budget: a 429
	// that re-targets a pending resume (Claude Code retrying inside one turn,
	// or a second failed turn before the reset) is the same attempt, and
	// burning a slot for it used to exhaust the cap without a single retry
	// ever firing.
	attempts int
	// abandoned marks a cap already announced, so the two triggers that can
	// reach scheduleRateLimitResumeLocked (the 429 notification and the idle
	// transition) publish the abandonment once rather than twice per 429.
	// Cleared by a successful turn, which re-opens the budget.
	abandoned bool
	// timer is the pending resume, if any. At most one per child: a newer 429
	// supersedes it.
	timer *time.Timer
}

// rateLimitWatch is the per-child in-memory watch.
//
// The WATCH is in-memory; the pending RESUME is not — scheduling stamps the
// fire instant on the child's row (rateLimitResumeLabel) and the boot walk
// re-arms from it, so a restart inside the wait window no longer breaks a
// promise the operator was already told about.
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
// daemon's own shutdown, where nothing will be around to act on a fire. The
// pending-resume labels are deliberately LEFT on the rows — that is the resume
// the next boot owes, and armRecoveredRateLimitResumes re-arms it from there.
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
	// The row write (stamp/clear) happens after the lock is released: the watch
	// mutex is process-wide, and a database round trip under it would hold up
	// every other child's 429 notification and idle transition.
	var fireAt time.Time
	var abandoned bool
	func() {
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
			fireAt, abandoned = c.scheduleRateLimitResumeLocked(session, st)
		}
	}()
	c.syncRateLimitResumeLabel(session, fireAt, abandoned)
}

// TurnSucceeded implements server.RateLimitObserver: a clean main-thread
// completion means the model answered — cancel any pending resume and forget
// the consecutive-attempt count. Runs once per proxied request; everything
// under the lock is in-memory only.
func (c *Controller) TurnSucceeded(session string) {
	func() {
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
				"rate limit cleared; scheduled auto-resume canceled", time.Time{})
		}
		st.attempts = 0
		// The model answered: the budget re-opens and a future breach is worth
		// announcing again.
		st.abandoned = false
	}()
	// The model answered, so nothing is owed: drop the row's pending-resume
	// label. A no-op (and no row write) for a child that never carried one.
	c.clearRateLimitResumeAt(session)
}

// maybeRateLimitResume schedules a resume when the just-settled turn of a
// claude child died of a rate limit. Called on the idle transition (the turn's
// result frame) and from RateLimited (the 429 notification landing after it) —
// one predicate, two triggers, whichever arrives second wins.
func (c *Controller) maybeRateLimitResume(childID string) {
	var fireAt time.Time
	var abandoned bool
	func() {
		c.rateWatch.mu.Lock()
		defer c.rateWatch.mu.Unlock()
		st := c.rateWatch.stateLocked(childID, false)
		if st == nil || st.limitedAt.IsZero() || !st.limitedAt.After(st.lastOKAt) {
			return // never limited here, or a success already post-dated the 429
		}
		if st.timer != nil {
			return // already scheduled; a newer 429 supersedes via RateLimited
		}
		fireAt, abandoned = c.scheduleRateLimitResumeLocked(childID, st)
	}()
	c.syncRateLimitResumeLabel(childID, fireAt, abandoned)
}

// scheduleRateLimitResumeLocked arms the timer and publishes the TUI notice.
// rateLimitWatch.mu must be held.
// It returns the instant it armed, and whether it instead abandoned the watch
// (the attempt cap), so the caller can persist the decision on the child's row.
//
// Arming does not spend an attempt: `attempts` counts deliveries, so the timer
// carries the ordinal of the resume it is about to make (attempts+1), and a 429
// that re-targets a pending timer re-announces that same ordinal against the
// newer reset instead of counting up.
func (c *Controller) scheduleRateLimitResumeLocked(childID string, st *rateLimitState) (fireAt time.Time, abandoned bool) {
	if st.attempts >= maxRateLimitResumes {
		// Announce the cap ONCE per breach: with no timer armed, both triggers
		// (the 429 notification and the idle transition) reach this branch for
		// the same rejection.
		if !st.abandoned {
			st.abandoned = true
			c.publishRateLimitNotice(childID, false, st.attempts, fmt.Sprintf(
				"auto-resume abandoned after %d attempts; resume manually with `rafiki resume`", maxRateLimitResumes), time.Time{})
			// The Retry resolution above only clears the rail's glyph; the
			// transcript needs a block of its own, which is what an Error is:
			// the turn is over and nothing more is coming on its own.
			c.publishEvent(childID, &rafikiv1.Event{
				ChildId: childID,
				Ts:      timestamppb.Now(),
				Payload: &rafikiv1.Event_Error{Error: &rafikiv1.ErrorEvent{
					Code: "rate_limit_auto_resume_abandoned",
					Message: fmt.Sprintf("auto-resume abandoned after %d rate-limited turns; "+
						"nothing more will be retried — prompt it or run `rafiki resume` when the window reopens",
						maxRateLimitResumes),
				}},
			})
			slog.Warn("claude child rate limited; auto-resume attempts exhausted",
				"childId", childID, "attempts", st.attempts, "resetAt", st.resetAt)
		}
		return time.Time{}, true
	}
	attempt := st.attempts + 1
	delay := c.rateWatch.delay(st.attempts, st.resetAt)
	fireAt = time.Now().Add(delay)
	st.timer = time.AfterFunc(delay, func() { c.fireRateLimitResume(childID, attempt) })
	// The fire instant travels as resume_at, not formatted into the
	// reason: this daemon's clock zone is arbitrary (a container runs UTC),
	// and the viewer renders its own local zone.
	c.publishRateLimitNotice(childID, true, attempt, "rate limited (HTTP 429)", fireAt)
	slog.Warn("claude child rate limited; auto-resume scheduled",
		"childId", childID, "resume_at", fireAt.Format(time.RFC3339),
		"reset_at", st.resetAt.Format(time.RFC3339), "attempt", attempt,
		"delivered", st.attempts, "max", maxRateLimitResumes)
	return fireAt, false
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
		c.dropRateLimitWatch(childID)
		return
	}
	if st.lastOKAt.After(st.limitedAt) {
		return // a success post-dated the 429; nothing to resume
	}
	switch snap.Status {
	case protocol.StatusIdle, protocol.StatusExited:
		// Idle: the failed turn settled and the child has been sitting on it.
		// Exited: the process died while the resume was pending, and
		// deliverRateLimitResume relaunches it via Resume — the same path a
		// manual `rafiki resume` takes. Either way a resume is going out, so
		// THIS is where an attempt is spent (not when it was scheduled).
		c.noteRateLimitAttempt(childID, attempt)
		c.deliverRateLimitResume(childID, attempt)
	default:
		// Mid-turn: the child recovered on its own (or a coordinator prompted
		// it). Its turn's own verdict drives the watch from here — no resume is
		// delivered, so nothing is spent.
	}
}

// noteRateLimitAttempt records the delivery of resume `attempt` against the
// child's budget. Called at fire time, on the fire path only; the max() guards a
// timer that raced its own supersession (RateLimited stops a pending timer, but
// Stop can lose to a callback already running).
func (c *Controller) noteRateLimitAttempt(childID string, attempt int) {
	c.rateWatch.mu.Lock()
	defer c.rateWatch.mu.Unlock()
	if st := c.rateWatch.stateLocked(childID, false); st != nil && attempt > st.attempts {
		st.attempts = attempt
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

	// The delivery half is shared with the restart auto-resume
	// (pending_resume.go): a failed delivery leaves the row pending, and the
	// next resume replays it.
	if err := c.sendClaudeNudge(childID, rateLimitResumeText); err != nil {
		slog.Warn("auto-resume prompt delivery failed", "childId", childID, "error", err)
		return
	}
	c.publishRateLimitNotice(childID, false, attempt, fmt.Sprintf("auto-resume %d firing", attempt), time.Time{})
	// The resume is delivered, so the row no longer owes one: clear the label
	// before the next 429 re-stamps it. A restart between here and the child's
	// next verdict must not deliver the same nudge twice.
	c.clearRateLimitResumeAt(childID)
	slog.Info("rate-limited claude child auto-resumed", "childId", childID, "attempt", attempt)
}

// syncRateLimitResumeLabel makes the child's row agree with the watch: the
// fire instant when a timer is armed, nothing when the watch was abandoned.
// Called with rateLimitWatch.mu RELEASED (see RateLimited), and a no-op when
// the watch reached no decision at all — a 429 arriving mid-turn must not clear
// a label an earlier one stamped.
func (c *Controller) syncRateLimitResumeLabel(childID string, fireAt time.Time, abandoned bool) {
	switch {
	case abandoned:
		c.clearRateLimitResumeAt(childID)
	case !fireAt.IsZero():
		c.stampRateLimitResumeAt(childID, fireAt)
	}
}

// stampRateLimitResumeAt records the pending fire instant on the child's row,
// so a daemon restart inside the wait window re-arms it (armRecoveredRateLimitResumes)
// instead of silently breaking a promise the operator already saw. The row write
// is asynchronous: this runs on a proxy request's goroutine and must not add a
// database round trip to it.
func (c *Controller) stampRateLimitResumeAt(childID string, at time.Time) {
	if at.IsZero() {
		return
	}
	value := at.UTC().Format(time.RFC3339)
	changed := false
	if err := c.st.Update(childID, func(s *childstore.Session) {
		if s.Labels == nil {
			s.Labels = map[string]string{}
		}
		if s.Labels[rateLimitResumeLabel] == value {
			return
		}
		s.Labels[rateLimitResumeLabel] = value
		changed = true
	}); err != nil {
		slog.Warn("stamp rate-limit resume label", "childId", childID, "error", err)
		return
	}
	if changed {
		c.writeRateLimitResumeRow(childID)
	}
}

// clearRateLimitResumeAt drops the pending-resume label. It writes nothing when
// the child carries no label, so the per-turn call in TurnSucceeded costs no
// row write on a healthy child.
func (c *Controller) clearRateLimitResumeAt(childID string) {
	changed := false
	if err := c.st.Update(childID, func(s *childstore.Session) {
		if _, ok := s.Labels[rateLimitResumeLabel]; !ok {
			return
		}
		delete(s.Labels, rateLimitResumeLabel)
		changed = true
	}); err != nil {
		return // child gone: there is no row left to clear
	}
	if changed {
		c.writeRateLimitResumeRow(childID)
	}
}

// writeRateLimitResumeRow persists the child's row off the caller's goroutine.
// writeRecord snapshots the store when it RUNS, so a stamp or clear that lands
// in between still wins: whichever write runs last writes the store's current
// truth rather than a captured one.
func (c *Controller) writeRateLimitResumeRow(childID string) {
	go func() {
		if err := c.writeRecord(childID); err != nil {
			slog.Warn("write child row after rate-limit resume change", "childId", childID, "error", err)
		}
	}()
}

// dropRateLimitWatch forgets a child's watch AND its persisted resume. An
// operator's Kill (or a forget) must not be undone by the next boot re-arming
// the label; the daemon's own shutdown calls rateWatch.dropAll instead, which
// deliberately leaves every label in place.
func (c *Controller) dropRateLimitWatch(childID string) {
	c.rateWatch.drop(childID)
	c.clearRateLimitResumeAt(childID)
}

// armRecoveredRateLimitResumes re-arms the auto-resume a daemon restart
// interrupted. The watch itself lived only in the previous daemon's memory, but
// the fire instant rides the child's row, so a child that was parked on a rate
// limit when its daemon died is re-armed here — and the schedule notice is
// re-published, which is also what puts "will retry at HH:MM" back into a
// viewer that attached after the original notice.
//
// Only an IDLE claude child is re-armed. An exited one is recovery's business
// (recoverOne decides whether it resumes at all, and arming here could
// relaunch a child recovery deliberately left alone); a working one gets the
// restart continuation nudge instead. An instant already in the past — the
// daemon was down through the reset — fires on the buffer floor, so the child
// resumes promptly instead of waiting for another full window.
func (c *Controller) armRecoveredRateLimitResumes() {
	for _, s := range c.st.List() {
		if s.Kind != protocol.KindClaude || s.Native || s.Status != protocol.StatusIdle {
			continue
		}
		raw := s.Labels[rateLimitResumeLabel]
		if raw == "" {
			continue
		}
		at, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			slog.Warn("rate-limit resume label is not an RFC3339 instant; dropping it",
				"childId", s.ChildID, "value", raw)
			c.clearRateLimitResumeAt(s.ChildID)
			continue
		}
		var fireAt time.Time
		var abandoned bool
		func() {
			c.rateWatch.mu.Lock()
			defer c.rateWatch.mu.Unlock()
			st := c.rateWatch.stateLocked(s.ChildID, true)
			// The row is the whole record: the 429 that stamped it is the latest
			// upstream verdict for this child, and a restart is not a success.
			st.limitedAt = time.Now()
			st.resetAt = at
			// The row carries no attempt count, so a restart re-opens the
			// budget (and re-arms a breach announcement): the alternative is a
			// crash loop that never retries at all.
			st.attempts = 0
			st.abandoned = false
			fireAt, abandoned = c.scheduleRateLimitResumeLocked(s.ChildID, st)
		}()
		c.syncRateLimitResumeLabel(s.ChildID, fireAt, abandoned)
		if abandoned {
			slog.Info("rate-limit auto-resume abandoned on re-arm: attempt cap reached",
				"childId", s.ChildID, "reset_at", at.Format(time.RFC3339))
			continue
		}
		slog.Info("re-armed a rate-limit auto-resume from the child's row",
			"childId", s.ChildID, "reset_at", at.Format(time.RFC3339),
			"resume_at", fireAt.Format(time.RFC3339))
	}
}

// publishRateLimitNotice publishes the retry-family event the TUI renders:
// will_retry=true shows the rail's ⟳ and appends a system notice to the
// transcript with the scheduled time; will_retry=false clears the glyph.
// resumeAt is the schedule's fire instant — set only for will_retry=true —
// and rides the event as resume_at, never as a formatted
// time in reason: this daemon's clock zone is arbitrary, so the client owns
// the rendering. maxAttempts rides along on every event so a client can
// render "attempt 1/3" without parsing text.
// Event_Retry is durable-tier, so the notice replays on reattach. Durable and
// best-effort at once: publishEvent logs a warn on a failed log append and
// publishes anyway, the same contract every other caller relies on.
func (c *Controller) publishRateLimitNotice(childID string, willRetry bool, attempt int, reason string, resumeAt time.Time) {
	retry := &rafikiv1.Retry{
		Attempt:     int32(attempt),
		WillRetry:   willRetry,
		Reason:      reason,
		MaxAttempts: int32(maxRateLimitResumes),
	}
	if !resumeAt.IsZero() {
		retry.ResumeAt = timestamppb.New(resumeAt)
	}
	c.publishEvent(childID, &rafikiv1.Event{
		ChildId: childID,
		Ts:      timestamppb.Now(),
		Payload: &rafikiv1.Event_Retry{Retry: retry},
	})
}
