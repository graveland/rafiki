// SPDX-License-Identifier: Apache-2.0

// Package usersdb is the Postgres implementation of users.Store. It is the
// only package that may touch pgx on the identity path — see pkg/users.
package usersdb

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"go.graveland.dev/rafiki/pkg/users"
)

// NewPostgresStore creates a users.Store backed by pool.
func NewPostgresStore(pool *pgxpool.Pool) users.Store { return &pgStore{pool: pool} }

type pgStore struct {
	pool *pgxpool.Pool
}

// uniqueViolation is Postgres SQLSTATE 23505.
const uniqueViolation = "23505"

// taken maps the unique violations a users write can hit onto their
// sentinels. The indexes are partial (active rows only), so each is also
// what makes a tombstoned name or address reusable rather than a special
// case here. Only USERNAME and EMAIL mean "taken" — token_sha256 is UNIQUE
// too, and while a digest collision is unreachable (2^-256), reporting one
// as "already taken" would be maximally confusing for the one person who
// ever saw it. nil means the error is not a refusal and must stay wrapped.
func taken(err error) error {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != uniqueViolation {
		return nil
	}
	switch pgErr.ConstraintName {
	case "users_username_active":
		return users.ErrUsernameTaken
	case "users_email_active":
		return users.ErrEmailTaken
	default:
		return nil
	}
}

func (s *pgStore) Create(ctx context.Context, u users.NewUser) (users.User, string, error) {
	username, err := users.NormalizeUsername(u.Username)
	if err != nil {
		return users.User{}, "", err
	}
	email, err := users.NormalizeEmail(u.Email)
	if err != nil {
		return users.User{}, "", err
	}
	var token string
	if u.MintToken {
		if token, err = users.NewBearerToken(); err != nil {
			return users.User{}, "", fmt.Errorf("mint token: %w", err)
		}
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return users.User{}, "", fmt.Errorf("begin create user: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op once Commit has run

	var created users.User
	// token_sha256 stays NULL: credentials live in user_token since 0044.
	err = tx.QueryRow(ctx,
		`INSERT INTO conversations.users (username, email, is_admin)
		 VALUES ($1, NULLIF($2,''), $3)
		 RETURNING id::text, username, is_admin, created_at, COALESCE(email,'')`,
		username, email, u.IsAdmin).Scan(&created.ID, &created.Username, &created.IsAdmin, &created.CreatedAt, &created.Email)
	if err != nil {
		if refuse := taken(err); refuse != nil {
			return users.User{}, "", refuse
		}
		return users.User{}, "", fmt.Errorf("insert user: %w", err)
	}
	if token != "" {
		// Same transaction as the users row: a create that mints its
		// credential is atomic — never a user without its initial token.
		if _, err := tx.Exec(ctx,
			`INSERT INTO conversations.user_token (user_id, token_sha256, name, origin)
			 VALUES ($1, $2, 'initial', $3)`,
			created.ID, users.HashToken(token), users.OriginService); err != nil {
			return users.User{}, "", fmt.Errorf("insert initial token: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return users.User{}, "", fmt.Errorf("commit create user: %w", err)
	}
	return created, token, nil
}

func (s *pgStore) Authenticate(ctx context.Context, token string) (users.Identity, error) {
	var id users.Identity
	err := s.pool.QueryRow(ctx,
		`SELECT u.id::text, u.username, u.is_admin, t.id::text
		   FROM conversations.user_token t
		   JOIN conversations.users u ON u.id = t.user_id
		  WHERE t.token_sha256 = $1
		    AND t.revoked_at IS NULL
		    AND (t.expires_at IS NULL OR t.expires_at > now())
		    AND u.deleted_at IS NULL`,
		users.HashToken(token)).Scan(&id.UserID, &id.Username, &id.IsAdmin, &id.TokenID)
	if errors.Is(err, pgx.ErrNoRows) {
		return users.Identity{}, users.ErrNotFound
	}
	if err != nil {
		// NOT ErrNotFound. "I could not check" is not "this is invalid";
		// the caller turns this into 503, never 401.
		return users.Identity{}, fmt.Errorf("authenticate: %w", err)
	}
	return id, nil
}

func (s *pgStore) List(ctx context.Context, includeDeleted bool, limit int) ([]users.User, error) {
	if limit <= 0 {
		limit = 100
	}
	q := `SELECT id::text, username, is_admin, created_at, deleted_at, COALESCE(email,'')
	        FROM conversations.users`
	if !includeDeleted {
		q += ` WHERE deleted_at IS NULL`
	}
	// created_at alone is not a total order: it defaults to now(), which is
	// TRANSACTION time, so rows created in one transaction tie and the page
	// order becomes arbitrary. id breaks the tie.
	q += ` ORDER BY created_at, id LIMIT $1`

	rows, err := s.pool.Query(ctx, q, limit)
	if err != nil {
		return nil, fmt.Errorf("list users: %w", err)
	}
	defer rows.Close()
	var out []users.User
	for rows.Next() {
		var u users.User
		var deletedAt *time.Time
		if err := rows.Scan(&u.ID, &u.Username, &u.IsAdmin, &u.CreatedAt, &deletedAt, &u.Email); err != nil {
			return nil, fmt.Errorf("scan user: %w", err)
		}
		u.DeletedAt = deletedAt
		out = append(out, u)
	}
	return out, rows.Err()
}

func (s *pgStore) Delete(ctx context.Context, username string) error {
	// A tombstone, never a DELETE: hard-deleting would cascade an UPDATE
	// across every historical turn's author_user_id, inside compressed
	// hypertable chunks. See the design doc, Decision 6.
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin delete user: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var userID string
	err = tx.QueryRow(ctx,
		`UPDATE conversations.users SET deleted_at = now()
		  WHERE username = $1 AND deleted_at IS NULL
		  RETURNING id::text`, username).Scan(&userID)
	if errors.Is(err, pgx.ErrNoRows) {
		return users.ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("delete user: %w", err)
	}

	// Nothing else revokes a user_identity row. Without this, a recreated
	// user with the same email could never rebind: ResolveOIDC's step 1
	// would keep finding the tombstoned owner's stale active binding.
	if _, err := tx.Exec(ctx,
		`UPDATE conversations.user_identity SET revoked_at = now()
		  WHERE user_id = $1 AND revoked_at IS NULL`, userID); err != nil {
		return fmt.Errorf("revoke user identities: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit delete user: %w", err)
	}
	return nil
}

func (s *pgStore) CountActive(ctx context.Context) (int, error) {
	var n int
	if err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM conversations.users WHERE deleted_at IS NULL`).Scan(&n); err != nil {
		return 0, fmt.Errorf("count active users: %w", err)
	}
	return n, nil
}

// SetEmail stores a normalized address on the named ACTIVE user; "" clears
// it. ErrNotFound and ErrEmailTaken are answers; anything else is an outage.
func (s *pgStore) SetEmail(ctx context.Context, username, email string) (users.User, error) {
	username, err := users.NormalizeUsername(username)
	if err != nil {
		return users.User{}, err
	}
	email, err = users.NormalizeEmail(email)
	if err != nil {
		return users.User{}, err
	}
	var u users.User
	var deletedAt *time.Time
	err = s.pool.QueryRow(ctx,
		`UPDATE conversations.users SET email = NULLIF($2,'')
		  WHERE username = $1 AND deleted_at IS NULL
		  RETURNING id::text, username, is_admin, created_at, deleted_at, COALESCE(email,'')`,
		username, email).Scan(&u.ID, &u.Username, &u.IsAdmin, &u.CreatedAt, &deletedAt, &u.Email)
	if errors.Is(err, pgx.ErrNoRows) {
		return users.User{}, users.ErrNotFound
	}
	if err != nil {
		if refuse := taken(err); refuse != nil {
			return users.User{}, refuse
		}
		return users.User{}, fmt.Errorf("set email for %q: %w", username, err)
	}
	u.DeletedAt = deletedAt
	return u, nil
}

// MintToken adds a credential to an existing user. The INSERT..SELECT only
// yields rows for an ACTIVE user, so an unknown or tombstoned userID inserts
// nothing and surfaces as ErrNoRows — the answer. The CTE carries the
// username out with the token row, which plain RETURNING cannot do (it sees
// the target table only).
func (s *pgStore) MintToken(ctx context.Context, userID string, t users.NewToken) (users.Token, string, error) {
	if t.Name == "" {
		return users.Token{}, "", fmt.Errorf("users: token name must not be empty")
	}
	if t.Origin != users.OriginService && t.Origin != users.OriginOIDC {
		return users.Token{}, "", fmt.Errorf("users: unknown token origin %q", t.Origin)
	}
	if t.TTL < 0 {
		return users.Token{}, "", fmt.Errorf("users: token TTL must not be negative")
	}
	if _, err := uuid.Parse(userID); err != nil {
		return users.Token{}, "", users.ErrNotFound
	}
	token, err := users.NewBearerToken()
	if err != nil {
		return users.Token{}, "", fmt.Errorf("mint token: %w", err)
	}
	var tok users.Token
	var expiresAt *time.Time
	err = s.pool.QueryRow(ctx, `
		WITH target AS (
			SELECT id, username FROM conversations.users
			 WHERE id = $1::uuid AND deleted_at IS NULL
		), inserted AS (
			INSERT INTO conversations.user_token
			       (user_id, token_sha256, name, origin, expires_at)
			SELECT target.id, $2, $3, $4,
			       CASE WHEN $5::bigint > 0
			            THEN now() + ($5::bigint * interval '1 microsecond')
			            ELSE NULL END
			  FROM target
			RETURNING id::text, user_id::text, name, origin, created_at, expires_at
		)
		SELECT inserted.id, inserted.user_id, target.username, inserted.name,
		       inserted.origin, inserted.created_at, inserted.expires_at
		  FROM inserted CROSS JOIN target`,
		userID, users.HashToken(token), t.Name, string(t.Origin),
		t.TTL.Microseconds()).Scan(
		&tok.ID, &tok.UserID, &tok.Username, &tok.Name, &tok.Origin,
		&tok.CreatedAt, &expiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return users.Token{}, "", users.ErrNotFound
	}
	if err != nil {
		return users.Token{}, "", fmt.Errorf("mint token for user %s: %w", userID, err)
	}
	tok.ExpiresAt = expiresAt
	return tok, token, nil
}

// tokenColumns is the projection every token query shares; ListTokens and
// GetToken join users for Username, since a Token names its owner.
const tokenColumns = `t.id::text, t.user_id::text, u.username, t.name, t.origin,
	t.created_at, t.expires_at, t.revoked_at`

func scanToken(row pgx.Row) (users.Token, error) {
	var t users.Token
	var expiresAt, revokedAt *time.Time
	if err := row.Scan(&t.ID, &t.UserID, &t.Username, &t.Name, &t.Origin,
		&t.CreatedAt, &expiresAt, &revokedAt); err != nil {
		return users.Token{}, err
	}
	t.ExpiresAt = expiresAt
	t.RevokedAt = revokedAt
	return t, nil
}

func (s *pgStore) ListTokens(ctx context.Context, userID string, includeRevoked bool) ([]users.Token, error) {
	// A malformed userID names nobody: an empty list, not a 22P02 outage.
	if userID != "" {
		if _, err := uuid.Parse(userID); err != nil {
			return nil, nil
		}
	}
	// The parameter cast (not a column cast) keeps user_token_user_idx
	// usable for the per-user listing. The default view (includeRevoked
	// false) shows only LIVE credentials: an unrevoked token of a tombstoned
	// user is dead with its owner (Delete revokes identities but never
	// touches token rows), so it lists only in the audit view.
	q := `SELECT ` + tokenColumns + `
	        FROM conversations.user_token t
	        JOIN conversations.users u ON u.id = t.user_id
	       WHERE ($1 = '' OR t.user_id = $1::uuid)
	         AND ($2::bool OR (t.revoked_at IS NULL AND u.deleted_at IS NULL))
	       ORDER BY t.created_at DESC, t.id DESC`
	rows, err := s.pool.Query(ctx, q, userID, includeRevoked)
	if err != nil {
		return nil, fmt.Errorf("list tokens: %w", err)
	}
	defer rows.Close()
	var out []users.Token
	for rows.Next() {
		t, err := scanToken(rows)
		if err != nil {
			return nil, fmt.Errorf("scan token: %w", err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *pgStore) GetToken(ctx context.Context, id string) (users.Token, error) {
	// A malformed id is no token: an answer, not a 22P02 outage.
	if _, err := uuid.Parse(id); err != nil {
		return users.Token{}, users.ErrNotFound
	}
	t, err := scanToken(s.pool.QueryRow(ctx, `SELECT `+tokenColumns+`
	        FROM conversations.user_token t
	        JOIN conversations.users u ON u.id = t.user_id
	       WHERE t.id = $1::uuid`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return users.Token{}, users.ErrNotFound
	}
	if err != nil {
		return users.Token{}, fmt.Errorf("get token %s: %w", id, err)
	}
	return t, nil
}

// RevokeToken tombstones one credential. The row is never hard-deleted, so a
// second revoke changes nothing and falls through to GetToken, which returns
// the existing tombstone — revocation is idempotent. An unknown id is
// ErrNotFound via the same GetToken miss.
func (s *pgStore) RevokeToken(ctx context.Context, id string) (users.Token, error) {
	if _, err := uuid.Parse(id); err != nil {
		return users.Token{}, users.ErrNotFound
	}
	if _, err := s.pool.Exec(ctx,
		`UPDATE conversations.user_token SET revoked_at = now()
		  WHERE id = $1::uuid AND revoked_at IS NULL`, id); err != nil {
		return users.Token{}, fmt.Errorf("revoke token %s: %w", id, err)
	}
	return s.GetToken(ctx, id)
}

// LookupUsername resolves an active username to its id. The WHERE clause
// covers active rows only — see users.Store.LookupUsername's doc comment for
// why a tombstone must never satisfy this lookup. pgx.ErrNoRows is the
// answer, everything else is the store failing.
func (s *pgStore) LookupUsername(ctx context.Context, username string) (string, error) {
	username, err := users.NormalizeUsername(username)
	if err != nil {
		return "", err
	}
	var id string
	err = s.pool.QueryRow(ctx,
		`SELECT id::text FROM conversations.users
		  WHERE username = $1 AND deleted_at IS NULL`, username).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", users.ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("lookup user %q: %w", username, err)
	}
	return id, nil
}
