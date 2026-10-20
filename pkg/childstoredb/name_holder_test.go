// SPDX-License-Identifier: Apache-2.0

package childstoredb

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"go.graveland.dev/rafiki/pkg/childstore"
	"go.graveland.dev/rafiki/pkg/protocol"

	"github.com/multigres/testkit/assert"
)

// uniqueName returns a name no earlier run of these tests can have used. The
// test database is shared and never reset, so a literal name would collide with
// a previous run's still-live row and every first insert would fail. The
// counter keeps two names generated in one run distinct even if the clock
// repeats a reading.
func uniqueName(tag string) string {
	return fmt.Sprintf("nh_%s_%d_%d", tag, time.Now().UnixNano(), uniqueSeq.Add(1))
}

var uniqueSeq atomic.Int64

// childRecord builds a live sibling row. parent "" makes it top-level; the
// label key is a parameter so the legacy fundi/parent spelling can be exercised.
func childRecord(id, owner, parent, name, status string) childstore.ChildRecord {
	rec := childstore.ChildRecord{
		ChildID:     id,
		OwnerUserID: owner,
		Kind:        protocol.KindFundi,
		Name:        name,
		Status:      status,
		SpawnedAt:   time.Now(),
	}
	if parent != "" {
		rec.Labels = map[string]string{childstore.LabelParent: parent}
	}
	return rec
}

// upsertChild writes rec and hard-deletes the row when the test ends, so this
// file's fixtures do not accumulate in the shared database. The delete is
// registered before the write so a partially failed test still cleans up.
func upsertChild(t *testing.T, s *Store, pool *pgxpool.Pool, rec childstore.ChildRecord) error {
	t.Helper()
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM conversations.child WHERE child_id = $1`, rec.ChildID)
	})
	return s.Upsert(context.Background(), rec)
}

// insertOwner creates a user row to own children, since owner_user_id is a
// foreign key. The username and token are unique per call for the same
// shared-database reason as uniqueName.
func insertOwner(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	username := fmt.Sprintf("nh-owner-%d", time.Now().UnixNano())
	var id string
	err := pool.QueryRow(context.Background(),
		`INSERT INTO conversations.users (username, token_sha256) VALUES ($1, $2) RETURNING id::text`,
		username, "tok-"+username).Scan(&id)
	assert.NewAborting(t).NoError(err, "insert owner %s", username)
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DELETE FROM conversations.users WHERE id = $1::uuid`, id)
	})
	return id
}

// TestSiblingNameIndexSiblingsCollide pins the index itself and the sentinel
// mapping: two live children of one parent, under one owner, cannot share a
// name — while re-upserting the holder with its own name is not a conflict.
func TestSiblingNameIndexSiblingsCollide(t *testing.T) {
	c := assert.NewCollecting(t)
	ctx := context.Background()
	pool := testPool(t)
	s := New(pool)

	owner := insertOwner(t, pool)
	name := uniqueName("sibling")
	parent := uniqueName("parent")

	holder := childRecord(uniqueName("holder"), owner, parent, name, string(protocol.StatusIdle))
	c.Require().NoError(upsertChild(t, s, pool, holder), "first sibling Upsert")

	err := upsertChild(t, s, pool, childRecord(uniqueName("intruder"), owner, parent, name, string(protocol.StatusIdle)))
	c.True(errors.Is(err, childstore.ErrNameTaken), "second sibling Upsert err = %v, want childstore.ErrNameTaken", err)

	// The holder writing again — a status change — keeps its own name.
	holder.Status = string(protocol.StatusExited)
	c.NoError(s.Upsert(ctx, holder), "re-upsert of the holder with its own name must not conflict")
}

// TestSiblingNameIndexCousinsDoNot: siblings are scoped by parent, so the same
// name under two different parents of one owner is fine.
func TestSiblingNameIndexCousinsDoNot(t *testing.T) {
	c := assert.NewCollecting(t)
	pool := testPool(t)
	s := New(pool)

	owner := insertOwner(t, pool)
	name := uniqueName("cousin")

	c.Require().NoError(upsertChild(t, s, pool,
		childRecord(uniqueName("cousin-a"), owner, uniqueName("parent-a"), name, string(protocol.StatusIdle))), "first parent")
	c.NoError(upsertChild(t, s, pool,
		childRecord(uniqueName("cousin-b"), owner, uniqueName("parent-b"), name, string(protocol.StatusIdle))), "second parent, same name")
}

// TestSiblingNameIndexClosedFrees pins the index's closed_at clause: closing a
// child frees its name for the next sibling.
func TestSiblingNameIndexClosedFrees(t *testing.T) {
	c := assert.NewCollecting(t)
	ctx := context.Background()
	pool := testPool(t)
	s := New(pool)

	owner := insertOwner(t, pool)
	name := uniqueName("closed")
	parent := uniqueName("parent")

	first := childRecord(uniqueName("closed-first"), owner, parent, name, string(protocol.StatusExited))
	c.Require().NoError(upsertChild(t, s, pool, first), "first sibling Upsert")
	c.Require().NoError(s.Delete(ctx, first.ChildID), "close the first sibling")

	c.NoError(upsertChild(t, s, pool,
		childRecord(uniqueName("closed-second"), owner, parent, name, string(protocol.StatusIdle))), "a closed sibling must not hold its name")
}

// TestSiblingNameIndexEmptyNamesExempt pins the index's non-empty-name clause:
// an unnamed child is never in conflict with another unnamed sibling.
func TestSiblingNameIndexEmptyNamesExempt(t *testing.T) {
	c := assert.NewCollecting(t)
	pool := testPool(t)
	s := New(pool)

	owner := insertOwner(t, pool)
	parent := uniqueName("parent")

	c.Require().NoError(upsertChild(t, s, pool,
		childRecord(uniqueName("anon-a"), owner, parent, "", string(protocol.StatusIdle))), "first unnamed sibling")
	c.NoError(upsertChild(t, s, pool,
		childRecord(uniqueName("anon-b"), owner, parent, "", string(protocol.StatusIdle))), "second unnamed sibling")
}

// TestSiblingNameIndexNullOwnersCollide pins the owner coalesce: the anonymous
// owner (owner_user_id IS NULL) is one owner, not one owner per NULL.
func TestSiblingNameIndexNullOwnersCollide(t *testing.T) {
	c := assert.NewCollecting(t)
	pool := testPool(t)
	s := New(pool)

	name := uniqueName("nullowner")

	c.Require().NoError(upsertChild(t, s, pool,
		childRecord(uniqueName("null-a"), "", "", name, string(protocol.StatusIdle))), "first anonymous sibling")

	err := upsertChild(t, s, pool,
		childRecord(uniqueName("null-b"), "", "", name, string(protocol.StatusIdle)))
	c.True(errors.Is(err, childstore.ErrNameTaken), "second anonymous sibling err = %v, want childstore.ErrNameTaken", err)
}

// TestSiblingNameIndexLegacyParentLabel pins the label coalesce: children
// labeled with the pre-rename fundi/parent are siblings of children labeled
// rafiki/parent, because the subtree label was renamed without a backfill.
func TestSiblingNameIndexLegacyParentLabel(t *testing.T) {
	c := assert.NewCollecting(t)
	pool := testPool(t)
	s := New(pool)

	owner := insertOwner(t, pool)
	name := uniqueName("legacy")
	parent := uniqueName("parent")

	legacy := childRecord(uniqueName("legacy-a"), owner, "", name, string(protocol.StatusIdle))
	legacy.Labels = map[string]string{"fundi/parent": parent}
	c.Require().NoError(upsertChild(t, s, pool, legacy), "fundi/parent sibling")

	err := upsertChild(t, s, pool, childRecord(uniqueName("legacy-b"), owner, parent, name, string(protocol.StatusIdle)))
	c.True(errors.Is(err, childstore.ErrNameTaken), "rafiki/parent sibling of a fundi/parent one err = %v, want childstore.ErrNameTaken", err)
}

// TestNameHolderFindsLiveSibling covers the found case: the holder's id and its
// status come back, and a name nobody holds reports ok=false with no error.
func TestNameHolderFindsLiveSibling(t *testing.T) {
	c := assert.NewCollecting(t)
	ctx := context.Background()
	pool := testPool(t)
	s := New(pool)

	owner := insertOwner(t, pool)
	name := uniqueName("holder")
	parent := uniqueName("parent")
	holder := childRecord(uniqueName("holder-row"), owner, parent, name, string(protocol.StatusExited))
	c.Require().NoError(upsertChild(t, s, pool, holder), "holder Upsert")

	childID, status, ok, err := s.NameHolder(ctx, owner, parent, name)
	c.Require().NoError(err, "NameHolder")
	c.True(ok, "NameHolder did not find the live holder")
	c.Eq(holder.ChildID, childID, "holder child id")
	c.Eq(string(protocol.StatusExited), status, "holder status")

	_, _, ok, err = s.NameHolder(ctx, owner, parent, uniqueName("nobody"))
	c.NoError(err, "NameHolder for a free name")
	c.False(ok, "NameHolder reported a holder for a name nobody holds")
}

// TestNameHolderIgnoresClosed pins the live filter: a closed holder does not
// answer, so the name reads free exactly when the index has released it.
func TestNameHolderIgnoresClosed(t *testing.T) {
	c := assert.NewCollecting(t)
	ctx := context.Background()
	pool := testPool(t)
	s := New(pool)

	owner := insertOwner(t, pool)
	name := uniqueName("holderclosed")
	parent := uniqueName("parent")
	holder := childRecord(uniqueName("holderclosed-row"), owner, parent, name, string(protocol.StatusExited))
	c.Require().NoError(upsertChild(t, s, pool, holder), "holder Upsert")
	c.Require().NoError(s.Delete(ctx, holder.ChildID), "close the holder")

	childID, status, ok, err := s.NameHolder(ctx, owner, parent, name)
	c.NoError(err, "NameHolder")
	c.False(ok, "NameHolder reported a closed child as holder")
	c.Eq("", childID, "child id")
	c.Eq("", status, "status")
}

// TestNameHolderTopLevel pins the "" parent: it means the owner's top-level
// children, and a same-named child of some parent is not one of them.
func TestNameHolderTopLevel(t *testing.T) {
	c := assert.NewCollecting(t)
	ctx := context.Background()
	pool := testPool(t)
	s := New(pool)

	owner := insertOwner(t, pool)
	name := uniqueName("toplevel")
	parent := uniqueName("parent")

	top := childRecord(uniqueName("top-row"), owner, "", name, string(protocol.StatusIdle))
	c.Require().NoError(upsertChild(t, s, pool, top), "top-level Upsert")
	nested := childRecord(uniqueName("nested-row"), owner, parent, name, string(protocol.StatusIdle))
	c.Require().NoError(upsertChild(t, s, pool, nested), "nested Upsert")

	childID, _, ok, err := s.NameHolder(ctx, owner, "", name)
	c.Require().NoError(err, "NameHolder")
	c.True(ok, "NameHolder did not find the top-level holder")
	c.Eq(top.ChildID, childID, "top-level holder is not the nested child")

	childID, _, ok, err = s.NameHolder(ctx, owner, parent, name)
	c.Require().NoError(err, "NameHolder for the parent")
	c.True(ok, "NameHolder did not find the nested holder")
	c.Eq(nested.ChildID, childID, "nested holder")
}

// TestUpsertNameTakenOnlyForThisIndex: only the sibling-name constraint maps to
// ErrNameTaken. A different unique/lookup failure — here the conversation_id
// foreign key — must reach the caller as the database's own error, because a
// caller told "pick another name" for it would retry a broken write forever.
func TestUpsertNameTakenOnlyForThisIndex(t *testing.T) {
	c := assert.NewCollecting(t)
	pool := testPool(t)
	s := New(pool)

	owner := insertOwner(t, pool)
	rec := childRecord(uniqueName("fk"), owner, "", uniqueName("fkname"), string(protocol.StatusIdle))
	rec.ConversationID = "00000000-0000-0000-0000-000000000000" // no such conversation

	err := upsertChild(t, s, pool, rec)
	c.Require().Error(err, "Upsert with an unknown conversation_id")
	c.False(errors.Is(err, childstore.ErrNameTaken), "foreign-key violation mapped to ErrNameTaken: %v", err)

	var pgErr *pgconn.PgError
	c.True(errors.As(err, &pgErr), "err = %v, want a *pgconn.PgError passing through", err)
	c.Eq("23503", pgErr.Code, "SQLSTATE")
}
