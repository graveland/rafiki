// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"
	"testing"
	"time"

	"github.com/multigres/testkit/assert"
)

func TestDedupTTLAndCap(t *testing.T) {
	start := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)

	t.Run("expires after 24h", func(t *testing.T) {
		c := assert.NewAborting(t)
		d := newDedup()
		c.False(d.seen("a", start), "first sighting of an id is not a duplicate")
		c.True(d.seen("a", start.Add(time.Hour)), "a retry inside the TTL is a duplicate")
		c.True(d.seen("a", start.Add(23*time.Hour)), "still inside the TTL")
		c.False(d.seen("a", start.Add(24*time.Hour)), "24h later the id is forgotten")
		c.False(d.contains("b", start), "contains does not record")
		c.True(d.size() == 1)

		// contains drops an expired entry rather than reporting it.
		d.record("old", start)
		c.False(d.contains("old", start.Add(25*time.Hour)))
	})

	t.Run("empty id is never seen", func(t *testing.T) {
		c := assert.NewAborting(t)
		d := newDedup()
		c.False(d.seen("", start))
		c.False(d.seen("", start))
		c.False(d.contains("", start))
		d.record("", start)
		c.Eq(0, d.size())
	})

	t.Run("cap evicts the oldest", func(t *testing.T) {
		c := assert.NewAborting(t)
		d := newDedup()
		c.Eq(dedupCap, d.max)
		for i := 0; i < d.max; i++ {
			id := fmt.Sprintf("id-%05d", i)
			d.record(id, start.Add(time.Duration(i)*time.Second))
		}
		c.Eq(d.max, d.size())
		c.True(d.contains("id-00000", start.Add(time.Hour)))

		last := start.Add(time.Duration(d.max) * time.Second)
		d.record("newest", last)
		c.Eq(d.max, d.size(), "the map never grows past the cap")
		c.False(d.contains("id-00000", last), "the oldest entry was evicted to make room")
		c.True(d.contains("newest", last), "the entry just recorded is kept")
		c.True(d.contains("id-00001", last))
	})

	t.Run("expired entries are pruned at the cap", func(t *testing.T) {
		c := assert.NewAborting(t)
		d := newDedup()
		for i := 0; i < d.max; i++ {
			d.record(fmt.Sprintf("expired-%05d", i), start)
		}
		d.record("fresh", start.Add(dedupTTL))
		c.Eq(1, d.size(), "a full map of expired ids collapses instead of evicting live ones")
		c.True(d.contains("fresh", start.Add(dedupTTL)))
	})
}
