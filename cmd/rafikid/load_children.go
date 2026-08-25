// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"log/slog"
	"syscall"
	"time"

	"go.graveland.dev/rafiki/pkg/childstore"
	"go.graveland.dev/rafiki/pkg/persist"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/store"
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
	// planRebindUnbound clears the dead executor binding and resumes the child
	// unbound, so it rebinds on first workspace-tool use.
	planRebindUnbound
)

// shouldAutoResume reports whether a recovered record is a fundi child that was
// ALIVE when the daemon went down.
//
// The predicate reads LastStatus, not Status: Status is "exited" for every
// recovered row by construction. A child was alive when its last state was
// neither "exited" (a clean stop) nor "shutting_down" (a deliberate one).
// Everything else — idle, streaming, tool_running, compacting, blocked_ui,
// spawning — means the daemon died underneath it.
func shouldAutoResume(rec childstore.ChildRecord) bool {
	if rec.Kind != protocol.KindFundi {
		return false
	}
	if rec.LastStatus == "" {
		return false
	}
	return rec.LastStatus != string(protocol.StatusExited) &&
		rec.LastStatus != string(protocol.StatusShuttingDown)
}

// recoveryAction decides what to do with a resumable child whose executor
// binding is stale by construction — an executor's workspace registry is in
// memory, so a restart loses every workspace id.
//
// A PINNED child is never moved. HandleExecutorLost fails a pinned child where
// it stood and boundExecutor.recover refuses to re-select for one; recovery
// must not become the single path that quietly migrates one to a new machine.
// An unknown mode is pinned, matching workspaceModeOrPinned.
func recoveryAction(rec childstore.ChildRecord) recoveryPlan {
	if !shouldAutoResume(rec) {
		return planStayExited
	}
	if rec.WorkspaceMode == "ephemeral" {
		return planRebindUnbound
	}
	return planStayExited
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

// loadChildren rebuilds the in-memory store from the database and resumes the
// children this daemon wins a lease for.
//
// diskRecords is the one-shot import of §5.2: on the first startup after
// upgrade the database has no rows for this daemon and the on-disk records are
// imported. It is not a fallback — once this daemon has rows, the files are
// ignored, so a Forget cannot be undone by a stale file.
func (c *Controller) loadChildren(ctx context.Context, diskRecords []persist.Record) {
	if c.children == nil {
		// No pool: children live in memory only. Fall back to the disk records
		// so a database-less daemon keeps working during the dual-write window.
		c.loadFromDisk(diskRecords)
		return
	}

	recs, err := c.children.List(ctx)
	if err != nil {
		slog.Error("load children from database failed; falling back to disk records", "error", err)
		c.loadFromDisk(diskRecords)
		return
	}

	if !c.hasOwnRows(recs) && len(diskRecords) > 0 {
		slog.Info("importing on-disk child records", "count", len(diskRecords))
		recs = c.importDiskRecords(ctx, diskRecords)
	}

	for _, rec := range recs {
		c.recoverOne(ctx, rec)
	}
}

// hasOwnRows reports whether any row was last written by this daemon. It is
// what gates the one-shot import: a daemon that already has rows must never
// re-import files.
func (c *Controller) hasOwnRows(recs []childstore.ChildRecord) bool {
	for _, r := range recs {
		if r.DaemonID != "" && r.DaemonID == c.daemonID {
			return true
		}
	}
	return false
}

// importDiskRecords upserts every on-disk record and returns the resulting
// records. conversation_id is recovered for fundi children from external_ref —
// the correlation --ref established — so imported rows are immediately complete.
func (c *Controller) importDiskRecords(ctx context.Context, diskRecords []persist.Record) []childstore.ChildRecord {
	out := make([]childstore.ChildRecord, 0, len(diskRecords))
	for _, dr := range diskRecords {
		sess := sessionFromRecord(dr)
		rec := childstore.RecordFromSnapshot(sess.Snapshot())
		rec.LastStatus = dr.LastStatus
		rec.DaemonID = c.daemonID
		if dr.Kind == protocol.KindFundi {
			rec.ConversationID = c.conversationIDForRef(ctx, dr.ChildID)
		}
		if err := c.children.Upsert(ctx, rec); err != nil {
			slog.Warn("import child record", "childId", dr.ChildID, "error", err)
			continue
		}
		out = append(out, rec)
	}
	return out
}

// conversationIDForRef resolves the conversation a legacy fundi child was
// correlated to by --ref. Empty when there is none.
func (c *Controller) conversationIDForRef(ctx context.Context, childID string) string {
	if c.pool == nil {
		return ""
	}
	var id string
	err := c.pool.QueryRow(ctx, `
		SELECT id::text FROM conversations.conversation
		 WHERE external_ref = $1 AND driven_by = 'server'
		 ORDER BY created_at DESC LIMIT 1`, childID).Scan(&id)
	if err != nil {
		return ""
	}
	return id
}

// recoverOne loads a single record into the store and decides whether to resume.
func (c *Controller) recoverOne(ctx context.Context, rec childstore.ChildRecord) {
	// Signal a still-live orphan, but ONLY when this daemon wrote the pid. A
	// pid from a different daemon names an unrelated process on another host.
	if rec.PID > 0 && rec.DaemonID != "" && rec.DaemonID == c.daemonID {
		if err := syscall.Kill(rec.PID, 0); err == nil {
			_ = syscall.Kill(rec.PID, syscall.SIGTERM)
			slog.Info("sigterm orphan", "childId", rec.ChildID, "pid", rec.PID)
		}
	}

	plan := recoveryAction(rec)
	if plan == planRebindUnbound {
		rec.Labels = stripStaleBindings(rec.Labels)
	}

	sess := childstore.SessionFromRecord(rec)
	sess.Status = protocol.StatusExited
	c.st.Insert(sess)

	if plan != planRebindUnbound {
		if shouldAutoResume(rec) {
			slog.Info("child not auto-resumed: pinned workspace cannot change machines",
				"childId", rec.ChildID, "workspaceMode", rec.WorkspaceMode)
		}
		return
	}

	if c.leases == nil || c.daemonID == "" || rec.ConversationID == "" {
		slog.Info("child not auto-resumed: no lease available",
			"childId", rec.ChildID, "conversationId", rec.ConversationID)
		return
	}

	lease, ok, err := c.leases.Acquire(ctx, rec.ConversationID, c.daemonID, leaseTTL)
	if err != nil {
		slog.Warn("lease acquire failed; child stays exited", "childId", rec.ChildID, "error", err)
		return
	}
	if !ok {
		slog.Info("another daemon holds this conversation; child stays exited",
			"childId", rec.ChildID, "conversationId", rec.ConversationID)
		return
	}

	c.trackLease(rec.ChildID, lease)
	slog.Info("auto-resuming fundi child", "childId", rec.ChildID)
	go func(id string) {
		rctx, cancel := context.WithTimeout(c.baseCtx, 60*time.Second)
		defer cancel()
		if _, err := c.resumeWithAutoRecovery(rctx, id); err != nil {
			slog.Warn("auto-resume failed; child stays exited", "childId", id, "error", err)
			c.dropLease(id)
		}
	}(rec.ChildID)
}

// loadFromDisk is the no-pool path: the old loadOrphans behaviour, minus the
// lease (a daemon with no database has no one to contend with).
func (c *Controller) loadFromDisk(records []persist.Record) {
	for _, rec := range records {
		if rec.PID > 0 {
			if err := syscall.Kill(rec.PID, 0); err == nil {
				_ = syscall.Kill(rec.PID, syscall.SIGTERM)
				slog.Info("sigterm orphan", "childId", rec.ChildID, "pid", rec.PID)
			}
		}
		sess := sessionFromRecord(rec)
		sess.Status = protocol.StatusExited
		c.st.Insert(sess)
	}
}

// Stubs replaced in Task 10.
func (c *Controller) trackLease(childID string, l store.Lease) {}
func (c *Controller) dropLease(childID string)                 {}
