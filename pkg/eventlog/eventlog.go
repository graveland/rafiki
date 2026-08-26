// SPDX-License-Identifier: Apache-2.0

package eventlog

import (
	"context"
	"errors"
	"time"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
)

// ErrNotFound is returned by Latest when the child has no events.
var ErrNotFound = errors.New("eventlog: no events for child")

// Record is a stored event in the durable log.
type Record struct {
	ChildID   string
	Ordinal   int32
	Type      string
	Payload   []byte
	CreatedAt time.Time
}

// Store is the durable event log contract.
type Store interface {
	// Append assigns the next per-child ordinal (starting at 0, gap-free per child)
	// and persists ev. Appending an ephemeral event returns an error.
	Append(ctx context.Context, childID string, ev *rafikiv1.Event) (int32, error)

	// Read returns records with Ordinal > afterOrdinal in ascending order,
	// capped at limit (or implementation default if limit <= 0).
	Read(ctx context.Context, childID string, afterOrdinal int32, limit int) ([]Record, error)

	// Latest returns the highest ordinal for childID, or ErrNotFound if none exist.
	Latest(ctx context.Context, childID string) (int32, error)
}
