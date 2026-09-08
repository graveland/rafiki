// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"

	"go.graveland.dev/rafiki/pkg/capture"
	"go.graveland.dev/rafiki/pkg/store"
	"go.graveland.dev/rafiki/pkg/users"
)

// mcpLedger resolves the durable conversation row that backs one MCP user's
// task ledger.
//
// The task_* tools scope by conversation id and the column is a UUID with a
// foreign key to conversations.conversation, so a synthetic string key cannot
// be stored. Each user therefore gets a real, turn-less conversation row keyed
// by external_ref = "mcp:user:<user-id>". Keying on the user id means a future
// multi-user rafiki inherits per-user ledgers with no further work.
type mcpLedger struct {
	store *capture.CaptureStore // nil on a DB-less daemon

	mu    sync.Mutex
	byUID map[string]string
}

func newMCPLedger(store *capture.CaptureStore) *mcpLedger {
	return &mcpLedger{store: store, byUID: make(map[string]string)}
}

// mcpLedgerExternalRef is the correlation key. It is stable for the lifetime
// of the user row, which is what makes the ledger durable across daemon
// restarts.
func mcpLedgerExternalRef(userID string) string { return "mcp:user:" + userID }

// mcpLedgerOriginEntrypoint labels these rows in conversations.v_conversation
// and the insights surfaces. They are real conversations carrying no turns,
// which is correct — the tasks in them are real work.
const mcpLedgerOriginEntrypoint = "mcp"

// ConversationID returns the ledger key for owner, creating the row on first
// use. On a DB-less daemon it returns the synthetic "user:<id>" string, which
// the in-memory task store accepts — the same degradation BuildRuntime already
// applies for a pool-less agent, with state lost on restart.
func (l *mcpLedger) ConversationID(ctx context.Context, owner users.Identity) (string, error) {
	if owner.UserID == "" {
		return "", errors.New("task tools require an authenticated user")
	}
	if l.store == nil {
		return "user:" + owner.UserID, nil
	}
	l.mu.Lock()
	id, ok := l.byUID[owner.UserID]
	l.mu.Unlock()
	if ok {
		return id, nil
	}
	// No lock across the resolve: EnsureConversationByExternalRef is race-safe
	// by construction (partial unique index on (external_ref, driven_by) with
	// ON CONFLICT DO NOTHING falling through to SELECT), and two in-flight
	// resolves for one user both land on the same row.
	id, err := l.store.EnsureConversationByExternalRef(ctx, capture.ConversationRef{
		ExternalRef:      mcpLedgerExternalRef(owner.UserID),
		OriginEntrypoint: mcpLedgerOriginEntrypoint,
		DrivenBy:         string(store.DrivenByClient),
		OwnerUserID:      owner.UserID,
		Name:             "MCP task ledger",
	})
	if err != nil {
		return "", fmt.Errorf("resolve mcp task ledger for user %s: %w", owner.UserID, err)
	}
	l.mu.Lock()
	l.byUID[owner.UserID] = id
	l.mu.Unlock()
	slog.Debug("mcp task ledger resolved", "userId", owner.UserID, "conversationId", id)
	return id, nil
}
