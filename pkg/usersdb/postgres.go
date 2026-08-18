// SPDX-License-Identifier: Apache-2.0

// Package usersdb is the Postgres implementation of users.Store. It is the
// only package that may touch pgx on the identity path — see pkg/users.
package usersdb

import (
	"context"
	"errors"
	"fmt"
	"time"

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

func (s *pgStore) Create(ctx context.Context, username string) (users.User, string, error) {
	if username == "" {
		return users.User{}, "", errors.New("users: username must not be empty")
	}
	token, err := users.NewToken()
	if err != nil {
		return users.User{}, "", fmt.Errorf("mint token: %w", err)
	}
	var u users.User
	err = s.pool.QueryRow(ctx,
		`INSERT INTO conversations.users (username, token_sha256)
		 VALUES ($1,$2) RETURNING id::text, username, created_at`,
		username, users.HashToken(token)).Scan(&u.ID, &u.Username, &u.CreatedAt)
	if err != nil {
		var pgErr *pgconn.PgError
		// The partial unique index is the ONLY thing enforcing name
		// uniqueness, and it covers active rows only — so this is also what
		// makes a tombstoned name reusable rather than a special case here.
		if errors.As(err, &pgErr) && pgErr.Code == uniqueViolation {
			return users.User{}, "", users.ErrUsernameTaken
		}
		return users.User{}, "", fmt.Errorf("insert user: %w", err)
	}
	return u, token, nil
}

func (s *pgStore) Authenticate(ctx context.Context, token string) (users.Identity, error) {
	var id users.Identity
	err := s.pool.QueryRow(ctx,
		`SELECT id::text, username FROM conversations.users
		  WHERE token_sha256 = $1 AND deleted_at IS NULL`,
		users.HashToken(token)).Scan(&id.UserID, &id.Username)
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
	q := `SELECT id::text, username, created_at, deleted_at
	        FROM conversations.users`
	if !includeDeleted {
		q += ` WHERE deleted_at IS NULL`
	}
	q += ` ORDER BY created_at LIMIT $1`

	rows, err := s.pool.Query(ctx, q, limit)
	if err != nil {
		return nil, fmt.Errorf("list users: %w", err)
	}
	defer rows.Close()
	var out []users.User
	for rows.Next() {
		var u users.User
		var deletedAt *time.Time
		if err := rows.Scan(&u.ID, &u.Username, &u.CreatedAt, &deletedAt); err != nil {
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
	tag, err := s.pool.Exec(ctx,
		`UPDATE conversations.users SET deleted_at = now()
		  WHERE username = $1 AND deleted_at IS NULL`, username)
	if err != nil {
		return fmt.Errorf("delete user: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return users.ErrNotFound
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
