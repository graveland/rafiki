// SPDX-License-Identifier: Apache-2.0

package sandbox

import (
	"context"
	"errors"
	"time"

	"go.graveland.dev/rafiki/pkg/protocol"
)

// Row is one conversations.sandbox record. Every access-gating field — the
// isolation, workspace mode, owner, executor and scope — is written by the
// daemon from a value it verified; nothing the container reports gates access.
//
// Spec is the raw resolved SandboxSpec JSON as stored (JSONB), WITHOUT the
// executor credential. It is []byte rather than a decoded struct so the store
// never has to know the spec's shape and a round trip is byte-exact.
type Row struct {
	ID                 string
	OwnerUserID        string
	Name               string // "" for a spawn block
	ExecutorID         string
	LauncherExecutorID string
	ContainerID        string
	Image              string
	Spec               []byte
	CreatedBy          string
	OwnerChild         string
	Scope              protocol.SandboxScope
	State              string
	CreatedAt          time.Time
	ExpiresAt          *time.Time
	RemovedAt          *time.Time
}

// Store is the sandbox table's persistence layer. The implementation is in
// pkg/sandboxdb (daemon-only, it owns a pgx pool); this interface stays in the
// pgx-free pkg/sandbox so the CLI can name it without linking postgres.
//
// A sandbox row is TOMBSTONED, never deleted: MarkRemoved stamps removed_at,
// which frees the name for reuse while keeping the row for lineage.
type Store interface {
	// Insert writes a new row. It returns ErrNameTaken when the live-row
	// owner/name unique index rejects a NAME collision. CreatedAt is set by the
	// database clock, not by r.
	Insert(ctx context.Context, r Row) error
	// Get returns the row by id, INCLUDING removed rows. ok is false when no
	// such row exists.
	Get(ctx context.Context, id string) (Row, bool, error)
	// GetByName returns the live row by (owner, name). Spawn blocks (name "")
	// are never matched.
	GetByName(ctx context.Context, ownerUserID, name string) (Row, bool, error)
	// ListLive returns the owner's live rows (removed_at IS NULL), oldest first.
	ListLive(ctx context.Context, ownerUserID string) ([]Row, error)
	// ListAllLive returns every live row regardless of owner — the sweep's read.
	ListAllLive(ctx context.Context) ([]Row, error)
	// SetContainer records the container the launcher created.
	SetContainer(ctx context.Context, id, containerID string) error
	// SetState records the row's state; the CHECK constraint rejects an
	// unknown one.
	SetState(ctx context.Context, id, state string) error
	// MarkRemoved tombstones the row. It is idempotent: a second call leaves
	// the first timestamp in place, and an unknown id is not an error.
	MarkRemoved(ctx context.Context, id string, at time.Time) error
}

// ErrNameTaken is returned by Insert when a live sandbox already holds the
// requested (owner, name). It is a plain error, not a database error: the
// caller must never see a pgx error or a DSN.
var ErrNameTaken = errors.New("sandbox name already in use")
