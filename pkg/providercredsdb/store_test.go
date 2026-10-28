// SPDX-License-Identifier: Apache-2.0

package providercredsdb

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
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

// lockWaitTimeout bounds every wait in the concurrency test. It is generous
// because it only elapses when the property under test is broken.
const lockWaitTimeout = 10 * time.Second

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

// ownerUsername is unique to this test and this run: the shared database keeps
// earlier runs' rows until their cleanup ran, and a crashed run's rows may
// still be there.
func ownerUsername(prefix string) string {
	return fmt.Sprintf("%s_%d_%d", prefix, time.Now().UnixNano(), ownerSeq.Add(1))
}

// dropOwner removes everything a test created for one owner.
func dropOwner(t *testing.T, pool *pgxpool.Pool, owner string) {
	t.Helper()
	t.Cleanup(func() {
		ctx := context.Background()
		_, _ = pool.Exec(ctx,
			`DELETE FROM conversations.provider_credential WHERE owner_user_id = $1::uuid`, owner)
		_, _ = pool.Exec(ctx, `DELETE FROM conversations.users WHERE id = $1::uuid`, owner)
	})
}

// newOwner creates a real conversations.users row (owner_user_id is a uuid FK
// to it) with a username unique to this run.
func newOwner(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	c := assert.NewAborting(t)
	username := ownerUsername("pc")
	u, _, err := usersdb.NewPostgresStore(pool).Create(context.Background(),
		users.NewUser{Username: username})
	c.NoError(err, "create user %s", username)
	dropOwner(t, pool, u.ID)
	return u.ID
}

// newFacedOwner is a user row whose id certainly contains hex letters, so a
// test can spell the same id in two cases and the difference is never vacuous.
// The tail is a fresh v4 UUID, so runs cannot collide.
func newFacedOwner(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	c := assert.NewAborting(t)
	id := "abcdef01" + uuid.NewString()[8:]
	username := ownerUsername("pcc")
	_, err := pool.Exec(context.Background(),
		`INSERT INTO conversations.users (id, username, token_sha256) VALUES ($1::uuid, $2, $3)`,
		id, username, "pccase-"+username)
	c.NoError(err, "create user %s", username)
	dropOwner(t, pool, id)
	return id
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

// ageRow backdates a row's last refresh, which is how a test asks ListStale
// for it and how it proves a rotation moves the timestamp forward.
func ageRow(t *testing.T, pool *pgxpool.Pool, owner, provider string, age time.Duration) {
	t.Helper()
	_, err := pool.Exec(context.Background(),
		`UPDATE conversations.provider_credential
		 SET last_refresh_at = now() - make_interval(secs => $3), updated_at = now()
		 WHERE owner_user_id = $1::uuid AND provider = $2`, owner, provider, age.Seconds())
	assert.NewAborting(t).NoError(err, "age %s", provider)
}

// rowSnapshot is the part of a stored row that proves whether a write happened:
// the sealed blobs (re-sealing uses a fresh nonce, so identical bytes mean no
// write at all) plus the two timestamps.
type rowSnapshot struct {
	keyID         string
	refreshSealed []byte
	accessSealed  []byte
	lastRefresh   time.Time
	updatedAt     time.Time
}

func snapshotRow(t *testing.T, pool *pgxpool.Pool, owner, provider string) rowSnapshot {
	t.Helper()
	var s rowSnapshot
	err := pool.QueryRow(context.Background(),
		`SELECT key_id, refresh_sealed, access_sealed, last_refresh_at, updated_at
		 FROM conversations.provider_credential
		 WHERE owner_user_id = $1::uuid AND provider = $2`, owner, provider).
		Scan(&s.keyID, &s.refreshSealed, &s.accessSealed, &s.lastRefresh, &s.updatedAt)
	assert.NewAborting(t).NoError(err, "snapshot row")
	return s
}

// captureSlog sends the process logger into a buffer for one test, so a test
// can assert what a package logged and keep the output clean.
func captureSlog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

// The associated data a token is sealed under is a wire format between
// everything that ever seals or opens these columns, so it is pinned here,
// including the owner normalisation that keeps Seal and Open in agreement.
func TestProviderCredAADShape(t *testing.T) {
	c := assert.NewAborting(t)
	c.Eq("alice|openai-responses-chatgpt|refresh",
		string(aad("alice", "openai-responses-chatgpt", fieldRefresh)), "refresh AAD")
	c.Eq("alice|openai-responses-chatgpt|access",
		string(aad("alice", "openai-responses-chatgpt", fieldAccess)), "access AAD")
	// Seal gets the caller's spelling and Open gets the row's canonical
	// lowercase text, so the AAD must fold the case itself.
	c.Eq("alice|openai-responses-chatgpt|refresh",
		string(aad("ALICE", "openai-responses-chatgpt", fieldRefresh)), "uppercase owner AAD")
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

// The AAD must not depend on how the caller spells the owner: Seal gets the
// caller's string, Open gets the row's canonical owner_user_id::text. An
// uppercase Put that cannot be read back, or an uppercase Refresh that re-seals
// a good row into an unopenable one, is silent data loss.
func TestProviderCredOwnerCaseVariantsAgree(t *testing.T) {
	c := assert.NewAborting(t)
	pool := testPool(t)
	ctx := context.Background()
	owner := newFacedOwner(t, pool)
	upper := strings.ToUpper(owner)
	c.True(upper != owner, "the fixture id %q has no hex letters to re-case", owner)

	st := New(pool, newBox(t, 0x13))
	want := fixture(owner)
	want.OwnerUserID = upper // sealed under the uglier spelling
	c.NoError(st.Put(ctx, want), "put with an uppercase owner")

	got, err := st.Get(ctx, owner, testProvider)
	c.NoError(err, "get with the canonical owner")
	c.Eq(want.RefreshToken, got.RefreshToken, "refresh token")
	c.Eq(owner, got.OwnerUserID, "returned owner is not the row's canonical form")

	up, err := st.Get(ctx, upper, testProvider)
	c.NoError(err, "get with the uppercase owner")
	c.Eq(want.AccessToken, up.AccessToken, "access token")

	// A refresh issued under a differently-cased id must rotate the row, not
	// re-seal it into something the canonical owner can never open again.
	refreshed, err := st.Refresh(ctx, upper, testProvider, func(cur providercreds.Credential) (providercreds.Credential, error) {
		next := cur
		next.AccessToken = "access-token-rotated-under-uppercase"
		return next, nil
	})
	c.NoError(err, "refresh under an uppercase owner")
	c.Eq(owner, refreshed.OwnerUserID, "refreshed credential owner is not canonical")
	after, err := st.Get(ctx, owner, testProvider)
	c.NoError(err, "get after the uppercase refresh")
	c.Eq("access-token-rotated-under-uppercase", after.AccessToken, "the rotation did not survive")

	c.NoError(st.SetModels(ctx, upper, testProvider, []byte(`[]`), "e"), "set models under an uppercase owner")
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

// The AAD binds each ciphertext to its COLUMN and to its OWNER, not just to a
// key: a row whose two blobs are swapped, or that is copied to another owner,
// must fail to open rather than hand out someone else's credential in the
// wrong field.
func TestProviderCredFieldAndOwnerBinding(t *testing.T) {
	c := assert.NewAborting(t)
	pool := testPool(t)
	ctx := context.Background()
	owner := newOwner(t, pool)
	st := New(pool, newBox(t, 0x23))
	c.NoError(st.Put(ctx, fixture(owner)), "put")

	// Swap the two ciphertext columns: refresh_sealed now holds the blob that
	// was sealed as "access", and vice versa. Opening either must fail.
	_, err := pool.Exec(ctx,
		`UPDATE conversations.provider_credential
		 SET refresh_sealed = access_sealed, access_sealed = refresh_sealed
		 WHERE owner_user_id = $1::uuid AND provider = $2`, owner, testProvider)
	c.NoError(err, "swap the sealed columns")
	_, err = st.Get(ctx, owner, testProvider)
	c.ErrorIs(err, providercreds.ErrDead, "a swapped blob opened = %v, want ErrDead", err)

	// Re-seal cleanly, then copy the row to another owner verbatim: the bytes
	// were bound to owner A, so owner B cannot open them either.
	c.NoError(st.Put(ctx, fixture(owner)), "re-put")
	other := newOwner(t, pool)
	_, err = pool.Exec(ctx,
		`INSERT INTO conversations.provider_credential
			(owner_user_id, provider, key_id, refresh_sealed, access_sealed, access_expiry, account_id, last_refresh_at)
		 SELECT $1::uuid, provider, key_id, refresh_sealed, access_sealed, access_expiry, account_id, last_refresh_at
		 FROM conversations.provider_credential
		 WHERE owner_user_id = $2::uuid AND provider = $3`, other, owner, testProvider)
	c.NoError(err, "copy the row to another owner")
	_, err = st.Get(ctx, other, testProvider)
	c.ErrorIs(err, providercreds.ErrDead, "a row copied to another owner opened = %v, want ErrDead", err)
	// Owner A's own row is unaffected by the copy.
	if _, err := st.Get(ctx, owner, testProvider); err != nil {
		t.Fatalf("owner A's row after the copy = %v, want it readable", err)
	}

	// The same binding must hold on the REFRESH write: a rotated pair that
	// reads back with each token in its own field is what proves the two Seal
	// calls use the two different fields (a shared field would let a swapped
	// or single-field call site pass every read-only test).
	rotated, err := st.Refresh(ctx, owner, testProvider, func(cur providercreds.Credential) (providercreds.Credential, error) {
		next := cur
		next.RefreshToken, next.AccessToken = "refresh-token-rotated", "access-token-rotated"
		return next, nil
	})
	c.NoError(err, "refresh")
	c.Eq("refresh-token-rotated", rotated.RefreshToken, "rotated refresh token")
	c.Eq("access-token-rotated", rotated.AccessToken, "rotated access token")
	back, err := st.Get(ctx, owner, testProvider)
	c.NoError(err, "get after a refresh write")
	c.Eq("refresh-token-rotated", back.RefreshToken, "refresh token after a refresh write")
	c.Eq("access-token-rotated", back.AccessToken, "access token after a refresh write")
}

// probeRowLockHeld asserts that somebody else holds a row lock on this
// credential right now: a FOR UPDATE NOWAIT on the same row must be refused
// with SQLSTATE 55P03 (lock_not_available). Nothing about timing is inferred.
func probeRowLockHeld(t *testing.T, pool *pgxpool.Pool, owner, provider string) {
	t.Helper()
	_, err := pool.Exec(context.Background(),
		`SELECT 1 FROM conversations.provider_credential
		 WHERE owner_user_id = $1::uuid AND provider = $2 FOR UPDATE NOWAIT`, owner, provider)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "55P03" {
		t.Fatalf("FOR UPDATE NOWAIT probe = %v, want SQLSTATE 55P03: no other session holds the row lock", err)
	}
}

// waitRowLockWaiter waits until another backend is blocked on a lock for this
// table, which is what a second Refresh does while the first holds the row
// lock. It reports whether one showed up before lockWaitTimeout.
//
// The waiting backend sits in SELECT ... FOR UPDATE for the credential table,
// so it is both waiting on a Lock and running that statement. The first
// transaction is inside fn at that moment (idle in transaction, not waiting on
// anything), so it can never be counted here.
func waitRowLockWaiter(t *testing.T, pool *pgxpool.Pool) bool {
	t.Helper()
	c := assert.NewAborting(t)
	deadline := time.Now().Add(lockWaitTimeout)
	for time.Now().Before(deadline) {
		var waiting bool
		err := pool.QueryRow(context.Background(), `
			SELECT EXISTS (
				SELECT 1 FROM pg_stat_activity a
				WHERE a.datname = current_database()
				  AND a.pid <> pg_backend_pid()
				  AND a.wait_event_type = 'Lock'
				  AND a.query LIKE '%conversations.provider_credential%'
			)`).Scan(&waiting)
		c.NoError(err, "probe lock waiters")
		if waiting {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}

// Refresh runs fn under a row lock, so two goroutines refreshing at once make
// exactly one upstream call: refresh tokens are single-use and the second
// caller must adopt the already-rotated one.
//
// The overlap is forced, not hoped for. The first fn stays inside the lock
// until (a) a FOR UPDATE NOWAIT probe proves the lock is held WHILE fn runs and
// (b) the second Refresh is observed blocked on that lock in pg_stat_activity.
// Only then is the first released. A missing FOR UPDATE, or a lock taken after
// fn instead of before it, fails one of those two probes rather than
// occasionally racing the wrong way.
func TestProviderCredConcurrentRefreshOneUpstreamCall(t *testing.T) {
	c := assert.NewAborting(t)
	pool := testPool(t)
	ctx := context.Background()
	owner := newOwner(t, pool)
	st := New(pool, newBox(t, 0x31))
	base := fixture(owner)
	base.RefreshToken, base.AccessToken = "r0", "a0"
	c.NoError(st.Put(ctx, base), "put")

	type result struct {
		cred providercreds.Credential
		err  error
	}
	var rotations atomic.Int64
	firstInFn := make(chan struct{})
	releaseFirst := make(chan struct{})

	first := make(chan result, 1)
	go func() {
		cred, err := st.Refresh(ctx, owner, testProvider, func(cur providercreds.Credential) (providercreds.Credential, error) {
			close(firstInFn)
			select {
			case <-releaseFirst:
			case <-time.After(lockWaitTimeout):
				return cur, errors.New("timed out inside fn: the second Refresh never blocked on the row lock")
			}
			if cur.RefreshToken != "r0" {
				return cur, nil
			}
			rotations.Add(1)
			next := cur
			next.RefreshToken, next.AccessToken = "r1", "a1"
			return next, nil
		})
		first <- result{cred, err}
	}()

	<-firstInFn
	// fn is running: the transaction must already hold the row lock.
	probeRowLockHeld(t, pool, owner, testProvider)

	second := make(chan result, 1)
	go func() {
		cred, err := st.Refresh(ctx, owner, testProvider, func(cur providercreds.Credential) (providercreds.Credential, error) {
			if cur.RefreshToken == "r0" {
				// It read the spent token: a second upstream call, which is
				// exactly what the lock exists to prevent.
				rotations.Add(1)
				next := cur
				next.RefreshToken, next.AccessToken = "r2", "a2"
				return next, nil
			}
			return cur, nil
		})
		second <- result{cred, err}
	}()

	// The second Refresh must be blocked on the row lock before the first one
	// does anything else, so it cannot observe the pre-rotation token.
	if !waitRowLockWaiter(t, pool) {
		close(releaseFirst)
		t.Fatal("the second Refresh never blocked on the row lock within " + lockWaitTimeout.String())
	}
	close(releaseFirst)

	gotFirst, gotSecond := <-first, <-second
	c.NoError(gotFirst.err, "first Refresh")
	c.NoError(gotSecond.err, "second Refresh")
	c.Eq("r1", gotFirst.cred.RefreshToken, "first returned refresh token")
	c.Eq("r1", gotSecond.cred.RefreshToken, "second returned refresh token")
	c.Eq(int64(1), rotations.Load(), "upstream refreshes")

	stored, err := st.Get(ctx, owner, testProvider)
	c.NoError(err, "get after concurrent refresh")
	c.Eq("r1", stored.RefreshToken, "stored refresh token")
	c.Eq("a1", stored.AccessToken, "stored access token")
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

// When fn reports ErrDead and the write that records the death fails too, the
// sentinel must survive: a caller told only "the write failed" would retry a
// token the upstream has already refused, forever. The failure is logged
// instead, and the row is left as it was.
func TestProviderCredDeadRefreshKeepsSentinelWhenMarkFails(t *testing.T) {
	c := assert.NewAborting(t)
	pool := testPool(t)
	ctx := context.Background()
	logs := captureSlog(t)
	owner := newOwner(t, pool)
	st := New(pool, newBox(t, 0x42))
	want := fixture(owner)
	c.NoError(st.Put(ctx, want), "put")

	failedCtx, cancel := context.WithCancel(ctx)
	got, err := st.Refresh(failedCtx, owner, testProvider, func(providercreds.Credential) (providercreds.Credential, error) {
		// The death is already known; the tombstone write is what fails.
		cancel()
		return providercreds.Credential{}, fmt.Errorf("upstream refused the token: %w", providercreds.ErrDead)
	})
	c.ErrorIs(err, providercreds.ErrDead, "error = %v, want it to still wrap ErrDead", err)
	c.StrContains(err.Error(), "could not be marked dead", "error does not report the failed write: %v", err)
	c.False(got.Dead, "the returned credential claims a tombstone that did not happen")
	c.StrContains(logs.String(), "could not mark a dead credential", "the failed write was not logged")
	c.NotStrContains(logs.String(), want.RefreshToken, "the warning leaked the refresh token")
	c.NotStrContains(err.Error(), want.RefreshToken, "the error leaked the refresh token")

	// The tombstone did not land, so the row is unchanged.
	after, err := st.Get(ctx, owner, testProvider)
	c.NoError(err, "get after the failed tombstone")
	c.False(after.Dead, "the row was marked dead despite the reported failure")
	c.Eq(want.RefreshToken, after.RefreshToken, "the row changed")
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

// Logout must always work. Deleting a row this box cannot decrypt removes it
// and reports no error, handing back the metadata with no tokens so the caller
// skips a revoke it has no way to perform.
func TestProviderCredDeleteUndecryptableStillSucceeds(t *testing.T) {
	c := assert.NewAborting(t)
	pool := testPool(t)
	ctx := context.Background()
	logs := captureSlog(t)
	owner := newOwner(t, pool)
	stA := New(pool, newBox(t, 0x52))
	stB := New(pool, newBox(t, 0x53))
	want := fixture(owner)
	c.NoError(stA.Put(ctx, want), "put with box A")

	del, err := stB.Delete(ctx, owner, testProvider)
	c.NoError(err, "delete under the wrong key = %v, want it to succeed", err)
	c.Eq("", del.RefreshToken, "refresh token of an undecryptable delete")
	c.Eq("", del.AccessToken, "access token of an undecryptable delete")
	c.Eq(owner, del.OwnerUserID, "owner")
	c.Eq(want.AccountID, del.AccountID, "account id")
	c.NotStrContains(logs.String(), want.RefreshToken, "the warning leaked the refresh token")

	// The row really is gone.
	_, err = stA.Get(ctx, owner, testProvider)
	c.ErrorIs(err, providercreds.ErrNotFound, "get after the undecryptable delete = %v, want ErrNotFound", err)
}

// A write that fails after fn succeeded still returns the ROTATED credential:
// upstream has already spent the old refresh token, so dropping the new one
// would strand the owner until they log in again.
func TestProviderCredRefreshWriteFailureReturnsNewTokens(t *testing.T) {
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

// sqlLiteral quotes a string for the one test that has to build DDL from it.
func sqlLiteral(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

// installCommitFailure makes the COMMIT of an UPDATE on one provider's row
// fail: a DEFERRABLE INITIALLY DEFERRED constraint trigger fires when the
// transaction commits, i.e. after the UPDATE itself has already succeeded. It
// is the only deliberate way to reach Refresh's commit-failure branch.
func installCommitFailure(t *testing.T, pool *pgxpool.Pool, provider string) {
	t.Helper()
	c := assert.NewAborting(t)
	ctx := context.Background()
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	fnName := "pc_commit_fail_fn_" + suffix
	trgName := "pc_commit_fail_trg_" + suffix
	_, err := pool.Exec(ctx, fmt.Sprintf(`CREATE FUNCTION %s() RETURNS trigger LANGUAGE plpgsql AS $fn$
		BEGIN
			RAISE EXCEPTION 'providercredsdb test: forced commit failure';
		END
	$fn$`, fnName))
	c.NoError(err, "create trigger function")
	t.Cleanup(func() {
		bg := context.Background()
		_, _ = pool.Exec(bg, `DROP TRIGGER IF EXISTS `+trgName+` ON conversations.provider_credential`)
		_, _ = pool.Exec(bg, `DROP FUNCTION IF EXISTS `+fnName+`()`)
	})
	_, err = pool.Exec(ctx, fmt.Sprintf(`CREATE CONSTRAINT TRIGGER %s
		AFTER UPDATE ON conversations.provider_credential
		DEFERRABLE INITIALLY DEFERRED
		FOR EACH ROW
		WHEN (NEW.provider = %s)
		EXECUTE FUNCTION %s()`, trgName, sqlLiteral(provider), fnName))
	c.NoError(err, "create constraint trigger")
}

// The COMMIT failing (rather than the write) is the same contract: the rotated
// credential comes back with ErrNotPersisted, and the row is unchanged because
// the transaction never landed.
func TestProviderCredCommitFailureAfterUpdateReturnsNewTokens(t *testing.T) {
	c := assert.NewAborting(t)
	pool := testPool(t)
	ctx := context.Background()
	owner := newOwner(t, pool)
	st := New(pool, newBox(t, 0x62))
	provider := fmt.Sprintf("commitfail_%d", time.Now().UnixNano())
	base := fixture(owner)
	base.Provider = provider
	base.RefreshToken, base.AccessToken = "r0", "a0"
	c.NoError(st.Put(ctx, base), "put")
	installCommitFailure(t, pool, provider)

	rotated, err := st.Refresh(ctx, owner, provider, func(cur providercreds.Credential) (providercreds.Credential, error) {
		next := cur
		next.RefreshToken, next.AccessToken = "r1", "a1"
		return next, nil
	})
	c.ErrorIs(err, providercreds.ErrNotPersisted, "refresh whose COMMIT fails = %v, want ErrNotPersisted", err)
	c.Eq("r1", rotated.RefreshToken, "returned refresh token")
	c.Eq("a1", rotated.AccessToken, "returned access token")

	stored, err := st.Get(ctx, owner, provider)
	c.NoError(err, "get after the failed commit")
	c.Eq("r0", stored.RefreshToken, "stored refresh token after the failed commit")
}

// An fn that decides no refresh is needed (another holder already rotated)
// writes NOTHING: not the row, not the timestamps. Re-sealing identical tokens
// would burn a fresh nonce and reset the staleness clock that ListStale uses.
func TestProviderCredUnchangedRefreshWritesNothing(t *testing.T) {
	c := assert.NewAborting(t)
	pool := testPool(t)
	ctx := context.Background()
	owner := newOwner(t, pool)
	st := New(pool, newBox(t, 0x63))
	c.NoError(st.Put(ctx, fixture(owner)), "put")
	ageRow(t, pool, owner, testProvider, 3*24*time.Hour)
	before := snapshotRow(t, pool, owner, testProvider)

	got, err := st.Refresh(ctx, owner, testProvider, func(cur providercreds.Credential) (providercreds.Credential, error) {
		return cur, nil
	})
	c.NoError(err, "no-op refresh")

	after := snapshotRow(t, pool, owner, testProvider)
	c.EqDeep(before.refreshSealed, after.refreshSealed, "refresh_sealed was rewritten by a no-op refresh")
	c.EqDeep(before.accessSealed, after.accessSealed, "access_sealed was rewritten by a no-op refresh")
	c.True(before.updatedAt.Equal(after.updatedAt), "updated_at = %v, want it left at %v", after.updatedAt, before.updatedAt)
	c.True(before.lastRefresh.Equal(after.lastRefresh), "last_refresh_at = %v, want it left at %v", after.lastRefresh, before.lastRefresh)
	c.True(got.LastRefresh.Equal(before.lastRefresh), "returned last refresh = %v, want the stored %v", got.LastRefresh, before.lastRefresh)
}

// A real rotation refreshes the staleness clock: ListStale reads
// last_refresh_at, and a credential that keeps being rotated must stop being
// listed.
func TestProviderCredRefreshBumpsLastRefreshOnRotation(t *testing.T) {
	c := assert.NewAborting(t)
	pool := testPool(t)
	ctx := context.Background()
	owner := newOwner(t, pool)
	st := New(pool, newBox(t, 0x64))
	base := fixture(owner)
	base.RefreshToken, base.AccessToken = "r0", "a0"
	c.NoError(st.Put(ctx, base), "put")
	ageRow(t, pool, owner, testProvider, 9*24*time.Hour)
	before := snapshotRow(t, pool, owner, testProvider)

	rotated, err := st.Refresh(ctx, owner, testProvider, func(cur providercreds.Credential) (providercreds.Credential, error) {
		next := cur
		next.RefreshToken, next.AccessToken = "r1", "a1"
		return next, nil
	})
	c.NoError(err, "refresh")

	after := snapshotRow(t, pool, owner, testProvider)
	c.True(after.lastRefresh.After(before.lastRefresh.Add(24*time.Hour)),
		"last_refresh_at = %v, want it advanced past the backdated %v", after.lastRefresh, before.lastRefresh)
	c.True(after.lastRefresh.After(time.Now().Add(-time.Hour)),
		"last_refresh_at = %v, want it set to about now", after.lastRefresh)
	c.True(rotated.LastRefresh.Equal(after.lastRefresh),
		"returned last refresh = %v, want the stored %v", rotated.LastRefresh, after.lastRefresh)
}

// Refresh is owner-scoped: rotating one owner's credential must not touch
// another owner's row for the same provider.
func TestProviderCredRefreshIsOwnerScoped(t *testing.T) {
	c := assert.NewAborting(t)
	pool := testPool(t)
	ctx := context.Background()
	ownerA := newOwner(t, pool)
	ownerB := newOwner(t, pool)
	st := New(pool, newBox(t, 0x65))

	a := fixture(ownerA)
	a.RefreshToken, a.AccessToken = "a-r0", "a-a0"
	c.NoError(st.Put(ctx, a), "put A")
	b := fixture(ownerB)
	b.RefreshToken, b.AccessToken = "b-r0", "b-a0"
	c.NoError(st.Put(ctx, b), "put B")
	ageRow(t, pool, ownerA, testProvider, 3*24*time.Hour)
	ageRow(t, pool, ownerB, testProvider, 3*24*time.Hour)
	beforeB := snapshotRow(t, pool, ownerB, testProvider)

	rotatedA, err := st.Refresh(ctx, ownerA, testProvider, func(cur providercreds.Credential) (providercreds.Credential, error) {
		next := cur
		next.RefreshToken, next.AccessToken = "a-r1", "a-a1"
		return next, nil
	})
	c.NoError(err, "refresh A")
	c.Eq("a-r1", rotatedA.RefreshToken, "A's refreshed token")

	afterB := snapshotRow(t, pool, ownerB, testProvider)
	c.EqDeep(beforeB.refreshSealed, afterB.refreshSealed, "B's refresh_sealed was rewritten by A's refresh")
	c.EqDeep(beforeB.accessSealed, afterB.accessSealed, "B's access_sealed was rewritten by A's refresh")
	c.True(beforeB.lastRefresh.Equal(afterB.lastRefresh), "B's last_refresh_at = %v, want it left at %v", afterB.lastRefresh, beforeB.lastRefresh)
	c.True(beforeB.updatedAt.Equal(afterB.updatedAt), "B's updated_at = %v, want it left at %v", afterB.updatedAt, beforeB.updatedAt)

	gotB, err := st.Get(ctx, ownerB, testProvider)
	c.NoError(err, "get B")
	c.Eq("b-r0", gotB.RefreshToken, "B's refresh token after A's rotation")
	// And B's own refresh still rotates B's row, from B's token.
	rotatedB, err := st.Refresh(ctx, ownerB, testProvider, func(cur providercreds.Credential) (providercreds.Credential, error) {
		next := cur
		next.RefreshToken = "b-r1"
		return next, nil
	})
	c.NoError(err, "refresh B")
	c.Eq("b-r1", rotatedB.RefreshToken, "B's refreshed token")
}

// ListStale is the sweep's read: live credentials only, and only ones whose
// last refresh is older than the age asked for.
func TestProviderCredListStale(t *testing.T) {
	c := assert.NewAborting(t)
	pool := testPool(t)
	ctx := context.Background()
	owner := newOwner(t, pool)
	st := New(pool, newBox(t, 0x71))

	for _, provider := range []string{"stale-provider", "fresh-provider", "dead-provider"} {
		cred := fixture(owner)
		cred.Provider = provider
		c.NoError(st.Put(ctx, cred), "put %s", provider)
	}
	// The dead row is as old as the stale one, so its absence below is about
	// dead_at, not about time.
	ageRow(t, pool, owner, "stale-provider", 9*24*time.Hour)
	ageRow(t, pool, owner, "dead-provider", 9*24*time.Hour)
	markDead(t, pool, owner, "dead-provider")

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
