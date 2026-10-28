// SPDX-License-Identifier: Apache-2.0

package providercredsdb

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"go.graveland.dev/rafiki/pkg/providercreds"
	"go.graveland.dev/rafiki/pkg/secretbox"
	"go.graveland.dev/rafiki/pkg/store"
	"go.graveland.dev/rafiki/pkg/users"
	"go.graveland.dev/rafiki/pkg/usersdb"

	"github.com/multigres/testkit/assert"
)

// testProvider is the provider string these tests store. It is the real slug
// a ChatGPT login is stored under, so the row looks like production's.
const testProvider = "openai-responses-chatgpt"

// testPool skips without RAFIKI_TEST_DSN, then migrates the SHARED test
// database. That database is not reset between runs and the whole suite shares
// it, so every test creates its own user row with a unique username and
// deletes what it inserted in t.Cleanup; nothing here asserts a global count.
func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	c := assert.NewAborting(t)
	dsn := os.Getenv("RAFIKI_TEST_DSN")
	if dsn == "" {
		c.Eq("", os.Getenv("RAFIKI_REQUIRE_DB"), "RAFIKI_TEST_DSN not set but RAFIKI_REQUIRE_DB is")
		t.Skip("RAFIKI_TEST_DSN not set")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	c.NoError(err, "pgxpool.New")
	t.Cleanup(pool.Close)
	c.NoError(store.Migrate(ctx, pool), "migrate")
	return pool
}

// newBox builds a box from a single repeated byte, so two calls with different
// fills are two different keys.
func newBox(t *testing.T, fill byte) *secretbox.Box {
	t.Helper()
	key := bytes.Repeat([]byte{fill}, 32)
	box, err := secretbox.New(key)
	assert.NewAborting(t).NoError(err, "secretbox.New")
	return box
}

var ownerSeq atomic.Int64

// newOwner creates a real conversations.users row (owner_user_id is a uuid FK
// to it) with a username unique to this run, and deletes the row and every
// credential under it on cleanup.
func newOwner(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	c := assert.NewAborting(t)
	username := fmt.Sprintf("pc_%d_%d", time.Now().UnixNano(), ownerSeq.Add(1))
	u, _, err := usersdb.NewPostgresStore(pool).Create(context.Background(),
		users.NewUser{Username: username})
	c.NoError(err, "create user %s", username)
	t.Cleanup(func() {
		ctx := context.Background()
		_, _ = pool.Exec(ctx,
			`DELETE FROM conversations.provider_credential WHERE owner_user_id = $1::uuid`, u.ID)
		_, _ = pool.Exec(ctx, `DELETE FROM conversations.users WHERE id = $1::uuid`, u.ID)
	})
	return u.ID
}

// fixture is a complete credential, with the tokens spelled out so a test can
// look for them in the ciphertext and in error text.
func fixture(owner string) providercreds.Credential {
	return providercreds.Credential{
		OwnerUserID:  owner,
		Provider:     testProvider,
		AccountID:    "acct_round_trip",
		RefreshToken: "refresh-token-4f9a-plaintext",
		AccessToken:  "access-token-7c1b-plaintext",
		// Postgres keeps microseconds, so the fixture is truncated to them.
		AccessExpiry: time.Now().Add(time.Hour).Truncate(time.Microsecond),
		ModelsJSON:   []byte(`[{"id":"gpt-5"}]`),
		ModelsETag:   `W/"etag-1"`,
	}
}

// decodeJSON compares a stored model document as JSON, not as bytes: the
// column is JSONB, so Postgres returns its own canonical rendering of the
// value rather than the bytes that were written.
func decodeJSON(t *testing.T, b []byte) any {
	t.Helper()
	var v any
	assert.NewAborting(t).NoError(json.Unmarshal(b, &v), "unmarshal %q", b)
	return v
}

// markDead tombstones a stored row the way a refused refresh does, without
// going through Refresh.
func markDead(t *testing.T, pool *pgxpool.Pool, owner, provider string) {
	t.Helper()
	_, err := pool.Exec(context.Background(),
		`UPDATE conversations.provider_credential SET dead_at = now()
		 WHERE owner_user_id = $1::uuid AND provider = $2`, owner, provider)
	assert.NewAborting(t).NoError(err, "mark %s dead", provider)
}

// The associated data a token is sealed under is a wire format between
// everything that ever seals or opens these columns, so it is pinned here.
func TestProviderCredAADShape(t *testing.T) {
	c := assert.NewAborting(t)
	c.Eq("alice|openai-responses-chatgpt|refresh",
		string(aad("alice", "openai-responses-chatgpt", fieldRefresh)), "refresh AAD")
	c.Eq("alice|openai-responses-chatgpt|access",
		string(aad("alice", "openai-responses-chatgpt", fieldAccess)), "access AAD")
}

// Every field survives the round trip, and a Put over a dead row brings it
// back to life: a fresh login is exactly how an owner recovers.
func TestProviderCredRoundTrip(t *testing.T) {
	c := assert.NewAborting(t)
	pool := testPool(t)
	ctx := context.Background()
	owner := newOwner(t, pool)
	st := New(pool, newBox(t, 0x11))
	want := fixture(owner)

	c.NoError(st.Put(ctx, want), "put")
	got, err := st.Get(ctx, owner, testProvider)
	c.NoError(err, "get")
	c.Eq(want.OwnerUserID, got.OwnerUserID, "owner")
	c.Eq(want.Provider, got.Provider, "provider")
	c.Eq(want.AccountID, got.AccountID, "account id")
	c.Eq(want.RefreshToken, got.RefreshToken, "refresh token")
	c.Eq(want.AccessToken, got.AccessToken, "access token")
	c.True(got.AccessExpiry.Equal(want.AccessExpiry), "access expiry = %v, want %v", got.AccessExpiry, want.AccessExpiry)
	c.EqDeep(decodeJSON(t, want.ModelsJSON), decodeJSON(t, got.ModelsJSON), "models json")
	c.Eq(want.ModelsETag, got.ModelsETag, "models etag")
	c.False(got.Dead, "a freshly stored credential is dead")
	c.False(got.LastRefresh.IsZero(), "last refresh is zero")

	// A dead row is ErrDead to read, and a Put clears dead_at.
	markDead(t, pool, owner, testProvider)
	if _, err := st.Get(ctx, owner, testProvider); !errors.Is(err, providercreds.ErrDead) {
		t.Fatalf("Get after markDead = %v, want ErrDead", err)
	}
	c.NoError(st.Put(ctx, want), "put after death")
	again, err := st.Get(ctx, owner, testProvider)
	c.NoError(err, "get after re-login")
	c.False(again.Dead, "re-login did not clear dead_at")
	c.Eq(want.RefreshToken, again.RefreshToken, "refresh token after re-login")
}

// The table is dumped by ordinary backups, so the token bytes must not be
// recoverable from the columns that hold them.
func TestProviderCredCiphertextIsNotPlaintext(t *testing.T) {
	c := assert.NewAborting(t)
	pool := testPool(t)
	ctx := context.Background()
	owner := newOwner(t, pool)
	box := newBox(t, 0x12)
	st := New(pool, box)
	want := fixture(owner)
	c.NoError(st.Put(ctx, want), "put")

	var keyID string
	var refreshSealed, accessSealed []byte
	err := pool.QueryRow(ctx,
		`SELECT key_id, refresh_sealed, access_sealed
		 FROM conversations.provider_credential
		 WHERE owner_user_id = $1::uuid AND provider = $2`, owner, testProvider).
		Scan(&keyID, &refreshSealed, &accessSealed)
	c.NoError(err, "read sealed columns")
	c.Eq(box.KeyID(), keyID, "stored key id")
	c.False(bytes.Contains(refreshSealed, []byte(want.RefreshToken)),
		"refresh_sealed contains the refresh token in plaintext")
	c.False(bytes.Contains(accessSealed, []byte(want.AccessToken)),
		"access_sealed contains the access token in plaintext")
	// Sealed is longer than plaintext (nonce and tag), so this is ciphertext,
	// not an empty column that happens not to match.
	c.True(len(refreshSealed) > len(want.RefreshToken), "refresh_sealed is %d bytes, want more than the %d byte token", len(refreshSealed), len(want.RefreshToken))
}

// A key rotation makes every row unreadable rather than yielding garbage or
// someone else's token. The report names the key, never the secret, and the
// row is still readable under the key that sealed it.
func TestProviderCredWrongKeyIsDead(t *testing.T) {
	c := assert.NewAborting(t)
	pool := testPool(t)
	ctx := context.Background()
	owner := newOwner(t, pool)
	stA := New(pool, newBox(t, 0x21))
	stB := New(pool, newBox(t, 0x22))
	want := fixture(owner)
	c.NoError(stA.Put(ctx, want), "put with box A")

	_, err := stB.Get(ctx, owner, testProvider)
	c.ErrorIs(err, providercreds.ErrDead, "get under the wrong key = %v, want ErrDead", err)
	c.StrContains(err.Error(), "RAFIKI_SECRETS_KEY", "error does not name the key variable: %v", err)
	c.NotStrContains(err.Error(), want.RefreshToken, "error leaks the refresh token: %v", err)
	c.NotStrContains(err.Error(), want.AccessToken, "error leaks the access token: %v", err)

	// The correct key still works: a rotation bug is not data loss.
	got, err := stA.Get(ctx, owner, testProvider)
	c.NoError(err, "get under the sealing key")
	c.Eq(want.RefreshToken, got.RefreshToken, "refresh token under the sealing key")
}

// Refresh runs fn under a row lock, so two goroutines refreshing at once make
// exactly one upstream call: refresh tokens are single-use and the second
// caller must adopt the already-rotated one.
func TestProviderCredConcurrentRefreshOneUpstreamCall(t *testing.T) {
	c := assert.NewAborting(t)
	pool := testPool(t)
	ctx := context.Background()
	owner := newOwner(t, pool)
	st := New(pool, newBox(t, 0x31))
	base := fixture(owner)
	base.RefreshToken, base.AccessToken = "r0", "a0"
	c.NoError(st.Put(ctx, base), "put")

	var rotations atomic.Int64
	rotate := func(cur providercreds.Credential) (providercreds.Credential, error) {
		if cur.RefreshToken != "r0" {
			return cur, nil // another holder already refreshed
		}
		rotations.Add(1)
		next := cur
		next.RefreshToken, next.AccessToken = "r1", "a1"
		return next, nil
	}

	const callers = 2
	results := make([]providercreds.Credential, callers)
	errs := make([]error, callers)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range callers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			results[i], errs[i] = st.Refresh(ctx, owner, testProvider, rotate)
		}(i)
	}
	close(start)
	wg.Wait()

	for i := range results {
		c.NoError(errs[i], "Refresh #%d", i)
		c.Eq("r1", results[i].RefreshToken, "Refresh #%d returned refresh token", i)
	}
	c.Eq(int64(1), rotations.Load(), "upstream refreshes")
	stored, err := st.Get(ctx, owner, testProvider)
	c.NoError(err, "get after concurrent refresh")
	c.Eq("r1", stored.RefreshToken, "stored refresh token")
}

// fn reporting ErrDead tombstones the row in the same transaction, and a dead
// row is never handed to fn again: its refresh token is spent.
func TestProviderCredRefreshDeadMarksRow(t *testing.T) {
	c := assert.NewAborting(t)
	pool := testPool(t)
	ctx := context.Background()
	owner := newOwner(t, pool)
	st := New(pool, newBox(t, 0x41))
	c.NoError(st.Put(ctx, fixture(owner)), "put")

	revoked := fmt.Errorf("upstream refused the refresh token: %w", providercreds.ErrDead)
	got, err := st.Refresh(ctx, owner, testProvider, func(providercreds.Credential) (providercreds.Credential, error) {
		return providercreds.Credential{}, revoked
	})
	c.ErrorIs(err, providercreds.ErrDead, "Refresh reporting dead = %v, want ErrDead", err)
	c.True(got.Dead, "returned credential is not marked dead")

	var deadAt *time.Time
	c.NoError(pool.QueryRow(ctx,
		`SELECT dead_at FROM conversations.provider_credential
		 WHERE owner_user_id = $1::uuid AND provider = $2`, owner, testProvider).
		Scan(&deadAt), "read dead_at")
	c.True(deadAt != nil, "dead_at was not set")

	if _, err := st.Get(ctx, owner, testProvider); !errors.Is(err, providercreds.ErrDead) {
		t.Fatalf("Get after a dead refresh = %v, want ErrDead", err)
	}
	called := false
	_, err = st.Refresh(ctx, owner, testProvider, func(cur providercreds.Credential) (providercreds.Credential, error) {
		called = true
		return cur, nil
	})
	c.ErrorIs(err, providercreds.ErrDead, "Refresh on a dead row")
	c.False(called, "fn was called for a dead credential")
}

// Delete hands back the row it removed (a caller revokes those tokens), and
// afterwards every verb reports ErrNotFound without calling fn.
func TestProviderCredDeleteThenRefreshNotFound(t *testing.T) {
	c := assert.NewAborting(t)
	pool := testPool(t)
	ctx := context.Background()
	owner := newOwner(t, pool)
	st := New(pool, newBox(t, 0x51))
	want := fixture(owner)
	c.NoError(st.Put(ctx, want), "put")

	del, err := st.Delete(ctx, owner, testProvider)
	c.NoError(err, "delete")
	c.Eq(want.RefreshToken, del.RefreshToken, "deleted refresh token")
	c.Eq(want.AccessToken, del.AccessToken, "deleted access token")
	c.Eq(want.AccountID, del.AccountID, "deleted account id")

	_, err = st.Delete(ctx, owner, testProvider)
	c.ErrorIs(err, providercreds.ErrNotFound, "second delete = %v, want ErrNotFound", err)

	called := false
	_, err = st.Refresh(ctx, owner, testProvider, func(cur providercreds.Credential) (providercreds.Credential, error) {
		called = true
		return cur, nil
	})
	c.ErrorIs(err, providercreds.ErrNotFound, "refresh after delete = %v, want ErrNotFound", err)
	c.False(called, "fn was called for a deleted credential")

	_, err = st.Get(ctx, owner, testProvider)
	c.ErrorIs(err, providercreds.ErrNotFound, "get after delete = %v, want ErrNotFound", err)
}

// A write that fails after fn succeeded still returns the ROTATED credential:
// upstream has already spent the old refresh token, so dropping the new one
// would strand the owner until they log in again.
func TestProviderCredRefreshCommitFailureReturnsNewTokens(t *testing.T) {
	c := assert.NewAborting(t)
	pool := testPool(t)
	ctx := context.Background()
	owner := newOwner(t, pool)
	st := New(pool, newBox(t, 0x61))
	base := fixture(owner)
	base.RefreshToken, base.AccessToken = "r0", "a0"
	c.NoError(st.Put(ctx, base), "put")

	refreshCtx, cancel := context.WithCancel(ctx)
	rotated, err := st.Refresh(refreshCtx, owner, testProvider, func(cur providercreds.Credential) (providercreds.Credential, error) {
		// The rotation has happened upstream; the write is what fails.
		cancel()
		next := cur
		next.RefreshToken, next.AccessToken = "r1", "a1"
		return next, nil
	})
	c.ErrorIs(err, providercreds.ErrNotPersisted, "refresh with a cancelled context = %v, want ErrNotPersisted", err)
	c.Eq("r1", rotated.RefreshToken, "returned refresh token")
	c.Eq("a1", rotated.AccessToken, "returned access token")

	// The transaction rolled back, so the row still holds the spent token.
	stored, err := st.Get(ctx, owner, testProvider)
	c.NoError(err, "get after the failed refresh")
	c.Eq("r0", stored.RefreshToken, "stored refresh token after rollback")
}

// A no-op refresh (fn returning cur unchanged, because someone else already
// rotated) must not touch last_refresh_at: the row was refreshed by whoever
// won the lock, and bumping it here would hide a credential that is going
// stale.
func TestProviderCredRefreshNoOpKeepsLastRefresh(t *testing.T) {
	c := assert.NewAborting(t)
	pool := testPool(t)
	ctx := context.Background()
	owner := newOwner(t, pool)
	st := New(pool, newBox(t, 0x62))
	c.NoError(st.Put(ctx, fixture(owner)), "put")

	_, err := pool.Exec(ctx,
		`UPDATE conversations.provider_credential SET last_refresh_at = now() - interval '3 days'
		 WHERE owner_user_id = $1::uuid AND provider = $2`, owner, testProvider)
	c.NoError(err, "age the row")

	got, err := st.Refresh(ctx, owner, testProvider, func(cur providercreds.Credential) (providercreds.Credential, error) {
		return cur, nil
	})
	c.NoError(err, "no-op refresh")
	c.False(got.LastRefresh.After(time.Now().Add(-48*time.Hour)),
		"returned last refresh = %v, want the stored 3-day-old value", got.LastRefresh)

	var lastRefresh time.Time
	c.NoError(pool.QueryRow(ctx,
		`SELECT last_refresh_at FROM conversations.provider_credential
		 WHERE owner_user_id = $1::uuid AND provider = $2`, owner, testProvider).
		Scan(&lastRefresh), "read last_refresh_at")
	c.False(lastRefresh.After(time.Now().Add(-48*time.Hour)),
		"stored last refresh = %v, want it left 3 days old", lastRefresh)
}

// ListStale is the sweep's read: live credentials only, and only ones whose
// last refresh is older than the age asked for.
func TestProviderCredListStale(t *testing.T) {
	c := assert.NewAborting(t)
	pool := testPool(t)
	ctx := context.Background()
	owner := newOwner(t, pool)
	st := New(pool, newBox(t, 0x71))

	season := time.Now().Add(-9 * 24 * time.Hour).Truncate(time.Microsecond)
	for _, provider := range []string{"stale-provider", "fresh-provider", "dead-provider"} {
		cred := fixture(owner)
		cred.Provider = provider
		cred.LastRefresh = season
		c.NoError(st.Put(ctx, cred), "put %s", provider)
	}
	// The dead row is as old as the stale one, so its absence below is about
	// dead_at, not about time.
	markDead(t, pool, owner, "dead-provider")
	// The fresh row is refreshed now, whatever the fixture's LastRefresh said.
	_, err := pool.Exec(ctx,
		`UPDATE conversations.provider_credential SET last_refresh_at = now()
		 WHERE owner_user_id = $1::uuid AND provider = $2`, owner, "fresh-provider")
	c.NoError(err, "freshen the fresh row")

	keys, err := st.ListStale(ctx, 8*24*time.Hour)
	c.NoError(err, "list stale")
	c.Contains(keys, providercreds.Key{Owner: owner, Provider: "stale-provider"}, "9-day-old row")
	c.NotContains(keys, providercreds.Key{Owner: owner, Provider: "fresh-provider"}, "fresh row")
	c.NotContains(keys, providercreds.Key{Owner: owner, Provider: "dead-provider"}, "dead row")

	// A longer age excludes even the stale row: the cutoff is read, not ignored.
	fewer, err := st.ListStale(ctx, 30*24*time.Hour)
	c.NoError(err, "list stale at 30 days")
	c.NotContains(fewer, providercreds.Key{Owner: owner, Provider: "stale-provider"}, "9-day-old row at 30 days")
}

// The store is owner-scoped: one owner can neither read nor delete another's
// credential, and writing one does not disturb the other.
func TestProviderCredOwnerIsolation(t *testing.T) {
	c := assert.NewAborting(t)
	pool := testPool(t)
	ctx := context.Background()
	ownerA := newOwner(t, pool)
	ownerB := newOwner(t, pool)
	st := New(pool, newBox(t, 0x81))

	a := fixture(ownerA)
	c.NoError(st.Put(ctx, a), "put A")
	b := fixture(ownerB)
	b.RefreshToken, b.AccessToken = "refresh-token-of-b", "access-token-of-b"
	c.NoError(st.Put(ctx, b), "put B")

	gotB, err := st.Get(ctx, ownerB, testProvider)
	c.NoError(err, "get B")
	c.Eq(b.RefreshToken, gotB.RefreshToken, "B's refresh token")

	gotA, err := st.Get(ctx, ownerA, testProvider)
	c.NoError(err, "get A")
	c.Eq(a.RefreshToken, gotA.RefreshToken, "A's refresh token survived B's Put")

	third := newOwner(t, pool)
	_, err = st.Get(ctx, third, testProvider)
	c.ErrorIs(err, providercreds.ErrNotFound, "another owner's Get = %v, want ErrNotFound", err)
	_, err = st.Delete(ctx, third, testProvider)
	c.ErrorIs(err, providercreds.ErrNotFound, "another owner's Delete = %v, want ErrNotFound", err)
	setErr := st.SetModels(ctx, third, testProvider, []byte(`[]`), "x")
	c.ErrorIs(setErr, providercreds.ErrNotFound, "another owner's SetModels = %v, want ErrNotFound", setErr)
}

// The model directory has its own writer, and Refresh leaves it alone: a
// token rotation must not discard a cache another call just fetched.
func TestProviderCredSetModels(t *testing.T) {
	c := assert.NewAborting(t)
	pool := testPool(t)
	ctx := context.Background()
	owner := newOwner(t, pool)
	st := New(pool, newBox(t, 0x91))
	c.NoError(st.Put(ctx, fixture(owner)), "put")

	c.NoError(st.SetModels(ctx, owner, testProvider, []byte(`[{"id":"gpt-5-mini"}]`), `W/"e2"`), "set models")
	got, err := st.Get(ctx, owner, testProvider)
	c.NoError(err, "get after set models")
	c.EqDeep(decodeJSON(t, []byte(`[{"id":"gpt-5-mini"}]`)), decodeJSON(t, got.ModelsJSON), "models json")
	c.Eq(`W/"e2"`, got.ModelsETag, "models etag")

	rotated, err := st.Refresh(ctx, owner, testProvider, func(cur providercreds.Credential) (providercreds.Credential, error) {
		next := cur
		next.AccessToken = "access-token-rotated"
		return next, nil
	})
	c.NoError(err, "refresh")
	c.EqDeep(decodeJSON(t, []byte(`[{"id":"gpt-5-mini"}]`)), decodeJSON(t, rotated.ModelsJSON), "models json after refresh")
	c.Eq(`W/"e2"`, rotated.ModelsETag, "models etag after refresh")

	// nil is "never fetched", and it is stored as SQL NULL so it reads back
	// as nil rather than an empty document.
	c.NoError(st.SetModels(ctx, owner, testProvider, nil, ""), "clear models")
	got, err = st.Get(ctx, owner, testProvider)
	c.NoError(err, "get after clear")
	c.True(got.ModelsJSON == nil, "models json after clear = %q, want nil", got.ModelsJSON)
	c.Eq("", got.ModelsETag, "models etag after clear")

	c.ErrorIs(st.SetModels(ctx, owner, "no-such-provider", []byte(`[]`), ""), providercreds.ErrNotFound,
		"SetModels for an unknown provider")
}
