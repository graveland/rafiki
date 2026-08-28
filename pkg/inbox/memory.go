// SPDX-License-Identifier: Apache-2.0

package inbox

import (
	"context"
	"errors"
	"sort"
	"sync"
	"time"
)

// Memory is an in-memory inbox.Store. It is what the daemon uses when there is
// no database and what unit tests use everywhere.
//
// It is NOT the old synchronous-delivery Memory: delivery is the Queue's job
// now, and a store that delivered inside Accept could not have a pending state
// to replay.
type Memory struct {
	mu   sync.Mutex
	rows map[string]*Inbound
	st   map[string]State
	seq  int64 // total order for rows accepted inside the same clock tick
	ord  map[string]int64
}

// NewMemory returns an empty in-memory store.
func NewMemory() *Memory {
	return &Memory{
		rows: make(map[string]*Inbound),
		st:   make(map[string]State),
		ord:  make(map[string]int64),
	}
}

func (m *Memory) Accept(_ context.Context, in Inbound) (Inbound, error) {
	if in.ChildID == "" {
		return Inbound{}, errors.New("inbox: ChildID is required")
	}
	id, err := NewID()
	if err != nil {
		return Inbound{}, err
	}
	in.ID = id
	in.AcceptedAt = time.Now().UTC()

	m.mu.Lock()
	defer m.mu.Unlock()
	m.seq++
	m.ord[id] = m.seq
	row := in
	m.rows[id] = &row
	m.st[id] = StatePending
	return in, nil
}

func (m *Memory) Pending(_ context.Context, childID string) ([]Inbound, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Inbound
	for id, r := range m.rows {
		if r.ChildID == childID && m.st[id] == StatePending {
			out = append(out, *r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return m.ord[out[i].ID] < m.ord[out[j].ID] })
	return out, nil
}

func (m *Memory) MarkSent(_ context.Context, ids []string) error {
	return m.setState(ids, StateSent, func(cur State) bool { return cur == StatePending })
}

func (m *Memory) MarkConsumed(_ context.Context, ids []string) error {
	return m.setState(ids, StateConsumed, func(cur State) bool {
		return cur == StatePending || cur == StateSent
	})
}

func (m *Memory) setState(ids []string, to State, ok func(State) bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, id := range ids {
		if cur, exists := m.st[id]; exists && ok(cur) {
			m.st[id] = to
		}
	}
	return nil
}

func (m *Memory) ResetSent(_ context.Context, childID string) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for id, r := range m.rows {
		if r.ChildID == childID && m.st[id] == StateSent {
			m.st[id] = StatePending
			n++
		}
	}
	return n, nil
}

func (m *Memory) Drop(_ context.Context, childID, _ string) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for id, r := range m.rows {
		if r.ChildID != childID {
			continue
		}
		if m.st[id] == StatePending || m.st[id] == StateSent {
			m.st[id] = StateDropped
			n++
		}
	}
	return n, nil
}

func (m *Memory) Sweep(_ context.Context, before time.Time) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for id, r := range m.rows {
		st := m.st[id]
		if (st == StateConsumed || st == StateDropped) && r.AcceptedAt.Before(before) {
			delete(m.rows, id)
			delete(m.st, id)
			delete(m.ord, id)
			n++
		}
	}
	return n, nil
}

var _ Store = (*Memory)(nil)
