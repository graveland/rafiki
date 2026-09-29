// SPDX-License-Identifier: Apache-2.0

package usersdb

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"go.graveland.dev/rafiki/pkg/store"
	"go.graveland.dev/rafiki/pkg/users"

	"github.com/multigres/testkit/assert"
)

// testStore gives each test its own scratch database, migrated fresh —
// mirrors pkg/store/migrate_test.go's testPool rather than the DSN-and-
// DELETE-FROM pattern, so this never touches a developer's real database.
func testStore(t *testing.T) (users.Store, *pgxpool.Pool) {
	t.Helper()
	c := assert.NewAborting(t)
	dsn := os.Getenv("RAFIKI_TEST_DSN")
	if dsn == "" {
		c.Eq("", os.Getenv("RAFIKI_REQUIRE_DB"), "RAFIKI_TEST_DSN not set but RAFIKI_REQUIRE_DB is")
		t.Skip("RAFIKI_TEST_DSN not set; skipping integration test")
	}
	ctx := context.Background()

	admin, err := pgxpool.New(ctx, dsn)
	c.NoError(err, "connect admin")
	t.Cleanup(admin.Close)

	name := fmt.Sprintf("rafiki_users_%d", time.Now().UnixNano())
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatalf("create scratch db: %v", err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(context.Background(), "DROP DATABASE "+name+" WITH (FORCE)")
	})

	cfg, err := pgxpool.ParseConfig(dsn)
	c.NoError(err, "parse dsn")
	cfg.ConnConfig.Database = name
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	c.NoError(err, "connect scratch db")
	t.Cleanup(pool.Close)

	c.NoError(store.Migrate(ctx, pool), "migrate")
	return NewPostgresStore(pool), pool
}

func TestCreateReturnsPlaintextOnceAndAuthenticates(t *testing.T) {
	c := assert.NewAborting(t)
	ctx := context.Background()
	s, pool := testStore(t)

	u, token, err := s.Create(ctx, users.NewUser{Username: "brent", MintToken: true})
	c.NoError(err, "create")
	c.False(u.ID == "" || u.Username != "brent", "create returned %+v", u)
	c.False(len(token) < 20 || token[:4] != "rfk_", "token %q does not look like rfk_<base64url>", token)

	// The plaintext is never stored — in EITHER place. users.token_sha256 is
	// retired (0044 left it nullable and unread); the digest lives in
	// user_token.
	var usersDigest *string
	c.NoError(pool.QueryRow(ctx,
		`SELECT token_sha256 FROM conversations.users WHERE id=$1`, u.ID).Scan(&usersDigest), "read users row")
	c.Nil(usersDigest, "users.token_sha256 was written; it is retired")
	var stored string
	c.NoError(pool.QueryRow(ctx,
		`SELECT token_sha256 FROM conversations.user_token WHERE user_id::text=$1`, u.ID).Scan(&stored), "read token row")
	c.NotEq(token, stored, "plaintext token was stored in user_token.token_sha256")
	c.Eq(users.HashToken(token), stored, "stored digest")

	id, err := s.Authenticate(ctx, token)
	c.NoError(err, "authenticate")
	c.False(id.UserID != u.ID || id.Username != "brent", "identity = %+v, want %s/brent", id, u.ID)
	c.NotEq("", id.TokenID, "identity does not name the token that authenticated")
}

func TestAuthenticateUnknownTokenIsErrNotFound(t *testing.T) {
	ctx := context.Background()
	s, _ := testStore(t)
	_, err := s.Authenticate(ctx, "rfk_nope")
	assert.NewAborting(t).ErrorIs(err, users.ErrNotFound, "err")
}

func TestDuplicateActiveUsernameIsRejected(t *testing.T) {
	ctx := context.Background()
	s, _ := testStore(t)
	if _, _, err := s.Create(ctx, users.NewUser{Username: "brent"}); err != nil {
		t.Fatalf("first create: %v", err)
	}
	_, _, err := s.Create(ctx, users.NewUser{Username: "brent"})
	assert.NewAborting(t).ErrorIs(err, users.ErrUsernameTaken, "err")
}

func TestDeleteTombstonesRevokesAndFreesTheName(t *testing.T) {
	c := assert.NewAborting(t)
	ctx := context.Background()
	s, pool := testStore(t)

	u, token, err := s.Create(ctx, users.NewUser{Username: "brent", MintToken: true})
	c.NoError(err, "create")
	c.NoError(s.Delete(ctx, "brent"), "delete")

	// The row survives — history keeps resolving to it.
	var deletedAt *string
	c.NoError(pool.QueryRow(ctx,
		`SELECT deleted_at::text FROM conversations.users WHERE id=$1`, u.ID).Scan(&deletedAt), "row was hard-deleted")
	c.NotNil(deletedAt, "deleted_at is still NULL after Delete")

	// The token stops working immediately.
	if _, err := s.Authenticate(ctx, token); !errors.Is(err, users.ErrNotFound) {
		t.Fatalf("revoked token still authenticates: %v", err)
	}

	// And the name is reusable.
	if _, _, err := s.Create(ctx, users.NewUser{Username: "brent"}); err != nil {
		t.Fatalf("recreate after tombstone: %v", err)
	}
}

func TestCountActiveIgnoresTombstones(t *testing.T) {
	c := assert.NewAborting(t)
	ctx := context.Background()
	s, _ := testStore(t)

	n, err := s.CountActive(ctx)
	c.NoError(err, "count")
	c.Eq(0, n, "CountActive on empty table")
	if _, _, err := s.Create(ctx, users.NewUser{Username: "brent"}); err != nil {
		t.Fatalf("create: %v", err)
	}
	c.NoError(s.Delete(ctx, "brent"), "delete")
	n, err = s.CountActive(ctx)
	c.NoError(err, "count")
	c.Eq(0, n, "CountActive after tombstoning the only user")
}

func TestListExcludesTombstonesUnlessAsked(t *testing.T) {
	c := assert.NewAborting(t)
	ctx := context.Background()
	s, _ := testStore(t)
	if _, _, err := s.Create(ctx, users.NewUser{Username: "alice"}); err != nil {
		t.Fatalf("create alice: %v", err)
	}
	if _, _, err := s.Create(ctx, users.NewUser{Username: "bob"}); err != nil {
		t.Fatalf("create bob: %v", err)
	}
	c.NoError(s.Delete(ctx, "bob"), "delete bob")

	active, err := s.List(ctx, false, 100)
	c.NoError(err, "list")
	c.False(len(active) != 1 || active[0].Username != "alice", "active list = %+v, want [alice]", active)

	all, err := s.List(ctx, true, 100)
	c.NoError(err, "list all")
	c.Len(all, 2, "full list = %d rows, want 2", len(all))
}

// Tokens must never collide, and Create must not be the thing that notices.
func TestTokensAreDistinctAcrossUsers(t *testing.T) {
	c := assert.NewAborting(t)
	ctx := context.Background()
	s, _ := testStore(t)
	seen := map[string]bool{}
	for _, name := range []string{"a", "b", "c", "d", "e"} {
		_, tok, err := s.Create(ctx, users.NewUser{Username: name, MintToken: true})
		c.NoError(err, "create %s", name)
		c.False(seen[tok], "duplicate token minted for %s", name)
		seen[tok] = true
	}
}

// A store that cannot reach the database has not learned the credential is
// invalid — it has learned nothing. Authenticate must not collapse "I could
// not check" into ErrNotFound, because a caller maps ErrNotFound to 401 and
// everything else to 503; the reference implementation this package was
// modelled on (executorsdb.pgStore.authenticateByHash) gets this wrong today,
// returning ErrNotFound for every error including a closed pool.
func TestAuthenticateOnClosedPoolIsNotErrNotFound(t *testing.T) {
	c := assert.NewAborting(t)
	ctx := context.Background()
	s, pool := testStore(t)
	pool.Close()

	_, err := s.Authenticate(ctx, "rfk_whatever")
	c.Error(err, "expected an error against a closed pool, got nil")
	c.False(errors.Is(err, users.ErrNotFound), "closed-pool error must not be ErrNotFound (that means the credential is invalid, not that the check failed): %v", err)
}

// List's limit parameter must actually reach the query, not just be accepted
// and ignored.
func TestListRespectsLimit(t *testing.T) {
	c := assert.NewAborting(t)
	ctx := context.Background()
	s, _ := testStore(t)
	for _, name := range []string{"a", "b", "c"} {
		if _, _, err := s.Create(ctx, users.NewUser{Username: name}); err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
	}

	limited, err := s.List(ctx, false, 2)
	c.NoError(err, "list")
	c.Len(limited, 2, "List(limit=2) over 3 active users returned %d rows, want 2", len(limited))
}

func TestDeleteUnknownUsernameIsErrNotFound(t *testing.T) {
	ctx := context.Background()
	s, _ := testStore(t)
	assert.NewAborting(t).ErrorIs(s.Delete(ctx, "nobody"), users.ErrNotFound, "err")
}

func TestDeleteAlreadyTombstonedUsernameIsErrNotFound(t *testing.T) {
	c := assert.NewAborting(t)
	ctx := context.Background()
	s, _ := testStore(t)
	if _, _, err := s.Create(ctx, users.NewUser{Username: "brent"}); err != nil {
		t.Fatalf("create: %v", err)
	}
	c.NoError(s.Delete(ctx, "brent"), "first delete")
	c.ErrorIs(s.Delete(ctx, "brent"), users.ErrNotFound, "second delete on an already-tombstoned user: err")
}

// LookupUsername resolves ACTIVE rows only. A username is unique only among
// active users, so a lookup landing on a tombstone would attribute work to a
// deleted account — the whole reason this method exists beside List.
func TestLookupUsernameResolvesTheActiveRow(t *testing.T) {
	c := assert.NewAborting(t)
	ctx := context.Background()
	s, _ := testStore(t)

	u, _, err := s.Create(ctx, users.NewUser{Username: "brent"})
	c.NoError(err, "create")
	got, err := s.LookupUsername(ctx, "brent")
	c.NoError(err, "lookup")
	c.Eq(u.ID, got, "lookup")
}

func TestLookupUsernameMissesATombstone(t *testing.T) {
	c := assert.NewAborting(t)
	ctx := context.Background()
	s, _ := testStore(t)
	if _, _, err := s.Create(ctx, users.NewUser{Username: "brent"}); err != nil {
		t.Fatalf("create: %v", err)
	}
	c.NoError(s.Delete(ctx, "brent"), "delete")
	_, err := s.LookupUsername(ctx, "brent")
	c.ErrorIs(err, users.ErrNotFound, "lookup of a tombstoned name: err")
}

// One name, one active row plus any number of tombstones: the lookup must
// return the ACTIVE row, never the most recent row overall — created_at DESC
// over the whole table would hand back the tombstone here.
func TestLookupUsernameWithActiveAndTombstonesReturnsTheActiveRow(t *testing.T) {
	c := assert.NewAborting(t)
	ctx := context.Background()
	s, _ := testStore(t)

	if _, _, err := s.Create(ctx, users.NewUser{Username: "brent"}); err != nil {
		t.Fatalf("first create: %v", err)
	}
	c.NoError(s.Delete(ctx, "brent"), "delete")
	again, _, err := s.Create(ctx, users.NewUser{Username: "brent"})
	c.NoError(err, "recreate")

	got, err := s.LookupUsername(ctx, "brent")
	c.NoError(err, "lookup")
	c.Eq(again.ID, got, "lookup")
}

func TestLookupUsernameUnknownIsErrNotFound(t *testing.T) {
	ctx := context.Background()
	s, _ := testStore(t)
	_, err := s.LookupUsername(ctx, "nobody")
	assert.NewAborting(t).ErrorIs(err, users.ErrNotFound, "err")
}

// Same rule as Authenticate: a store that cannot reach the database has not
// learned the name is absent.
func TestLookupUsernameOnClosedPoolIsNotErrNotFound(t *testing.T) {
	c := assert.NewAborting(t)
	ctx := context.Background()
	s, pool := testStore(t)
	pool.Close()

	_, err := s.LookupUsername(ctx, "brent")
	c.Error(err, "expected an error against a closed pool, got nil")
	c.False(errors.Is(err, users.ErrNotFound), "closed-pool error must not be ErrNotFound (that means no such active user, not that the check failed): %v", err)
}

// The guard lives in the store so every caller gets it, not just the CLI.
func TestCreateNormalizesAndRejectsBadUsernames(t *testing.T) {
	c := assert.NewAborting(t)
	ctx := context.Background()
	s, _ := testStore(t)

	// Padding is trimmed, not stored — otherwise "brent" and "brent " would be
	// two different people and the partial unique index would allow both.
	u, _, err := s.Create(ctx, users.NewUser{Username: "  brent\t"})
	c.NoError(err, "create with padding")
	c.Eq("brent", u.Username, "username")
	if _, _, err := s.Create(ctx, users.NewUser{Username: "brent  "}); !errors.Is(err, users.ErrUsernameTaken) {
		t.Fatalf("a padded duplicate was accepted (err = %v); trimming must happen BEFORE the uniqueness check", err)
	}

	for _, bad := range []string{"", "   ", "\t\n"} {
		if _, _, err := s.Create(ctx, users.NewUser{Username: bad}); !errors.Is(err, users.ErrInvalidUsername) {
			t.Errorf("Create(%q) error = %v, want ErrInvalidUsername", bad, err)
		}
	}

	// Email is normalized the same way, in the same place.
	if _, _, err := s.Create(ctx, users.NewUser{Username: "e1", Email: "not-an-email"}); !errors.Is(err, users.ErrInvalidEmail) {
		t.Errorf("Create with a malformed email error = %v, want ErrInvalidEmail", err)
	}
	e, _, err := s.Create(ctx, users.NewUser{Username: "e2", Email: "  Brent@Graveland.NET\t"})
	c.NoError(err, "create with padded email")
	c.Eq("brent@graveland.net", e.Email, "stored email must be the normalized form")
}

// The admin bit must survive the full round trip: minted by Create, stored in
// the row, and read back by Authenticate — the ONLY path that ever populates
// Identity.IsAdmin. A plain user's identity must carry the bit false.
func TestCreateAdminSetsIsAdmin(t *testing.T) {
	c := assert.NewAborting(t)
	ctx := context.Background()
	s, _ := testStore(t)

	u, token, err := s.Create(ctx, users.NewUser{Username: "root", IsAdmin: true, MintToken: true})
	c.NoError(err, "create admin")
	c.True(u.IsAdmin, "returned User = %+v, want IsAdmin true", u)

	id, err := s.Authenticate(ctx, token)
	c.NoError(err, "authenticate")
	c.False(id.UserID != u.ID || !id.IsAdmin, "identity = %+v, want IsAdmin true for %s", id, u.ID)

	// The ordinary user created in the same database stays non-admin, so the
	// assertion above cannot be passing on a default-true column.
	plain, plainToken, err := s.Create(ctx, users.NewUser{Username: "peon", MintToken: true})
	c.NoError(err, "create plain user")
	c.False(plain.IsAdmin, "returned User = %+v, want IsAdmin false", plain)
	plainID, err := s.Authenticate(ctx, plainToken)
	c.NoError(err, "authenticate plain")
	c.False(plainID.IsAdmin, "plain identity = %+v, want IsAdmin false", plainID)
}

// mintSecond mints one extra credential on u via the production path.
func mintSecond(t *testing.T, s users.Store, userID, name string, ttl time.Duration) (users.Token, string) {
	t.Helper()
	tok, plaintext, err := s.MintToken(context.Background(), userID, users.NewToken{Name: name, Origin: users.OriginService, TTL: ttl})
	if err != nil {
		t.Fatalf("mint %q: %v", name, err)
	}
	return tok, plaintext
}

// One row per credential, one authenticating row per credential: two tokens
// for one user must both authenticate, and each identity must name the
// credential that produced it — otherwise revoke-by-TokenID can pick the
// wrong row.
func TestAuthenticateMultipleTokens(t *testing.T) {
	c := assert.NewAborting(t)
	ctx := context.Background()
	s, _ := testStore(t)

	u, initial, err := s.Create(ctx, users.NewUser{Username: "brent", MintToken: true})
	c.Require().NoError(err, "create")
	second, secondPlain := mintSecond(t, s, u.ID, "laptop", 0)

	firstID, err := s.Authenticate(ctx, initial)
	c.Require().NoError(err, "authenticate initial")
	c.Eq(u.ID, firstID.UserID, "initial identity user")
	c.NotEq("", firstID.TokenID, "initial identity must name its token")

	secondID, err := s.Authenticate(ctx, secondPlain)
	c.Require().NoError(err, "authenticate second")
	c.Eq(u.ID, secondID.UserID, "second identity user")
	c.NotEq(firstID.TokenID, secondID.TokenID, "the two identities must name different credentials")
	c.Eq(second.ID, secondID.TokenID, "second identity TokenID")
}

// Revocation tombstones ONE credential: the revoked token is ErrNotFound
// while its sibling still authenticates.
func TestAuthenticateRevokedToken(t *testing.T) {
	c := assert.NewAborting(t)
	ctx := context.Background()
	s, _ := testStore(t)

	u, initial, err := s.Create(ctx, users.NewUser{Username: "brent", MintToken: true})
	c.Require().NoError(err, "create")
	second, secondPlain := mintSecond(t, s, u.ID, "laptop", 0)
	revoked, err := s.RevokeToken(ctx, second.ID)
	c.Require().NoError(err, "revoke")
	c.Require().NotNil(revoked.RevokedAt, "revoked token row has no revoked_at")

	if _, err := s.Authenticate(ctx, secondPlain); !errors.Is(err, users.ErrNotFound) {
		t.Fatalf("revoked token still authenticates: %v", err)
	}
	sibling, err := s.Authenticate(ctx, initial)
	c.Require().NoError(err, "sibling token must still authenticate")
	c.Eq(u.ID, sibling.UserID, "sibling identity user")
}

// A token past its expires_at stops authenticating; the row is untouched
// (never deleted) and the user's never-expiring sibling keeps working.
func TestAuthenticateExpiredToken(t *testing.T) {
	c := assert.NewAborting(t)
	ctx := context.Background()
	s, pool := testStore(t)

	u, initial, err := s.Create(ctx, users.NewUser{Username: "brent", MintToken: true})
	c.Require().NoError(err, "create")
	expiring, plain := mintSecond(t, s, u.ID, "ephemeral", time.Second)
	c.Require().NotNil(expiring.ExpiresAt, "a TTL'd token must carry an expires_at")
	c.True(expiring.ExpiresAt.After(time.Now()), "expires_at = %v, want it in the future", expiring.ExpiresAt)

	// Shift the expiry into the past rather than sleeping out the TTL: same
	// predicate, no wall-clock wait.
	if _, err := pool.Exec(ctx,
		`UPDATE conversations.user_token SET expires_at = now() - interval '1 hour' WHERE id = $1::uuid`,
		expiring.ID); err != nil {
		t.Fatalf("age the token: %v", err)
	}

	if _, err := s.Authenticate(ctx, plain); !errors.Is(err, users.ErrNotFound) {
		t.Fatalf("expired token still authenticates: %v", err)
	}
	sibling, err := s.Authenticate(ctx, initial)
	c.Require().NoError(err, "never-expiring sibling must still authenticate")
	c.Eq(u.ID, sibling.UserID, "sibling identity user")
}

// Tombstoning a user kills every one of its credentials at once — the auth
// predicate joins users and filters deleted_at IS NULL.
func TestAuthenticateTombstonedUser(t *testing.T) {
	c := assert.NewAborting(t)
	ctx := context.Background()
	s, _ := testStore(t)

	u, initial, err := s.Create(ctx, users.NewUser{Username: "brent", MintToken: true})
	c.Require().NoError(err, "create")
	_, secondPlain := mintSecond(t, s, u.ID, "laptop", 0)
	c.Require().NoError(s.Delete(ctx, "brent"), "delete")

	if _, err := s.Authenticate(ctx, initial); !errors.Is(err, users.ErrNotFound) {
		t.Fatalf("initial token of a tombstoned user still authenticates: %v", err)
	}
	if _, err := s.Authenticate(ctx, secondPlain); !errors.Is(err, users.ErrNotFound) {
		t.Fatalf("second token of a tombstoned user still authenticates: %v", err)
	}
}

// A user created without a credential gets none: MintToken:false returns no
// plaintext and inserts no user_token row. That is how an OIDC-provisioned
// user arrives, and a silent extra credential would be a live secret nobody
// asked for.
func TestCreateWithoutToken(t *testing.T) {
	c := assert.NewAborting(t)
	ctx := context.Background()
	s, pool := testStore(t)

	u, token, err := s.Create(ctx, users.NewUser{Username: "federated"})
	c.Require().NoError(err, "create without token")
	c.Eq("", token, "MintToken:false must return no plaintext token")
	var n int
	c.Require().NoError(pool.QueryRow(ctx,
		`SELECT count(*) FROM conversations.user_token WHERE user_id::text = $1`, u.ID).Scan(&n), "count tokens")
	c.Eq(0, n, "a user created without MintToken has %d token rows, want 0", n)
}

// An address an active user already holds is refused at Create — and a
// tombstoned user's address is reusable, because the partial index
// users_email_active covers active rows only.
func TestCreateEmailTaken(t *testing.T) {
	c := assert.NewAborting(t)
	ctx := context.Background()
	s, _ := testStore(t)

	_, _, err := s.Create(ctx, users.NewUser{Username: "alice", Email: "shared@graveland.net"})
	c.Require().NoError(err, "create alice")
	// Case-insensitive: the stored form is lowercased, so an uppercase
	// duplicate must not slip past the index.
	_, _, err = s.Create(ctx, users.NewUser{Username: "bob", Email: "Shared@Graveland.NET"})
	c.Require().ErrorIs(err, users.ErrEmailTaken, "duplicate email at create")

	c.Require().NoError(s.Delete(ctx, "alice"), "tombstone alice")
	_, _, err = s.Create(ctx, users.NewUser{Username: "carol", Email: "shared@graveland.net"})
	c.Require().NoError(err, "a tombstoned user's email must be reusable")
}

// SetEmail takes an existing user's address: normalized on the way in, taken
// on collision, cleared by "", reusable once the holder is tombstoned, and
// ErrNotFound for an unknown name.
func TestSetEmailTaken(t *testing.T) {
	c := assert.NewAborting(t)
	ctx := context.Background()
	s, _ := testStore(t)

	if _, _, err := s.Create(ctx, users.NewUser{Username: "alice"}); err != nil {
		t.Fatalf("create alice: %v", err)
	}
	if _, _, err := s.Create(ctx, users.NewUser{Username: "bob"}); err != nil {
		t.Fatalf("create bob: %v", err)
	}

	a, err := s.SetEmail(ctx, "alice", "  Alice@Graveland.NET ")
	c.Require().NoError(err, "set alice's email")
	c.Eq("alice@graveland.net", a.Email, "SetEmail must return the normalized form")

	_, err = s.SetEmail(ctx, "bob", "alice@graveland.net")
	c.Require().ErrorIs(err, users.ErrEmailTaken, "SetEmail onto a taken address")

	cleared, err := s.SetEmail(ctx, "bob", "")
	c.Require().NoError(err, "clear email")
	c.Eq("", cleared.Email, "an empty email must clear the address")

	c.Require().NoError(s.Delete(ctx, "alice"), "tombstone alice")
	b, err := s.SetEmail(ctx, "bob", "alice@graveland.net")
	c.Require().NoError(err, "a tombstoned user's email must be reusable")
	c.Eq("alice@graveland.net", b.Email, "bob's email")

	if _, err := s.SetEmail(ctx, "nobody", "x@y.dev"); !errors.Is(err, users.ErrNotFound) {
		t.Errorf("SetEmail on an unknown user error = %v, want ErrNotFound", err)
	}
}

// Revocation is idempotent: the second call finds the WHERE revoked_at IS
// NULL guard already false, changes nothing, and returns the same tombstone
// with nil — not an error, and not a moved revoked_at.
func TestRevokeTokenIdempotent(t *testing.T) {
	c := assert.NewAborting(t)
	ctx := context.Background()
	s, _ := testStore(t)

	u, _, err := s.Create(ctx, users.NewUser{Username: "brent", MintToken: true})
	c.Require().NoError(err, "create")
	tok, plain := mintSecond(t, s, u.ID, "laptop", 0)

	first, err := s.RevokeToken(ctx, tok.ID)
	c.Require().NoError(err, "first revoke")
	c.Require().NotNil(first.RevokedAt, "revoked_at after first revoke")

	second, err := s.RevokeToken(ctx, tok.ID)
	c.Require().NoError(err, "second revoke")
	c.Require().NotNil(second.RevokedAt, "revoked_at after second revoke")
	c.True(first.RevokedAt.Equal(*second.RevokedAt), "second revoke must not move revoked_at: %v then %v", first.RevokedAt, second.RevokedAt)

	if _, err := s.Authenticate(ctx, plain); !errors.Is(err, users.ErrNotFound) {
		t.Fatalf("revoked token still authenticates: %v", err)
	}
}

// An unknown (well-formed) id and a malformed id are both the ANSWER
// ErrNotFound — never a wrapped SQLSTATE 22P02.
func TestRevokeTokenUnknownAndMalformed(t *testing.T) {
	ctx := context.Background()
	s, _ := testStore(t)
	c := assert.NewAborting(t)
	_, err := s.RevokeToken(ctx, uuid.NewString())
	c.ErrorIs(err, users.ErrNotFound, "unknown id")
	_, err = s.RevokeToken(ctx, "nope")
	c.ErrorIs(err, users.ErrNotFound, "malformed id")
}

// Tokens cannot be minted onto a tombstoned or unknown user: the
// INSERT..SELECT only yields a row for an active user.
func TestMintTokenTombstonedUser(t *testing.T) {
	ctx := context.Background()
	s, _ := testStore(t)
	c := assert.NewAborting(t)

	u, _, err := s.Create(ctx, users.NewUser{Username: "brent", MintToken: true})
	c.Require().NoError(err, "create")
	c.Require().NoError(s.Delete(ctx, "brent"), "delete")

	_, _, err = s.MintToken(ctx, u.ID, users.NewToken{Name: "laptop", Origin: users.OriginService})
	c.ErrorIs(err, users.ErrNotFound, "mint onto a tombstoned user")
	_, _, err = s.MintToken(ctx, uuid.NewString(), users.NewToken{Name: "laptop", Origin: users.OriginService})
	c.ErrorIs(err, users.ErrNotFound, "mint onto an unknown user")
}

// Name and origin are validated before anything is minted; both are plain
// errors (answers), not sentinels and not outages.
func TestMintTokenRejectsBadNameAndOrigin(t *testing.T) {
	ctx := context.Background()
	s, _ := testStore(t)
	c := assert.NewAborting(t)

	u, _, err := s.Create(ctx, users.NewUser{Username: "brent", MintToken: true})
	c.Require().NoError(err, "create")

	_, _, err = s.MintToken(ctx, u.ID, users.NewToken{Origin: users.OriginService})
	if err == nil || err.Error() != "users: token name must not be empty" {
		t.Errorf("empty name error = %v, want users: token name must not be empty", err)
	}
	_, _, err = s.MintToken(ctx, u.ID, users.NewToken{Name: "x", Origin: users.TokenOrigin("api-key")})
	if err == nil || err.Error() != `users: unknown token origin "api-key"` {
		t.Errorf("bad origin error = %v, want users: unknown token origin %q", err, "api-key")
	}
}

// W2-M4 regression: a sub-second TTL must not truncate to "never expires",
// and the expiry must be exact to the microsecond rather than the nearest
// second.
func TestMintTokenSubSecondTTLIsExact(t *testing.T) {
	c := assert.NewAborting(t)
	ctx := context.Background()
	s, _ := testStore(t)

	u, _, err := s.Create(ctx, users.NewUser{Username: "brent", MintToken: true})
	c.Require().NoError(err, "create")

	before := time.Now()
	tok, _, err := s.MintToken(ctx, u.ID, users.NewToken{Name: "ephemeral", Origin: users.OriginService, TTL: 500 * time.Millisecond})
	c.Require().NoError(err, "mint")
	c.Require().NotNil(tok.ExpiresAt, "a 500ms TTL must not be treated as never-expiring")

	want := before.Add(500 * time.Millisecond)
	delta := tok.ExpiresAt.Sub(want)
	if delta < -100*time.Millisecond || delta > 100*time.Millisecond {
		t.Fatalf("expires_at = %v, want within 100ms of %v (delta %v)", tok.ExpiresAt, want, delta)
	}
}

// W2-M4 regression: a negative TTL is refused rather than silently minting a
// never-expiring token.
func TestMintTokenNegativeTTLIsRejected(t *testing.T) {
	ctx := context.Background()
	s, _ := testStore(t)
	c := assert.NewAborting(t)

	u, _, err := s.Create(ctx, users.NewUser{Username: "brent", MintToken: true})
	c.Require().NoError(err, "create")

	_, _, err = s.MintToken(ctx, u.ID, users.NewToken{Name: "x", Origin: users.OriginService, TTL: -time.Second})
	c.Error(err, "negative TTL")
	c.False(errors.Is(err, users.ErrNotFound), "negative TTL must be a plain error, not ErrNotFound")
}

// ListTokens filters by user and by revocation, orders newest first, and an
// empty userID means every user's tokens. The default view lists only live
// credentials — a tombstoned user's unrevoked token is dead with its owner
// and answers only in the includeRevoked audit view, which shows everything.
func TestListTokensFiltersAndOrders(t *testing.T) {
	c := assert.NewAborting(t)
	ctx := context.Background()
	s, _ := testStore(t)

	u1, _, err := s.Create(ctx, users.NewUser{Username: "alice", MintToken: true})
	c.Require().NoError(err, "create alice")
	u2, _, err := s.Create(ctx, users.NewUser{Username: "bob", MintToken: true})
	c.Require().NoError(err, "create bob")
	laptop, _ := mintSecond(t, s, u2.ID, "laptop", 0)
	c.Require().NoError(s.Delete(ctx, "alice"), "tombstone alice")
	_, err = s.RevokeToken(ctx, laptop.ID)
	c.Require().NoError(err, "revoke bob's laptop")

	mine, err := s.ListTokens(ctx, u2.ID, false)
	c.Require().NoError(err, "list bob's active tokens")
	c.Require().Len(mine, 1, "bob's active tokens = %v, want just his initial", mine)
	c.Eq(u2.ID, mine[0].UserID, "token owner")

	// A tombstoned user's unrevoked token is out of the default view — both
	// scoped to her id and in the all-users listing — but stays in the audit
	// view: rows are filtered, never deleted.
	afterTombstone, err := s.ListTokens(ctx, u1.ID, false)
	c.Require().NoError(err, "list a tombstoned user's default view")
	c.Len(afterTombstone, 0, "a tombstoned user's tokens are not live: %v", afterTombstone)
	live, err := s.ListTokens(ctx, "", false)
	c.Require().NoError(err, "all-users default view")
	for _, tok := range live {
		c.NotEq(u1.ID, tok.UserID, "a tombstoned user's token must not be live: %v", tok)
	}

	withRevoked, err := s.ListTokens(ctx, u2.ID, true)
	c.Require().NoError(err, "list bob's tokens including revoked")
	c.Require().Len(withRevoked, 2, "bob's tokens with revoked = %v, want 2", withRevoked)
	c.Eq(laptop.ID, withRevoked[0].ID, "newest token first")

	every, err := s.ListTokens(ctx, "", true)
	c.Require().NoError(err, "list all tokens")
	c.True(len(every) >= 3, "all tokens = %d, want alice's one plus bob's two", len(every))
	c.Eq(laptop.ID, every[0].ID, "newest token first across users")

	// A malformed userID names nobody: an empty list, not a 22P02 outage.
	junk, err := s.ListTokens(ctx, "nope", false)
	c.Require().NoError(err, "list with a malformed userID")
	c.Len(junk, 0, "a malformed userID must list nothing")
}

// A credential minted before 0044 lived on users.token_sha256; the migration
// carried each one over as ('initial', 'service'). This inserts a users row
// the old way, runs that carry-over statement verbatim, and proves the
// plaintext still authenticates.
func TestLegacyTokenCarriedOver(t *testing.T) {
	c := assert.NewAborting(t)
	ctx := context.Background()
	s, pool := testStore(t)

	plain, err := users.NewBearerToken()
	c.Require().NoError(err, "mint legacy token")
	var legacyID string
	c.Require().NoError(pool.QueryRow(ctx,
		`INSERT INTO conversations.users (username, token_sha256, is_admin)
		 VALUES ('legacy', $1, false) RETURNING id::text`,
		users.HashToken(plain)).Scan(&legacyID), "insert legacy users row")

	// The 0044 carry-over, verbatim.
	_, err = pool.Exec(ctx,
		`INSERT INTO conversations.user_token (user_id, token_sha256, name, origin, created_at)
		 SELECT id, token_sha256, 'initial', 'service', created_at
		   FROM conversations.users
		  WHERE token_sha256 IS NOT NULL`)
	c.Require().NoError(err, "run 0044 carry-over")

	id, err := s.Authenticate(ctx, plain)
	c.Require().NoError(err, "legacy plaintext must authenticate after carry-over")
	c.Eq(legacyID, id.UserID, "legacy identity user")
	c.Eq("legacy", id.Username, "legacy identity username")
	c.NotEq("", id.TokenID, "legacy identity must name its carried-over token")

	tok, err := s.GetToken(ctx, id.TokenID)
	c.Require().NoError(err, "get carried-over token")
	c.Eq("initial", tok.Name, "carried-over name")
	c.Eq(users.OriginService, tok.Origin, "carried-over origin")
	c.Eq(legacyID, tok.UserID, "carried-over user")
	c.Nil(tok.ExpiresAt, "carried-over expiry")
	c.Nil(tok.RevokedAt, "carried-over revocation")
}
