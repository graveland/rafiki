// SPDX-License-Identifier: Apache-2.0

package batch

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
	"time"
)

// MemStore is the in-memory Store. It exists so the Batcher and the
// conformance suite can run without a database; the durable implementation is
// pkg/batchdb. All methods are safe for concurrent use.
//
// Transitions are conditional: a transition applies only to the live row in
// the state it moves from (MarkSubmitting from queued, MarkSubmitted from
// submitting, Requeue from submitting, Complete from submitted, Fail from
// submitting or submitted) and is a silent no-op otherwise. The Batcher is the
// only writer and never issues an out-of-order transition; the condition
// guards the row against a stale sweep racing an outcome that already landed.
type MemStore struct {
	mu   sync.Mutex
	seq  int64
	rows map[int64]*memRow
	now  func() time.Time
}

type memRow struct {
	Row
	deletedAt time.Time // zero while the row is live
}

// NewMemStore returns an empty MemStore stamping time.Now.
func NewMemStore() *MemStore {
	return &MemStore{rows: make(map[int64]*memRow), now: time.Now}
}

// NewMemStoreWithClock returns an empty MemStore stamping now. Tests that
// exercise the Batcher's recovery windows inject the same clock into both the
// store (which stamps row times) and the Batcher's Options.Now (which reads
// them), so the two agree.
func NewMemStoreWithClock(now func() time.Time) *MemStore {
	if now == nil {
		now = time.Now
	}
	return &MemStore{rows: make(map[int64]*memRow), now: now}
}

// Insert stores a new queued row. It refuses a State other than StateQueued,
// an empty CustomID, and a CustomID that collides with a live row.
func (m *MemStore) Insert(_ context.Context, r Row) (Row, error) {
	if r.State != StateQueued {
		return Row{}, insertStateErr(r.State)
	}
	if r.CustomID == "" {
		return Row{}, fmt.Errorf("batch: insert requires a custom_id")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.liveLocked(r.CustomID); ok {
		return Row{}, fmt.Errorf("batch: custom_id %q already live", r.CustomID)
	}
	m.seq++
	r.ID = m.seq
	if r.CreatedAt.IsZero() {
		r.CreatedAt = m.now()
	}
	if r.UpdatedAt.IsZero() {
		r.UpdatedAt = r.CreatedAt
	}
	m.rows[r.ID] = &memRow{Row: r}
	return r, nil
}

// Live returns the non-tombstoned row for customID.
func (m *MemStore) Live(_ context.Context, customID string) (Row, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	row, ok := m.liveLocked(customID)
	return row, ok, nil
}

// Tombstone marks the row deleted; the row is kept and its custom_id freed.
func (m *MemStore) Tombstone(_ context.Context, id int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	row, ok := m.rows[id]
	if !ok || !row.deletedAt.IsZero() {
		return nil
	}
	row.deletedAt = m.now()
	return nil
}

// MarkSubmitting moves queued rows to StateSubmitting.
func (m *MemStore) MarkSubmitting(_ context.Context, ids []int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.transitionLocked(ids, StateQueued, func(r *memRow) {
		r.State = StateSubmitting
	})
}

// MarkSubmitted records the provider batch id on submitting rows.
func (m *MemStore) MarkSubmitted(_ context.Context, ids []int64, providerBatchID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.transitionLocked(ids, StateSubmitting, func(r *memRow) {
		r.State = StateSubmitted
		r.ProviderBatchID = providerBatchID
	})
}

// Requeue returns submitting rows to StateQueued.
func (m *MemStore) Requeue(_ context.Context, ids []int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.transitionLocked(ids, StateSubmitting, func(r *memRow) {
		r.State = StateQueued
		r.ProviderBatchID = ""
	})
}

// Complete stores the response on a submitted row.
func (m *MemStore) Complete(_ context.Context, id int64, response json.RawMessage) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.transitionLocked([]int64{id}, StateSubmitted, func(r *memRow) {
		r.State = StateCompleted
		r.Response = response
		r.Error = ""
	})
}

// Fail stores the error text on a submitting or submitted row.
func (m *MemStore) Fail(_ context.Context, id int64, msg string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.failLocked(id, msg)
}

// failLocked applies Fail to the row when it is live in a state Fail may move
// (submitting or submitted); otherwise it is a no-op.
func (m *MemStore) failLocked(id int64, msg string) error {
	row, ok := m.rows[id]
	if !ok || !row.deletedAt.IsZero() {
		return nil
	}
	if row.State != StateSubmitting && row.State != StateSubmitted {
		return nil
	}
	row.State = StateFailed
	row.Error = msg
	row.UpdatedAt = m.now()
	return nil
}

// InState returns the live rows in state s, oldest first. An empty or unknown
// State is refused.
func (m *MemStore) InState(_ context.Context, s State) ([]Row, error) {
	if !validState(s) {
		return nil, stateErr(s)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Row
	for _, row := range m.rows {
		if row.deletedAt.IsZero() && row.State == s {
			out = append(out, row.Row)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// liveLocked returns the live row for customID, if any. Callers hold m.mu.
func (m *MemStore) liveLocked(customID string) (Row, bool) {
	for _, row := range m.rows {
		if row.deletedAt.IsZero() && row.CustomID == customID {
			return row.Row, true
		}
	}
	return Row{}, false
}

// transitionLocked applies apply to every live row in ids whose state is
// from, bumping UpdatedAt. A row in any other state is left untouched. Rows
// that do not exist are ignored: the caller works from a snapshot it read
// earlier, and the row may have been tombstoned since. Callers hold m.mu.
func (m *MemStore) transitionLocked(ids []int64, from State, apply func(*memRow)) error {
	for _, id := range ids {
		row, ok := m.rows[id]
		if !ok || !row.deletedAt.IsZero() || row.State != from {
			continue
		}
		apply(row)
		row.UpdatedAt = m.now()
	}
	return nil
}
