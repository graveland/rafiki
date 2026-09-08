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
// daemon restart — a client that reconnects re-subscribes by initializing.
type mcpSessions struct {
	mu sync.Mutex
	// byUID holds one set per user id. The inner map is deleted when it
	// empties, so a departed user leaks nothing.
	byUID map[string]map[*mcp.ServerSession]struct{}
}

func newMCPSessions() *mcpSessions {
	return &mcpSessions{byUID: make(map[string]map[*mcp.ServerSession]struct{})}
}

// Add registers one live session under userID.
//
// REGISTRATION IS NOT WIRED (task 3.1 verdict: BLOCKED on the seam). Both
// candidate seams sit outside this task's touches declaration, so the ruling
// belongs to the coordinator:
//
//   - The SDK hook route: mcp.ServerOptions carries InitializedHandler in
//     v1.6.1 (server.go:66) and no session-closed hook at all, so Remove would
//     ride a per-session Wait() goroutine — workable, but the hook cannot be
//     threaded to this registry without editing pkg/mcpserver/bridge.go
//     (mcpserver.New hardcodes nil options at its mcp.NewServer call) AND
//     cmd/rafikid/mcp_face.go (getServer builds the server; Routes passes nil
//     to NewStreamableHTTPHandler).
//   - The fallback route the brief names: req.Session is reachable from any
//     MCP tool call, but only inside pkg/mcpserver/bridge.go's handlerFor,
//     which drops it on the floor before calling the rafiki tool. Forwarding
//     it is an edit to that same file, plus a caller for Add — and the only
//     per-call code this package owns lives in mcp_face.go, also outside
//     touches. Taken alone it also means a client that never calls a tool is
//     never registered.
//
// Until the ruling lands, nothing calls Add, Notify finds no sessions, and the
// fan-out is a no-op: zero behavior change. agent_list's status field remains
// the only settlement signal an MCP caller can rely on.
func (m *mcpSessions) Add(userID string, ss *mcp.ServerSession) {
	m.mu.Lock()
	defer m.mu.Unlock()
	set, ok := m.byUID[userID]
	if !ok {
		set = make(map[*mcp.ServerSession]struct{})
		m.byUID[userID] = set
	}
	set[ss] = struct{}{}
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
// a held lock would wedge every other user's notification. A failing send is
// logged at debug and skipped — one dead session must not abort the fan-out.
//
// Note the send is also silently dropped by the SDK when the client has never
// issued logging/setLevel (mcp/server.go:1312 returns nil without writing
// while ss.state.LogLevel is empty), so a delivered notification and a dropped
// one are indistinguishable here. This push is therefore best-effort by
// construction; agent_list's status field is the reliable answer.
func (m *mcpSessions) Notify(ctx context.Context, userID, message string) {
	m.mu.Lock()
	sessions := make([]*mcp.ServerSession, 0, len(m.byUID[userID]))
	for ss := range m.byUID[userID] {
		sessions = append(sessions, ss)
	}
	m.mu.Unlock()

	for _, ss := range sessions {
		err := ss.Log(ctx, &mcp.LoggingMessageParams{
			Level:  "info",
			Logger: "rafiki",
			Data:   message,
		})
		if err != nil {
			slog.Debug("mcp settlement notification failed", "sessionId", ss.ID(), "error", err)
		}
	}
}

// mcpSettlements is the daemon-wide registry of live MCP sessions, keyed by
// user id. Package-level like mcpBlueprints, because its two touchpoints —
// whatever registers sessions (currently unwired, see Add) and the settlement
// source that fans out — have no shared owner. Tests swap it wholesale.
var mcpSettlements = newMCPSessions()

// notifyMCPSettled pushes the settlement fragment to every live MCP session
// owned by the settling child's user.
//
// An MCP caller is not a rafiki child and has no inbox, so the existing
// parent-gated push cannot reach it — and every MCP-spawned child is
// top-level, so the parent gate would skip it every time. This fan-out runs
// BEFORE that gate, independently of lineage and of the event buffer.
//
// The owner comes from STORED STATE — childstore.Snapshot.OwnerUserID, the
// conversations.child.owner_user_id column — never from an argument: this
// runs on the child's own settle path, and a caller-supplied id would let one
// child steer another user's notifications. OwnerUserID is the users.id (the
// user-bound spawner passes the authenticated identity to Controller.Spawn),
// so no username→id resolution happens here. Empty means an anonymous spawn
// (e.g. the local unix socket), which owns no MCP session; that case is
// skipped.
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
	// database hiccup would.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	mcpSettlements.Notify(ctx, snap.OwnerUserID, settleFragment(childID, snap.Name, reason))
}
