// SPDX-License-Identifier: Apache-2.0

// The owner_user_id column: MintToken and Create persist it, Enroll copies it
// off the token, every read path returns it — and a tombstoned owner refuses
// the credential. Each refusal is ErrNotFound, an ANSWER (terminal, the owner
// is gone), never an outage: the other half of that rule lives in
// auth_outage_test.go, and a database failure must stay a wrapped error.
package executorsdb

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"go.graveland.dev/rafiki/pkg/executors"
	"go.graveland.dev/rafiki/pkg/users"
	"go.graveland.dev/rafiki/pkg/usersdb"

	"github.com/multigres/testkit/assert"
)

// testStores returns the executor Store and the user Store over one pool of
// the shared test database — testStore plus a user Store, because the owner
// tests need real users rows for owner_user_id to reference and a real
// users.Store.Delete to tombstone.
func testStores(t *testing.T) (executors.Store, users.Store) {
	t.Helper()
	c := assert.NewAborting(t)
	dsn := os.Getenv("RAFIKI_TEST_DSN")
	if dsn == "" {
		t.Skip("RAFIKI_TEST_DSN is not set")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	c.NoError(err, "connect")
	t.Cleanup(pool.Close)
	c.NoError(ensureTables(ctx, pool), "ensure tables")
	return NewPostgresStore(pool), usersdb.NewPostgresStore(pool)
}

// ownerUser creates a uniquely named active user for a test to attribute
// executors to, tombstoning it on cleanup. The shared database is never
// cleaned and usernames are unique only among active rows, so a fixed name
// would collide with a previous run — the same reason conformance_test.go
// randomizes machine names.
func ownerUser(t *testing.T, us users.Store) users.User {
	t.Helper()
	var b [8]byte
	_, err := rand.Read(b[:])
	assert.NewAborting(t).NoError(err, "rand")
	name := "owner-" + hex.EncodeToString(b[:])
	u, _, err := us.Create(context.Background(), name, false)
	assert.NewAborting(t).NoError(err, "create owner user")
	t.Cleanup(func() { _ = us.Delete(context.Background(), name) })
	return u
}

// listFind returns the executor with id from List's output, failing the test
// if List has dropped it.
func listFind(t *testing.T, s executors.Store, id string) executors.Executor {
	t.Helper()
	c := assert.NewAborting(t)
	list, err := s.List(context.Background())
	c.NoError(err, "list")
	for _, e := range list {
		if e.ID == id {
			return e
		}
	}
	t.Fatalf("List() does not contain executor %s", id)
	return executors.Executor{}
}

// MintToken→Enroll must carry the token's owner onto the executor row, Create
// must insert it directly, and every read path — Authenticate, Get, List —
// must return it. SetLabels returns through Get, so it is covered by the same
// column.
func TestExecutorOwnerRoundTrip(t *testing.T) {
	c := assert.NewAborting(t)
	s, us := testStores(t)
	ctx := context.Background()
	owner := ownerUser(t, us)

	tok, err := s.MintToken(ctx, executors.NewToken{
		OwnerUserID: owner.ID,
		ExpiresAt:   time.Now().Add(time.Hour),
	})
	c.NoError(err, "mint")
	e, cred, err := s.Enroll(ctx, tok, nil)
	c.NoError(err, "enroll")
	t.Cleanup(func() { _ = s.Delete(ctx, e.ID) })
	c.Eq(owner.ID, e.OwnerUserID, "Enroll must copy the token's owner onto the executor row, got %q", e.OwnerUserID)

	authenticated, err := s.Authenticate(ctx, cred)
	c.NoError(err, "authenticate")
	c.Eq(owner.ID, authenticated.OwnerUserID, "Authenticate")

	byID, err := s.Get(ctx, e.ID)
	c.NoError(err, "get")
	c.Eq(owner.ID, byID.OwnerUserID, "Get")

	c.Eq(owner.ID, listFind(t, s, e.ID).OwnerUserID, "List")

	direct, _, err := s.Create(ctx, executors.NewToken{
		OwnerUserID: owner.ID,
		Roots:       []string{},
		ExpiresAt:   time.Now().Add(time.Hour),
	})
	c.NoError(err, "create")
	t.Cleanup(func() { _ = s.Delete(ctx, direct.ID) })
	c.Eq(owner.ID, direct.OwnerUserID, "Create must persist the owner, got %q", direct.OwnerUserID)
}

// A tombstoned owner refuses the executor's own credential AND refuses to
// enroll any further token minted for it — both ErrNotFound, terminal because
// retrying cannot resurrect the user. The error must be the sentinel itself,
// not the wrapped form a store failure returns: that distinction is what keeps
// an executor whose owner was deleted from reconnect-looping forever, and a
// fleet mid-blip from exiting.
func TestExecutorOwnerTombstoned(t *testing.T) {
	c := assert.NewAborting(t)
	s, us := testStores(t)
	ctx := context.Background()
	owner := ownerUser(t, us)

	tok, err := s.MintToken(ctx, executors.NewToken{
		OwnerUserID: owner.ID,
		ExpiresAt:   time.Now().Add(time.Hour),
	})
	c.NoError(err, "mint")
	e, cred, err := s.Enroll(ctx, tok, nil)
	c.NoError(err, "enroll")
	t.Cleanup(func() { _ = s.Delete(ctx, e.ID) })

	// A second, still-unconsumed token for the same owner, enrolled only
	// after the owner is gone.
	unconsumed, err := s.MintToken(ctx, executors.NewToken{
		OwnerUserID: owner.ID,
		ExpiresAt:   time.Now().Add(time.Hour),
	})
	c.NoError(err, "mint second token")

	c.NoError(us.Delete(ctx, owner.Username), "tombstone the owner")

	_, err = s.Authenticate(ctx, cred)
	c.ErrorIs(err, ErrNotFound, "an executor whose owner was tombstoned must not authenticate: err")
	_, _, err = s.Enroll(ctx, unconsumed, nil)
	c.ErrorIs(err, ErrNotFound, "a token whose owner was tombstoned must not enroll: err")
}

// An owner-less token enrolls unowned, the credential authenticates, and every
// read returns "" — the empty/NULL equality both sides of the ownership rule
// count on, and what keeps a pre-existing fleet working untouched.
func TestExecutorUnownedStillWorks(t *testing.T) {
	c := assert.NewAborting(t)
	s, _ := testStores(t)
	ctx := context.Background()

	tok, err := s.MintToken(ctx, executors.NewToken{ExpiresAt: time.Now().Add(time.Hour)})
	c.NoError(err, "mint")
	e, cred, err := s.Enroll(ctx, tok, nil)
	c.NoError(err, "enroll")
	t.Cleanup(func() { _ = s.Delete(ctx, e.ID) })
	c.Eq("", e.OwnerUserID, "an owner-less token must enroll unowned, got %q", e.OwnerUserID)

	authenticated, err := s.Authenticate(ctx, cred)
	c.NoError(err, "authenticate")
	c.Eq("", authenticated.OwnerUserID, "Authenticate")

	byID, err := s.Get(ctx, e.ID)
	c.NoError(err, "get")
	c.Eq("", byID.OwnerUserID, "Get")

	c.Eq("", listFind(t, s, e.ID).OwnerUserID, "List")
}
