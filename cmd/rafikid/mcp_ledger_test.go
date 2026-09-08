// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"go.graveland.dev/rafiki/pkg/capture"
	"go.graveland.dev/rafiki/pkg/users"
)

// seedLedgerUser inserts a fresh users row and returns its identity. The
// ledger's owner_user_id column FKs conversations.users, so a synthetic id
// would fail the insert before it failed anything else; and the username must
// be unique per run because tombstones keep their row.
func seedLedgerUser(t *testing.T, pool *pgxpool.Pool) users.Identity {
	t.Helper()
	username := fmt.Sprintf("mcp-ledger-%d", time.Now().UnixNano())
	var id string
	err := pool.QueryRow(t.Context(),
		`INSERT INTO conversations.users (username, token_sha256)
		 VALUES ($1, $1 || ':' || gen_random_uuid()::text) RETURNING id::text`,
		username).Scan(&id)
	if err != nil {
		t.Fatalf("insert user %q: %v", username, err)
	}
	t.Cleanup(func() {
		// Ledger conversations FK the user row; remove them first. t.Context()
		// is already canceled by cleanup time, hence the background context.
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM conversations.conversation WHERE external_ref = $1`,
			mcpLedgerExternalRef(id))
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM conversations.users WHERE id = $1::uuid`, id)
	})
	return users.Identity{UserID: id, Username: username}
}

// TestMCPLedgerKeyIsStableForOneUser pins the durability property: the same
// identity resolves to one UUID across repeated calls and across ledgers with
// a cold cache, because the key lives in the database, not in the process.
func TestMCPLedgerKeyIsStableForOneUser(t *testing.T) {
	pool := openTestPool(t)
	owner := seedLedgerUser(t, pool)
	l := newMCPLedger(capture.NewCaptureStore(pool))

	id1, err := l.ConversationID(t.Context(), owner)
	if err != nil {
		t.Fatalf("first resolve: %v", err)
	}
	id2, err := l.ConversationID(t.Context(), owner)
	if err != nil {
		t.Fatalf("second resolve: %v", err)
	}
	if id1 != id2 {
		t.Fatalf("same user must resolve to one id: %q != %q", id1, id2)
	}

	id3, err := newMCPLedger(capture.NewCaptureStore(pool)).ConversationID(t.Context(), owner)
	if err != nil {
		t.Fatalf("cold-cache resolve: %v", err)
	}
	if id3 != id1 {
		t.Fatalf("a cold ledger must resolve the durable row: %q != %q", id3, id1)
	}
}

func TestMCPLedgerKeysAreDistinctBetweenUsers(t *testing.T) {
	pool := openTestPool(t)
	alice := seedLedgerUser(t, pool)
	bob := seedLedgerUser(t, pool)
	l := newMCPLedger(capture.NewCaptureStore(pool))

	idA, err := l.ConversationID(t.Context(), alice)
	if err != nil {
		t.Fatalf("resolve alice: %v", err)
	}
	idB, err := l.ConversationID(t.Context(), bob)
	if err != nil {
		t.Fatalf("resolve bob: %v", err)
	}
	if idA == idB {
		t.Fatalf("distinct users must get distinct ledgers, both got %q", idA)
	}
}

// TestMCPLedgerKeyIsAValidUUID guards the failure mode this task exists to
// close: a key that only looks like one, which would have passed against the
// in-memory task store and broken on the UUID column.
func TestMCPLedgerKeyIsAValidUUID(t *testing.T) {
	pool := openTestPool(t)
	owner := seedLedgerUser(t, pool)
	id, err := newMCPLedger(capture.NewCaptureStore(pool)).ConversationID(t.Context(), owner)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if _, err := uuid.Parse(id); err != nil {
		t.Fatalf("ledger key %q is not a UUID: %v", id, err)
	}
}

// TestMCPLedgerConcurrentResolveYieldsOneRow races eight ledgers over one
// fresh user. The separate mcpLedger values bypass the in-process cache, so
// the partial unique index is what actually enforces one row.
func TestMCPLedgerConcurrentResolveYieldsOneRow(t *testing.T) {
	pool := openTestPool(t)
	owner := seedLedgerUser(t, pool)

	const n = 8
	ids := make([]string, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id, err := newMCPLedger(capture.NewCaptureStore(pool)).ConversationID(t.Context(), owner)
			if err != nil {
				t.Errorf("resolve %d: %v", i, err)
				return
			}
			ids[i] = id
		}()
	}
	wg.Wait()

	for i, id := range ids {
		if id == "" {
			t.Fatalf("resolve %d produced no id", i)
		}
		if id != ids[0] {
			t.Fatalf("concurrent resolves disagreed: goroutine %d got %q, first got %q", i, id, ids[0])
		}
	}

	var count int
	if err := pool.QueryRow(t.Context(),
		`SELECT count(*) FROM conversations.conversation WHERE external_ref = $1`,
		mcpLedgerExternalRef(owner.UserID)).Scan(&count); err != nil {
		t.Fatalf("count ledger rows: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected exactly one ledger row for %q, got %d", mcpLedgerExternalRef(owner.UserID), count)
	}
}

func TestMCPLedgerWithoutStoreFallsBackToMemoryKey(t *testing.T) {
	owner := users.Identity{UserID: "00000000-0000-0000-0000-000000000042", Username: "dbless"}
	id, err := newMCPLedger(nil).ConversationID(t.Context(), owner)
	if err != nil {
		t.Fatalf("DB-less resolve: %v", err)
	}
	if id != "user:"+owner.UserID {
		t.Fatalf("DB-less daemon must fall back to the memory key: got %q, want %q", id, "user:"+owner.UserID)
	}
}

func TestMCPLedgerRefusesAnonymous(t *testing.T) {
	if _, err := newMCPLedger(nil).ConversationID(t.Context(), users.Identity{}); err == nil {
		t.Fatal("the zero identity must error on a DB-less ledger")
	}
	pool := openTestPool(t)
	if _, err := newMCPLedger(capture.NewCaptureStore(pool)).ConversationID(t.Context(), users.Identity{}); err == nil {
		t.Fatal("the zero identity must error before the store is consulted")
	}
}
