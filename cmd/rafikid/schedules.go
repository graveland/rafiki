// SPDX-License-Identifier: Apache-2.0

package main

// The schedules runtime: the daemon-side binding behind the CronCreate /
// CronDelete / CronList / ScheduleWakeup tools, and the fire loop that turns a
// due row into an inbox fragment.
//
// A schedule is a durable row (conversations.schedule, pkg/schedulesdb) plus a
// timer in the daemon's scheduler. The row is the truth; the timer is a hint.
// Every fire path re-reads the row before it acts, so a stale timer (the row
// was deleted, the child was closed, another daemon owns it) is a wasted
// wakeup rather than a bug. Fires are PUSHED into the event buffer BEFORE the
// row advances, so a crash between the two is a replay the event key collapses
// while the first fragment is still pending.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"go.graveland.dev/rafiki/pkg/fundi/tools"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/schedules"
)

// scheduleEventSource is the event-buffer source a fired schedule rides.
const scheduleEventSource = "schedule"

// scheduleKey is the scheduler key for schedule id. armSchedule and
// cancelTimer both key off it, so a re-arm replaces the pending fire.
func scheduleKey(id string) string { return "schedule:" + id }

// holdClearKey is the scheduler key for a held-clear bookkeeping retry. It is
// deliberately DISTINCT from scheduleKey(id): the retry must never replace the
// job's independently armed next-fire timer.
func holdClearKey(id string) string { return "schedule-holdclear:" + id }

// scheduleGate serializes the boot walk's catch-up registrations with the
// budget seed. A catch-up registration that arrives before openScheduleGate
// runs is queued; openScheduleGate seeds the breach set, then drains the queue,
// so no recovered fire is evaluated against an unseeded budget.
type scheduleGate struct {
	mu      sync.Mutex
	open    bool
	pending []string
}

// SetScheduleStore wires the schedule table and resolves the daemon's zone
// once. Called from main before loadChildren. A daemon with no database has no
// schedules: the tools decline.
func (c *Controller) SetScheduleStore(st schedules.Store) {
	c.schedules = st
	zone, err := schedules.DaemonZone()
	if err != nil {
		slog.Warn("schedules: daemon timezone unresolved; CronCreate refuses for owners with no timezone", "error", err)
		return
	}
	c.daemonZone = zone
}

// --- the tools binding ---

// scheduleBinding is the tools.ScheduleManager bound to ONE child. childID is
// closed over at construction and never read from an argument, so a tool call
// cannot address another child's jobs.
type scheduleBinding struct {
	c       *Controller
	childID string
}

func newScheduleBinding(c *Controller, childID string) tools.ScheduleManager {
	return &scheduleBinding{c: c, childID: childID}
}

// zone resolves the zone a job is created in: the owner's users.timezone when
// their row names one, otherwise the daemon's own zone. A job whose owner has
// no zone of their own means the machine's wall clock, so an unresolvable
// daemon zone is ErrNoTimezone rather than a silent UTC.
func (b *scheduleBinding) zone(ctx context.Context) (string, error) {
	if snap, ok := b.c.st.Get(b.childID); ok && snap.OwnerUserID != "" {
		if tz, ok, err := b.c.schedules.OwnerTimezone(ctx, snap.OwnerUserID); err == nil && ok {
			return tz, nil
		}
	}
	if b.c.daemonZone != "" {
		return b.c.daemonZone, nil
	}
	return "", fmt.Errorf("%w: run 'rafikid user set-timezone <name> <iana-zone>' for this agent's owner, or set TZ for the daemon", schedules.ErrNoTimezone)
}

func (b *scheduleBinding) CronCreate(ctx context.Context, cron, prompt string, recurring bool) (tools.ScheduleInfo, error) {
	tz, err := b.zone(ctx)
	if err != nil {
		return tools.ScheduleInfo{}, err
	}
	spec, err := schedules.Parse(cron, tz)
	if err != nil {
		return tools.ScheduleInfo{}, err
	}
	now := b.c.now()
	if recurring {
		if err := spec.CheckMinGap(now, schedules.MinGap); err != nil {
			return tools.ScheduleInfo{}, err
		}
	}
	next := spec.Next(now)
	if next.IsZero() {
		return tools.ScheduleInfo{}, schedules.ErrNeverFires
	}
	id, err := schedules.NewID()
	if err != nil {
		return tools.ScheduleInfo{}, err
	}
	job, err := b.c.schedules.Create(ctx, schedules.Job{
		ID: id, ChildID: b.childID, Kind: schedules.KindCron, Cron: cron, TZ: tz,
		Prompt: prompt, Recurring: recurring, NextFireAt: next,
	})
	if err != nil {
		return tools.ScheduleInfo{}, wrapScheduleCreateErr(err)
	}
	b.c.armSchedule(job)
	return scheduleInfo(job), nil
}

func (b *scheduleBinding) ScheduleWakeup(ctx context.Context, delay time.Duration, prompt string) (tools.ScheduleInfo, error) {
	// A wakeup needs no zone — it fires at a fixed instant — but the column is
	// never empty, so an unresolvable owner/daemon zone degrades to UTC here
	// rather than refusing the wakeup.
	tz, err := b.zone(ctx)
	if err != nil {
		tz = "UTC"
	}
	d := schedules.ClampWakeup(delay)
	id, err := schedules.NewID()
	if err != nil {
		return tools.ScheduleInfo{}, err
	}
	job, err := b.c.schedules.Create(ctx, schedules.Job{
		ID: id, ChildID: b.childID, Kind: schedules.KindWakeup, TZ: tz,
		Prompt: prompt, Recurring: false, NextFireAt: b.c.now().Add(d),
	})
	if err != nil {
		return tools.ScheduleInfo{}, wrapScheduleCreateErr(err)
	}
	b.c.armSchedule(job)
	return scheduleInfo(job), nil
}

func (b *scheduleBinding) CancelWakeup(ctx context.Context) (bool, error) {
	return b.c.schedules.CancelWakeup(ctx, b.childID)
}

func (b *scheduleBinding) CronDelete(ctx context.Context, id string) error {
	if err := b.c.schedules.Delete(ctx, b.childID, id); err != nil {
		if errors.Is(err, schedules.ErrNotFound) {
			return fmt.Errorf("no scheduled job %q", id)
		}
		return err
	}
	b.c.cancelTimer(scheduleKey(id))
	return nil
}

func (b *scheduleBinding) CronList(ctx context.Context) ([]tools.ScheduleInfo, error) {
	jobs, err := b.c.schedules.ListOpen(ctx, b.childID)
	if err != nil {
		return nil, err
	}
	out := make([]tools.ScheduleInfo, 0, len(jobs))
	for _, j := range jobs {
		out = append(out, scheduleInfo(j))
	}
	return out, nil
}

// wrapScheduleCreateErr turns the store's cap refusal into the tool-facing
// advice a model can act on.
func wrapScheduleCreateErr(err error) error {
	if errors.Is(err, schedules.ErrCapReached) {
		return fmt.Errorf("you already have %d scheduled jobs; delete one with CronDelete first", schedules.MaxOpenPerChild)
	}
	return err
}

func scheduleInfo(j schedules.Job) tools.ScheduleInfo {
	return tools.ScheduleInfo{
		ID:        j.ID,
		Kind:      j.Kind,
		Cron:      j.Cron,
		TZ:        j.TZ,
		Prompt:    j.Prompt,
		Recurring: j.Recurring,
		NextFire:  j.NextFireAt,
		HeldCount: j.HeldCount,
	}
}

// --- arming and firing ---

// armSchedule registers job's next fire. It arms every open job, held
// one-shots included: an overdue held one-shot fires immediately, and the
// fire callback either holds it again (still halted — without re-counting)
// or delivers it with its held note. That is what keeps a one-shot held
// across a kill from being stranded when its budget was raised while it was
// exited (release skips exited children).
func (c *Controller) armSchedule(j schedules.Job) {
	id := j.ID
	c.at(scheduleKey(id), j.NextFireAt, func() { c.fireSchedule(id) })
}

// fireSchedule is the timer callback: the durable row decides what happens.
// Every early return below is a guard against a stale timer — the row was
// deleted, its child closed, or moved on since the timer was armed.
func (c *Controller) fireSchedule(id string) {
	if c.schedules == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	row, err := c.schedules.Get(ctx, id)
	if err != nil {
		if errors.Is(err, schedules.ErrNotFound) {
			return
		}
		slog.Warn("schedule: load failed; retrying", "id", id, "error", err)
		c.at(scheduleKey(id), c.now().Add(time.Minute), func() { c.fireSchedule(id) })
		return
	}
	j := row.Job
	if j.ClosedAt != nil {
		return // deleted or already fired
	}
	if !row.ChildFound || row.ChildClosed {
		// The child is gone for good: tombstone and stop, never re-arm.
		if err := c.schedules.Tombstone(ctx, id, nil); err != nil {
			slog.Warn("schedule: tombstone failed", "id", id, "error", err)
		}
		return
	}
	if c.daemonID != "" && row.ChildDaemonID != c.daemonID {
		return // another daemon's child; leave the row untouched
	}
	snap, ok := c.st.Get(j.ChildID)
	if !ok || snap.Status == protocol.StatusExited {
		// Not live here: the next resume re-registers. Do not tombstone — the
		// job is still the child's.
		return
	}

	now := c.now()
	if now.Before(j.NextFireAt) {
		// The timer fired early, or the row moved since it was armed.
		c.armSchedule(j)
		return
	}
	due := j.NextFireAt

	var missed int
	var lastMissed time.Time
	var next time.Time
	if j.Recurring {
		spec, perr := schedules.Parse(j.Cron, j.TZ)
		if perr != nil {
			// An unparseable stored row can never fire; tombstone it.
			slog.Warn("schedule: stored cron unparseable; tombstoning", "id", id, "error", perr)
			if err := c.schedules.Tombstone(ctx, id, &due); err != nil {
				slog.Warn("schedule: tombstone failed", "id", id, "error", err)
			}
			return
		}
		missed, lastMissed = spec.CountThrough(due, now, 10000)
		next = spec.Next(now)
	}

	if root, halted := c.budgetHalted(j.ChildID); halted {
		if j.Recurring {
			// A zero next means the stored expression has no future occurrence;
			// hold without moving NextFireAt (a nil next leaves it untouched)
			// rather than writing a zero instant.
			var nextPtr *time.Time
			if !next.IsZero() {
				nextPtr = &next
			}
			if err := c.schedules.RecordHeld(ctx, id, due, 1+missed, nextPtr); err != nil {
				slog.Warn("schedule: record held failed", "id", id, "error", err)
			}
			if !next.IsZero() {
				j.NextFireAt = next
				c.armSchedule(j)
			}
		} else if j.HeldCount == 0 {
			// A re-armed held one-shot is the same single fire, not a new one.
			if err := c.schedules.RecordHeld(ctx, id, due, 1, nil); err != nil {
				// A failed hold on a one-shot leaves it unheld and unarmed until
				// the next boot; this warn is its only visible trace.
				slog.Warn("schedule: record held failed", "id", id, "error", err)
			}
		}
		slog.Info("schedule: fire held; agent tree over budget", "id", id, "childId", j.ChildID, "root", root)
		return
	}

	// Push FIRST: advancing before pushing would lose the fire on a crash
	// between the two. A crash after the push but before the advance is a
	// replay that the key collapses while the first row is still pending.
	if c.evbuf == nil {
		slog.Warn("schedule: event buffer unavailable; fire dropped", "id", id)
		return
	}
	key := id + "@" + due.UTC().Format(time.RFC3339)
	c.evbuf.Push(j.ChildID, scheduleEventSource, key, renderScheduleFrame(j, due, missed, lastMissed))

	// Then advance.
	if j.Recurring && !next.IsZero() {
		if err := c.schedules.Advance(ctx, id, next, &now); err != nil {
			slog.Warn("schedule: advance failed; retrying", "id", id, "error", err)
			c.at(scheduleKey(id), now.Add(time.Minute), func() { c.retryAdvance(id, next, now) })
			return
		}
		j.NextFireAt = next
		c.armSchedule(j)
		return
	}
	if err := c.schedules.Tombstone(ctx, id, &now); err != nil {
		slog.Warn("schedule: tombstone failed; retrying", "id", id, "error", err)
		c.at(scheduleKey(id), now.Add(time.Minute), func() { c.retryTombstone(id, now) })
	}
}

// retryAdvance retries a failed Advance without re-pushing. The first push was
// already delivered (or is still pending and will be), so re-pushing a minute
// on would deliver the fire twice; only the row move is retried. On success it
// arms the next fire.
func (c *Controller) retryAdvance(id string, next time.Time, firedAt time.Time) {
	if c.schedules == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := c.schedules.Advance(ctx, id, next, &firedAt); err != nil {
		slog.Warn("schedule: advance retry failed", "id", id, "error", err)
		c.at(scheduleKey(id), c.now().Add(time.Minute), func() { c.retryAdvance(id, next, firedAt) })
		return
	}
	c.armSchedule(schedules.Job{ID: id, NextFireAt: next})
}

// retryTombstone retries a failed Tombstone without re-pushing, the same shape
// as retryAdvance.
func (c *Controller) retryTombstone(id string, firedAt time.Time) {
	if c.schedules == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := c.schedules.Tombstone(ctx, id, &firedAt); err != nil {
		slog.Warn("schedule: tombstone retry failed", "id", id, "error", err)
		c.at(scheduleKey(id), c.now().Add(time.Minute), func() { c.retryTombstone(id, firedAt) })
	}
}

// retryClearHeld retries a failed held-clear WITHOUT re-pushing: the release
// frame was already delivered when ClearHeld failed, so only the row's held
// state is cleared. It runs under holdClearKey — distinct from the job's
// next-fire key — so it never touches an independently armed fire timer. It
// first re-reads the row and stops when the row is gone or closed: a row
// closed in between has no held state left to clear.
func (c *Controller) retryClearHeld(id string) {
	if c.schedules == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	row, err := c.schedules.Get(ctx, id)
	if err != nil {
		if errors.Is(err, schedules.ErrNotFound) {
			return // gone; nothing to clear
		}
		slog.Warn("schedule: held-clear retry load failed", "id", id, "error", err)
		c.at(holdClearKey(id), c.now().Add(time.Minute), func() { c.retryClearHeld(id) })
		return
	}
	if row.Job.ClosedAt != nil {
		return // closed in between; nothing to clear
	}
	if err := c.schedules.ClearHeld(ctx, id); err != nil {
		if errors.Is(err, schedules.ErrNotFound) {
			return
		}
		slog.Warn("schedule: clear held retry failed", "id", id, "error", err)
		c.at(holdClearKey(id), c.now().Add(time.Minute), func() { c.retryClearHeld(id) })
	}
}

// renderScheduleFrame renders the inbox fragment a fire delivers: a header
// naming the job, the prompt, then optional notes for missed occurrences and
// held fires. loc falls back to UTC when the stored zone cannot be loaded — a
// rendering failure must never swallow a fire. Times are RFC3339 in loc.
func renderScheduleFrame(j schedules.Job, due time.Time, missed int, lastMissed time.Time) string {
	loc, err := time.LoadLocation(j.TZ)
	if err != nil {
		loc = time.UTC
	}
	var b strings.Builder
	if j.Kind == schedules.KindWakeup {
		fmt.Fprintf(&b, "[scheduled wakeup %s, due %s]", j.ID, due.In(loc).Format(time.RFC3339))
	} else {
		fmt.Fprintf(&b, "[scheduled job %s, cron %q in %s, due %s]", j.ID, j.Cron, j.TZ, due.In(loc).Format(time.RFC3339))
	}
	b.WriteString("\n")
	b.WriteString(j.Prompt)
	if missed > 0 {
		fmt.Fprintf(&b, "\n(%d later occurrence(s) up to %s were missed while the daemon was down; this one delivery covers them)",
			missed, lastMissed.In(loc).Format(time.RFC3339))
	}
	if j.HeldCount > 0 {
		first, last := "", ""
		if j.HeldFirstDue != nil {
			first = j.HeldFirstDue.In(loc).Format(time.RFC3339)
		}
		if j.HeldLastDue != nil {
			last = j.HeldLastDue.In(loc).Format(time.RFC3339)
		}
		fmt.Fprintf(&b, "\n(%d fire(s) were held while the agent tree was over budget, first due %s, last due %s)",
			j.HeldCount, first, last)
	}
	return b.String()
}

// --- release on a budget raise ---

// releaseHeldSchedules delivers the held fires of rootID's subtree once the
// budget no longer halts them: ONE fragment per job, then the held fields
// clear and released one-shots tombstone.
func (c *Controller) releaseHeldSchedules(rootID string) {
	if c.schedules == nil {
		return
	}
	ids := []string{rootID}
	for _, s := range c.st.Descendants(rootID) {
		ids = append(ids, s.ChildID)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	jobs, err := c.schedules.ListHeld(ctx, ids)
	if err != nil {
		slog.Warn("schedule: list held failed", "rootId", rootID, "error", err)
		return
	}
	now := c.now()
	for _, j := range jobs {
		if _, halted := c.budgetHalted(j.ChildID); halted {
			continue // still blocked by this or an ancestor's budget
		}
		snap, ok := c.st.Get(j.ChildID)
		if !ok || snap.Status == protocol.StatusExited {
			continue // released on the next resume instead
		}
		row, err := c.schedules.Get(ctx, j.ID)
		if err != nil {
			slog.Warn("schedule: release load failed; skipping", "id", j.ID, "childId", j.ChildID, "error", err)
			continue
		}
		if c.daemonID != "" && row.ChildDaemonID != c.daemonID {
			continue // another daemon's child
		}
		if c.evbuf == nil {
			return
		}
		lastDue := j.NextFireAt
		if j.HeldLastDue != nil {
			lastDue = *j.HeldLastDue
		}
		key := j.ID + "@held@" + lastDue.UTC().Format(time.RFC3339)
		c.evbuf.Push(j.ChildID, scheduleEventSource, key, renderScheduleFrame(j, lastDue, 0, time.Time{}))
		if j.Recurring {
			if err := c.schedules.ClearHeld(ctx, j.ID); err != nil {
				// Bookkeeping-only retry under a key DISTINCT from the job's
				// next-fire timer, so it can never replace that timer.
				slog.Warn("schedule: clear held failed; retrying", "id", j.ID, "error", err)
				c.at(holdClearKey(j.ID), c.now().Add(time.Minute), func() { c.retryClearHeld(j.ID) })
			}
		} else if err := c.schedules.Tombstone(ctx, j.ID, &now); err != nil {
			// The held one-shot's release tombstone: reuse the ordinary
			// tombstone retry, which never re-pushes.
			slog.Warn("schedule: release tombstone failed; retrying", "id", j.ID, "error", err)
			c.at(scheduleKey(j.ID), c.now().Add(time.Minute), func() { c.retryTombstone(j.ID, now) })
		}
	}
}

// --- registration on resume, and the startup gate ---

// registerSchedules arms childID's open jobs as it comes up on this daemon.
// catchUp=true (auto-recovery): overdue rows are armed as-is, so the fire
// callback's CountThrough turns N missed occurrences into ONE wake.
// catchUp=false (manual resume): an overdue RECURRING row is first advanced
// to its next FUTURE occurrence (dropping its held count — a kill ends the
// pile); an overdue one-shot still fires once.
// Catch-up registrations made before the boot walk finishes are queued and
// run by openScheduleGate, after the budget seed.
func (c *Controller) registerSchedules(ctx context.Context, childID string, catchUp bool) {
	if c.schedules == nil {
		return
	}
	if catchUp {
		c.schedGate.mu.Lock()
		if !c.schedGate.open {
			c.schedGate.pending = append(c.schedGate.pending, childID)
			c.schedGate.mu.Unlock()
			return
		}
		c.schedGate.mu.Unlock()
	}
	jobs, err := c.schedules.ListOpen(ctx, childID)
	if err != nil {
		slog.Warn("schedule: list open failed", "childId", childID, "error", err)
		return
	}
	now := c.now()
	for _, j := range jobs {
		if !catchUp && j.Recurring && !j.NextFireAt.After(now) {
			spec, perr := schedules.Parse(j.Cron, j.TZ)
			if perr != nil {
				slog.Warn("schedule: stored cron unparseable on resume; tombstoning", "id", j.ID, "error", perr)
				if err := c.schedules.Tombstone(ctx, j.ID, nil); err != nil {
					slog.Warn("schedule: resume tombstone failed", "id", j.ID, "error", err)
				}
				continue
			}
			next := spec.Next(now)
			if next.IsZero() {
				if err := c.schedules.Tombstone(ctx, j.ID, nil); err != nil {
					slog.Warn("schedule: resume tombstone failed", "id", j.ID, "error", err)
				}
				continue
			}
			if err := c.schedules.Advance(ctx, j.ID, next, nil); err != nil {
				slog.Warn("schedule: resume advance failed", "id", j.ID, "error", err)
				continue
			}
			j.NextFireAt = next
			j.HeldCount = 0
		}
		c.armSchedule(j)
	}
}

// openScheduleGate seeds the breach set, then runs every queued catch-up
// registration. It rides loadChildren's defer so the seed runs on every exit
// path, and a recovered fire is never evaluated before the budget is known.
func (c *Controller) openScheduleGate(ctx context.Context) {
	c.seedBudgetBreaches(ctx)
	c.schedGate.mu.Lock()
	c.schedGate.open = true
	pending := c.schedGate.pending
	c.schedGate.pending = nil
	c.schedGate.mu.Unlock()
	for _, id := range pending {
		c.registerSchedules(ctx, id, true)
	}
}
