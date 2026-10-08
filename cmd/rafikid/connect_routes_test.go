// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"testing"
	"time"

	"go.graveland.dev/rafiki/pkg/connectapi"
	"go.graveland.dev/rafiki/pkg/routepolicy"
	"go.graveland.dev/rafiki/pkg/routing"

	"github.com/multigres/testkit/assert"
)

// fakeRouteStore is an append-only routepolicy.Store over a slice, applying
// the same newest-row-wins rule the Postgres store's queries do: a Set
// supersedes, a Delete appends a tombstone, Active picks the newest row per
// line and drops tombstoned lines.
type fakeRouteStore struct {
	log []fakeRouteLogRow
}

type fakeRouteLogRow struct {
	row     routepolicy.Row
	deleted bool
}

func (s *fakeRouteStore) Set(_ context.Context, line, spec string) error {
	s.log = append(s.log, fakeRouteLogRow{row: routepolicy.Row{
		ModelLine: line, Spec: spec, CreatedAt: time.Unix(100, 0).UTC(),
	}})
	return nil
}

func (s *fakeRouteStore) Delete(_ context.Context, line string) error {
	for i := len(s.log) - 1; i >= 0; i-- {
		if s.log[i].row.ModelLine != line {
			continue
		}
		if s.log[i].deleted {
			return fmt.Errorf("store: %w", routepolicy.ErrNotFound)
		}
		s.log = append(s.log, fakeRouteLogRow{row: routepolicy.Row{ModelLine: line}, deleted: true})
		return nil
	}
	return fmt.Errorf("store: %w", routepolicy.ErrNotFound)
}

func (s *fakeRouteStore) Active(context.Context) ([]routepolicy.Row, error) {
	latest := make(map[string]fakeRouteLogRow, len(s.log))
	var lines []string
	for _, r := range s.log {
		if _, seen := latest[r.row.ModelLine]; !seen {
			lines = append(lines, r.row.ModelLine)
		}
		latest[r.row.ModelLine] = r
	}
	sort.Strings(lines)
	var out []routepolicy.Row
	for _, l := range lines {
		if r := latest[l]; !r.deleted {
			out = append(out, r.row)
		}
	}
	return out, nil
}

// TestRouteAdapterWritesThenReloads pins the adapter's contract: a write goes
// to the store first, the in-memory Policy is reloaded from Active so the
// resolver changes immediately, the returned row is the one as stored, and a
// delete of an absent line surfaces connectapi.ErrRouteNotFound for the
// handler to map to NotFound.
func TestRouteAdapterWritesThenReloads(t *testing.T) {
	ctx := context.Background()
	store := &fakeRouteStore{}
	policy := routepolicy.NewPolicy()
	a := newConnectRoutes(store, policy)
	c := assert.NewCollecting(t)

	row, err := a.SetRoute(ctx, "z-ai/glm-5.3", "sort=price")
	c.Require().NoError(err)
	c.Eq("z-ai/glm-5.3", row.ModelLine, "returned row's line")
	c.Eq("sort=price", row.Spec, "returned row's spec")
	c.False(row.CreatedAt.IsZero(), "returned row carries the stored created_at")

	// The reload is the point: the in-memory resolver answers the new spec
	// without a restart, and ListRoutes reads the reloaded view.
	c.Eq(routing.SortPrice, policy.Resolve("openrouter/z-ai/glm-5.3").Sort,
		"resolver reflects the write immediately")
	rows, err := a.ListRoutes(ctx)
	c.Require().NoError(err)
	c.Eq(1, len(rows), "one live row after Set")
	c.Eq(time.Unix(100, 0).UTC(), rows[0].CreatedAt, "ListRoutes returns the stored row verbatim")

	// A second Set supersedes rather than stacking.
	if _, err := a.SetRoute(ctx, "z-ai/glm-5.3", "sort=latency,quant=fp8+"); err != nil {
		t.Fatal(err)
	}
	c.Eq(routing.SortLatency, policy.Resolve("openrouter/z-ai/glm-5.3").Sort,
		"newest row wins in the resolver")
	rows, _ = a.ListRoutes(ctx)
	c.Eq(1, len(rows), "still one live row")

	// Deleting an absent line is the sentinel, whatever wrapping the store
	// put around it — the handler maps it to NotFound.
	err = a.DeleteRoute(ctx, "never-set")
	c.Require().Error(err)
	c.True(errors.Is(err, connectapi.ErrRouteNotFound), "absent delete: got %v", err)

	// Deleting the live line tombstones it and the reload drops it from the
	// resolver.
	c.Require().NoError(a.DeleteRoute(ctx, "z-ai/glm-5.3"))
	rows, err = a.ListRoutes(ctx)
	c.Require().NoError(err)
	c.Eq(0, len(rows), "no live rows after Delete")
	c.True(policy.Resolve("openrouter/z-ai/glm-5.3").IsZero(), "resolver is back to zero")

	// A second delete of the same line is the sentinel too: the tombstone is
	// itself the newest row.
	err = a.DeleteRoute(ctx, "z-ai/glm-5.3")
	c.True(errors.Is(err, connectapi.ErrRouteNotFound), "double delete: got %v", err)
}
