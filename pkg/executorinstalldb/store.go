// SPDX-License-Identifier: Apache-2.0

// Package executorinstalldb is the daemon-only Postgres implementation of
// executorinstall.Store. It owns a pgx pool and lives here — never in
// pkg/executorinstall — because the CLI and the pgx-free daemon code name
// executorinstall.Store without linking postgres.
package executorinstalldb

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"go.graveland.dev/rafiki/pkg/executorinstall"
)

// uniqueViolation is SQLSTATE 23505.
const uniqueViolation = "23505"

// liveExecutorIndex is the partial unique index over executor_id added by
// migration 0070. Insert matches a 23505 against it BY NAME, so another unique
// index on this table later cannot inherit the ErrExists advice.
const liveExecutorIndex = "executor_install_executor_live"

// Store is the pgx-backed executorinstall.Store.
type Store struct {
	pool *pgxpool.Pool
}

var _ executorinstall.Store = (*Store)(nil)

// New creates an executor-install Store backed by pool.
func New(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

// columns is the projection every read shares, in scanRow order.
const columns = `id, owner_user_id, executor_id, launcher_executor_id, target,
	instance, root, installed_by_child, platform, mode, created_at, removed_at`

type rowScanner interface{ Scan(dest ...any) error }

// scanRow decodes one row in columns order. The nullable timestamp scans into
// *time.Time so NULL is distinguishable from the zero time.
func scanRow(rs rowScanner) (executorinstall.Row, error) {
	var r executorinstall.Row
	if err := rs.Scan(
		&r.ID, &r.OwnerUserID, &r.ExecutorID, &r.LauncherExecutorID, &r.Target,
		&r.Instance, &r.Root, &r.InstalledByChild, &r.Platform, &r.Mode,
		&r.CreatedAt, &r.RemovedAt); err != nil {
		return executorinstall.Row{}, err
	}
	r.CreatedAt = r.CreatedAt.UTC()
	return r, nil
}

// alreadyInstalled translates a rejected INSERT into
// conversations.executor_install when — and only when — the live executor_id
// unique index is what rejected it. Matched by constraint NAME as well as
// SQLSTATE, and returned bare (never wrapped in the pgconn error, which carries
// the DSN) so the caller can compare errors.Is.
func alreadyInstalled(err error) error {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return nil
	}
	if pgErr.Code != uniqueViolation || pgErr.ConstraintName != liveExecutorIndex {
		return nil
	}
	return executorinstall.ErrExists
}

// Insert writes a new row. created_at is the database clock; r.CreatedAt is
// ignored. removed_at is written so a caller may record a row that is already
// tombstoned.
func (s *Store) Insert(ctx context.Context, r executorinstall.Row) error {
	mode := r.Mode
	if mode == "" {
		mode = executorinstall.ModeTunnel
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO conversations.executor_install
		   (id, owner_user_id, executor_id, launcher_executor_id, target,
		    instance, root, installed_by_child, platform, mode, removed_at)
		 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`,
		r.ID, r.OwnerUserID, r.ExecutorID, r.LauncherExecutorID, r.Target,
		r.Instance, r.Root, r.InstalledByChild, r.Platform, mode, r.RemovedAt)
	if err != nil {
		if dup := alreadyInstalled(err); dup != nil {
			return dup
		}
		return fmt.Errorf("insert executor install: %w", err)
	}
	return nil
}

// ByExecutor returns the live row for executorID. A tombstoned row is not
// returned: the caller asks "is this executor installed right now".
func (s *Store) ByExecutor(ctx context.Context, executorID string) (executorinstall.Row, bool, error) {
	row, err := scanRow(s.pool.QueryRow(ctx,
		`SELECT `+columns+` FROM conversations.executor_install
		  WHERE executor_id=$1 AND removed_at IS NULL`, executorID))
	if errors.Is(err, pgx.ErrNoRows) {
		return executorinstall.Row{}, false, nil
	}
	if err != nil {
		return executorinstall.Row{}, false, fmt.Errorf("get executor install: %w", err)
	}
	return row, true, nil
}

// ListLive returns the live rows for EXACTLY ownerUserID, oldest first. An
// empty ownerUserID is the ownerless rows (""), never every owner: an install
// is caller-bound, so "" must not widen a scoped read into a daemon-wide one.
func (s *Store) ListLive(ctx context.Context, ownerUserID string) ([]executorinstall.Row, error) {
	return s.query(ctx, `SELECT `+columns+` FROM conversations.executor_install
		 WHERE owner_user_id=$1 AND removed_at IS NULL
		 ORDER BY created_at, id`, ownerUserID)
}

// ListLiveAll returns every live row regardless of owner — the operator/admin
// surface only, never a caller-bound path.
func (s *Store) ListLiveAll(ctx context.Context) ([]executorinstall.Row, error) {
	return s.query(ctx, `SELECT `+columns+` FROM conversations.executor_install
		 WHERE removed_at IS NULL
		 ORDER BY created_at, id`)
}

func (s *Store) query(ctx context.Context, sql string, args ...any) ([]executorinstall.Row, error) {
	rows, err := s.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("list executor installs: %w", err)
	}
	defer rows.Close()
	var out []executorinstall.Row
	for rows.Next() {
		r, err := scanRow(rows)
		if err != nil {
			return nil, fmt.Errorf("scan executor install: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list executor installs: %w", err)
	}
	return out, nil
}

// SetLauncher records the launcher executor and the remote platform a live row
// was installed through. removed_at IS NULL: a stale writer must not resurrect a
// tombstoned row.
func (s *Store) SetLauncher(ctx context.Context, id, launcherExecutorID, platform string) error {
	if _, err := s.pool.Exec(ctx,
		`UPDATE conversations.executor_install
		    SET launcher_executor_id=$2, platform=$3
		  WHERE id=$1 AND removed_at IS NULL`,
		id, launcherExecutorID, platform); err != nil {
		return fmt.Errorf("set executor install launcher: %w", err)
	}
	return nil
}

// SetMode records the transport a live row was installed with. removed_at IS
// NULL: a stale writer must not resurrect a tombstoned row, and an unknown id is
// a silent no-op exactly as SetLauncher treats one.
func (s *Store) SetMode(ctx context.Context, id, mode string) error {
	if mode != executorinstall.ModeDirect {
		mode = executorinstall.ModeTunnel
	}
	if _, err := s.pool.Exec(ctx,
		`UPDATE conversations.executor_install SET mode=$2
		  WHERE id=$1 AND removed_at IS NULL`,
		id, mode); err != nil {
		return fmt.Errorf("set executor install mode: %w", err)
	}
	return nil
}

// Tombstone stamps removed_at. It is idempotent — only a live row is stamped,
// so a second call leaves the first timestamp in place — and an unknown id is a
// silent no-op.
func (s *Store) Tombstone(ctx context.Context, id string) error {
	if _, err := s.pool.Exec(ctx,
		`UPDATE conversations.executor_install SET removed_at=now()
		  WHERE id=$1 AND removed_at IS NULL`, id); err != nil {
		return fmt.Errorf("tombstone executor install: %w", err)
	}
	return nil
}
