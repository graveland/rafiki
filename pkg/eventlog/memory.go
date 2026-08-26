// SPDX-License-Identifier: Apache-2.0

package eventlog

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"google.golang.org/protobuf/encoding/protojson"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
)

const defaultReadLimit = 1000

// Memory is an in-memory Store implementation, suitable for unit tests
// and database-free runs.
type Memory struct {
	mu   sync.Mutex
	recs map[string][]Record
}

// NewMemory returns an empty in-memory eventlog Store.
func NewMemory() *Memory {
	return &Memory{
		recs: make(map[string][]Record),
	}
}

// Append persists ev and returns its per-child ordinal.
//
// It REFUSES an ephemeral event rather than dropping it. A caller trying to
// persist a content delta has misunderstood the tier split, and a silent
// no-op would hide that until the log is enormous.
func (m *Memory) Append(ctx context.Context, childID string, ev *rafikiv1.Event) (int32, error) {
	if TierOf(ev) != TierDurable {
		return 0, fmt.Errorf("eventlog: refusing to append ephemeral event %q", TypeName(ev))
	}
	if childID == "" {
		return 0, errors.New("eventlog: empty child id")
	}
	payload, err := protojson.Marshal(ev)
	if err != nil {
		return 0, fmt.Errorf("eventlog: marshal: %w", err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	ord := int32(len(m.recs[childID]))
	m.recs[childID] = append(m.recs[childID], Record{
		ChildID:   childID,
		Ordinal:   ord,
		Type:      TypeName(ev),
		Payload:   payload,
		CreatedAt: time.Now().UTC(),
	})
	return ord, nil
}

// Read returns records for childID with Ordinal > afterOrdinal, in ascending order.
func (m *Memory) Read(ctx context.Context, childID string, afterOrdinal int32, limit int) ([]Record, error) {
	if limit <= 0 {
		limit = defaultReadLimit
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	all := m.recs[childID]
	var out []Record
	for _, r := range all {
		if r.Ordinal > afterOrdinal {
			out = append(out, r)
			if len(out) >= limit {
				break
			}
		}
	}
	return out, nil
}

// Latest returns the highest ordinal for childID, or ErrNotFound if no events exist.
func (m *Memory) Latest(ctx context.Context, childID string) (int32, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	all := m.recs[childID]
	if len(all) == 0 {
		return 0, ErrNotFound
	}
	return all[len(all)-1].Ordinal, nil
}
