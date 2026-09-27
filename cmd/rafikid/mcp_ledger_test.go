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

	"github.com/multigres/testkit/assert"
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
	assert.NewAborting(t).NoError(err, "insert user %q", username)
	t.Cleanup(func() {
		// Ledger conversations FK the user row; remove them first. t.Context()
		// is already canceled by cleanup time, hence the background context.
		if _, err := pool.Exec(context.Background(),
			`DELETE FROM conversations.conversation WHERE external_ref = $1`,
			mcpLedgerExternalRef(id)); err != nil {
			t.Errorf("cleanup ledger conversation: %v", err)
		}
		if _, err := pool.Exec(context.Background(),
			`DELETE FROM conversations.users WHERE id = $1::uuid`, id); err != nil {
			t.Errorf("cleanup user row: %v", err)
		}
	})
	return users.Identity{UserID: id, Username: username}
}

// TestMCPLedgerKeyIsStableForOneUser pins the durability property: the same
// identity resolves to one UUID across repeated calls and across ledgers with
// a cold cache, because the key lives in the database, not in the process.
func TestMCPLedgerKeyIsStableForOneUser(t *testing.T) {
	c := assert.NewCollecting(t)
	pool := openTestPool(t)
	owner := seedLedgerUser(t, pool)
	l := newMCPLedger(capture.NewCaptureStore(pool))

	id1, err := l.ConversationID(t.Context(), owner)
	c.Require().NoError(err, "first resolve")

	// The row's persisted key values are pinned against literals here, not
	// through mcpLedgerExternalRef or the ledger's own DrivenBy field — those
	// share the values under test, so a drift in either would pass green while
	// silently splitting every existing deployment's ledger into a second row.
	var gotDrivenBy, gotExternalRef string
	c.Require().NoError(pool.QueryRow(t.Context(),
		`SELECT driven_by, external_ref FROM conversations.conversation WHERE id = $1::uuid`,
		id1).Scan(&gotDrivenBy, &gotExternalRef), "read back ledger row")
	c.Eq("client", gotDrivenBy, "persisted driven_by = %q, want the literal \"client\"", gotDrivenBy)
	c.Eq("mcp:user:"+owner.UserID, gotExternalRef, "persisted external_ref = %q, want the literal \"mcp:user:\" prefix + %q", gotExternalRef, owner.UserID)

	id2, err := l.ConversationID(t.Context(), owner)
	c.Require().NoError(err, "second resolve")
	c.Require().Eq(id2, id1, "same user must resolve to one id")

	id3, err := newMCPLedger(capture.NewCaptureStore(pool)).ConversationID(t.Context(), owner)
	c.Require().NoError(err, "cold-cache resolve")
	c.Require().Eq(id1, id3, "a cold ledger must resolve the durable row")
}

func TestMCPLedgerKeysAreDistinctBetweenUsers(t *testing.T) {
	c := assert.NewAborting(t)
	pool := openTestPool(t)
	alice := seedLedgerUser(t, pool)
	bob := seedLedgerUser(t, pool)
	l := newMCPLedger(capture.NewCaptureStore(pool))

	idA, err := l.ConversationID(t.Context(), alice)
	c.NoError(err, "resolve alice")
	idB, err := l.ConversationID(t.Context(), bob)
	c.NoError(err, "resolve bob")
	c.NotEq(idB, idA, "distinct users must get distinct ledgers, both got")
}

// TestMCPLedgerKeyIsAValidUUID guards the failure mode this task exists to
// close: a key that only looks like one, which would have passed against the
// in-memory task store and broken on the UUID column.
func TestMCPLedgerKeyIsAValidUUID(t *testing.T) {
	pool := openTestPool(t)
	owner := seedLedgerUser(t, pool)
	id, err := newMCPLedger(capture.NewCaptureStore(pool)).ConversationID(t.Context(), owner)
	assert.NewAborting(t).NoError(err, "resolve")
	if _, err := uuid.Parse(id); err != nil {
		t.Fatalf("ledger key %q is not a UUID: %v", id, err)
	}
}

// TestMCPLedgerConcurrentResolveYieldsOneRow races eight ledgers over one
// fresh user. The separate mcpLedger values bypass the in-process cache, so
// the partial unique index is what actually enforces one row.
func TestMCPLedgerConcurrentResolveYieldsOneRow(t *testing.T) {
	c := assert.NewAborting(t)
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
		c.NotEq("", id, "resolve %d produced no id", i)
		c.Eq(ids[0], id, "concurrent resolves disagreed: goroutine %d got %q, first got", i, id)
	}

	var count int
	c.NoError(pool.QueryRow(t.Context(),
		`SELECT count(*) FROM conversations.conversation WHERE external_ref = $1`,
		mcpLedgerExternalRef(owner.UserID)).Scan(&count), "count ledger rows")
	c.Eq(1, count, "expected exactly one ledger row for %q, got", mcpLedgerExternalRef(owner.UserID))
}

func TestMCPLedgerWithoutStoreFallsBackToMemoryKey(t *testing.T) {
	c := assert.NewAborting(t)
	owner := users.Identity{UserID: "00000000-0000-0000-0000-000000000042", Username: "dbless"}
	id, err := newMCPLedger(nil).ConversationID(t.Context(), owner)
	c.NoError(err, "DB-less resolve")
	c.Eq("user:"+owner.UserID, id, "DB-less daemon must fall back to the memory key: got")
}

func TestMCPLedgerRefusesAnonymous(t *testing.T) {
	if _, err := newMCPLedger(nil).ConversationID(t.Context(), users.Identity{}); err == nil {
		t.Fatal("the zero identity must error on a DB-less ledger")
	}
	pool := openTestPool(t)
	_, err := newMCPLedger(capture.NewCaptureStore(pool)).ConversationID(t.Context(), users.Identity{})
	assert.NewAborting(t).Error(err, "the zero identity must error before the store is consulted")
}
