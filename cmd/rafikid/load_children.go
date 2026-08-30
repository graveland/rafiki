// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"log/slog"
	"syscall"
	"time"

	"go.graveland.dev/rafiki/pkg/childstore"
	"go.graveland.dev/rafiki/pkg/protocol"
)

// leaseTTL and leaseRenewInterval: five renewal attempts inside one TTL, so a
// single slow query never drops a healthy lease. The TTL is long because it
// only ever gates takeover by a DIFFERENT holder — a restarted daemon with an
// unchanged id reclaims its own leases instantly (see LeaseStore.Acquire).
const (
	leaseTTL           = 5 * time.Minute
	leaseRenewInterval = 60 * time.Second
)

// recoveryPlan is what to do with one recovered child.
type recoveryPlan int

const (
	// planStayExited loads the child as exited and does not resume it.
	planStayExited recoveryPlan = iota
	// planResumeBound resumes the child with its executor identity intact.
	// The stale workspace id is stripped, but rafiki/executor is kept so
	// boundExecutor.doRecover can re-provision on the SAME executor when it
	// reconnects. Used for pinned children whose machine is restarting
	// alongside the daemon — they cannot change machines, but they CAN
	// re-provision on the one they were already on.
	planResumeBound
	// planRebindUnbound clears the dead executor binding and resumes the child
	// unbound, so it rebinds on first workspace-tool use.
	planRebindUnbound
)

// shouldAutoResume reports whether a recovered record is a fundi child that was
// ALIVE when this daemon stopped writing to it.
//
// It reads last_status when there is one, and falls back to status when there
// is not. The fallback is the whole point: last_status is written ONLY by
// handleChildExit, so an empty value means the daemon never got to record an
// exit — SIGKILL, OOM, a node eviction. That is the strongest possible evidence
// the child was alive, and it is the case a pod restart actually hits, so
// reading it as "do not resume" broke recovery for the scenario this design
// exists for.
//
// A child was alive when its effective status is neither "exited" (it stopped
// on its own) nor "shutting_down" (the daemon stopped it deliberately). Note
// handleChildExit substitutes PreShutdownStatus for "shutting_down", so a child
// the daemon gracefully stopped still records "idle"/"streaming" and still
// resumes — stopping the daemon is not the same as stopping the child.
func shouldAutoResume(rec childstore.ChildRecord) bool {
	if rec.Kind != protocol.KindFundi {
		return false
	}
	effective := rec.LastStatus
	if effective == "" {
		effective = rec.Status
	}
	if effective == "" {
		return false
	}
	return effective != string(protocol.StatusExited) &&
		effective != string(protocol.StatusShuttingDown)
}

// ownership is recovery's answer to "may this daemon run this child".
type ownership int

const (
	// ownedByMe: this daemon's own row. The k8s pod-restart path.
	// RAFIKI_DAEMON_ID is pinned across restarts precisely so a daemon
	// reclaims its own work immediately.
	ownedByMe ownership = iota
	// unclaimed: no daemon has ever stamped the row. Rows written before
	// 0021, or by a daemon with no identity.
	unclaimed
	// foreignLive: another daemon claims it and is still live on the
	// conversation. Do not touch — not the process, not the inbox.
	foreignLive
	// foreignLapsed: another daemon claims it but its lease has lapsed.
	// Adoptable; this is what keeps k8s failover and replicas>1 reachable.
	foreignLapsed
)

func (o ownership) String() string {
	switch o {
	case ownedByMe:
		return "ownedByMe"
	case unclaimed:
		return "unclaimed"
	case foreignLive:
		return "foreignLive"
	case foreignLapsed:
		return "foreignLapsed"
	}
	return "unknown"
}

// recoveryOwnership classifies one child row for recovery.
//
// Every daemon sharing a database sees every row — childstoredb's listSQL is
// `FROM conversations.child` with no WHERE clause — so this predicate, not the
// lease, is what stops a daemon from running someone else's child. The lease
// refusal is still there and still correct; it is simply not sufficient on its
// own, because activateLiveChild cannot distinguish "became idle" from "the
// engine build already failed, including on a refused lease", so
// resumeWithAutoRecovery returns success either way.
//
// It reads rec.DaemonID, the authoritative column, never the rafiki/daemon
// label. The label mirrors it for display under the same condition; a guard
// that reads a display field is a guard waiting to be broken by a cosmetic
// change.
//
// Evaluation order is load-bearing — see the test.
func recoveryOwnership(rec childstore.ChildRecord, me string, live map[string]bool) ownership {
	if rec.DaemonID == "" {
		return unclaimed
	}
	if rec.DaemonID == me {
		return ownedByMe
	}
	// A daemon with no identity cannot prove any claim is its own, so it
	// declines every claimed row. This also closes the hole where c.daemonID
	// == "" skipped the holdsLease gate entirely and replayed another
	// daemon's inbox.
	if me == "" {
		return foreignLive
	}
	if rec.ConversationID != "" && live[rec.ConversationID] {
		return foreignLive
	}
	return foreignLapsed
}

// recoveryAction decides what to do with a resumable child whose executor
// binding is stale by construction — an executor's workspace registry is in
// memory, so a restart loses every workspace id.
//
// A PINNED child is never MOVED to a different machine. HandleExecutorLost
// fails a pinned child where it stood and boundExecutor.recover refuses to
// re-select for one; recovery must not become the single path that quietly
// migrates one. But a pinned child CAN resume when its machine restarts
// alongside the daemon — boundExecutor.doRecover already re-provisions on the
// same executor when IsLive returns true. An unknown mode is pinned, matching
// workspaceModeOrPinned.
func recoveryAction(rec childstore.ChildRecord) recoveryPlan {
	if !shouldAutoResume(rec) {
		return planStayExited
	}
	if rec.WorkspaceMode == "ephemeral" {
		return planRebindUnbound
	}
	return planResumeBound
}

// stripStaleBindings removes the dead executor binding from a recovered child's
// labels and marks it unbound, which is the state a parented spawn whose
// selector matched nothing already uses.
func stripStaleBindings(labels map[string]string) map[string]string {
	out := make(map[string]string, len(labels)+1)
	for k, v := range labels {
		if k == "rafiki/workspace" || k == "rafiki/executor" {
			continue
		}
		out[k] = v
	}
	out["rafiki/executor-state"] = "unbound"
	return out
}

// stripStaleWorkspace removes only the dead workspace id from a recovered
// child's labels, keeping the executor identity. Used for pinned children:
// the executor restarted alongside the daemon and will reconnect under the same
// id, so the label stays as a hint for boundExecutor.doRecover's same-executor
// re-provision path.
func stripStaleWorkspace(labels map[string]string) map[string]string {
	out := make(map[string]string, len(labels)+1)
	for k, v := range labels {
		if k == "rafiki/workspace" {
			continue
		}
		out[k] = v
	}
	out["rafiki/executor-state"] = "unbound"
	return out
}

// loadChildren rebuilds the in-memory store from the database and resumes the
// children this daemon wins a lease for.
//
// With no database pool, children live in memory only and do not survive a
// restart — the store starts empty.
func (c *Controller) loadChildren(ctx context.Context) {
	if c.children == nil {
		return
	}

	recs, err := c.children.List(ctx)
	if err != nil {
		slog.Error("load children from database failed", "error", err)
		return
	}

	for _, rec := range recs {
		c.recoverOne(ctx, rec)
	}
}

// recoverOne loads a single record into the store and decides whether to resume.
func (c *Controller) recoverOne(ctx context.Context, rec childstore.ChildRecord) {
	// Signal a still-live orphan only when the recorded pid provably belongs to
	// THIS PID namespace. daemon_id is not that proof — it is pinned across pod
	// restarts, and a restarted pod has the same id with a fresh namespace.
	// An unknown token never signals: a missed orphan is harmless, a wrong
	// signal kills a live process.
	if rec.PID > 0 && c.nsToken != "" && rec.NSToken == c.nsToken {
		if err := syscall.Kill(rec.PID, 0); err == nil {
			_ = syscall.Kill(rec.PID, syscall.SIGTERM)
			slog.Info("sigterm orphan", "childId", rec.ChildID, "pid", rec.PID)
		}
	}

	plan := recoveryAction(rec)
	switch plan {
	case planRebindUnbound:
		rec.Labels = stripStaleBindings(rec.Labels)
	case planResumeBound:
		rec.Labels = stripStaleWorkspace(rec.Labels)
	}

	sess := childstore.SessionFromRecord(rec)
	sess.Status = protocol.StatusExited
	c.st.Insert(sess)

	if plan == planStayExited {
		if shouldAutoResume(rec) {
			slog.Info("child not auto-resumed: pinned workspace cannot change machines",
				"childId", rec.ChildID, "workspaceMode", rec.WorkspaceMode)
		}
		// NOT a drop. Final review corrected this: the rationale that used to
		// justify one ("no turn to inject into and there will not be one")
		// described a branch recoveryAction no longer produces — a pinned
		// child whose executor comes back CAN be resumed later (manually,
		// via `rafiki resume`), and that resume's own idle transition drives
		// the ordinary idle drain. Recovery-stays-exited is just an exit that
		// already happened, on a daemon that never attempted to run it, and
		// this phase's rule is exit RESETS — the same treatment
		// handleChildExit gives a real exit. A `Drop` is terminal with no
		// repair; a later resume's idle drain can only ever redeliver rows
		// that are 'pending', not 'sent', so without this reset a manual
		// resume of a never-auto-resumed child would silently never see rows
		// left 'sent' by whichever daemon incarnation last wrote them. Forget
		// remains the only drop.
		//
		// No ownership gate needed here, unlike Forget's: shouldAutoResume
		// returning false for THIS row already means it reads as
		// exited/shutting_down (or non-fundi) — a currently-live child under
		// another daemon would read as non-terminal and take the resume path
		// instead, never this one. A Reset is also idempotent and merely
		// un-hides pending work; it cannot cause the duplicate-delivery
		// hazard a Reset that runs AFTER a resume's idle transition can (see
		// resetUnconfirmedOnOwnership's doc comment) because nothing here
		// delivers anything, and there is no engine running yet to race.
		c.releaseInboxOnExit(rec.ChildID)
		return
	}

	// The lease is acquired at engine build, not here. Two acquisition sites
	// would mint two tokens for one conversation and the second would silently
	// invalidate the first, leaving the tracked lease holding a token no write
	// carries. resumeWithAutoRecovery is SUPPOSED to surface the refusal when
	// another daemon holds it, but a success return here does not by itself
	// prove that happened — see the holdsLease check below.
	slog.Info("auto-resuming fundi child", "childId", rec.ChildID)
	go func(id string) {
		rctx, cancel := context.WithTimeout(c.baseCtx, 60*time.Second)
		defer cancel()
		if _, err := c.resumeWithAutoRecovery(rctx, id); err != nil {
			slog.Warn("auto-resume failed; child stays exited", "childId", id, "error", err)
			c.dropLease(id)
			return
		}
		// A success return does not prove THIS daemon owns the child: the
		// in-process engine build runs on its own goroutine (Runner.Start
		// returns before Build completes), and activateLiveChild's
		// Idle-or-5s-timeout select cannot tell "became idle" apart from
		// "the build already failed, including on a refused lease" — see
		// holdsLease's doc comment. Every child row is visible to every
		// daemon (loadChildren lists the whole table), so recoverOne WILL
		// walk a row another live daemon already owns; without this check a
		// lease refusal there still replays as though it succeeded, flipping
		// the OTHER daemon's live child's 'sent' rows to 'pending' and
		// stranding them.
		//
		// The gate applies only when leasing is actually in play — the same
		// condition OnConversationResolved itself uses to decide whether to
		// acquire at all (c.leases is guaranteed non-nil here; loadChildren
		// already returned if c.children, set in the same pool!=nil block,
		// were nil). A daemon with no identity (c.daemonID == "") never
		// tracks a lease for anything and is already unfenced by design in
		// that case (see NewController), so it has nothing to gate on.
		if c.daemonID != "" && !c.holdsLease(id) {
			slog.Warn("auto-resume reported success without holding this child's lease; "+
				"not replaying its inbox", "childId", id)
			return
		}
		// The child is live and its runtime is wired. Its unconfirmed rows
		// were already reset to pending back when ownership was established
		// (resetUnconfirmedOnOwnership, inside OnConversationResolved) —
		// this call is the delivery half only, and mainly catches whatever
		// the child's own idle-transition drain deliberately leaves alone
		// (fragment-sourced rows; see replayInbox's doc comment). This is
		// what stops a coordinator waiting forever for a settle that already
		// happened.
		c.replayInbox(rctx, id)
	}(rec.ChildID)
}
