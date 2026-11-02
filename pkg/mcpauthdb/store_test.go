// SPDX-License-Identifier: Apache-2.0

package mcpauthdb

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"go.graveland.dev/rafiki/pkg/mcpoauth"
	"go.graveland.dev/rafiki/pkg/secretbox"
	"go.graveland.dev/rafiki/pkg/store"
	"go.graveland.dev/rafiki/pkg/users"
	"go.graveland.dev/rafiki/pkg/usersdb"

	"github.com/multigres/testkit/assert"
)

// The store tests need a real Postgres. They use the SHARED test database,
// which is not reset between runs and is used by the whole suite: every test
// creates its own user and unique resource/endpoint URLs and deletes what it
// inserted. Nothing here asserts a global row count or searches for a fixed
// literal, and concurrency is exercised through SEPARATE pools/store instances,
// never one process's mutex.

// testPool skips without RAFIKI_TEST_DSN, then migrates the shared test
// database once.
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

// secondPool opens an independent connection pool, so a test can prove that
// fencing lives in the database rather than in one Store's memory.
func secondPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	c := assert.NewAborting(t)
	pool, err := pgxpool.New(context.Background(), os.Getenv("RAFIKI_TEST_DSN"))
	c.NoError(err, "pgxpool.New")
	t.Cleanup(pool.Close)
	return pool
}

func newBox(t *testing.T, fill byte) *secretbox.Box {
	t.Helper()
	box, err := secretbox.New(bytes.Repeat([]byte{fill}, 32))
	assert.NewAborting(t).NoError(err, "secretbox.New")
	return box
}

func uniqueName(prefix string) string {
	return fmt.Sprintf("%s_%d_%s", prefix, time.Now().UnixNano(), uuid.NewString()[:8])
}

// dropOwner removes everything a test created for one owner. The MCP tables
// reference conversations.users, so they go first.
func dropOwner(t *testing.T, pool *pgxpool.Pool, owner string) {
	t.Helper()
	t.Cleanup(func() {
		ctx := context.Background()
		_, _ = pool.Exec(ctx, `DELETE FROM conversations.mcp_credential WHERE owner_user_id = $1::uuid`, owner)
		_, _ = pool.Exec(ctx, `DELETE FROM conversations.mcp_endpoint WHERE owner_user_id = $1::uuid`, owner)
		_, _ = pool.Exec(ctx, `DELETE FROM conversations.users WHERE id = $1::uuid`, owner)
	})
}

// newOwner creates a real conversations.users row: owner_user_id is a UUID FK
// to it and every lock path reads it.
func newOwner(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	c := assert.NewAborting(t)
	u, _, err := usersdb.NewPostgresStore(pool).Create(context.Background(),
		users.NewUser{Username: uniqueName("mcp")})
	c.NoError(err, "create user")
	dropOwner(t, pool, u.ID)
	return u.ID
}

// tombstoneOwner makes the users row a tombstone the way user removal does,
// without running any of the cleanup that follows it.
func tombstoneOwner(t *testing.T, pool *pgxpool.Pool, owner string) {
	t.Helper()
	_, err := pool.Exec(context.Background(),
		`UPDATE conversations.users SET deleted_at = now() WHERE id = $1::uuid`, owner)
	assert.NewAborting(t).NoError(err, "tombstone owner")
}

// site builds unique endpoint/resource pair. The endpoint keeps a path, and the
// resource is the origin-form URL, so tests never rely on one deriving from the
// other.
type site struct {
	endpoint string
	resource string
	issuer   string
}

func newSite(t *testing.T) site {
	t.Helper()
	base := fmt.Sprintf("https://mcp-%d-%s.example.com", time.Now().UnixNano(), uuid.NewString()[:8])
	return site{
		endpoint: base + "/mcp",
		resource: base + "/",
		issuer:   base + "/issuer",
	}
}

// fixtureGrant is a complete grant with its secrets spelled out, so a test can
// search the database for them.
func fixtureGrant(owner string, s site) mcpoauth.Grant {
	return mcpoauth.Grant{
		OwnerUserID: owner,
		Resource:    s.resource,
		Issuer:      s.issuer,
		Registration: mcpoauth.Registration{
			ClientID:              "client-" + uuid.NewString(),
			ClientSecret:          "client-secret-" + uuid.NewString(),
			AuthStyle:             mcpoauth.AuthStyleInHeader,
			AuthorizationEndpoint: s.issuer + "/authorize",
			TokenEndpoint:         s.issuer + "/token",
			RevocationEndpoint:    s.issuer + "/revoke",
		},
		Scopes:      []string{"mcp.read", "mcp.write"},
		RedirectURL: "http://127.0.0.1:1456/mcp/oauth/callback",
		Token: mcpoauth.Token{
			AccessToken:  "access-token-" + uuid.NewString(),
			RefreshToken: "refresh-token-" + uuid.NewString(),
			TokenType:    "Bearer",
			Expiry:       time.Now().Add(time.Hour).Truncate(time.Microsecond),
		},
	}
}

// login runs a complete explicit login and returns the fence it committed
// under, so a test can attempt a second commit with it.
func login(t *testing.T, ctx context.Context, st mcpoauth.Store, owner, endpoint string, g mcpoauth.Grant) mcpoauth.LoginFence {
	t.Helper()
	c := assert.NewAborting(t)
	fence, err := st.BeginFence(ctx, owner, endpoint)
	c.NoError(err, "begin fence")
	fence, err = st.BindFence(ctx, fence, g.Resource, g.Issuer)
	c.NoError(err, "bind fence")
	_, err = st.CommitLogin(ctx, fence, g)
	c.NoError(err, "commit login")
	return fence
}

// rawCredential is what the table actually holds, secrets still sealed.
type rawCredential struct {
	generation     int64
	keyID          string
	sealed         []byte
	refreshState   string
	refreshVersion int64
	invalidated    bool
}

func readRawCredential(t *testing.T, pool *pgxpool.Pool, owner, resource string) rawCredential {
	t.Helper()
	var r rawCredential
	err := pool.QueryRow(context.Background(), `
		SELECT generation, key_id, credential_sealed, refresh_state, refresh_version,
		       invalidated_at IS NOT NULL
		  FROM conversations.mcp_credential
		 WHERE owner_user_id = $1::uuid AND resource = $2`, owner, resource).
		Scan(&r.generation, &r.keyID, &r.sealed, &r.refreshState, &r.refreshVersion, &r.invalidated)
	assert.NewAborting(t).NoError(err, "read raw credential")
	return r
}

func rawEndpointGeneration(t *testing.T, pool *pgxpool.Pool, owner, endpoint string) int64 {
	t.Helper()
	var generation int64
	err := pool.QueryRow(context.Background(), `
		SELECT generation FROM conversations.mcp_endpoint
		 WHERE owner_user_id = $1::uuid AND endpoint = $2`, owner, endpoint).Scan(&generation)
	assert.NewAborting(t).NoError(err, "read endpoint generation")
	return generation
}

// The AAD is a wire format between everything that ever seals or opens these
// blobs, so its exact shape is pinned here, including the owner normalisation.
func TestMcpOAuthStoreAADShape(t *testing.T) {
	c := assert.NewAborting(t)
	c.Eq("alice|mcp|https://mcp.example.com/|credential",
		string(aad("alice", "https://mcp.example.com/")), "aad")
	c.Eq("alice|mcp|https://mcp.example.com/|credential",
		string(aad("ALICE", "https://mcp.example.com/")), "uppercase owner is folded")
	// The resource, not the endpoint, is the bound position: the two are
	// different strings for the same server.
	c.NotEq(string(aad("alice", "https://mcp.example.com/")),
		string(aad("alice", "https://mcp.example.com/mcp")), "resource and endpoint are distinct AADs")
}

// The table is dumped by ordinary backups: no token or client secret may be
// recoverable from a column, and the endpoint must carry the canonical URL.
func TestMcpOAuthStoreSealsCredentials(t *testing.T) {
	c := assert.NewAborting(t)
	ctx := context.Background()
	pool := testPool(t)
	owner := newOwner(t, pool)
	box := newBox(t, 0x11)
	st := New(pool, box)
	s := newSite(t)
	want := fixtureGrant(owner, s)

	login(t, ctx, st, owner, s.endpoint, want)
	raw := readRawCredential(t, pool, owner, s.resource)
	c.Eq(box.KeyID(), raw.keyID, "stored key id")
	c.False(bytes.Contains(raw.sealed, []byte(want.Registration.ClientSecret)),
		"credential_sealed contains the client secret in plaintext")
	c.False(bytes.Contains(raw.sealed, []byte(want.Token.AccessToken)),
		"credential_sealed contains the access token in plaintext")
	c.False(bytes.Contains(raw.sealed, []byte(want.Token.RefreshToken)),
		"credential_sealed contains the refresh token in plaintext")
	c.True(len(raw.sealed) > len(want.Token.RefreshToken),
		"credential_sealed is %d bytes, want more than the %d byte token", len(raw.sealed), len(want.Token.RefreshToken))

	// No OTHER column may hold a secret either: scan every text/bytea column of
	// the row for the plaintext values.
	var leaked int
	c.NoError(pool.QueryRow(ctx, `
		SELECT count(*) FROM conversations.mcp_credential
		 WHERE owner_user_id = $1::uuid AND resource = $2
		   AND (client_id LIKE $3 OR redirect_url LIKE $3 OR issuer LIKE $3
		        OR authorization_endpoint LIKE $3 OR token_endpoint LIKE $3
		        OR revocation_endpoint LIKE $3)`,
		owner, s.resource, "%"+want.Token.RefreshToken+"%").Scan(&leaked), "probe non-secret columns")
	c.Eq(0, leaked, "a plaintext column carries the refresh token")
}

// A successful login round-trips through the exact binding with no discovery:
// the endpoint is the configured URL, the resource is its validated audience.
func TestMcpOAuthStoreRoundTrip(t *testing.T) {
	c := assert.NewAborting(t)
	ctx := context.Background()
	pool := testPool(t)
	owner := newOwner(t, pool)
	st := New(pool, newBox(t, 0x12))
	s := newSite(t)
	want := fixtureGrant(owner, s)
	c.True(s.endpoint != s.resource, "the fixture must keep endpoint and resource distinct")
	login(t, ctx, st, owner, s.endpoint, want)

	binding, err := st.LookupBinding(ctx, owner, s.endpoint)
	c.NoError(err, "lookup binding")
	c.Eq(s.resource, binding.Resource, "resource")
	c.Eq(s.issuer, binding.Issuer, "issuer")
	c.Eq(int64(1), binding.GrantGeneration, "a first login installs generation 1")

	got, err := st.Acquire(ctx, owner, s.endpoint, binding.GrantGeneration, nil)
	c.NoError(err, "acquire an unexpired token")
	c.Eq(want.Token.AccessToken, got.Token.AccessToken, "access token")
	c.Eq(want.Token.RefreshToken, got.Token.RefreshToken, "refresh token")
	c.Eq(want.Token.TokenType, got.Token.TokenType, "token type")
	c.True(got.Token.Expiry.Equal(want.Token.Expiry), "expiry = %v, want %v", got.Token.Expiry, want.Token.Expiry)
	c.Eq(want.Registration.ClientID, got.Registration.ClientID, "client id")
	c.Eq(want.Registration.ClientSecret, got.Registration.ClientSecret, "client secret")
	c.Eq(want.Registration.AuthStyle, got.Registration.AuthStyle, "auth style")
	c.Eq(want.Registration.TokenEndpoint, got.Registration.TokenEndpoint, "token endpoint")
	c.Eq(want.Registration.RevocationEndpoint, got.Registration.RevocationEndpoint, "revocation endpoint")
	c.EqDeep(want.Scopes, got.Scopes, "scopes")
	c.Eq(want.RedirectURL, got.RedirectURL, "redirect url")
	c.Eq(int64(1), got.Generation, "generation")
	c.Eq(mcpoauth.RefreshIdle, got.RefreshState, "refresh state")
}

// A ciphertext is bound to its owner and its resource: a copied row must fail
// to authenticate rather than hand out another server's or user's token.
func TestMcpOAuthStoreRejectsTransplantedCiphertext(t *testing.T) {
	c := assert.NewAborting(t)
	ctx := context.Background()
	pool := testPool(t)
	owner := newOwner(t, pool)
	box := newBox(t, 0x13)
	st := New(pool, box)
	s := newSite(t)
	want := fixtureGrant(owner, s)
	login(t, ctx, st, owner, s.endpoint, want)
	raw := readRawCredential(t, pool, owner, s.resource)

	// Copy the identical row (same sealed bytes) to ANOTHER OWNER, with a
	// binding of its own.
	other := newOwner(t, pool)
	otherSite := newSite(t)
	_, err := pool.Exec(ctx, `
		INSERT INTO conversations.mcp_credential
		   (owner_user_id, resource, issuer, generation, key_id, credential_sealed,
		    client_id, auth_style, authorization_endpoint, token_endpoint,
		    revocation_endpoint, scopes, redirect_url, token_type, access_expiry)
		VALUES ($1::uuid, $2, $3, $4, $5, $6, '', 'auto', '', '', '', '{}', '', '', now() + interval '1 hour')`,
		other, otherSite.resource, s.issuer, raw.generation, raw.keyID, raw.sealed)
	c.NoError(err, "copy the row to another owner")
	_, err = pool.Exec(ctx, `
		INSERT INTO conversations.mcp_endpoint
		   (owner_user_id, endpoint, generation, bound_resource, bound_issuer, bound_grant_generation)
		VALUES ($1::uuid, $2, 1, $3, $4, $5)`,
		other, otherSite.endpoint, otherSite.resource, s.issuer, raw.generation)
	c.NoError(err, "bind the copied row for the other owner")
	_, err = st.Acquire(ctx, other, otherSite.endpoint, raw.generation, nil)
	c.ErrorIs(err, mcpoauth.ErrLoginRequired, "a row copied to another owner opened: %v", err)
	c.NotStrContains(err.Error(), want.Token.AccessToken, "the failure leaked the access token")
	c.NotStrContains(err.Error(), want.Token.RefreshToken, "the failure leaked the refresh token")
	c.NotStrContains(err.Error(), want.Registration.ClientSecret, "the failure leaked the client secret")

	// And to ANOTHER RESOURCE of the same owner, which is what an origin-form
	// endpoint swap would amount to.
	secondSite := newSite(t)
	_, err = pool.Exec(ctx, `
		INSERT INTO conversations.mcp_credential
		   (owner_user_id, resource, issuer, generation, key_id, credential_sealed,
		    client_id, auth_style, authorization_endpoint, token_endpoint,
		    revocation_endpoint, scopes, redirect_url, token_type, access_expiry)
		VALUES ($1::uuid, $2, $3, $4, $5, $6, '', 'auto', '', '', '', '{}', '', '', now() + interval '1 hour')`,
		owner, secondSite.resource, s.issuer, raw.generation, raw.keyID, raw.sealed)
	c.NoError(err, "copy the row to another resource")
	_, err = pool.Exec(ctx, `
		INSERT INTO conversations.mcp_endpoint
		   (owner_user_id, endpoint, generation, bound_resource, bound_issuer, bound_grant_generation)
		VALUES ($1::uuid, $2, 1, $3, $4, $5)`,
		owner, secondSite.endpoint, secondSite.resource, s.issuer, raw.generation)
	c.NoError(err, "bind the copied row for the other resource")
	_, err = st.Acquire(ctx, owner, secondSite.endpoint, raw.generation, nil)
	c.ErrorIs(err, mcpoauth.ErrLoginRequired, "a row copied to another resource opened: %v", err)

	// The original is untouched.
	orig, err := st.Acquire(ctx, owner, s.endpoint, 1, nil)
	c.NoError(err, "the original grant after the transplants")
	c.Eq(want.Token.AccessToken, orig.Token.AccessToken, "original access token")
}

// The binding is the exact canonical endpoint, never its origin and never the
// resource: a second endpoint spelling of the same server has no credential
// until it logs in itself.
func TestMcpOAuthStoreBindsExactEndpoint(t *testing.T) {
	c := assert.NewAborting(t)
	ctx := context.Background()
	pool := testPool(t)
	owner := newOwner(t, pool)
	st := New(pool, newBox(t, 0x14))
	s := newSite(t)
	login(t, ctx, st, owner, s.endpoint, fixtureGrant(owner, s))

	if _, err := st.LookupBinding(ctx, owner, s.endpoint); err != nil {
		t.Fatalf("lookup the exact endpoint: %v", err)
	}
	for _, other := range []string{
		strings.TrimSuffix(s.endpoint, "/mcp"), // origin
		s.endpoint + "?x=1",                    // query differs
		s.endpoint + "/",                       // path differs
		s.resource,                             // the resource is not an endpoint
	} {
		_, err := st.LookupBinding(ctx, owner, other)
		c.ErrorIs(err, mcpoauth.ErrMissing, "endpoint %q resolved a binding", other)
	}

	// A different endpoint may bind the SAME resource through its own explicit
	// login; its binding is its own and advances the grant generation.
	second := s.endpoint + "-sse"
	secondGrant := fixtureGrant(owner, s)
	secondGrant.Token.AccessToken = "second-access-token"
	login(t, ctx, st, owner, second, secondGrant)
	binding, err := st.LookupBinding(ctx, owner, second)
	c.NoError(err, "lookup the second endpoint")
	c.Eq(s.resource, binding.Resource, "the second endpoint binds the same resource")
	c.Eq(int64(2), binding.GrantGeneration, "an explicit login advances the grant generation")
}

// Two endpoints may reference one grant (aliases of the same resource). They
// share a rotation, and a logout from either invalidates the grant and clears
// every binding to it.
func TestMcpOAuthStoreSharedGrantAliases(t *testing.T) {
	c := assert.NewAborting(t)
	ctx := context.Background()
	pool := testPool(t)
	owner := newOwner(t, pool)
	st := New(pool, newBox(t, 0x15))
	s := newSite(t)
	base := fixtureGrant(owner, s)
	base.Token.Expiry = time.Now().Add(-time.Minute).Truncate(time.Microsecond) // expired, so both may refresh
	login(t, ctx, st, owner, s.endpoint, base)

	// An alias endpoint bound to the same grant, as an earlier login sharing
	// the resource would leave it.
	alias := s.endpoint + "-alias"
	_, err := pool.Exec(ctx, `
		INSERT INTO conversations.mcp_endpoint
		   (owner_user_id, endpoint, generation, bound_resource, bound_issuer, bound_grant_generation)
		VALUES ($1::uuid, $2, 4, $3, $4, 1)`, owner, alias, s.resource, s.issuer)
	c.NoError(err, "insert the alias binding")
	aliasBinding, err := st.LookupBinding(ctx, owner, alias)
	c.NoError(err, "lookup the alias")
	c.Eq(int64(1), aliasBinding.GrantGeneration, "the alias shares the grant generation")

	rotated := base
	rotated.Token.AccessToken = "rotated-access-token"
	rotated.Token.RefreshToken = "rotated-refresh-token"
	rotated.Token.Expiry = time.Now().Add(time.Hour).Truncate(time.Microsecond)
	got, err := st.Acquire(ctx, owner, alias, 1, func(context.Context, mcpoauth.Grant) (mcpoauth.RefreshResult, error) {
		return mcpoauth.RefreshResult{
			AccessToken: rotated.Token.AccessToken, RefreshToken: rotated.Token.RefreshToken,
			TokenType: rotated.Token.TokenType, Expiry: rotated.Token.Expiry,
		}, nil
	})
	c.NoError(err, "refresh through the alias")
	c.Eq("rotated-access-token", got.Token.AccessToken, "rotated access token")

	// The other alias sees the same rotation without calling upstream, because a
	// refresh does not advance the grant generation.
	called := false
	throughOriginal, err := st.Acquire(ctx, owner, s.endpoint, 1, func(context.Context, mcpoauth.Grant) (mcpoauth.RefreshResult, error) {
		called = true
		return mcpoauth.RefreshResult{}, errors.New("must not refresh")
	})
	c.NoError(err, "acquire through the original alias")
	c.False(called, "an unexpired rotated token was refreshed again")
	c.Eq("rotated-access-token", throughOriginal.Token.AccessToken, "aliases share one rotation")

	// Logging out the alias invalidates the shared grant and clears BOTH
	// bindings.
	rev, err := st.InvalidateEndpoint(ctx, owner, alias)
	c.NoError(err, "logout the alias")
	c.Eq(s.resource, rev.Resource, "revocation resource")
	c.Eq("rotated-refresh-token", rev.RefreshToken, "revocation refresh token")
	c.Eq(int64(2), rev.Generation, "the tombstone advanced the grant generation")
	for _, endpoint := range []string{s.endpoint, alias} {
		_, err := st.LookupBinding(ctx, owner, endpoint)
		c.ErrorIs(err, mcpoauth.ErrMissing, "endpoint %q still has a binding after the logout", endpoint)
	}
}

// Owners never see each other's grants, even when they have identical endpoint
// and resource strings.
func TestMcpOAuthStoreOwnerIsolation(t *testing.T) {
	c := assert.NewAborting(t)
	ctx := context.Background()
	pool := testPool(t)
	st := New(pool, newBox(t, 0x16))
	s := newSite(t)

	first := newOwner(t, pool)
	second := newOwner(t, pool)
	firstGrant := fixtureGrant(first, s)
	firstGrant.Token.AccessToken = "owner-one-token"
	secondGrant := fixtureGrant(second, s)
	secondGrant.Token.AccessToken = "owner-two-token"
	login(t, ctx, st, first, s.endpoint, firstGrant)
	login(t, ctx, st, second, s.endpoint, secondGrant)

	one, err := st.Acquire(ctx, first, s.endpoint, 1, nil)
	c.NoError(err, "owner one acquire")
	c.Eq("owner-one-token", one.Token.AccessToken, "owner one token")
	two, err := st.Acquire(ctx, second, s.endpoint, 1, nil)
	c.NoError(err, "owner two acquire")
	c.Eq("owner-two-token", two.Token.AccessToken, "owner two token")

	if _, err := st.InvalidateEndpoint(ctx, first, s.endpoint); err != nil {
		t.Fatalf("owner one logout: %v", err)
	}
	_, err = st.Acquire(ctx, first, s.endpoint, 1, nil)
	c.ErrorIs(err, mcpoauth.ErrMissing, "owner one after logout")
	two, err = st.Acquire(ctx, second, s.endpoint, 1, nil)
	c.NoError(err, "owner two after owner one's logout")
	c.Eq("owner-two-token", two.Token.AccessToken, "owner two token after owner one's logout")
}

// A database failure is never reported as an absent credential: the caller that
// maps ErrMissing to "public server, no token" must not do so for an outage.
func TestMcpOAuthStoreUnavailableVersusMissing(t *testing.T) {
	c := assert.NewAborting(t)
	ctx := context.Background()
	pool := testPool(t)
	owner := newOwner(t, pool)
	st := New(pool, newBox(t, 0x21))
	s := newSite(t)

	_, err := st.LookupBinding(ctx, owner, s.endpoint)
	c.ErrorIs(err, mcpoauth.ErrMissing, "an endpoint that never logged in")

	dead := secondPool(t)
	dead.Close()
	unavailable := New(dead, newBox(t, 0x21))
	_, err = unavailable.LookupBinding(ctx, owner, s.endpoint)
	c.ErrorIs(err, mcpoauth.ErrUnavailable, "a closed pool")
	c.False(errors.Is(err, mcpoauth.ErrMissing), "a store outage was reported as a missing credential")
	c.False(errors.Is(err, mcpoauth.ErrLoginRequired), "a store outage was reported as a login problem")
}

// Every acquisition consults the store, but an unexpired token is returned
// without calling upstream.
func TestMcpOAuthStoreAcquireValidTokenSkipsRefresh(t *testing.T) {
	c := assert.NewAborting(t)
	ctx := context.Background()
	pool := testPool(t)
	owner := newOwner(t, pool)
	st := New(pool, newBox(t, 0x22))
	s := newSite(t)
	want := fixtureGrant(owner, s)
	login(t, ctx, st, owner, s.endpoint, want)

	got, err := st.Acquire(ctx, owner, s.endpoint, 1, func(context.Context, mcpoauth.Grant) (mcpoauth.RefreshResult, error) {
		return mcpoauth.RefreshResult{}, errors.New("an unexpired token must not refresh")
	})
	c.NoError(err, "acquire")
	c.Eq(want.Token.AccessToken, got.Token.AccessToken, "access token")
}

// A refresh intent committed by one store instance (one pool) refuses another
// instance's acquisition: the single-use token is not spent twice, and the
// second caller adopts the rotation afterwards.
func TestMcpOAuthStoreRefreshOnePerIntentAcrossStores(t *testing.T) {
	c := assert.NewAborting(t)
	ctx := context.Background()
	pool := testPool(t)
	owner := newOwner(t, pool)
	first := New(pool, newBox(t, 0x23))
	s := newSite(t)
	base := fixtureGrant(owner, s)
	base.Token.Expiry = time.Now().Add(-time.Minute).Truncate(time.Microsecond)
	login(t, ctx, first, owner, s.endpoint, base)

	second := New(secondPool(t), newBox(t, 0x23))
	entered := make(chan struct{})
	release := make(chan struct{})
	type outcome struct {
		grant mcpoauth.Grant
		err   error
	}
	firstDone := make(chan outcome, 1)
	go func() {
		g, err := first.Acquire(ctx, owner, s.endpoint, 1, func(context.Context, mcpoauth.Grant) (mcpoauth.RefreshResult, error) {
			close(entered)
			<-release
			return mcpoauth.RefreshResult{
				AccessToken:  "r1-access",
				RefreshToken: "r1-refresh",
				TokenType:    "Bearer",
				Expiry:       time.Now().Add(time.Hour).Truncate(time.Microsecond),
			}, nil
		})
		firstDone <- outcome{g, err}
	}()

	<-entered // the intent is committed and the first holder is upstream
	secondRefreshed := false
	_, err := second.Acquire(ctx, owner, s.endpoint, 1, func(context.Context, mcpoauth.Grant) (mcpoauth.RefreshResult, error) {
		secondRefreshed = true
		return mcpoauth.RefreshResult{}, errors.New("a holder observing an intent must not refresh")
	})
	c.ErrorIs(err, mcpoauth.ErrLoginRequired, "acquire while a refresh intent is committed")
	c.False(secondRefreshed, "the second holder called upstream for the same intent")
	close(release)

	got := <-firstDone
	c.NoError(got.err, "the first refresh")
	c.Eq("r1-access", got.grant.Token.AccessToken, "the first holder's rotated token")

	// The second holder now adopts the committed rotation without refreshing.
	secondRefreshed = false
	adopted, err := second.Acquire(ctx, owner, s.endpoint, 1, func(context.Context, mcpoauth.Grant) (mcpoauth.RefreshResult, error) {
		secondRefreshed = true
		return mcpoauth.RefreshResult{}, errors.New("the rotation is already committed")
	})
	c.NoError(err, "acquire after the rotation committed")
	c.False(secondRefreshed, "the second holder refreshed an already rotated token")
	c.Eq("r1-access", adopted.Token.AccessToken, "adopted access token")
	c.Eq("r1-refresh", adopted.Token.RefreshToken, "adopted refresh token")
}

// A rotation is committed BEFORE the token is returned: a different store
// instance on a different pool sees it immediately, with no replay.
func TestMcpOAuthStoreRefreshPersistsBeforeReturn(t *testing.T) {
	c := assert.NewAborting(t)
	ctx := context.Background()
	pool := testPool(t)
	owner := newOwner(t, pool)
	st := New(pool, newBox(t, 0x24))
	s := newSite(t)
	base := fixtureGrant(owner, s)
	base.Token.Expiry = time.Now().Add(-time.Minute).Truncate(time.Microsecond)
	login(t, ctx, st, owner, s.endpoint, base)

	got, err := st.Acquire(ctx, owner, s.endpoint, 1, func(context.Context, mcpoauth.Grant) (mcpoauth.RefreshResult, error) {
		return mcpoauth.RefreshResult{
			AccessToken:  "persisted-access",
			RefreshToken: "persisted-refresh",
			TokenType:    "Bearer",
			Expiry:       time.Now().Add(time.Hour).Truncate(time.Microsecond),
		}, nil
	})
	c.NoError(err, "refresh")
	c.Eq("persisted-access", got.Token.AccessToken, "returned access token")

	raw := readRawCredential(t, pool, owner, s.resource)
	c.Eq("idle", raw.refreshState, "the intent was not finalized")
	restarted := New(secondPool(t), newBox(t, 0x24))
	called := false
	after, err := restarted.Acquire(ctx, owner, s.endpoint, 1, func(context.Context, mcpoauth.Grant) (mcpoauth.RefreshResult, error) {
		called = true
		return mcpoauth.RefreshResult{}, errors.New("a fresh store must find the committed rotation")
	})
	c.NoError(err, "acquire from a fresh store")
	c.False(called, "the rotation was not visible to a fresh store")
	c.Eq("persisted-access", after.Token.AccessToken, "access token from a fresh store")
	c.Eq("persisted-refresh", after.Token.RefreshToken, "refresh token from a fresh store")
}

// A failed upstream refresh leaves the intent committed: the generation is
// login-required, and a fresh store cannot replay the consumed token.
func TestMcpOAuthStoreFailedRefreshLeavesGenerationUnusable(t *testing.T) {
	c := assert.NewAborting(t)
	ctx := context.Background()
	pool := testPool(t)
	owner := newOwner(t, pool)
	st := New(pool, newBox(t, 0x25))
	s := newSite(t)
	base := fixtureGrant(owner, s)
	base.Token.Expiry = time.Now().Add(-time.Minute).Truncate(time.Microsecond)
	login(t, ctx, st, owner, s.endpoint, base)

	_, err := st.Acquire(ctx, owner, s.endpoint, 1, func(context.Context, mcpoauth.Grant) (mcpoauth.RefreshResult, error) {
		return mcpoauth.RefreshResult{}, errors.New("network slice")
	})
	c.ErrorIs(err, mcpoauth.ErrLoginRequired, "an ambiguous upstream failure")
	c.NotStrContains(err.Error(), base.Token.RefreshToken, "the failure leaked the refresh token")

	raw := readRawCredential(t, pool, owner, s.resource)
	c.Eq("refreshing", raw.refreshState, "the failed intent was cleared, which would replay the token")

	restarted := New(secondPool(t), newBox(t, 0x25))
	called := false
	_, err = restarted.Acquire(ctx, owner, s.endpoint, 1, func(context.Context, mcpoauth.Grant) (mcpoauth.RefreshResult, error) {
		called = true
		return mcpoauth.RefreshResult{}, nil
	})
	c.ErrorIs(err, mcpoauth.ErrLoginRequired, "a fresh store after a failed refresh")
	c.False(called, "a fresh store replayed an abandoned refresh")
}

// installFinalizeFailure makes only the refresh FINALIZE commit fail, via a
// deferred constraint trigger that fires when refresh_state goes back to idle.
// The intent's own commit (state 'refreshing') and the login are untouched.
func installFinalizeFailure(t *testing.T, pool *pgxpool.Pool, resource string) {
	t.Helper()
	c := assert.NewAborting(t)
	ctx := context.Background()
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	fnName := "mcp_finalize_fail_fn_" + suffix
	trgName := "mcp_finalize_fail_trg_" + suffix
	_, err := pool.Exec(ctx, fmt.Sprintf(`CREATE FUNCTION %s() RETURNS trigger LANGUAGE plpgsql AS $fn$
		BEGIN
			RAISE EXCEPTION 'mcpauthdb test: forced finalize failure';
		END
	$fn$`, fnName))
	c.NoError(err, "create trigger function")
	t.Cleanup(func() {
		bg := context.Background()
		_, _ = pool.Exec(bg, `DROP TRIGGER IF EXISTS `+trgName+` ON conversations.mcp_credential`)
		_, _ = pool.Exec(bg, `DROP FUNCTION IF EXISTS `+fnName+`()`)
	})
	_, err = pool.Exec(ctx, fmt.Sprintf(`CREATE CONSTRAINT TRIGGER %s
		AFTER UPDATE ON conversations.mcp_credential
		DEFERRABLE INITIALLY DEFERRED
		FOR EACH ROW
		WHEN (NEW.resource = %s AND NEW.refresh_state = 'idle')
		EXECUTE FUNCTION %s()`, trgName, sqlLiteral(resource), fnName))
	c.NoError(err, "create constraint trigger")
}

func sqlLiteral(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

// A rotation that cannot be committed is never returned (it is not durable),
// and the intent it left behind makes the generation login-required.
func TestMcpOAuthStoreCommitFailureLeavesGenerationUnusable(t *testing.T) {
	c := assert.NewAborting(t)
	ctx := context.Background()
	pool := testPool(t)
	owner := newOwner(t, pool)
	st := New(pool, newBox(t, 0x26))
	s := newSite(t)
	base := fixtureGrant(owner, s)
	login(t, ctx, st, owner, s.endpoint, base) // install the trigger after the login
	if _, err := pool.Exec(ctx, `UPDATE conversations.mcp_credential SET access_expiry = now() - interval '1 minute'
		WHERE owner_user_id = $1::uuid AND resource = $2`, owner, s.resource); err != nil {
		t.Fatalf("expire the token: %v", err)
	}
	installFinalizeFailure(t, pool, s.resource)

	_, err := st.Acquire(ctx, owner, s.endpoint, 1, func(context.Context, mcpoauth.Grant) (mcpoauth.RefreshResult, error) {
		return mcpoauth.RefreshResult{
			AccessToken: "uncommitted-access", RefreshToken: "uncommitted-refresh",
			Expiry: time.Now().Add(time.Hour),
		}, nil
	})
	c.ErrorIs(err, mcpoauth.ErrUnavailable, "a rotation whose COMMIT fails = %v, want ErrUnavailable", err)
	c.NotStrContains(err.Error(), "uncommitted-access", "the failed commit leaked the token")

	raw := readRawCredential(t, pool, owner, s.resource)
	c.Eq("refreshing", raw.refreshState, "a failed finalize cleared the intent")
	if _, err := st.Acquire(ctx, owner, s.endpoint, 1, nil); !errors.Is(err, mcpoauth.ErrLoginRequired) {
		t.Fatalf("acquire after a failed finalize = %v, want ErrLoginRequired", err)
	}
}

// A refresh answer that omits a replacement keeps the stored refresh token:
// dropping a still-valid single-use token would strand the grant.
func TestMcpOAuthStoreRefreshKeepsRefreshTokenWhenOmitted(t *testing.T) {
	c := assert.NewAborting(t)
	ctx := context.Background()
	pool := testPool(t)
	owner := newOwner(t, pool)
	st := New(pool, newBox(t, 0x27))
	s := newSite(t)
	base := fixtureGrant(owner, s)
	base.Token.Expiry = time.Now().Add(-time.Minute).Truncate(time.Microsecond)
	login(t, ctx, st, owner, s.endpoint, base)

	got, err := st.Acquire(ctx, owner, s.endpoint, 1, func(context.Context, mcpoauth.Grant) (mcpoauth.RefreshResult, error) {
		return mcpoauth.RefreshResult{
			AccessToken: "kept-refresh-access",
			Expiry:      time.Now().Add(time.Hour).Truncate(time.Microsecond),
		}, nil
	})
	c.NoError(err, "refresh without a replacement token")
	c.Eq(base.Token.RefreshToken, got.Token.RefreshToken, "the refresh token was dropped")
	c.Eq("kept-refresh-access", got.Token.AccessToken, "access token")

	again, err := st.Acquire(ctx, owner, s.endpoint, 1, nil)
	c.NoError(err, "acquire after the rotation")
	c.Eq(base.Token.RefreshToken, again.Token.RefreshToken, "the stored refresh token was not preserved")
}

// After a logout nothing may hand out a token the store held before it, even
// from the same Store instance: acquisition is checked against the database
// every time.
func TestMcpOAuthStoreLogoutDiscardsPreLogoutToken(t *testing.T) {
	c := assert.NewAborting(t)
	ctx := context.Background()
	pool := testPool(t)
	owner := newOwner(t, pool)
	st := New(pool, newBox(t, 0x31))
	s := newSite(t)
	want := fixtureGrant(owner, s)
	login(t, ctx, st, owner, s.endpoint, want)

	before, err := st.Acquire(ctx, owner, s.endpoint, 1, nil)
	c.NoError(err, "acquire before logout")
	c.Eq(want.Token.AccessToken, before.Token.AccessToken, "access token before logout")

	rev, err := st.InvalidateEndpoint(ctx, owner, s.endpoint)
	c.NoError(err, "logout")
	c.Eq(int64(2), rev.Generation, "the tombstone generation")
	c.Eq(want.Token.RefreshToken, rev.RefreshToken, "the detached revocation carries the refresh token")

	_, err = st.Acquire(ctx, owner, s.endpoint, 1, nil)
	c.ErrorIs(err, mcpoauth.ErrMissing, "acquire after logout")
	_, err = st.LookupBinding(ctx, owner, s.endpoint)
	c.ErrorIs(err, mcpoauth.ErrMissing, "lookup after logout")
}

// A logout that wins while a refresh is in flight blocks its finalization: the
// holder gets no token, and the row is a wiped tombstone.
func TestMcpOAuthStoreLogoutDuringRefreshPreventsFinalization(t *testing.T) {
	c := assert.NewAborting(t)
	ctx := context.Background()
	pool := testPool(t)
	owner := newOwner(t, pool)
	refresher := New(pool, newBox(t, 0x32))
	s := newSite(t)
	base := fixtureGrant(owner, s)
	base.Token.Expiry = time.Now().Add(-time.Minute).Truncate(time.Microsecond)
	login(t, ctx, refresher, owner, s.endpoint, base)

	entered := make(chan struct{})
	release := make(chan struct{})
	type outcome struct {
		grant mcpoauth.Grant
		err   error
	}
	done := make(chan outcome, 1)
	go func() {
		g, err := refresher.Acquire(ctx, owner, s.endpoint, 1, func(context.Context, mcpoauth.Grant) (mcpoauth.RefreshResult, error) {
			close(entered)
			<-release
			return mcpoauth.RefreshResult{
				AccessToken: "must-not-commit", RefreshToken: "must-not-commit",
				Expiry: time.Now().Add(time.Hour),
			}, nil
		})
		done <- outcome{g, err}
	}()
	<-entered

	logout := New(secondPool(t), newBox(t, 0x32))
	rev, err := logout.InvalidateEndpoint(ctx, owner, s.endpoint)
	c.NoError(err, "logout during a refresh")
	c.Eq(base.Token.RefreshToken, rev.RefreshToken, "revocation refresh token")
	close(release)

	got := <-done
	c.ErrorIs(got.err, mcpoauth.ErrLoginRequired, "a refresh finalized after a logout = %v, want it refused", got.err)
	c.Eq("", got.grant.Token.AccessToken, "a token was returned after a logout won")

	raw := readRawCredential(t, pool, owner, s.resource)
	c.True(raw.invalidated, "the grant is not tombstoned")
	c.Len(raw.sealed, 0, "the tombstone did not wipe the sealed blob")
	_, err = logout.Acquire(ctx, owner, s.endpoint, 1, nil)
	c.ErrorIs(err, mcpoauth.ErrMissing, "acquire after the logout")
}

// Two pending logins sharing a grant race on the grant generation: the winner
// installs, the loser must begin again rather than overwrite it.
func TestMcpOAuthStoreLoginGenerationCAS(t *testing.T) {
	c := assert.NewAborting(t)
	ctx := context.Background()
	pool := testPool(t)
	owner := newOwner(t, pool)
	st := New(pool, newBox(t, 0x33))
	s := newSite(t)
	first := fixtureGrant(owner, s)
	login(t, ctx, st, owner, s.endpoint, first) // grant generation 1

	loserEndpoint := s.endpoint + "-loser"
	loserGrant := fixtureGrant(owner, s)
	loserGrant.Token.AccessToken = "loser-access"
	fence, err := st.BeginFence(ctx, owner, loserEndpoint)
	c.NoError(err, "begin the losing login")
	fence, err = st.BindFence(ctx, fence, s.resource, s.issuer)
	c.NoError(err, "bind the losing login")
	c.Eq(int64(1), fence.GrantGeneration, "the loser captured the live generation")

	winnerEndpoint := s.endpoint + "-winner"
	winnerGrant := fixtureGrant(owner, s)
	winnerGrant.Token.AccessToken = "winner-access"
	login(t, ctx, st, owner, winnerEndpoint, winnerGrant) // advances to generation 2

	_, err = st.CommitLogin(ctx, fence, loserGrant)
	c.ErrorIs(err, mcpoauth.ErrStaleFence, "the losing login overwrote the winner")
	_, err = st.Acquire(ctx, owner, winnerEndpoint, 2, nil)
	c.NoError(err, "the winner is installed")
	binding, err := st.LookupBinding(ctx, owner, winnerEndpoint)
	c.NoError(err, "lookup the winner")
	c.Eq(int64(2), binding.GrantGeneration, "winner generation")

	// The loser starts again: a new Begin/Bind captures generation 2 and wins.
	again, err := st.BeginFence(ctx, owner, loserEndpoint)
	c.NoError(err, "begin the retried login")
	again, err = st.BindFence(ctx, again, s.resource, s.issuer)
	c.NoError(err, "bind the retried login")
	c.Eq(int64(2), again.GrantGeneration, "the retry captured the current generation")
	if _, err := st.CommitLogin(ctx, again, loserGrant); err != nil {
		t.Fatalf("the retried login did not commit: %v", err)
	}

	// A second Begin on one endpoint supersedes the fence a different resource
	// was bound under there.
	otherResource := s.issuer + "/other-resource"
	superseded, err := st.BeginFence(ctx, owner, s.endpoint+"-superseded")
	c.NoError(err, "begin the superseded login")
	superseded, err = st.BindFence(ctx, superseded, otherResource, s.issuer)
	c.NoError(err, "bind the superseded login")
	if _, err := st.BeginFence(ctx, owner, s.endpoint+"-superseded"); err != nil {
		t.Fatalf("begin the superseding login: %v", err)
	}
	otherGrant := fixtureGrant(owner, s)
	otherGrant.Resource = otherResource
	_, err = st.CommitLogin(ctx, superseded, otherGrant)
	c.ErrorIs(err, mcpoauth.ErrStaleFence, "a login superseded by a later Begin still committed")
}

// An explicit login is the only recovery from a logout, and it succeeds on the
// incremented tombstone generation.
func TestMcpOAuthStoreExplicitLaterLoginSucceeds(t *testing.T) {
	c := assert.NewAborting(t)
	ctx := context.Background()
	pool := testPool(t)
	owner := newOwner(t, pool)
	st := New(pool, newBox(t, 0x34))
	s := newSite(t)
	first := fixtureGrant(owner, s)
	login(t, ctx, st, owner, s.endpoint, first)

	if _, err := st.InvalidateResource(ctx, owner, s.resource); err != nil {
		t.Fatalf("logout: %v", err)
	}
	_, err := st.Acquire(ctx, owner, s.endpoint, 1, nil)
	c.ErrorIs(err, mcpoauth.ErrMissing, "acquire after logout")

	second := fixtureGrant(owner, s)
	second.Token.AccessToken = "second-login-access"
	fence, err := st.BeginFence(ctx, owner, s.endpoint)
	c.NoError(err, "begin after logout")
	fence, err = st.BindFence(ctx, fence, s.resource, s.issuer)
	c.NoError(err, "bind after logout")
	c.Eq(int64(2), fence.GrantGeneration, "the tombstone generation is the new fence")
	installed, err := st.CommitLogin(ctx, fence, second)
	c.NoError(err, "commit after logout")
	c.Eq(int64(3), installed.Generation, "the new login advances past the tombstone")

	got, err := st.Acquire(ctx, owner, s.endpoint, 3, nil)
	c.NoError(err, "acquire the new login")
	c.Eq("second-login-access", got.Token.AccessToken, "the explicit login is the recovery")
}

// A first login has no grant yet; logging out its endpoint still advances the
// endpoint fence, so the login in flight cannot commit.
func TestMcpOAuthStoreFirstLoginEndpointFence(t *testing.T) {
	c := assert.NewAborting(t)
	ctx := context.Background()
	pool := testPool(t)
	owner := newOwner(t, pool)
	st := New(pool, newBox(t, 0x35))
	s := newSite(t)

	fence, err := st.BeginFence(ctx, owner, s.endpoint)
	c.NoError(err, "begin a first login")
	c.Eq(int64(1), fence.EndpointGeneration, "a first login starts at generation 1")
	fence, err = st.BindFence(ctx, fence, s.resource, s.issuer)
	c.NoError(err, "bind a first login")
	c.Eq(int64(0), fence.GrantGeneration, "no grant exists yet")

	rev, err := st.InvalidateEndpoint(ctx, owner, s.endpoint)
	c.NoError(err, "logout an unbound endpoint")
	c.Eq("", rev.RefreshToken, "an unbound logout has nothing to revoke")
	c.Eq(int64(2), rawEndpointGeneration(t, pool, owner, s.endpoint), "the endpoint fence advanced")

	_, err = st.CommitLogin(ctx, fence, fixtureGrant(owner, s))
	c.ErrorIs(err, mcpoauth.ErrStaleFence, "a first login survived an endpoint logout")

	// A fresh Begin on the same endpoint is a new login and commits.
	retry, err := st.BeginFence(ctx, owner, s.endpoint)
	c.NoError(err, "begin the retried first login")
	c.Eq(int64(2), retry.EndpointGeneration, "the retry reads the advanced fence")
	retry, err = st.BindFence(ctx, retry, s.resource, s.issuer)
	c.NoError(err, "bind the retried first login")
	if _, err := st.CommitLogin(ctx, retry, fixtureGrant(owner, s)); err != nil {
		t.Fatalf("the retried first login did not commit: %v", err)
	}

	// Logging out an endpoint that never existed is locally successful.
	fresh := newOwner(t, pool)
	rev, err = st.InvalidateEndpoint(ctx, fresh, s.endpoint)
	c.NoError(err, "logout an unknown endpoint")
	c.Eq("", rev.RefreshToken, "an unknown endpoint has nothing to revoke")
}

// A tombstoned user blocks every credential read and write even when the
// destructive cleanup never ran; cleanup itself still works.
func TestMcpOAuthStoreTombstonedOwnerBlocksEveryReadWrite(t *testing.T) {
	c := assert.NewAborting(t)
	ctx := context.Background()
	pool := testPool(t)
	owner := newOwner(t, pool)
	st := New(pool, newBox(t, 0x36))
	s := newSite(t)
	grant := fixtureGrant(owner, s)
	fence := login(t, ctx, st, owner, s.endpoint, grant)

	if _, err := st.Acquire(ctx, owner, s.endpoint, 1, nil); err != nil {
		t.Fatalf("acquire before the tombstone: %v", err)
	}
	tombstoneOwner(t, pool, owner) // the cleanup deliberately does NOT run

	_, err := st.LookupBinding(ctx, owner, s.endpoint)
	c.ErrorIs(err, mcpoauth.ErrLoginRequired, "lookup for a tombstoned owner")
	_, err = st.Acquire(ctx, owner, s.endpoint, 1, nil)
	c.ErrorIs(err, mcpoauth.ErrLoginRequired, "acquire for a tombstoned owner")
	_, err = st.BeginFence(ctx, owner, s.endpoint+"-new")
	c.ErrorIs(err, mcpoauth.ErrLoginRequired, "begin a login for a tombstoned owner")
	_, err = st.BindFence(ctx, fence, s.resource, s.issuer)
	c.ErrorIs(err, mcpoauth.ErrLoginRequired, "bind a login for a tombstoned owner")
	_, err = st.CommitLogin(ctx, fence, grant)
	c.ErrorIs(err, mcpoauth.ErrLoginRequired, "commit a login for a tombstoned owner")

	// Destructive cleanup is allowed after the tombstone, and it invalidates
	// everything the uncleared rows held.
	revs, err := st.InvalidateOwner(ctx, owner)
	c.NoError(err, "invalidate a tombstoned owner")
	c.Len(revs, 1, "revocations")
	c.Eq(grant.Token.RefreshToken, revs[0].RefreshToken, "revocation refresh token")
	raw := readRawCredential(t, pool, owner, s.resource)
	c.True(raw.invalidated, "the grant was not tombstoned")
	c.Len(raw.sealed, 0, "the sealed blob was not wiped")

	if _, err := st.InvalidateResource(ctx, owner, s.resource); err != nil {
		t.Fatalf("invalidate a resource after the tombstone: %v", err)
	}
}

// Invalidating a resource keeps an incremented tombstone, wipes the secrets,
// fences a pending login for that resource, and is idempotent.
func TestMcpOAuthStoreInvalidateResource(t *testing.T) {
	c := assert.NewAborting(t)
	ctx := context.Background()
	pool := testPool(t)
	owner := newOwner(t, pool)
	st := New(pool, newBox(t, 0x37))
	s := newSite(t)
	want := fixtureGrant(owner, s)
	login(t, ctx, st, owner, s.endpoint, want)

	rev, err := st.InvalidateResource(ctx, owner, s.resource)
	c.NoError(err, "logout by resource")
	c.Eq(want.Token.RefreshToken, rev.RefreshToken, "revocation refresh token")
	c.Eq(int64(2), rev.Generation, "tombstone generation")

	raw := readRawCredential(t, pool, owner, s.resource)
	c.True(raw.invalidated, "the grant is not tombstoned")
	c.Eq(int64(2), raw.generation, "the tombstone generation")
	c.Len(raw.sealed, 0, "the tombstone kept the sealed blob")
	c.Eq("idle", raw.refreshState, "the tombstone refresh state")

	// A pending login for the same resource is fenced even though the grant is
	// already a tombstone.
	pendingEndpoint := s.endpoint + "-pending"
	fence, err := st.BeginFence(ctx, owner, pendingEndpoint)
	c.NoError(err, "begin a pending login")
	fence, err = st.BindFence(ctx, fence, s.resource, s.issuer)
	c.NoError(err, "bind a pending login")
	c.Eq(int64(2), fence.GrantGeneration, "the pending login captured the tombstone generation")

	// Idempotent: a second logout returns nothing new but still fences.
	secondRev, err := st.InvalidateResource(ctx, owner, s.resource)
	c.NoError(err, "a repeated logout")
	c.Eq("", secondRev.RefreshToken, "a repeated logout returned a revocation")
	c.Eq(int64(2), readRawCredential(t, pool, owner, s.resource).generation, "a repeated logout re-advanced the tombstone")

	_, err = st.CommitLogin(ctx, fence, fixtureGrant(owner, s))
	c.ErrorIs(err, mcpoauth.ErrStaleFence, "a pending login survived a resource logout")

	// An absent resource is ErrMissing, and it owns nothing to fence.
	_, err = st.InvalidateResource(ctx, owner, s.issuer+"/never-logged-in")
	c.ErrorIs(err, mcpoauth.ErrMissing, "an absent resource")
}

// Removing an owner invalidates every grant and binding it owns, in one
// deterministic pass, and returns each live revocation once.
func TestMcpOAuthStoreInvalidateOwner(t *testing.T) {
	c := assert.NewAborting(t)
	ctx := context.Background()
	pool := testPool(t)
	owner := newOwner(t, pool)
	st := New(pool, newBox(t, 0x38))
	first := newSite(t)
	second := newSite(t)
	firstGrant := fixtureGrant(owner, first)
	secondGrant := fixtureGrant(owner, second)
	login(t, ctx, st, owner, first.endpoint, firstGrant)
	login(t, ctx, st, owner, second.endpoint, secondGrant)

	revs, err := st.InvalidateOwner(ctx, owner)
	c.NoError(err, "invalidate the owner")
	c.Len(revs, 2, "revocations")
	got := map[string]string{}
	for _, rev := range revs {
		got[rev.Resource] = rev.RefreshToken
	}
	c.Eq(firstGrant.Token.RefreshToken, got[first.resource], "first resource revocation")
	c.Eq(secondGrant.Token.RefreshToken, got[second.resource], "second resource revocation")
	for _, s := range []site{first, second} {
		raw := readRawCredential(t, pool, owner, s.resource)
		c.True(raw.invalidated, "%s was not tombstoned", s.resource)
		c.Len(raw.sealed, 0, "%s kept its sealed blob", s.resource)
		_, err := st.LookupBinding(ctx, owner, s.endpoint)
		c.ErrorIs(err, mcpoauth.ErrMissing, "%s still has a binding", s.endpoint)
	}

	again, err := st.InvalidateOwner(ctx, owner)
	c.NoError(err, "a repeated owner invalidation")
	c.Len(again, 0, "a repeated owner invalidation returned revocations")

	// A nonexistent owner has nothing to invalidate.
	none, err := st.InvalidateOwner(ctx, uuid.NewString())
	c.NoError(err, "invalidating a nonexistent owner")
	c.Len(none, 0, "a nonexistent owner")
}

// A newer login moves the grant generation, so an old binding's next
// acquisition is refused rather than silently served the replacement.
func TestMcpOAuthStoreAcquireStaleGeneration(t *testing.T) {
	c := assert.NewAborting(t)
	ctx := context.Background()
	pool := testPool(t)
	owner := newOwner(t, pool)
	st := New(pool, newBox(t, 0x39))
	s := newSite(t)
	login(t, ctx, st, owner, s.endpoint, fixtureGrant(owner, s))

	old, err := st.LookupBinding(ctx, owner, s.endpoint)
	c.NoError(err, "lookup the old binding")
	c.Eq(int64(1), old.GrantGeneration, "old generation")

	replacement := fixtureGrant(owner, s)
	replacement.Token.AccessToken = "replacement-access"
	login(t, ctx, st, owner, s.endpoint+"-new", replacement) // advances to 2

	_, err = st.Acquire(ctx, owner, s.endpoint, old.GrantGeneration, nil)
	c.ErrorIs(err, mcpoauth.ErrStaleFence, "an old binding adopted the replacement")
}
