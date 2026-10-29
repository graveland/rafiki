// SPDX-License-Identifier: Apache-2.0

package modelalias

import (
	"context"
	"sort"
	"sync"
	"time"
)

// NewMemStore returns an in-memory Store with the same semantics as the
// Postgres one -- including validation in Put and CreatedAt preserved on a
// replace. Owners are keyed exactly as stored ("" is its own bucket).
func NewMemStore() Store { return &memStore{owners: map[string]map[string]Alias{}} }

type memStore struct {
	mu     sync.Mutex
	owners map[string]map[string]Alias
}

func (s *memStore) Get(_ context.Context, ownerUserID, name string) (Alias, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	a, ok := s.owners[ownerUserID][name]
	if !ok {
		return Alias{}, ErrNotFound
	}
	return a, nil
}

func (s *memStore) List(_ context.Context, ownerUserID string) ([]Alias, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Alias, 0, len(s.owners[ownerUserID]))
	for _, a := range s.owners[ownerUserID] {
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (s *memStore) Put(_ context.Context, ownerUserID, name, target string) (Alias, error) {
	if err := Validate(name, target); err != nil {
		return Alias{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().UTC()
	byName, ok := s.owners[ownerUserID]
	if !ok {
		byName = map[string]Alias{}
		s.owners[ownerUserID] = byName
	}
	a, existed := byName[name]
	if existed {
		a.Target = target
		a.UpdatedAt = now
	} else {
		a = Alias{Name: name, Target: target, CreatedAt: now, UpdatedAt: now}
	}
	byName[name] = a
	return a, nil
}

func (s *memStore) Delete(_ context.Context, ownerUserID, name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.owners[ownerUserID][name]; !ok {
		return ErrNotFound
	}
	delete(s.owners[ownerUserID], name)
	return nil
}
