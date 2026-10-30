// SPDX-License-Identifier: Apache-2.0

package conv

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/multigres/testkit/assert"

	"go.graveland.dev/rafiki/pkg/msg"
)

// pageFixture writes one conversation whose nodes carry chosen CreatedAt
// offsets (in seconds from a base), so a test controls exactly which nodes
// share a timestamp. kinds, when non-empty at an index, sets that node's kind.
func pageFixture(t *testing.T, offsets []int, kinds map[int]string) (*Store, string, time.Time) {
	t.Helper()
	b := NewMemBackend()
	convID := uuid.Must(uuid.NewV7()).String()
	b.Ensure(convID)
	base := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	clock := base
	b.now = func() time.Time { return clock }
	h := NewStore(b, Options{}).Open(convID, AsOwner())
	for i, off := range offsets {
		clock = base.Add(time.Duration(off) * time.Second)
		node := NewNode{Msg: msg.Message{Role: msg.User, Content: []msg.Block{msg.Text{Text: fmt.Sprintf("row %d", i)}}}}
		if kind, ok := kinds[i]; ok {
			node.Kind = kind
		}
		if _, err := h.Append(context.Background(), Append{FromHead: true, Nodes: []NewNode{node}}); err != nil {
			t.Fatalf("append row %d: %v", i, err)
		}
	}
	return NewStore(b, Options{}), convID, base
}

// intPtr returns a pointer to v, for a PageQuery bound.
func intPtr(v int) *int { return &v }

// pageOrdinals pages through a cold store, so the read is a real walk.
func pageOrdinals(t *testing.T, s *Store, convID string, q PageQuery) []int {
	t.Helper()
	c := assert.NewAborting(t)
	nodes, err := s.Page(context.Background(), convID, q)
	c.NoError(err, "Page")
	out := []int{}
	for _, n := range nodes {
		out = append(out, n.Ordinal)
	}
	return out
}

func TestPageBounds(t *testing.T) {
	c := assert.NewAborting(t)
	// Nodes 0 and 1 share a timestamp; 2 and 3 are later and distinct.
	s, convID, base := pageFixture(t, []int{0, 0, 1, 2}, nil)
	before := base.Add(2 * time.Second)

	c.EqDeep([]int{0, 1, 2, 3}, pageOrdinals(t, s, convID, PageQuery{}), "no bounds")
	c.EqDeep([]int{3}, pageOrdinals(t, s, convID, PageQuery{Limit: intPtr(1)}), "limit keeps the newest")
	c.EqDeep([]int{2, 3}, pageOrdinals(t, s, convID, PageQuery{AfterOrdinal: intPtr(1)}), "after_ordinal is strict")
	c.EqDeep([]int{0, 1, 2}, pageOrdinals(t, s, convID, PageQuery{Before: &before}), "before is strict")
	c.EqDeep([]int{2}, pageOrdinals(t, s, convID, PageQuery{Before: &before, Limit: intPtr(1)}), "before then limit")
	c.EqDeep([]int{0, 1, 2, 3}, pageOrdinals(t, s, convID, PageQuery{Limit: intPtr(10)}),
		"a limit above the node count cuts nothing")
}

// A cut that would fall inside a same-CreatedAt run widens back to the start of
// that run, and only that run: a matching timestamp further back, past a node
// with a different one, is not part of the group.
func TestPageLimitNeverSplitsATimestampGroup(t *testing.T) {
	c := assert.NewAborting(t)
	s, convID, _ := pageFixture(t, []int{0, 0, 1}, nil)
	c.EqDeep([]int{0, 1, 2}, pageOrdinals(t, s, convID, PageQuery{Limit: intPtr(2)}), "the t0 group stays whole")

	s, convID, _ = pageFixture(t, []int{0, 1, 0}, nil)
	c.EqDeep([]int{2}, pageOrdinals(t, s, convID, PageQuery{Limit: intPtr(1)}), "a non-adjacent equal time is not widened to")
}

// compaction_tail nodes render nothing, so they are neither returned nor
// counted toward the limit.
func TestPageSkipsCompactionTailNodes(t *testing.T) {
	c := assert.NewAborting(t)
	s, convID, _ := pageFixture(t, []int{0, 1, 2}, map[int]string{1: KindCompactionTail})
	c.EqDeep([]int{0, 2}, pageOrdinals(t, s, convID, PageQuery{}), "no bounds")
	c.EqDeep([]int{0, 2}, pageOrdinals(t, s, convID, PageQuery{Limit: intPtr(2)}), "the tail does not count")
}

// Paging backwards with the oldest received CreatedAt as the next Before visits
// every node exactly once, groups included.
func TestPagePagingBackwardsVisitsEveryNodeOnce(t *testing.T) {
	c := assert.NewAborting(t)
	s, convID, _ := pageFixture(t, []int{0, 1, 1, 1, 2, 3, 3, 4, 5, 5}, nil)
	var seen []int
	var before *time.Time
	for range 20 {
		nodes, err := s.Page(context.Background(), convID, PageQuery{Before: before, Limit: intPtr(2)})
		c.NoError(err, "Page")
		if len(nodes) == 0 {
			break
		}
		for i := len(nodes) - 1; i >= 0; i-- {
			seen = append(seen, nodes[i].Ordinal)
		}
		oldest := nodes[0].CreatedAt
		before = &oldest
	}
	c.EqDeep([]int{9, 8, 7, 6, 5, 4, 3, 2, 1, 0}, seen, "every node once, newest first")
}

func TestPageRefusesNonPositiveLimit(t *testing.T) {
	c := assert.NewAborting(t)
	s, convID, _ := pageFixture(t, []int{0}, nil)
	for _, n := range []int{0, -1} {
		_, err := s.Page(context.Background(), convID, PageQuery{Limit: intPtr(n)})
		c.ErrorContains(err, fmt.Sprintf("conv: page: limit %d is not positive", n), "limit %d", n)
	}
}

// The zero options are the documented defaults, not an off switch: a cache of
// 0 bytes is 256 MiB and an idle TTL of 0 is five minutes.
func TestOptionsZeroValuesAreDefaults(t *testing.T) {
	c := assert.NewAborting(t)
	for _, bytes := range []int64{0, -1, -1 << 40} {
		s := NewStore(NewMemBackend(), Options{CacheBytes: bytes})
		c.Eq(int64(defaultCacheBytes), s.cacheBytes, "CacheBytes %d means 256 MiB, never no cache", bytes)
		c.Eq(defaultIdleTTL, s.ttl, "IdleTTL 0 means five minutes")
		c.NotNil(s.now, "a nil Now is time.Now")
	}
}
