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

// setEmail stores email on username via the store's own SetEmail.
func setEmail(t *testing.T, ctx context.Context, s users.Store, username, email string) {
	t.Helper()
	if _, err := s.SetEmail(ctx, username, email); err != nil {
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

	u, _, err := s.Create(ctx, users.NewUser{Username: "brent"})
	c.NoError(err, "create")
	setEmail(t, ctx, s, "brent", "brent@example.com")

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

	u, _, err := s.Create(ctx, users.NewUser{Username: "brent"})
	c.NoError(err, "create")
	setEmail(t, ctx, s, "brent", "brent@example.com")

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

	_, _, err := s.Create(ctx, users.NewUser{Username: "brent"})
	c.NoError(err, "create")
	setEmail(t, ctx, s, "brent", "brent@example.com")
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

	_, _, err := s.Create(ctx, users.NewUser{Username: "brent"})
	c.NoError(err, "create")
	setEmail(t, ctx, s, "brent", "brent@example.com")
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

	u, _, err := s.Create(ctx, users.NewUser{Username: "brent"})
	c.NoError(err, "create")
	setEmail(t, ctx, s, "brent", "brent@example.com")
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

	u, _, err := s.Create(ctx, users.NewUser{Username: "brent"})
	c.NoError(err, "create")
	setEmail(t, ctx, s, "brent", "brent@example.com")
	_, err = resolver.ResolveOIDC(ctx, users.OIDCClaims{Issuer: "https://idp-a.example", Subject: "sub-1", Email: "brent@example.com"})
	c.NoError(err, "first login under issuer A")

	got, err := resolver.ResolveOIDC(ctx, users.OIDCClaims{Issuer: "https://idp-b.example", Subject: "sub-2", Email: "brent@example.com"})
	c.NoError(err, "login under issuer B binds fresh")
	c.Eq(u.ID, got.ID, "resolved user id")
	c.Eq(2, identityCount(t, ctx, pool, u.ID), "identity rows after binding under a second issuer")
}

// Two concurrent first logins for the SAME (issuer, subject) must resolve to
// one binding. Since the step-2 email lookup now takes FOR UPDATE, the two
// attempts serialize there and the second one's step 1 finds the row the
// first committed — the 23505-on-insert retry branch is defense-in-depth for
// a race this lock already closes, not the primary mechanism.
func TestResolveOIDCConcurrentFirstLogin(t *testing.T) {
	c := assert.NewAborting(t)
	ctx := context.Background()
	s, pool := testStore(t)
	resolver := NewOIDCResolver(pool)

	u, _, err := s.Create(ctx, users.NewUser{Username: "brent"})
	c.NoError(err, "create")
	setEmail(t, ctx, s, "brent", "brent@example.com")

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

// W2-M1 regression: two concurrent FIRST logins for the same email, same
// issuer, but DIFFERENT subjects must not both bind. Without the FOR UPDATE
// lock in step 2, both goroutines can pass the step-3 conflict check before
// either has inserted, leaving the user with two active bindings under one
// issuer — exactly the takeover signal the conflict rule exists to catch.
func TestResolveOIDCSameEmailDifferentSubjects(t *testing.T) {
	c := assert.NewAborting(t)
	ctx := context.Background()
	s, pool := testStore(t)
	resolver := NewOIDCResolver(pool)

	u, _, err := s.Create(ctx, users.NewUser{Username: "brent"})
	c.NoError(err, "create")
	setEmail(t, ctx, s, "brent", "brent@example.com")

	claimsA := users.OIDCClaims{Issuer: "https://idp.example", Subject: "sub-1", Email: "brent@example.com"}
	claimsB := users.OIDCClaims{Issuer: "https://idp.example", Subject: "sub-2", Email: "brent@example.com"}
	results := make([]users.User, 2)
	errs := make([]error, 2)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); results[0], errs[0] = resolver.ResolveOIDC(ctx, claimsA) }()
	go func() { defer wg.Done(); results[1], errs[1] = resolver.ResolveOIDC(ctx, claimsB) }()
	wg.Wait()

	successes, conflicts := 0, 0
	for _, err := range errs {
		switch {
		case err == nil:
			successes++
		case errors.Is(err, users.ErrOIDCConflict):
			conflicts++
		default:
			t.Fatalf("unexpected error: %v", err)
		}
	}
	c.Eq(1, successes, "exactly one goroutine must bind")
	c.Eq(1, conflicts, "exactly one goroutine must be refused as a conflict")
	c.Eq(1, identityCount(t, ctx, pool, u.ID), "exactly one binding row after the race")

	for i, err := range errs {
		if err == nil {
			c.Eq(u.ID, results[i].ID, "the winning goroutine resolves to the original user")
		}
	}
}

// W2-M2 regression: a binding to a tombstoned user must not block that
// (issuer, subject) forever. Delete revokes the tombstoned user's active
// user_identity rows, so a recreated user with the same email can rebind
// fresh — even reusing the same subject the old user was bound to.
func TestResolveOIDCRebindsAfterRecreateWithSameEmail(t *testing.T) {
	c := assert.NewAborting(t)
	ctx := context.Background()
	s, pool := testStore(t)
	resolver := NewOIDCResolver(pool)

	old, _, err := s.Create(ctx, users.NewUser{Username: "brent"})
	c.NoError(err, "create")
	setEmail(t, ctx, s, "brent", "brent@example.com")

	claims := users.OIDCClaims{Issuer: "https://idp.example", Subject: "sub-1", Email: "brent@example.com"}
	bound, err := resolver.ResolveOIDC(ctx, claims)
	c.NoError(err, "first login")
	c.Eq(old.ID, bound.ID, "bound to the original user")

	c.NoError(s.Delete(ctx, "brent"), "delete")

	fresh, _, err := s.Create(ctx, users.NewUser{Username: "brent", Email: "brent@example.com"})
	c.NoError(err, "recreate with the same email")
	c.NotEq(old.ID, fresh.ID, "recreated user must have a new id")

	got, err := resolver.ResolveOIDC(ctx, claims)
	c.NoError(err, "login after recreate")
	c.Eq(fresh.ID, got.ID, "resolved to the recreated user, not the tombstoned one")
	c.Eq(1, identityCount(t, ctx, pool, fresh.ID), "fresh binding for the recreated user")
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
