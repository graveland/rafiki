// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// mcpSessions tracks the live MCP sessions per user so a settlement can be
// pushed to the caller that spawned the agent.
//
// A user may have several sessions (several clients, several tabs); every one
// gets the notification. Sessions are in-memory and are not restored across a
// daemon restart — a client that reconnects re-registers by initializing or
// by its first tool call, whichever handshake it speaks.
type mcpSessions struct {
	mu sync.Mutex
	// byUID holds one set per user id. The inner map is deleted when it
	// empties, so a departed user leaks nothing.
	byUID map[string]map[*mcp.ServerSession]struct{}
}

func newMCPSessions() *mcpSessions {
	return &mcpSessions{byUID: make(map[string]map[*mcp.ServerSession]struct{})}
}

// Add registers one live session under userID and reports whether it was
// newly added; false means the session was already registered and nothing
// changed.
//
// The registration points are settlementHooksFor and
// settlementRegistrationFor (both cmd/rafikid/mcp_face.go): the SDK's
// InitializedHandler fires when a legacy-handshake client completes its
// initialize with notifications/initialized, and the bridge's tool-call hook
// fires for every client, which is the only registration a go-sdk v1.7.0+
// client gets — its SEP-2575 discover handshake never sends that
// notification. A client that does neither is never registered.
//
// Removal is NOT hook-driven: the SDK's ServerOptions carries no session-closed
// hook, so the registering hook starts one per-session goroutine on Wait, which
// returns when the session's connection closes (client DELETE, handler
// timeout, teardown) and calls Remove. Add's report is what keeps that to one
// goroutine per session: a session reachable through both hooks spawns it
// exactly once.
func (m *mcpSessions) Add(userID string, ss *mcp.ServerSession) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	set, ok := m.byUID[userID]
	if !ok {
		set = make(map[*mcp.ServerSession]struct{})
		m.byUID[userID] = set
	}
	if _, dup := set[ss]; dup {
		return false
	}
	set[ss] = struct{}{}
	return true
}

// Remove drops one session. Removing the last one deletes the user's entry.
func (m *mcpSessions) Remove(userID string, ss *mcp.ServerSession) {
	m.mu.Lock()
	defer m.mu.Unlock()
	set, ok := m.byUID[userID]
	if !ok {
		return
	}
	delete(set, ss)
	if len(set) == 0 {
		delete(m.byUID, userID)
	}
}

// Notify sends message to every live session of userID.
//
// The session set is snapshotted under the lock and the lock is RELEASED
// before any send: Log writes to a network stream, and a stalled client inside
// a held lock would wedge every other user's notification.
//
// Sends then run CONCURRENTLY, one goroutine per session, and the caller's
// wait is bounded by ctx. The SDK's streamable Write path takes no context
// (streamableServerConn.Write → deliverLocked), so a send to a
// stalled-but-open client blocks indefinitely and a caller-side timeout can
// never interrupt it — a sequential loop would wedge the settle path (this
// runs from the child's status goroutine) while starving the user's other
// sessions. Concurrent sends keep one stalled client from doing either: the
// wait gives up at ctx's deadline while the blocked send is ABANDONED, not
// cancelled — its goroutine stays on the transport write and ends when the
// connection eventually dies, holding nothing but the session pointer. One
// such goroutine accumulates per settle against a session stuck that long;
// all of them are freed when the connection dies. Each
// send's result is logged at debug independently; one failing session never
// aborts the fan-out.
//
// Note a send may also SILENTLY do nothing: the SDK's Log returns nil without
// writing when the client has never issued logging/setLevel
// (mcp/server.go:1312 reads ss.state.LogLevel, empty until then), so a
// delivered notification and a dropped one are indistinguishable here. This
// push is therefore best-effort by construction; agent_list's status field is
// the reliable answer.
func (m *mcpSessions) Notify(ctx context.Context, userID, message string) {
	m.mu.Lock()
	sessions := make([]*mcp.ServerSession, 0, len(m.byUID[userID]))
	for ss := range m.byUID[userID] {
		sessions = append(sessions, ss)
	}
	m.mu.Unlock()

	// WithoutCancel, not the caller's ctx: the deadline bounds the WAIT
	// below, not the sends — a send that outlives it keeps going untouched
	// instead of dying to a cancellation it never observes on the streamable
	// path anyway.
	sendCtx := context.WithoutCancel(ctx)
	var wg sync.WaitGroup
	for _, ss := range sessions {
		wg.Add(1)
		go func(ss *mcp.ServerSession) {
			defer wg.Done()
			//nolint:staticcheck // the logging push is the only server-initiated
			// channel ServerSession offers; SEP-2577 deprecates the feature with
			// a >=12-month window, and there is no successor to move to yet.
			err := ss.Log(sendCtx, &mcp.LoggingMessageParams{
				Level:  "info",
				Logger: "rafiki",
				Data:   message,
			})
			if err != nil {
				slog.Debug("mcp settlement notification failed", "sessionId", ss.ID(), "error", err)
			}
		}(ss)
	}
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-ctx.Done():
	}
}

// mcpSettlements is the daemon-wide registry of live MCP sessions, keyed by
// user id. Package-level like mcpBlueprints, because its two touchpoints —
// cmd/rafikid/mcp_face.go's registration hooks, which add sessions from the
// SDK's InitializedHandler and the bridge's tool-call hook and remove them
// from a per-session Wait goroutine, and the settlement source that fans out
// below — have no shared owner. Tests swap it wholesale.
var mcpSettlements = newMCPSessions()

// notifyMCPSettled pushes the settlement fragment to every live MCP session
// owned by the settling child's user.
//
// An MCP caller is not a rafiki child and has no inbox, so the existing
// parent-gated push cannot reach it. The caller that spawned the child is
// simply not in the lineage: a user credential spawns top-level children,
// and since the child-token face landed, a CHILD credential spawns parented
// children whose OwnerUserID is empty (the controller spawner passes an
// empty identity) — both fan out to nobody here, and a child-caller's
// descendants reach it through agent_list instead. This fan-out runs BEFORE
// the parent gate, independently of lineage and of the event buffer.
//
// The owner comes from STORED STATE — childstore.Snapshot.OwnerUserID, the
// conversations.child.owner_user_id column — never from an argument: this
// runs on the child's own settle path, and a caller-supplied id would let one
// child steer another user's notifications. OwnerUserID is the users.id (the
// user-bound spawner passes the authenticated identity to Controller.Spawn),
// so no username→id resolution happens here. Empty means an anonymous spawn
// (e.g. the local unix socket), which owns no MCP session; that case is
// skipped.
//
// A DESCENDANT of an MCP-spawned child is also skipped, and that is the
// designed shape, not a gap: OwnerUserID is stamped only from the identity
// argument at fresh spawn (userSpawner passes the authenticated user; the
// child-bound controllerSpawner passes users.Identity{}), and nothing
// inherits it down the lineage — only the display-only Labels["owner"]
// username propagates, via attestOwner. The record round-trip
// (childstoredb record ⇄ SessionFromRecord) preserves the empty id across
// resume. A descendant's settlement reaches the MCP caller's own child — its
// parent — through the parent-gated inbox push instead, and the caller sees
// the whole subtree through agent_list.
func (c *Controller) notifyMCPSettled(childID, reason string) {
	if mcpSettlements == nil {
		return
	}
	snap, ok := c.st.Get(childID)
	if !ok {
		return
	}
	if snap.OwnerUserID == "" {
		return
	}
	// Bounded like checkTaskResidue beside it: this rides the child's status
	// goroutine, and one stalled client must not hold it for longer than a
	// database hiccup would. The deadline bounds the WAIT — Notify's sends
	// run concurrently, so the wait is reliable and one user's stalled
	// session starves neither this goroutine nor that user's other sessions.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	mcpSettlements.Notify(ctx, snap.OwnerUserID, settleFragment(childID, snap.Name, reason))
}
