// SPDX-License-Identifier: Apache-2.0

package usersdb

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"go.graveland.dev/rafiki/pkg/users"

	"github.com/multigres/testkit/assert"
)

// setEmail stamps an email directly onto a user row. pkg/users.Store gains a
// SetEmail method in task 2.1, developed concurrently in another worktree;
// until that lands, tests here set the column directly.
func setEmail(t *testing.T, ctx context.Context, pool *pgxpool.Pool, userID, email string) {
	t.Helper()
	_, err := pool.Exec(ctx, `UPDATE conversations.users SET email = $1 WHERE id = $2`, email, userID)
	if err != nil {
		t.Fatalf("set email: %v", err)
	}
}

// identityCount returns how many user_identity rows (active or revoked)
// exist for userID, so tests can assert a conflict or a race left exactly
// the bindings they expect and nothing extra.
func identityCount(t *testing.T, ctx context.Context, pool *pgxpool.Pool, userID string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM conversations.user_identity WHERE user_id = $1`, userID).Scan(&n); err != nil {
		t.Fatalf("count identities: %v", err)
	}
	return n
}

func TestResolveOIDCBindsByEmailOnFirstLogin(t *testing.T) {
	c := assert.NewAborting(t)
	ctx := context.Background()
	s, pool := testStore(t)
	resolver := NewOIDCResolver(pool)

	u, _, err := s.Create(ctx, "brent", false)
	c.NoError(err, "create")
	setEmail(t, ctx, pool, u.ID, "brent@example.com")

	got, err := resolver.ResolveOIDC(ctx, users.OIDCClaims{Issuer: "https://idp.example", Subject: "sub-1", Email: "brent@example.com"})
	c.NoError(err, "resolve")
	c.Eq(u.ID, got.ID, "resolved user id")
	c.Eq(1, identityCount(t, ctx, pool, u.ID), "identity rows after first login")
}

func TestResolveOIDCResolvesBySubjectAfterEmailChange(t *testing.T) {
	c := assert.NewAborting(t)
	ctx := context.Background()
	s, pool := testStore(t)
	resolver := NewOIDCResolver(pool)

	u, _, err := s.Create(ctx, "brent", false)
	c.NoError(err, "create")
	setEmail(t, ctx, pool, u.ID, "brent@example.com")

	_, err = resolver.ResolveOIDC(ctx, users.OIDCClaims{Issuer: "https://idp.example", Subject: "sub-1", Email: "brent@example.com"})
	c.NoError(err, "first login")

	// The IdP now reports a different email for the same subject. The
	// binding must win; the stale claim email must not re-point anything.
	got, err := resolver.ResolveOIDC(ctx, users.OIDCClaims{Issuer: "https://idp.example", Subject: "sub-1", Email: "changed@example.com"})
	c.NoError(err, "second login with changed email")
	c.Eq(u.ID, got.ID, "resolved user id")
	c.Eq(1, identityCount(t, ctx, pool, u.ID), "no new identity row from the email change")
}

func TestResolveOIDCUnknownEmail(t *testing.T) {
	ctx := context.Background()
	_, pool := testStore(t)
	resolver := NewOIDCResolver(pool)

	_, err := resolver.ResolveOIDC(ctx, users.OIDCClaims{Issuer: "https://idp.example", Subject: "sub-1", Email: "nobody@example.com"})
	assert.NewAborting(t).ErrorIs(err, users.ErrOIDCNoUser, "err")
}

func TestResolveOIDCTombstonedBoundUser(t *testing.T) {
	c := assert.NewAborting(t)
	ctx := context.Background()
	s, pool := testStore(t)
	resolver := NewOIDCResolver(pool)

	u, _, err := s.Create(ctx, "brent", false)
	c.NoError(err, "create")
	setEmail(t, ctx, pool, u.ID, "brent@example.com")
	_, err = resolver.ResolveOIDC(ctx, users.OIDCClaims{Issuer: "https://idp.example", Subject: "sub-1", Email: "brent@example.com"})
	c.NoError(err, "first login")

	c.NoError(s.Delete(ctx, "brent"), "delete")

	_, err = resolver.ResolveOIDC(ctx, users.OIDCClaims{Issuer: "https://idp.example", Subject: "sub-1", Email: "brent@example.com"})
	c.ErrorIs(err, users.ErrOIDCNoUser, "err")
}

func TestResolveOIDCTombstonedEmailUser(t *testing.T) {
	c := assert.NewAborting(t)
	ctx := context.Background()
	s, pool := testStore(t)
	resolver := NewOIDCResolver(pool)

	u, _, err := s.Create(ctx, "brent", false)
	c.NoError(err, "create")
	setEmail(t, ctx, pool, u.ID, "brent@example.com")
	c.NoError(s.Delete(ctx, "brent"), "delete")

	// No binding was ever created for this user, so the only way in is the
	// email lookup — which must exclude the tombstone.
	_, err = resolver.ResolveOIDC(ctx, users.OIDCClaims{Issuer: "https://idp.example", Subject: "sub-1", Email: "brent@example.com"})
	c.ErrorIs(err, users.ErrOIDCNoUser, "err")
}

func TestResolveOIDCSameIssuerConflict(t *testing.T) {
	c := assert.NewAborting(t)
	ctx := context.Background()
	s, pool := testStore(t)
	resolver := NewOIDCResolver(pool)

	u, _, err := s.Create(ctx, "brent", false)
	c.NoError(err, "create")
	setEmail(t, ctx, pool, u.ID, "brent@example.com")
	_, err = resolver.ResolveOIDC(ctx, users.OIDCClaims{Issuer: "https://idp.example", Subject: "sub-1", Email: "brent@example.com"})
	c.NoError(err, "first login")

	_, err = resolver.ResolveOIDC(ctx, users.OIDCClaims{Issuer: "https://idp.example", Subject: "sub-2", Email: "brent@example.com"})
	c.ErrorIs(err, users.ErrOIDCConflict, "err")
	c.Eq(1, identityCount(t, ctx, pool, u.ID), "no second binding row from the conflicting login")
}

func TestResolveOIDCOtherIssuerIgnored(t *testing.T) {
	c := assert.NewAborting(t)
	ctx := context.Background()
	s, pool := testStore(t)
	resolver := NewOIDCResolver(pool)

	u, _, err := s.Create(ctx, "brent", false)
	c.NoError(err, "create")
	setEmail(t, ctx, pool, u.ID, "brent@example.com")
	_, err = resolver.ResolveOIDC(ctx, users.OIDCClaims{Issuer: "https://idp-a.example", Subject: "sub-1", Email: "brent@example.com"})
	c.NoError(err, "first login under issuer A")

	got, err := resolver.ResolveOIDC(ctx, users.OIDCClaims{Issuer: "https://idp-b.example", Subject: "sub-2", Email: "brent@example.com"})
	c.NoError(err, "login under issuer B binds fresh")
	c.Eq(u.ID, got.ID, "resolved user id")
	c.Eq(2, identityCount(t, ctx, pool, u.ID), "identity rows after binding under a second issuer")
}

func TestResolveOIDCConcurrentFirstLogin(t *testing.T) {
	c := assert.NewAborting(t)
	ctx := context.Background()
	s, pool := testStore(t)
	resolver := NewOIDCResolver(pool)

	u, _, err := s.Create(ctx, "brent", false)
	c.NoError(err, "create")
	setEmail(t, ctx, pool, u.ID, "brent@example.com")

	claims := users.OIDCClaims{Issuer: "https://idp.example", Subject: "sub-1", Email: "brent@example.com"}
	results := make([]users.User, 2)
	errs := make([]error, 2)
	var wg sync.WaitGroup
	for i := range 2 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = resolver.ResolveOIDC(ctx, claims)
		}(i)
	}
	wg.Wait()

	cc := assert.NewCollecting(t)
	for i, err := range errs {
		cc.NoError(err, "goroutine %d", i)
	}
	cc.Eq(results[0].ID, results[1].ID, "both goroutines must resolve to the same user")
	cc.Eq(u.ID, results[0].ID, "resolved user id")
	cc.Check()

	c.Eq(1, identityCount(t, ctx, pool, u.ID), "exactly one binding row after concurrent first login")
}

func TestResolveOIDCEmptyClaims(t *testing.T) {
	ctx := context.Background()
	_, pool := testStore(t)
	resolver := NewOIDCResolver(pool)
	c := assert.NewAborting(t)

	for _, claims := range []users.OIDCClaims{
		{Issuer: "", Subject: "sub-1", Email: "brent@example.com"},
		{Issuer: "https://idp.example", Subject: "", Email: "brent@example.com"},
		{Issuer: "https://idp.example", Subject: "sub-1", Email: ""},
	} {
		_, err := resolver.ResolveOIDC(ctx, claims)
		c.Error(err, "claims %+v", claims)
		c.False(errors.Is(err, users.ErrOIDCNoUser), "empty claims must not surface as an answer sentinel: %v", err)
		c.False(errors.Is(err, users.ErrOIDCConflict), "empty claims must not surface as an answer sentinel: %v", err)
	}
}
