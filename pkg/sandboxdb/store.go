// SPDX-License-Identifier: Apache-2.0

// Package sandboxdb is the daemon-only Postgres implementation of
// sandbox.Store. It owns a pgx pool and lives here — never in pkg/sandbox —
// because the CLI names sandbox.Store without linking postgres.
package sandboxdb

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/sandbox"
)

// uniqueViolation is SQLSTATE 23505; checkViolation is 23514.
const (
	uniqueViolation = "23505"
	checkViolation  = "23514"
)

// ownerNameIndex is the partial unique index over (owner_user_id, name) added
// by migration 0047. Insert matches a 23505 against it BY NAME, so another
// unique index on this table later cannot inherit the ErrNameTaken advice.
const ownerNameIndex = "sandbox_owner_name_unique"

// NewPostgresStore creates a sandbox Store backed by pg.
func NewPostgresStore(pool *pgxpool.Pool) sandbox.Store {
	return &pgStore{pool: pool}
}

type pgStore struct {
	pool *pgxpool.Pool
}

// columns is the projection every read shares, in scanRow order.
const columns = `id, owner_user_id, name, executor_id, launcher_executor_id,
	container_id, image, spec, created_by, owner_child, scope, state,
	created_at, expires_at, removed_at`

type rowScanner interface{ Scan(dest ...any) error }

// scanRow decodes one row in columns order. `scope` is text on the wire and a
// protocol.SandboxScope in Go; the two nullable timestamps scan into
// *time.Time so NULL is distinguishable from the zero time.
func scanRow(rs rowScanner) (sandbox.Row, error) {
	var r sandbox.Row
	var scope string
	var expiresAt, removedAt *time.Time
	if err := rs.Scan(
		&r.ID, &r.OwnerUserID, &r.Name, &r.ExecutorID, &r.LauncherExecutorID,
		&r.ContainerID, &r.Image, &r.Spec, &r.CreatedBy, &r.OwnerChild, &scope, &r.State,
		&r.CreatedAt, &expiresAt, &removedAt); err != nil {
		return sandbox.Row{}, err
	}
	r.Scope = protocol.SandboxScope(scope)
	r.ExpiresAt, r.RemovedAt = expiresAt, removedAt
	r.CreatedAt = r.CreatedAt.UTC()
	return r, nil
}

// nameTaken translates a rejected INSERT into conversations.sandbox when — and
// only when — the (owner, name) live unique index is what rejected it. Matched
// by constraint NAME as well as SQLSTATE, and returned bare (never wrapped in
// the pgconn error, which carries the DSN) so the CLI can compare errors.Is.
func nameTaken(err error) error {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return nil
	}
	if pgErr.Code != uniqueViolation || pgErr.ConstraintName != ownerNameIndex {
		return nil
	}
	return sandbox.ErrNameTaken
}

func (s *pgStore) Insert(ctx context.Context, r sandbox.Row) error {
	_, err := s.pool.Exec(ctx, `
		INSERT INTO conversations.sandbox
		   (id, owner_user_id, name, executor_id, launcher_executor_id,
		    container_id, image, spec, created_by, owner_child, scope, state,
		    expires_at, removed_at)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8::jsonb,$9,$10,$11,$12,$13,$14)`,
		r.ID, r.OwnerUserID, r.Name, r.ExecutorID, r.LauncherExecutorID,
		r.ContainerID, r.Image, string(r.Spec), r.CreatedBy, r.OwnerChild,
		string(r.Scope), r.State, r.ExpiresAt, r.RemovedAt)
	if err != nil {
		if dup := nameTaken(err); dup != nil {
			return dup
		}
		return fmt.Errorf("insert sandbox: %w", err)
	}
	return nil
}

func (s *pgStore) Get(ctx context.Context, id string) (sandbox.Row, bool, error) {
	row, err := scanRow(s.pool.QueryRow(ctx,
		`SELECT `+columns+` FROM conversations.sandbox WHERE id=$1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return sandbox.Row{}, false, nil
	}
	if err != nil {
		return sandbox.Row{}, false, fmt.Errorf("get sandbox: %w", err)
	}
	return row, true, nil
}

func (s *pgStore) GetByName(ctx context.Context, ownerUserID, name string) (sandbox.Row, bool, error) {
	// name <> '' so a spawn block (name '') can never be resolved by name.
	row, err := scanRow(s.pool.QueryRow(ctx,
		`SELECT `+columns+` FROM conversations.sandbox
		  WHERE owner_user_id=$1 AND name=$2 AND name <> '' AND removed_at IS NULL`,
		ownerUserID, name))
	if errors.Is(err, pgx.ErrNoRows) {
		return sandbox.Row{}, false, nil
	}
	if err != nil {
		return sandbox.Row{}, false, fmt.Errorf("get sandbox by name: %w", err)
	}
	return row, true, nil
}

func (s *pgStore) ListLive(ctx context.Context, ownerUserID string) ([]sandbox.Row, error) {
	return s.query(ctx, `SELECT `+columns+` FROM conversations.sandbox
		 WHERE owner_user_id=$1 AND removed_at IS NULL
		 ORDER BY created_at, id`, ownerUserID)
}

func (s *pgStore) ListAllLive(ctx context.Context) ([]sandbox.Row, error) {
	return s.query(ctx, `SELECT `+columns+` FROM conversations.sandbox
		 WHERE removed_at IS NULL
		 ORDER BY created_at, id`)
}

func (s *pgStore) query(ctx context.Context, sql string, args ...any) ([]sandbox.Row, error) {
	rows, err := s.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("list sandboxes: %w", err)
	}
	defer rows.Close()
	var out []sandbox.Row
	for rows.Next() {
		r, err := scanRow(rows)
		if err != nil {
			return nil, fmt.Errorf("scan sandbox: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list sandboxes: %w", err)
	}
	return out, nil
}

func (s *pgStore) SetContainer(ctx context.Context, id, containerID string) error {
	// removed_at IS NULL: a stale or concurrent writer must not resurrect a
	// tombstoned row (the sweep re-reads rows, but a write can still race a
	// removal).
	if _, err := s.pool.Exec(ctx,
		`UPDATE conversations.sandbox SET container_id=$2 WHERE id=$1 AND removed_at IS NULL`, id, containerID); err != nil {
		return fmt.Errorf("set sandbox container: %w", err)
	}
	return nil
}

func (s *pgStore) SetState(ctx context.Context, id, state string) error {
	// removed_at IS NULL: a stale snapshot's SetState(creating/ready) must not
	// write to a tombstoned row. A no-op update is success — the row is gone.
	if _, err := s.pool.Exec(ctx,
		`UPDATE conversations.sandbox SET state=$2 WHERE id=$1 AND removed_at IS NULL`, id, state); err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == checkViolation {
			// An unknown state is the caller's error, not an outage; return it
			// bare so the message names the state, not the DSN.
			return fmt.Errorf("unknown sandbox state %q", state)
		}
		return fmt.Errorf("set sandbox state: %w", err)
	}
	return nil
}

func (s *pgStore) MarkRemoved(ctx context.Context, id string, at time.Time) error {
	// Idempotent: only a live row is stamped, so a second call leaves the first
	// timestamp in place and an unknown id is a silent no-op.
	if _, err := s.pool.Exec(ctx,
		`UPDATE conversations.sandbox SET removed_at=$2 WHERE id=$1 AND removed_at IS NULL`,
		id, at); err != nil {
		return fmt.Errorf("mark sandbox removed: %w", err)
	}
	return nil
}
