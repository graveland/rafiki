// SPDX-License-Identifier: Apache-2.0

package batch

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

// State is the lifecycle state of one parked call's durable row. There is no
// meaningful zero value: "" is invalid everywhere, and every Store method
// that takes or checks a State refuses an empty or unknown value (validState).
type State string

const (
	// StateQueued: durable, not yet sent to the provider.
	StateQueued State = "queued"
	// StateSubmitting is written BEFORE the provider POST and marks the
	// outcome-unknown window: the batch may or may not have been created.
	// Only the recovery sweep resolves a submitting row — it is never
	// resubmitted blind, because a second POST would double-bill.
	StateSubmitting State = "submitting"
	// StateSubmitted: the provider batch id is recorded on the row.
	StateSubmitted State = "submitted"
	// StateCompleted: the response is stored on the row.
	StateCompleted State = "completed"
	// StateFailed: the error text is stored on the row.
	StateFailed State = "failed"
)

// validState reports whether s is one of the five known states. The empty
// string and anything unknown are refused.
func validState(s State) bool {
	switch s {
	case StateQueued, StateSubmitting, StateSubmitted, StateCompleted, StateFailed:
		return true
	}
	return false
}

// Row is one parked call's durable state. Rows are never deleted: a finished
// call whose conversation is gone is tombstoned (Tombstone), which frees its
// custom_id for a fresh insert.
type Row struct {
	ID              int64
	CustomID        string
	Model           string
	State           State
	ProviderBatchID string          // "" until submitted
	Request         json.RawMessage // the call's request body (params JSON, model/stream/provider removed)
	Response        json.RawMessage // nil until completed
	Error           string
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// Store persists parked-call rows. Implementations must keep custom_id unique
// among live (non-tombstoned) rows, stamp CreatedAt/UpdatedAt when zero, and
// bump UpdatedAt on every transition.
type Store interface {
	// Live returns the non-tombstoned row for customID, ok=false if none.
	Live(ctx context.Context, customID string) (Row, bool, error)
	// Insert stores a new row; State must be StateQueued and the custom_id
	// must not collide with a live row.
	Insert(ctx context.Context, r Row) (Row, error)
	// Tombstone marks the row deleted. It never removes the row.
	Tombstone(ctx context.Context, id int64) error
	// MarkSubmitting moves queued rows to StateSubmitting. Called BEFORE the
	// provider POST, so a crash between POST and MarkSubmitted is visible to
	// the recovery sweep.
	MarkSubmitting(ctx context.Context, ids []int64) error
	// MarkSubmitted records the provider batch id on submitting rows.
	MarkSubmitted(ctx context.Context, ids []int64, providerBatchID string) error
	// Requeue returns submitting rows to StateQueued for a fresh submit
	// window. Only used when the POST provably created nothing.
	Requeue(ctx context.Context, ids []int64) error
	// Complete stores the response on a submitted row.
	Complete(ctx context.Context, id int64, response json.RawMessage) error
	// Fail stores the error text on a submitting or submitted row.
	Fail(ctx context.Context, id int64, msg string) error
	// InState returns the live rows in state s, oldest first. An empty or
	// unknown State is refused.
	InState(ctx context.Context, s State) ([]Row, error)
}

// insertStateErr builds the refusal for an Insert whose State is not queued.
func insertStateErr(got State) error {
	return fmt.Errorf("batch: insert requires state %q, got %q", StateQueued, got)
}

// stateErr builds the refusal for an empty or unknown State argument.
func stateErr(got State) error {
	return fmt.Errorf("batch: unknown state %q", string(got))
}
