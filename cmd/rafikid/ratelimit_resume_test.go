// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"go.graveland.dev/rafiki/pkg/childstore"
	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/protocol"

	"github.com/multigres/testkit/assert"
)

// shrinkRateWatch makes the auto-resume schedule fire in milliseconds: the
// buffer past a known reset, and the no-reset ladder's floor. The constants
// themselves are production values a test must not sleep for.
func shrinkRateWatch(t *testing.T, ctrl *Controller) {
	t.Helper()
	ctrl.rateWatch.mu.Lock()
	ctrl.rateWatch.buffer = 15 * time.Millisecond
	ctrl.rateWatch.minBackoff = 15 * time.Millisecond
	ctrl.rateWatch.mu.Unlock()
}

// noticeCollector drains one child's native event bus from subscription time,
// so a test sees every notice the watch publishes in order, including ones
// published while the test was busy asserting.
type noticeCollector struct {
	mu      sync.Mutex
	retries []*rafikiv1.Retry
	errors  []*rafikiv1.ErrorEvent
}

func collectNotices(t *testing.T, ctrl *Controller, childID string) (*noticeCollector, func()) {
	t.Helper()
	ch, cancel := ctrl.native.Subscribe(childID)
	nc := &noticeCollector{}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for ev := range ch {
			if r := ev.GetRetry(); r != nil {
				nc.mu.Lock()
				nc.retries = append(nc.retries, r)
				nc.mu.Unlock()
			}
			if e := ev.GetError(); e != nil {
				nc.mu.Lock()
				nc.errors = append(nc.errors, e)
				nc.mu.Unlock()
			}
		}
	}()
	return nc, func() { cancel(); <-done }
}

func (nc *noticeCollector) scheduled(attempt int) bool {
	nc.mu.Lock()
	defer nc.mu.Unlock()
	for _, r := range nc.retries {
		// The fire instant rides structurally (resume_at) — a schedule
		// notice without one would leave the client nothing to render locally.
		if r.GetWillRetry() && int(r.GetAttempt()) == attempt &&
			r.ResumeAt != nil && strings.Contains(r.GetReason(), "rate limited (HTTP 429)") {
			return true
		}
	}
	return false
}

// abandonedNotices counts the abandonment resolutions published so far, and
// abandonmentErrors the transcript-visible Error events behind them.
func (nc *noticeCollector) abandonedNotices() int {
	nc.mu.Lock()
	defer nc.mu.Unlock()
	n := 0
	for _, r := range nc.retries {
		if !r.GetWillRetry() && strings.Contains(r.GetReason(), "abandoned") {
			n++
		}
	}
	return n
}

func (nc *noticeCollector) abandonmentErrors() []*rafikiv1.ErrorEvent {
	nc.mu.Lock()
	defer nc.mu.Unlock()
	var out []*rafikiv1.ErrorEvent
	for _, e := range nc.errors {
		if e.GetCode() == "rate_limit_auto_resume_abandoned" {
			out = append(out, e)
		}
	}
	return out
}

// scheduledAttempts returns the attempt ordinal of every will_retry=true
// schedule notice so far, in order.
func (nc *noticeCollector) scheduledAttempts() []int {
	nc.mu.Lock()
	defer nc.mu.Unlock()
	var out []int
	for _, r := range nc.retries {
		if r.GetWillRetry() {
			out = append(out, int(r.GetAttempt()))
		}
	}
	return out
}

func (nc *noticeCollector) abandoned() bool {
	nc.mu.Lock()
	defer nc.mu.Unlock()
	for _, r := range nc.retries {
		if !r.GetWillRetry() && strings.Contains(r.GetReason(), "abandoned") {
			return true
		}
	}
	return false
}

func (nc *noticeCollector) fired(attempt int) bool {
	nc.mu.Lock()
	defer nc.mu.Unlock()
	for _, r := range nc.retries {
		if !r.GetWillRetry() && int(r.GetAttempt()) == attempt && strings.Contains(r.GetReason(), "firing") {
			return true
		}
	}
	return false
}

// waitForResumePrompts polls the child's stdin capture until it holds at
// least want copies of the auto-resume prompt (or the deadline passes).
// InSnapshot is the supervise loop's capture of every frame written to the
// child's stdin, so containing the resume text is the child having RECEIVED
// it; counting frames distinguishes a second delivery from the first one
// still sitting there.
func waitForResumePrompts(t *testing.T, ctrl *Controller, childID string, want int, timeout time.Duration) int {
	t.Helper()
	count := func() int {
		n := 0
		if ch, ok := ctrl.cm.Get(childID); ok {
			for _, f := range ch.InSnapshot() {
				if strings.Contains(string(f), rateLimitResumeText) {
					n++
				}
			}
		}
		return n
	}
	deadline := time.Now().Add(timeout)
	for {
		if got := count(); got >= want {
			return got
		}
		if time.Now().After(deadline) {
			return count()
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// A 429 landing while the child is already idle — the proxy notification
// racing the failed turn's own result frame — schedules the resume, delivers
// the continuation prompt through the ordinary send path, and publishes the
// retry notices the TUI renders (⟳ on schedule, cleared on fire).
func TestRateLimitResumeSchedulesOnA429WhileIdle(t *testing.T) {
	t.Parallel()
	c := assert.NewAborting(t)
	ctrl := newTestController(t)
	shrinkRateWatch(t, ctrl)

	childID := spawnTestChild(t, ctrl, nil)
	nc, stop := collectNotices(t, ctrl, childID)
	defer stop()

	ctrl.RateLimited(childID, time.Now().Add(50*time.Millisecond))

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && !nc.scheduled(1) {
		time.Sleep(5 * time.Millisecond)
	}
	c.True(nc.scheduled(1), "no will_retry=true schedule notice was published for attempt 1")
	c.GreaterOrEqual(1, waitForResumePrompts(t, ctrl, childID, 1, 2*time.Second), "the auto-resume prompt never reached the child")
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && !nc.fired(1) {
		time.Sleep(5 * time.Millisecond)
	}
	c.True(nc.fired(1), "no will_retry=false firing notice — the rail's ⟳ would never clear")
}

// The idle hook is the other trigger, and the success verdict is its veto: a
// 429 whose turn recovered (a clean completion post-dates it) must never
// schedule, while a 429 the latest success does not post-date must.
func TestMaybeRateLimitResumeSuccessVeto(t *testing.T) {
	t.Parallel()
	c := assert.NewAborting(t)
	ctrl := newTestController(t)
	shrinkRateWatch(t, ctrl)

	childID := spawnTestChild(t, ctrl, nil)

	ctrl.RateLimited(childID, time.Now().Add(50*time.Millisecond))
	ctrl.TurnSucceeded(childID)
	ctrl.maybeRateLimitResume(childID)
	c.Eq(0, waitForResumePrompts(t, ctrl, childID, 1, 300*time.Millisecond), "a success post-dating the 429 must not schedule a resume")

	ctrl.RateLimited(childID, time.Now().Add(50*time.Millisecond))
	ctrl.maybeRateLimitResume(childID)
	c.GreaterOrEqual(1, waitForResumePrompts(t, ctrl, childID, 1, 2*time.Second), "a limit the latest success does not post-date must schedule a resume")
}

// A success arriving while a resume is pending cancels it: the model answered,
// so there is nothing to resume and no prompt may land mid-task. The watch's
// consecutive-attempt count also resets.
func TestTurnSucceededCancelsAPendingResume(t *testing.T) {
	t.Parallel()
	ctrl := newTestController(t)
	shrinkRateWatch(t, ctrl)

	childID := spawnTestChild(t, ctrl, nil)
	ctrl.RateLimited(childID, time.Now().Add(400*time.Millisecond))
	if st := ctrl.rateWatch.state(childID, false); st == nil || st.timer == nil {
		t.Fatal("RateLimited did not arm a pending resume")
	}
	ctrl.TurnSucceeded(childID)

	if st := ctrl.rateWatch.state(childID, false); st == nil || st.timer != nil || st.attempts != 0 {
		t.Fatalf("after a success: timer=%v attempts=%d, want nil/0", st.timer, st.attempts)
	}
	time.Sleep(600 * time.Millisecond)
	assert.NewAborting(t).Eq(0, waitForResumePrompts(t, ctrl, childID, 1, 100*time.Millisecond), "a canceled resume delivered its prompt")
}

// Consecutive rate-limited turns — each resume re-429s with no intervening
// success — are bounded at maxRateLimitResumes; beyond it the scheduler
// publishes an abandonment notice and stops delivering prompts.
func TestRateLimitResumeAttemptsAreCapped(t *testing.T) {
	t.Parallel()
	c := assert.NewAborting(t)
	ctrl := newTestController(t)
	shrinkRateWatch(t, ctrl)

	childID := spawnTestChild(t, ctrl, nil)
	nc, stop := collectNotices(t, ctrl, childID)
	defer stop()

	for attempt := 1; attempt <= maxRateLimitResumes; attempt++ {
		ctrl.RateLimited(childID, time.Now().Add(30*time.Millisecond))
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) && !nc.scheduled(attempt) {
			time.Sleep(5 * time.Millisecond)
		}
		c.True(nc.scheduled(attempt), "resume attempt %d was never scheduled", attempt)
		c.GreaterOrEqual(attempt, waitForResumePrompts(t, ctrl, childID, attempt, 2*time.Second), "resume attempt")
	}

	// One more rate-limited cycle: no fourth schedule, and the abandonment
	// notice is the last word.
	ctrl.RateLimited(childID, time.Now().Add(30*time.Millisecond))
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) && !nc.abandoned() {
		time.Sleep(5 * time.Millisecond)
	}
	c.True(nc.abandoned(), "no abandonment notice after the attempt cap")
	c.Eq(maxRateLimitResumes, waitForResumePrompts(t, ctrl, childID, maxRateLimitResumes+1, 300*time.Millisecond), "a fourth prompt was delivered past the attempt cap")
}

// An exited child is RELAUNCHED, then prompted: the same --resume path a
// manual `rafiki resume` takes, with the continuation prompt delivered to the
// replacement process.
func TestRateLimitResumeRelaunchesAnExitedChild(t *testing.T) {
	t.Parallel()
	c := assert.NewAborting(t)
	ctrl := newTestController(t)
	shrinkRateWatch(t, ctrl)

	childID := spawnTestChild(t, ctrl, nil)
	killCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := ctrl.Kill(killCtx, childID, 2*time.Second, 500*time.Millisecond)
	c.NoError(err, "kill")
	waitForExited(t, ctrl.st, childID, 5*time.Second)

	// Kill's ByShutdown dropped the watch (a deliberate kill must not
	// resurrect); re-arm it to stand in for "exited on its own while limited",
	// which is the state the fire path must handle.
	ctrl.rateWatch.mu.Lock()
	st := ctrl.rateWatch.stateLocked(childID, true)
	st.limitedAt = time.Now().Add(-time.Minute)
	ctrl.rateWatch.mu.Unlock()

	ctrl.fireRateLimitResume(childID, 1)

	_, ok := ctrl.cm.Get(childID)
	c.True(ok, "the exited child was not relaunched by the auto-resume")
	c.GreaterOrEqual(1, waitForResumePrompts(t, ctrl, childID, 1, 2*time.Second), "the relaunched child never received the continuation prompt")
}

// An operator-driven death (Kill, daemon shutdown — anything ByShutdown)
// drops the watch, canceling the pending resume: the kill must not be undone.
func TestKillDropsAPendingResume(t *testing.T) {
	t.Parallel()
	c := assert.NewAborting(t)
	ctrl := newTestController(t)
	shrinkRateWatch(t, ctrl)

	childID := spawnTestChild(t, ctrl, nil)
	ctrl.RateLimited(childID, time.Now().Add(50*time.Millisecond))
	if st := ctrl.rateWatch.state(childID, false); st == nil || st.timer == nil {
		t.Fatal("RateLimited did not arm a pending resume")
	}

	killCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := ctrl.Kill(killCtx, childID, 2*time.Second, 500*time.Millisecond)
	c.NoError(err, "kill")
	// handleChildExit drops the watch before it marks the row exited, so an
	// exited store status proves the drop already ran.
	waitForExited(t, ctrl.st, childID, 5*time.Second)
	c.Nil(ctrl.rateWatch.state(childID, false), "a ByShutdown exit must drop the watch")
}

// The observer gate: an id that resolves to no supervised claude child — an
// interactive `rafiki claude` session's UUID, a hand-configured client's
// header, another daemon's child — must create no watch state at all.
func TestRateLimitedIgnoresUnknownSessions(t *testing.T) {
	t.Parallel()
	ctrl := newTestController(t)

	ctrl.RateLimited("not-a-child-id", time.Now().Add(time.Hour))
	assert.NewAborting(t).Nil(ctrl.rateWatch.state("not-a-child-id", false), "an unresolvable session must not arm the watch")
	ctrl.TurnSucceeded("not-a-child-id")
}

// labelOf returns the child's pending-resume label, or "".
func labelOf(ctrl *Controller, childID string) string {
	snap, ok := ctrl.st.Get(childID)
	if !ok {
		return ""
	}
	return snap.Labels[rateLimitResumeLabel]
}

// setLabel stamps the pending-resume label directly, standing in for the row a
// previous daemon wrote before it died.
func setLabel(t *testing.T, ctrl *Controller, childID string, at time.Time) {
	t.Helper()
	if err := ctrl.st.Update(childID, func(s *childstore.Session) {
		if s.Labels == nil {
			s.Labels = map[string]string{}
		}
		s.Labels[rateLimitResumeLabel] = at.UTC().Format(time.RFC3339)
	}); err != nil {
		t.Fatalf("stamp label: %v", err)
	}
}

// A scheduled resume is stamped on the child's ROW as the fire instant, and
// delivering it clears the stamp: the label is the resume a restart still owes,
// so it must never outlive the delivery (that would nudge the child twice) and
// must never be absent while a timer is pending (that would lose the promise).
func TestRateLimitResumeStampsAndClearsTheRowLabel(t *testing.T) {
	t.Parallel()
	c := assert.NewAborting(t)
	ctrl := newTestController(t)
	shrinkRateWatch(t, ctrl)

	childID := spawnTestChild(t, ctrl, nil)
	ctrl.RateLimited(childID, time.Now().Add(50*time.Millisecond))

	raw := labelOf(ctrl, childID)
	if raw == "" {
		t.Fatal("no pending-resume label was stamped when the resume was scheduled")
	}
	if _, err := time.Parse(time.RFC3339, raw); err != nil {
		t.Fatalf("label %q is not an RFC3339 instant: %v", raw, err)
	}

	c.GreaterOrEqual(1, waitForResumePrompts(t, ctrl, childID, 1, 2*time.Second), "the auto-resume prompt never reached the child")
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && labelOf(ctrl, childID) != "" {
		time.Sleep(5 * time.Millisecond)
	}
	c.Eq("", labelOf(ctrl, childID), "the pending-resume label survived the delivery; a restart would nudge the child twice")
}

// A clean completion is the upstream verdict that cancels the resume, and it
// must take the row's label with it — otherwise the next boot re-arms a retry
// for a child that already answered.
func TestTurnSucceededClearsTheRowLabel(t *testing.T) {
	t.Parallel()
	c := assert.NewAborting(t)
	ctrl := newTestController(t)
	shrinkRateWatch(t, ctrl)

	childID := spawnTestChild(t, ctrl, nil)
	ctrl.RateLimited(childID, time.Now().Add(time.Hour))
	if labelOf(ctrl, childID) == "" {
		t.Fatal("no label stamped")
	}
	ctrl.TurnSucceeded(childID)
	c.Eq("", labelOf(ctrl, childID), "a success must clear the pending-resume label")
}

// A kill is final: the row must not carry a resume a later boot would use to
// resurrect the child the operator stopped.
func TestKillClearsTheRowLabel(t *testing.T) {
	t.Parallel()
	c := assert.NewAborting(t)
	ctrl := newTestController(t)
	shrinkRateWatch(t, ctrl)

	childID := spawnTestChild(t, ctrl, nil)
	ctrl.RateLimited(childID, time.Now().Add(time.Hour))
	if labelOf(ctrl, childID) == "" {
		t.Fatal("no label stamped")
	}

	killCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := ctrl.Kill(killCtx, childID, 2*time.Second, 500*time.Millisecond)
	c.NoError(err, "kill")
	waitForExited(t, ctrl.st, childID, 5*time.Second)
	c.Eq("", labelOf(ctrl, childID), "a kill must clear the pending-resume label")
}

// The boot re-arm: the previous daemon's timer died with it, but the row still
// names the instant, so a restart re-arms the resume and re-publishes the
// notice (which is what puts the retry statement back in a viewer that
// attached after the original one). An instant already in the past fires on the
// buffer floor rather than waiting out another window.
func TestRateLimitResumeRearmsFromTheRowAtBoot(t *testing.T) {
	t.Parallel()
	c := assert.NewAborting(t)
	ctrl := newTestController(t)
	shrinkRateWatch(t, ctrl)

	childID := spawnTestChild(t, ctrl, nil)
	setLabel(t, ctrl, childID, time.Now().Add(-time.Minute))

	nc, stop := collectNotices(t, ctrl, childID)
	defer stop()
	ctrl.armRecoveredRateLimitResumes()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && !nc.scheduled(1) {
		time.Sleep(5 * time.Millisecond)
	}
	c.True(nc.scheduled(1), "boot did not re-publish the pending retry notice")
	c.GreaterOrEqual(1, waitForResumePrompts(t, ctrl, childID, 1, 2*time.Second), "the re-armed resume never reached the child")
}

// Only an idle claude child is re-armed at boot: a mid-turn child gets the
// restart continuation nudge instead, and its row label is left for the idle
// transition to decide on (arming it here would race recovery's own resume).
func TestArmRecoveredRateLimitResumesSkipsANonIdleChild(t *testing.T) {
	t.Parallel()
	c := assert.NewAborting(t)
	ctrl := newTestController(t)
	shrinkRateWatch(t, ctrl)

	childID := spawnTestChild(t, ctrl, nil)
	setLabel(t, ctrl, childID, time.Now().Add(-time.Minute))
	ctrl.st.SetStatus(childID, protocol.StatusStreaming)

	ctrl.armRecoveredRateLimitResumes()

	c.Eq(0, waitForResumePrompts(t, ctrl, childID, 1, 300*time.Millisecond), "a mid-turn child must not be re-armed at boot")
	if labelOf(ctrl, childID) == "" {
		t.Fatal("the label must survive: the idle transition decides on it")
	}
}

// A 429 that re-targets a pending resume — Claude Code retrying inside one
// turn, or a second failed turn before the reset — is the SAME attempt. It must
// re-announce against the newer reset without spending budget: counting
// schedules used to exhaust the three-attempt cap without a single resume ever
// firing, which reads to the operator as "it stopped retrying".
func TestRateLimitResumeRetargetingDoesNotSpendAnAttempt(t *testing.T) {
	t.Parallel()
	c := assert.NewAborting(t)
	ctrl := newTestController(t)
	shrinkRateWatch(t, ctrl)

	childID := spawnTestChild(t, ctrl, nil)
	nc, stop := collectNotices(t, ctrl, childID)
	defer stop()

	// A reset far enough out that nothing fires while the notifications land.
	for i := 0; i < 4; i++ {
		ctrl.RateLimited(childID, time.Now().Add(time.Hour))
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && len(nc.scheduledAttempts()) < 4 {
		time.Sleep(5 * time.Millisecond)
	}
	if got := nc.scheduledAttempts(); len(got) != 4 {
		t.Fatalf("schedule notices = %v, want 4 (one per 429)", got)
	}
	for _, a := range nc.scheduledAttempts() {
		c.Eq(1, a, "a re-targeted resume must stay attempt 1, never count up")
	}
	c.Eq(0, nc.abandonedNotices(), "retargeting must not exhaust the attempt cap")
	c.Eq(0, waitForResumePrompts(t, ctrl, childID, 1, 100*time.Millisecond), "nothing may be delivered before the reset")

	// Now a reset that is already past: it fires on the buffer floor.
	ctrl.RateLimited(childID, time.Now().Add(-time.Hour))
	c.GreaterOrEqual(1, waitForResumePrompts(t, ctrl, childID, 1, 2*time.Second), "the retargeted resume never fired")
	if st := ctrl.rateWatch.state(childID, false); st == nil || st.attempts != 1 {
		t.Fatalf("attempts after one delivery = %v, want 1", st)
	}
}

// Reaching the cap is a terminal answer, and it must reach the TRANSCRIPT:
// the Retry resolution only clears the rail's glyph, so the daemon also
// publishes an Error. And it must do so once per breach, not once per trigger
// (both the 429 notification and the idle transition reach the same branch).
func TestRateLimitResumeAbandonmentIsVisibleAndAnnouncedOnce(t *testing.T) {
	t.Parallel()
	c := assert.NewAborting(t)
	ctrl := newTestController(t)
	shrinkRateWatch(t, ctrl)

	childID := spawnTestChild(t, ctrl, nil)
	nc, stop := collectNotices(t, ctrl, childID)
	defer stop()

	for attempt := 1; attempt <= maxRateLimitResumes; attempt++ {
		ctrl.RateLimited(childID, time.Now().Add(30*time.Millisecond))
		c.GreaterOrEqual(attempt, waitForResumePrompts(t, ctrl, childID, attempt, 2*time.Second), "resume delivery")
	}

	// The fourth limit: no timer is armed now, so BOTH triggers run the
	// abandonment branch.
	ctrl.RateLimited(childID, time.Now().Add(time.Hour))
	ctrl.maybeRateLimitResume(childID)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && len(nc.abandonmentErrors()) == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	errs := nc.abandonmentErrors()
	if len(errs) != 1 {
		t.Fatalf("abandonment Error events = %d, want exactly 1 (got %v)", len(errs), errs)
	}
	c.StrContains(errs[0].GetMessage(), "resume", "the message must say what to do instead")
	c.Eq(1, nc.abandonedNotices(), "the abandonment resolution must be published once, not once per trigger")
	c.Eq(maxRateLimitResumes, waitForResumePrompts(t, ctrl, childID, maxRateLimitResumes+1, 300*time.Millisecond), "a prompt was delivered past the attempt cap")
}
