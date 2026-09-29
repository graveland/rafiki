// SPDX-License-Identifier: Apache-2.0

package usersdb

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"go.graveland.dev/rafiki/pkg/users"
)

// oidcMaxAttempts bounds the "insert lost the race" retry to one extra try:
// the retry exists to let a concurrent first login for the same (issuer,
// subject) resolve to the row the other transaction just committed, not to
// paper over a persistently failing database.
const oidcMaxAttempts = 2

// NewOIDCResolver creates a users.OIDCResolver backed by pool.
func NewOIDCResolver(pool *pgxpool.Pool) users.OIDCResolver { return &oidcResolver{pool: pool} }

type oidcResolver struct {
	pool *pgxpool.Pool
}

// ResolveOIDC maps a verified login to a rafiki user. See the package-level
// algorithm notes on oidcResolver.resolveOnce for the per-attempt steps; this
// wrapper only owns the empty-claims guard and the one-shot retry after a
// concurrent first login.
func (s *oidcResolver) ResolveOIDC(ctx context.Context, c users.OIDCClaims) (users.User, error) {
	if c.Issuer == "" || c.Subject == "" || c.Email == "" {
		return users.User{}, fmt.Errorf("resolve oidc: issuer, subject and email must all be non-empty")
	}

	var lastErr error
	for attempt := 0; attempt < oidcMaxAttempts; attempt++ {
		u, retry, err := s.resolveOnce(ctx, c)
		if retry {
			lastErr = err
			continue
		}
		return u, err
	}
	return users.User{}, fmt.Errorf("resolve oidc: exhausted retry after concurrent first login: %w", lastErr)
}

// resolveOnce runs one attempt of the algorithm in a single transaction.
// retry is true only when a concurrent first login won the race to insert
// the same (issuer, subject) binding; the caller starts over in a fresh
// transaction, which will now find that binding on the lookup below.
func (s *oidcResolver) resolveOnce(ctx context.Context, c users.OIDCClaims) (u users.User, retry bool, err error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return users.User{}, false, fmt.Errorf("resolve oidc: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Step 1: an existing active binding for this exact (issuer, subject)
	// wins outright. The claim's email is deliberately never consulted on
	// this path: an email change at the IdP must not re-point the binding.
	var userID string
	err = tx.QueryRow(ctx,
		`SELECT user_id::text FROM conversations.user_identity
		  WHERE issuer = $1 AND subject = $2 AND revoked_at IS NULL`,
		c.Issuer, c.Subject).Scan(&userID)
	switch {
	case err == nil:
		u, deleted, err := s.loadUser(ctx, tx, userID)
		if err != nil {
			return users.User{}, false, fmt.Errorf("resolve oidc: load bound user: %w", err)
		}
		if deleted {
			return users.User{}, false, fmt.Errorf("%w: user was removed", users.ErrOIDCNoUser)
		}
		if err := tx.Commit(ctx); err != nil {
			return users.User{}, false, fmt.Errorf("resolve oidc: commit: %w", err)
		}
		return u, false, nil
	case errors.Is(err, pgx.ErrNoRows):
		// Fall through to email lookup.
	default:
		return users.User{}, false, fmt.Errorf("resolve oidc: lookup binding: %w", err)
	}

	// Step 2: no binding yet. Find the active user this email belongs to,
	// locking the row: two concurrent first logins for the same email but
	// different subjects must serialize here, or both can pass the step-3
	// conflict check before either has inserted its binding. FOR NO KEY
	// UPDATE rather than FOR UPDATE: it still self-conflicts (so two
	// Resolves serialize) and still conflicts with Delete's own FOR NO KEY
	// UPDATE, but it doesn't block an unrelated FK insert that merely
	// references this user (a new conversation, turn, or child).
	err = tx.QueryRow(ctx,
		`SELECT id::text, username, is_admin, created_at, COALESCE(email,'') FROM conversations.users
		  WHERE email = $1 AND deleted_at IS NULL
		  FOR NO KEY UPDATE`,
		c.Email).Scan(&u.ID, &u.Username, &u.IsAdmin, &u.CreatedAt, &u.Email)
	if errors.Is(err, pgx.ErrNoRows) {
		return users.User{}, false, users.ErrOIDCNoUser
	}
	if err != nil {
		return users.User{}, false, fmt.Errorf("resolve oidc: lookup by email: %w", err)
	}

	// Step 3: that user already has an active binding under this SAME
	// issuer, but a different subject — the email now points somewhere the
	// issuer disagrees with. Bindings under other issuers are irrelevant.
	var conflictSubject string
	err = tx.QueryRow(ctx,
		`SELECT subject FROM conversations.user_identity
		  WHERE user_id = $1 AND issuer = $2 AND revoked_at IS NULL AND subject <> $3`,
		u.ID, c.Issuer, c.Subject).Scan(&conflictSubject)
	switch {
	case err == nil:
		slog.Warn("usersdb: oidc identity conflict",
			"issuer", c.Issuer, "existing_subject", conflictSubject, "new_subject", c.Subject, "user_id", u.ID)
		return users.User{}, false, users.ErrOIDCConflict
	case errors.Is(err, pgx.ErrNoRows):
		// No conflicting binding; proceed to bind.
	default:
		return users.User{}, false, fmt.Errorf("resolve oidc: lookup conflicting binding: %w", err)
	}

	// Step 4: first login for this (issuer, subject) — bind it.
	_, err = tx.Exec(ctx,
		`INSERT INTO conversations.user_identity (user_id, issuer, subject, email)
		 VALUES ($1,$2,$3,$4)`,
		u.ID, c.Issuer, c.Subject, c.Email)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == uniqueViolation && pgErr.ConstraintName == "user_identity_active" {
			// A concurrent first login for the same (issuer, subject) won
			// the race. Not an error: the caller retries in a fresh
			// transaction, where step 1 above will find that binding.
			return users.User{}, true, fmt.Errorf("concurrent first login for issuer %q", c.Issuer)
		}
		return users.User{}, false, fmt.Errorf("resolve oidc: insert binding: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return users.User{}, false, fmt.Errorf("resolve oidc: commit: %w", err)
	}
	slog.Info("usersdb: oidc binding created", "user_id", u.ID, "issuer", c.Issuer, "subject", c.Subject)
	return u, false, nil
}

// loadUser reads a user by id within tx, reporting separately whether it is
// tombstoned.
func (s *oidcResolver) loadUser(ctx context.Context, tx pgx.Tx, id string) (users.User, bool, error) {
	var u users.User
	var deletedAt *time.Time
	err := tx.QueryRow(ctx,
		`SELECT id::text, username, is_admin, created_at, deleted_at, COALESCE(email,'') FROM conversations.users
		  WHERE id = $1`, id).Scan(&u.ID, &u.Username, &u.IsAdmin, &u.CreatedAt, &deletedAt, &u.Email)
	if err != nil {
		return users.User{}, false, err
	}
	return u, deletedAt != nil, nil
}
