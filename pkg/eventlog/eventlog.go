// SPDX-License-Identifier: Apache-2.0

package eventlog

import (
	"context"
	"errors"
	"fmt"
	"time"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/timestamppb"

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

// Decode returns the Record's event. It is the ONE reader of the persisted
// payload, and it is deliberately tolerant of an event an older writer
// persisted: protojson.UnmarshalOptions{DiscardUnknown: true} drops a field
// this build no longer knows (ts_unix_ms, resume_at_unix_ms, duration_ms),
// and an event whose ts is unset — one written before ts was a Timestamp —
// falls back to the row's CreatedAt (the append time). This is the ONLY
// back-compat for stored events: no data is rewritten and no deprecated field
// is kept. DiscardUnknown drops unknown FIELDS only; a known field carrying a
// wrong type (a malformed NEW payload) still fails, so a real decode error is
// never swallowed silently.
func (r Record) Decode() (*rafikiv1.Event, error) {
	var ev rafikiv1.Event
	opts := protojson.UnmarshalOptions{DiscardUnknown: true}
	if err := opts.Unmarshal(r.Payload, &ev); err != nil {
		return nil, fmt.Errorf("eventlog: decode record %s:%d: %w", r.ChildID, r.Ordinal, err)
	}
	if ev.Ts == nil {
		ev.Ts = timestamppb.New(r.CreatedAt)
	}
	return &ev, nil
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
