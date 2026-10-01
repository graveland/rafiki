// SPDX-License-Identifier: Apache-2.0

package main

// Recovery's executor gate and claude auto-resume.
//
// A daemon restart's boot sweep runs before any executor has reconnected
// (pkg/execpool/dial.go's reconnect ladder starts at 1s, ceiling 30s), so a
// child whose resume needs a live executor — a daraja-hosted claude launch,
// or a fundi child whose workspace binding cannot be satisfied yet — would
// fail outright and stay exited forever. Instead such a child goes PENDING and
// is retried by a sweep fired on EVERY executor connection: multiple
// executors are standard, not exceptional, and a child's requirement may only
// be satisfiable by the one that connects last. The sweep is cheap when
// nothing is pending; there is no timer and no deadline — a child whose
// executor never returns simply waits, which is the honest state for an agent
// whose machine is gone.
//
// Claude children get the same restart treatment with one addition: their
// interrupted turn cannot be re-issued by agentloop (claude is claude Code's
// own loop), so when the row says the child was WORKING when its daemon died,
// the resumed child gets a continuation prompt through the durable inbox —
// the same relaunch-and-nudge shape the rate-limit auto-resume uses.

import (
	"context"
	"log/slog"
	"slices"
	"sync"
	"time"

	"go.graveland.dev/rafiki/pkg/childstore"
	"go.graveland.dev/rafiki/pkg/inbox"
	"go.graveland.dev/rafiki/pkg/protocol"
)

// preShutdownStatusLabel records, on the row, what a child was doing when its
// daemon's graceful shutdown began. The shutdown writes `shutting_down` over
// every live child's status — idle and mid-turn alike — and the status column
// is recovery's only resume signal, so without this stamp a restarted daemon
// cannot tell "was working, turn lost" from "was idle, nothing lost". The
// claude continuation prompt keys off it. Recovery's RESUME decision never
// reads it: it gates a nudge, not a resume (status gates that).
const preShutdownStatusLabel = "rafiki/pre-shutdown-status"

// claudeRestartResumeText is the prompt a recovered claude child gets when the
// row says it was mid-turn at daemon death. The child re-attaches its full
// session (`--resume`), so the instruction is a nudge, not a re-briefing.
const claudeRestartResumeText = "The daemon restarted while your previous turn was in flight; it was lost mid-request. " +
	"Continue your work from exactly where it stopped — do not restart the task and do not repeat steps you have already completed."

// claudeWasWorkingAtDeath reports whether a recovered claude row's child was
// mid-turn when its daemon died — the signal for the continuation prompt.
//
// rec.Status answers it directly for every working status, except
// `shutting_down`: the graceful path overwrites EVERY live child's status with
// that, so the pre-shutdown status label (stamped at that one transition in
// handleStatusChange) is the real answer for a graceful death. A row with
// neither signal reads idle — no nudge.
func claudeWasWorkingAtDeath(rec childstore.ChildRecord) bool {
	st := rec.Status
	if st == string(protocol.StatusShuttingDown) {
		st = rec.Labels[preShutdownStatusLabel]
	}
	return isWorkingStatus(protocol.Status(st))
}

// pendingResume is one recovered child waiting for its executor to connect.
// The record is everything: the sweep that pops it re-derives the resume
// request from the store, exactly as an immediate launch would.
type pendingResume struct {
	rec childstore.ChildRecord
}

// pendingResumes is the Controller's set of executor-blocked recovery
// resumes.
type pendingResumes struct {
	mu sync.Mutex
	// m is keyed by child id; membership IS the pending state.
	m map[string]pendingResume
}

// recoveryWalkDone flips once loadChildren's walk has classified every row.
// An executor connect arriving DURING the walk would sweep a half-filled
// pending set and miss the rest of the walk's children; the walk's own tail
// sweep covers them, so a mid-walk connect sweeps nothing.
type recoveryGate struct {
	mu   sync.Mutex
	done bool
}

func (g *recoveryGate) markDone() {
	g.mu.Lock()
	g.done = true
	g.mu.Unlock()
}

func (g *recoveryGate) isDone() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.done
}

// pendResume adds pr to the pending set. Called from recoverOne's walk —
// between the store insert and the walk's tail sweep, so no sweep can run
// concurrently with a fill (the walkDone gate keeps mid-walk connects out).
func (c *Controller) pendResume(pr pendingResume) {
	c.pendingResumes.mu.Lock()
	if c.pendingResumes.m == nil {
		c.pendingResumes.m = make(map[string]pendingResume)
	}
	c.pendingResumes.m[pr.rec.ChildID] = pr
	c.pendingResumes.mu.Unlock()
}

// dropPendingResume forgets childID's pending entry — a child that was closed
// or forgotten while waiting for its executor must never be resurrected by a
// sweep.
func (c *Controller) dropPendingResume(childID string) {
	c.pendingResumes.mu.Lock()
	delete(c.pendingResumes.m, childID)
	c.pendingResumes.mu.Unlock()
}

// pendingCount reports how many children are waiting (tests, and the walk's
// end-of-walk decision to log).
func (c *Controller) pendingCount() int {
	c.pendingResumes.mu.Lock()
	defer c.pendingResumes.mu.Unlock()
	return len(c.pendingResumes.m)
}

// claudeExecutorLive reports whether executorID is connected AND advertises
// claude launch support — the same two checks darajaLaunchExecutor's
// resume branch applies, as a side-effect-free probe. Reuse darajaLaunchExecutor
// itself and the probe would LAUNCH a daraja process.
func (c *Controller) claudeExecutorLive(executorID string) bool {
	if c.execPool == nil {
		return false
	}
	for _, le := range c.execPool.Live() {
		if le.Executor.ID != executorID {
			continue
		}
		return le.Describe != nil && slices.Contains(le.Describe.LaunchKinds, "claude")
	}
	return false
}

// resumeWaitsForExecutor reports whether pr's resume cannot succeed until an
// executor connects, and should therefore pend rather than run at boot-scan
// time. Only a daraja-hosted claude launch qualifies: it is the one resume
// that hard-requires a live executor (darajaLaunchExecutor refuses to resume a
// pinned claude child anywhere else).
//
// A fundi child NEVER waits: its binding is lazy — the engine starts unbound
// and binds on first tool use (pinned by
// TestAutoResumeWithNoLiveExecutorStartsUnbound) — so an executor that is not
// connected yet degrades the re-issued cycle's first tool calls, not the
// resume; waiting would strand the child on any daemon where nothing will
// ever connect (a fleet-less or DB-only setup).
//
// A claude child on a daemon with no executor pool takes the
// local-subprocess path: nothing to wait for. One WITH a pool waits for the
// PINNED executor; a row with no rafiki/executor label has no pin to wait for
// and fails outright — attempting it now surfaces that as a warn instead of
// parking a resume that can never fire.
func (c *Controller) resumeWaitsForExecutor(pr pendingResume) bool {
	if pr.rec.Kind != protocol.KindClaude || !c.claudeExecutorRouted() {
		return false
	}
	pinned := pr.rec.Labels["rafiki/executor"]
	if pinned == "" {
		return false
	}
	return !c.claudeExecutorLive(pinned)
}

// launchRecoveryResume resumes one recovered child — now, or after its
// executor connects.
//
// The kind branch is the resume shape: a fundi child's engine re-submits the
// interrupted turn itself (AutoResume), a claude child re-attaches its session
// and gets a continuation prompt only when the row says it was mid-turn.
func (c *Controller) launchRecoveryResume(rec childstore.ChildRecord, own ownership) {
	// A claude conversation has no lease to fence with (only a fundi engine's
	// build acquires one), so the classification IS the safety gate: this
	// daemon auto-resumes a claude row only when it can prove the row is its
	// own past incarnation (the daemon_id is pinned across restarts for
	// exactly this). An adopted row's original daemon may still be alive —
	// foreignLapsed for a claude row is proven by nothing but a stale
	// updated_at — and two claude processes on one session would interleave
	// writes. It loads exited and stays dead, same as before this existed.
	if rec.Kind == protocol.KindClaude && own != ownedByMe {
		slog.Info("claude child recovered from another daemon; not auto-resumed",
			"childId", rec.ChildID, "previousDaemonId", rec.DaemonID, "ownership", own.String())
		return
	}

	// The only field the deferred resume needs beyond the record is the
	// executor-availability predicate's answer, computed at launch time — so
	// the struct carries just the record.
	pr := pendingResume{rec: rec}

	if c.resumeWaitsForExecutor(pr) {
		c.pendResume(pr)
		slog.Info("deferring auto-resume until the child's executor connects",
			"childId", rec.ChildID, "kind", rec.Kind, "executor", rec.Labels["rafiki/executor"])
		return
	}
	c.spawnRecoveryResume(pr)
}

// spawnRecoveryResume takes childID's claim and runs the resume on its own
// goroutine — recoverOne's launch path, branched by kind.
//
// The claim is taken HERE, before the goroutine starts, not inside the
// resume: the row was just stored exited, and the window between that write
// and a claim taken on another goroutine is one in which a send (a sibling's
// settle fragment) is rejected as "child has exited" — see validateSendTarget.
// The goroutine releases it.
func (c *Controller) spawnRecoveryResume(pr pendingResume) {
	id := pr.rec.ChildID
	if c.stopping.Load() {
		// The daemon is dying: a child spawned now would miss the shutdown
		// ladder (ShutdownAllChildren already collected its ids) and survive
		// as an orphan. The next boot re-walks the row and resumes it.
		slog.Info("recovery resume skipped; the daemon is stopping", "childId", id)
		return
	}
	if !c.spawnClaims.tryClaim(id) {
		slog.Warn("resume already in progress; not auto-resuming", "childId", id)
		return
	}
	slog.Info("auto-resuming child", "childId", id, "kind", pr.rec.Kind)
	go func() {
		defer c.spawnClaims.release(id)
		rctx, cancel := context.WithTimeout(c.baseCtx, 60*time.Second)
		defer cancel()

		if pr.rec.Kind == protocol.KindClaude {
			// A claude child has no engine to re-issue its interrupted turn;
			// its unconfirmed inbox rows must be returned to pending BEFORE
			// the resumed child can mark anything 'sent', or rows left 'sent'
			// by the dead daemon strand (only a fundi engine's
			// OnConversationResolved resets them).
			c.releaseInboxOnExit(id)
			if _, err := c.resumeClaimed(rctx, id, "", false); err != nil {
				slog.Warn("auto-resume failed; child stays exited", "childId", id, "error", err)
				c.dropLease(id)
				return
			}
			// The nudge rides the durable inbox: a child that dies again
			// before reading it replays it on its next resume.
			if claudeWasWorkingAtDeath(pr.rec) {
				if err := c.sendClaudeNudge(id, claudeRestartResumeText); err != nil {
					slog.Warn("auto-resume continuation prompt failed; it stays queued",
						"childId", id, "error", err)
				}
			}
			// No lease to gate on (a claude child never holds one), so
			// replay runs unconditionally: ownership was already decided by
			// the walk's classification, which is what the fundi branch's
			// holdsLease check stands in for. replayInbox catches the
			// fragment-sourced rows the resumed child's own idle drain
			// deliberately leaves alone.
			rctx2, cancel2 := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel2()
			c.replayInbox(rctx2, id)
			return
		}

		if _, err := c.resumeClaimedWithAutoRecovery(rctx, id); err != nil {
			slog.Warn("auto-resume failed; child stays exited", "childId", id, "error", err)
			c.dropLease(id)
			return
		}
		// A success return does not prove THIS daemon owns the child: the
		// in-process engine build runs on its own goroutine (Runner.Start
		// returns before Build completes), and activateLiveChild's
		// Idle-or-5s-timeout select cannot tell "became idle" apart from
		// "the build already failed, including on a refused lease". Every
		// child row is visible to every daemon (loadChildren lists the whole
		// table), so a walked row may belong to another daemon that is live
		// RIGHT NOW; without this check a lease refusal there still replays
		// as though it succeeded, flipping the OTHER daemon's live child's
		// 'sent' rows to 'pending' and stranding them.
		//
		// The gate applies only when leasing is actually in play — the same
		// condition OnConversationResolved itself uses to decide whether to
		// acquire at all. A daemon with no identity (c.daemonID == "") never
		// tracks a lease for anything and is already unfenced by design in
		// that case (see NewController), so it has nothing to gate on.
		if c.daemonID != "" && !c.holdsLease(id) {
			slog.Warn("auto-resume reported success without holding this child's lease; "+
				"not replaying its inbox", "childId", id)
			return
		}
		rctx2, cancel2 := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel2()
		c.replayInbox(rctx2, id)
	}()
}

// sweepPendingResumes resumes every pending child whose executor requirement
// is now satisfiable. Fired on EVERY executor connect and once at the end of
// the boot walk. Pending entries that are still unsatisfied stay; a resumed
// child is removed before its goroutine starts, so a later sweep never
// double-launches one.
func (c *Controller) sweepPendingResumes() {
	if !c.recoveryWalk.isDone() {
		return // the walk's own tail sweep covers whatever it is still filling
	}
	if c.stopping.Load() {
		return // the daemon is dying; nothing it spawns would be shut down
	}
	for {
		pr, ok := c.takeResumablePending()
		if !ok {
			return
		}
		c.spawnRecoveryResume(pr)
	}
}

// takeResumablePending pops the first pending child whose executor requirement
// is satisfiable right now, removing it from the set.
func (c *Controller) takeResumablePending() (pendingResume, bool) {
	c.pendingResumes.mu.Lock()
	defer c.pendingResumes.mu.Unlock()
	for id, pr := range c.pendingResumes.m {
		if c.resumeWaitsForExecutor(pr) {
			continue
		}
		delete(c.pendingResumes.m, id)
		return pr, true
	}
	return pendingResume{}, false
}

// sendClaudeNudge delivers an automatic continuation prompt to a claude child
// through the durable inbox — the delivery half of both automatic claude
// resumes: the rate limit's (deliverRateLimitResume) and a daemon restart's
// (spawnRecoveryResume). A failed delivery leaves the row pending; the next
// resume replays it.
func (c *Controller) sendClaudeNudge(childID, text string) error {
	frame, err := buildInjectionFrame(inbox.Batch{Mode: inbox.ModePrompt, Frags: []string{text}}, "")
	if err != nil {
		return err
	}
	return c.Send(childID, frame)
}
