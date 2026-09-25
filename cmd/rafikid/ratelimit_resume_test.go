// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
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
		}
	}()
	return nc, func() { cancel(); <-done }
}

func (nc *noticeCollector) scheduled(attempt int) bool {
	nc.mu.Lock()
	defer nc.mu.Unlock()
	for _, r := range nc.retries {
		if r.GetWillRetry() && int(r.GetAttempt()) == attempt && strings.Contains(r.GetReason(), "auto-resume scheduled") {
			return true
		}
	}
	return false
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
	if !nc.scheduled(1) {
		t.Fatal("no will_retry=true schedule notice was published for attempt 1")
	}
	if waitForResumePrompts(t, ctrl, childID, 1, 2*time.Second) < 1 {
		t.Fatal("the auto-resume prompt never reached the child")
	}
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && !nc.fired(1) {
		time.Sleep(5 * time.Millisecond)
	}
	if !nc.fired(1) {
		t.Fatal("no will_retry=false firing notice — the rail's ⟳ would never clear")
	}
}

// The idle hook is the other trigger, and the success verdict is its veto: a
// 429 whose turn recovered (a clean completion post-dates it) must never
// schedule, while a 429 the latest success does not post-date must.
func TestMaybeRateLimitResumeSuccessVeto(t *testing.T) {
	t.Parallel()
	ctrl := newTestController(t)
	shrinkRateWatch(t, ctrl)

	childID := spawnTestChild(t, ctrl, nil)

	ctrl.RateLimited(childID, time.Now().Add(50*time.Millisecond))
	ctrl.TurnSucceeded(childID)
	ctrl.maybeRateLimitResume(childID)
	if waitForResumePrompts(t, ctrl, childID, 1, 300*time.Millisecond) != 0 {
		t.Fatal("a success post-dating the 429 must not schedule a resume")
	}

	ctrl.RateLimited(childID, time.Now().Add(50*time.Millisecond))
	ctrl.maybeRateLimitResume(childID)
	if waitForResumePrompts(t, ctrl, childID, 1, 2*time.Second) < 1 {
		t.Fatal("a limit the latest success does not post-date must schedule a resume")
	}
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
	if waitForResumePrompts(t, ctrl, childID, 1, 100*time.Millisecond) != 0 {
		t.Fatal("a canceled resume delivered its prompt")
	}
}

// Consecutive rate-limited turns — each resume re-429s with no intervening
// success — are bounded at maxRateLimitResumes; beyond it the scheduler
// publishes an abandonment notice and stops delivering prompts.
func TestRateLimitResumeAttemptsAreCapped(t *testing.T) {
	t.Parallel()
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
		if !nc.scheduled(attempt) {
			t.Fatalf("resume attempt %d was never scheduled", attempt)
		}
		if waitForResumePrompts(t, ctrl, childID, attempt, 2*time.Second) < attempt {
			t.Fatalf("resume attempt %d never delivered its prompt", attempt)
		}
	}

	// One more rate-limited cycle: no fourth schedule, and the abandonment
	// notice is the last word.
	ctrl.RateLimited(childID, time.Now().Add(30*time.Millisecond))
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) && !nc.abandoned() {
		time.Sleep(5 * time.Millisecond)
	}
	if !nc.abandoned() {
		t.Fatal("no abandonment notice after the attempt cap")
	}
	if waitForResumePrompts(t, ctrl, childID, maxRateLimitResumes+1, 300*time.Millisecond) != maxRateLimitResumes {
		t.Fatal("a fourth prompt was delivered past the attempt cap")
	}
}

// An exited child is RELAUNCHED, then prompted: the same --resume path a
// manual `rafiki resume` takes, with the continuation prompt delivered to the
// replacement process.
func TestRateLimitResumeRelaunchesAnExitedChild(t *testing.T) {
	t.Parallel()
	ctrl := newTestController(t)
	shrinkRateWatch(t, ctrl)

	childID := spawnTestChild(t, ctrl, nil)
	killCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := ctrl.Kill(killCtx, childID, 2000, 500); err != nil {
		t.Fatalf("kill: %v", err)
	}
	waitForExited(t, ctrl.st, childID, 5*time.Second)

	// Kill's ByShutdown dropped the watch (a deliberate kill must not
	// resurrect); re-arm it to stand in for "exited on its own while limited",
	// which is the state the fire path must handle.
	ctrl.rateWatch.mu.Lock()
	st := ctrl.rateWatch.stateLocked(childID, true)
	st.limitedAt = time.Now().Add(-time.Minute)
	ctrl.rateWatch.mu.Unlock()

	ctrl.fireRateLimitResume(childID, 1)

	if _, ok := ctrl.cm.Get(childID); !ok {
		t.Fatal("the exited child was not relaunched by the auto-resume")
	}
	if waitForResumePrompts(t, ctrl, childID, 1, 2*time.Second) < 1 {
		t.Fatal("the relaunched child never received the continuation prompt")
	}
}

// An operator-driven death (Kill, daemon shutdown — anything ByShutdown)
// drops the watch, canceling the pending resume: the kill must not be undone.
func TestKillDropsAPendingResume(t *testing.T) {
	t.Parallel()
	ctrl := newTestController(t)
	shrinkRateWatch(t, ctrl)

	childID := spawnTestChild(t, ctrl, nil)
	ctrl.RateLimited(childID, time.Now().Add(50*time.Millisecond))
	if st := ctrl.rateWatch.state(childID, false); st == nil || st.timer == nil {
		t.Fatal("RateLimited did not arm a pending resume")
	}

	killCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := ctrl.Kill(killCtx, childID, 2000, 500); err != nil {
		t.Fatalf("kill: %v", err)
	}
	// handleChildExit drops the watch before it marks the row exited, so an
	// exited store status proves the drop already ran.
	waitForExited(t, ctrl.st, childID, 5*time.Second)
	if st := ctrl.rateWatch.state(childID, false); st != nil {
		t.Fatal("a ByShutdown exit must drop the watch")
	}
}

// The observer gate: an id that resolves to no supervised claude child — an
// interactive `rafiki claude` session's UUID, a hand-configured client's
// header, another daemon's child — must create no watch state at all.
func TestRateLimitedIgnoresUnknownSessions(t *testing.T) {
	t.Parallel()
	ctrl := newTestController(t)

	ctrl.RateLimited("not-a-child-id", time.Now().Add(time.Hour))
	if st := ctrl.rateWatch.state("not-a-child-id", false); st != nil {
		t.Fatal("an unresolvable session must not arm the watch")
	}
	ctrl.TurnSucceeded("not-a-child-id")
}
