// SPDX-License-Identifier: Apache-2.0

// Package executorinstall holds the executor-install row and its store
// contract. It is STDLIB ONLY, like pkg/protocol: the CLI and the pgx-free
// daemon code name this package without linking a database driver, and the pgx
// implementation lives in pkg/executorinstalldb.
//
// Every access-gating fact on a row is written by the daemon from a value IT
// verified; nothing the target reports gates access. A row is TOMBSTONED with
// RemovedAt, never DELETE'd, and executor/launcher ids are plain text because
// executor rows hard-delete.
package executorinstall

import (
	"context"
	"errors"
	"time"
)

// Row is one conversations.executor_install record.
//
// Target is the ssh destination the launcher was told to reach (host alias or
// user@host), Instance is the remote executor instance name, and Root is the
// remote root the install lives under. InstalledByChild is the child that asked
// for the install ("" when a human did), and Platform is the remote OS/arch the
// daemon learned from the target itself — a LABEL, never an access gate.
type Row struct {
	ID                 string
	OwnerUserID        string
	ExecutorID         string
	LauncherExecutorID string
	Target             string
	Instance           string
	Root               string
	InstalledByChild   string
	Platform           string
	CreatedAt          time.Time
	RemovedAt          *time.Time
}

// Store is the executor_install table's persistence layer. The implementation
// is in pkg/executorinstalldb (daemon-only, it owns a pgx pool); this
// interface stays here so a pgx-free package can name it.
//
// A row is TOMBSTONED, never deleted: Tombstone stamps RemovedAt, which frees
// the executor_id for a fresh install while keeping the row for lineage.
type Store interface {
	// Insert writes a new row. It returns ErrExists when the live-row
	// executor_id unique index rejects a second live row for the same executor.
	// CreatedAt is set by the database clock, not by r.
	Insert(ctx context.Context, r Row) error
	// ByExecutor returns the LIVE row for an executor id (RemovedAt IS NULL).
	// ok is false when no live row exists.
	ByExecutor(ctx context.Context, executorID string) (Row, bool, error)
	// ListLive returns live rows. An empty ownerUserID means EVERY owner;
	// a non-empty one scopes to that owner. Oldest first.
	ListLive(ctx context.Context, ownerUserID string) ([]Row, error)
	// SetLauncher records the launcher executor and the remote platform a live
	// row was installed through.
	SetLauncher(ctx context.Context, id, launcherExecutorID, platform string) error
	// Tombstone tombstones the row. It is idempotent: a second call leaves the
	// first timestamp in place, and an unknown id is not an error.
	Tombstone(ctx context.Context, id string) error
}

// ErrExists is returned by Insert when a live row already holds the requested
// executor_id. It is a plain error, not a database error: the caller must never
// see a pgx error or a DSN.
var ErrExists = errors.New("executor install already exists for this executor")
