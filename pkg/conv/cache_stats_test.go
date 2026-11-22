// SPDX-License-Identifier: Apache-2.0

package conv

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/multigres/testkit/assert"

	"go.graveland.dev/rafiki/pkg/msg"
)

func statsConv(t *testing.T, o Options) (*Store, *MemBackend, string) {
	t.Helper()
	b := NewMemBackend()
	convID := uuid.Must(uuid.NewV7()).String()
	b.Ensure(convID)
	return NewStore(b, o), b, convID
}

func appendText(t *testing.T, h *Handle, s string) {
	t.Helper()
	c := assert.NewAborting(t)
	_, err := h.Append(context.Background(), Append{FromHead: true, Nodes: []NewNode{{
		Msg: msg.Message{Role: msg.User, Content: []msg.Block{msg.Text{Text: s}}},
	}}})
	c.NoError(err, "append")
}

// The decoded estimate and the charged bytes are maintained incrementally on
// insert and evict; both must equal a recount of what the LRU holds.
func TestCacheStatsBytesMatchWhatIsCached(t *testing.T) {
	c := assert.NewAborting(t)
	s, _, convID := statsConv(t, Options{CacheBytes: 4096})
	h := s.Open(convID, AsOwner())
	for range 12 {
		appendText(t, h, strings.Repeat("x", 700))
	}

	st := s.CacheStats()
	var charged, decoded int64
	s.mu.Lock()
	for el := s.lru.Front(); el != nil; el = el.Next() {
		n := el.Value.(*Node)
		charged += int64(len(n.Content))
		decoded += n.Msg.ApproxBytes()
	}
	nodes := s.lru.Len()
	s.mu.Unlock()

	c.Eq(charged, st.ChargedBytes, "charged recount")
	c.Eq(decoded, st.DecodedBytes, "decoded recount")
	c.Eq(nodes, st.Nodes, "node count")
	c.Eq(int64(4096), st.BudgetBytes, "budget")
	c.True(st.ChargedBytes <= st.BudgetBytes, "stays within budget")
	c.True(st.Evictions > 0, "a 12 x 700 byte conversation overflows a 4 KiB budget")
	c.True(st.DecodedBytes >= int64(nodes)*700, "decoded form is at least the text it holds")
}

func TestCacheStatsCountNodeAndPathHitsAndMisses(t *testing.T) {
	c := assert.NewAborting(t)
	s, _, convID := statsConv(t, Options{})
	h := s.Open(convID, AsOwner())
	appendText(t, h, "a")
	appendText(t, h, "b")

	before := s.CacheStats()
	_, err := h.Working(context.Background())
	c.NoError(err, "warm read")
	warm := s.CacheStats()
	c.Eq(before.PathHits+1, warm.PathHits, "a fully cached walk is one path hit")
	c.Eq(before.PathMisses, warm.PathMisses, "and no miss")
	c.True(warm.HeadServed > before.HeadServed, "the owner's warm read is served from the cached head")

	// A second Store over the same backend starts cold.
	cold := NewStore(s.backend, Options{})
	_, err = cold.Open(convID, AsOwner()).Working(context.Background())
	c.NoError(err, "cold read")
	cs := cold.CacheStats()
	c.True(cs.PathMisses >= 1, "cold walk falls back to the backend")
	c.True(cs.NodeMisses >= 2, "and every node it fetches was a miss")
	c.True(cs.Nodes >= 2, "which then populates the cache")

	_, err = cold.Open(convID, AsOwner()).Working(context.Background())
	c.NoError(err, "second cold-store read")
	c.True(cold.CacheStats().PathHits > cs.PathHits, "now cached")
}

func TestCacheStatsCountEntrySweepsAndInvalidations(t *testing.T) {
	c := assert.NewAborting(t)
	now := time.Unix(1_000_000, 0)
	s, _, convID := statsConv(t, Options{IdleTTL: time.Minute, Now: func() time.Time { return now }})
	h := s.Open(convID, AsOwner())
	appendText(t, h, "a")

	s.invalidate(convID)
	c.Eq(uint64(1), s.CacheStats().HeadInvalidations, "invalidate counted")

	now = now.Add(2 * time.Minute)
	s.Open("other-conversation")
	st := s.CacheStats()
	c.Eq(uint64(1), st.EntrySweeps, "the idle entry is swept on the next Open")
	c.Eq(0, st.Entries, "Open itself creates no entry")
}

func TestCacheStatsHeadInstallsAreCounted(t *testing.T) {
	c := assert.NewAborting(t)
	s, _, convID := statsConv(t, Options{})
	h := s.Open(convID, AsOwner())
	appendText(t, h, "a")
	c.True(s.CacheStats().HeadInstalls >= 1, "a commit's refresh installs the head")
}

// A walk that falls back to the backend resolves its nodes through the cache
// one ref at a time: the nodes still cached are hits and the evicted ones
// misses, which is the figure that says whether the budget fits the working set.
func TestCacheStatsCountPerNodeHitsAndMissesOfAPartiallyCachedWalk(t *testing.T) {
	c := assert.NewAborting(t)
	s, _, convID := statsConv(t, Options{CacheBytes: 4096})
	h := s.Open(convID, AsOwner())
	for range 12 {
		appendText(t, h, strings.Repeat("x", 700))
	}
	before := s.CacheStats()
	_, err := h.Working(context.Background())
	c.NoError(err, "read")
	after := s.CacheStats()
	c.True(after.PathMisses > before.PathMisses, "the oldest nodes were evicted, so the cached walk fails")
	c.True(after.NodeHits > before.NodeHits, "the recent nodes still cached are node hits")
	c.True(after.NodeMisses > before.NodeMisses, "the evicted ones are node misses")
}

func TestCachedNodeCountsAMissThenAHit(t *testing.T) {
	c := assert.NewAborting(t)
	s, _, convID := statsConv(t, Options{})
	h := s.Open(convID, AsOwner())
	appendText(t, h, "a")
	var id string
	s.mu.Lock()
	for k := range s.nodes {
		id = k
	}
	s.mu.Unlock()

	before := s.CacheStats()
	_, ok := s.cachedNode(convID, "no-such-node")
	c.False(ok, "unknown id")
	_, ok = s.cachedNode(convID, id)
	c.True(ok, "cached id")
	after := s.CacheStats()
	c.Eq(before.NodeMisses+1, after.NodeMisses, "one miss")
	c.Eq(before.NodeHits+1, after.NodeHits, "one hit")
}
