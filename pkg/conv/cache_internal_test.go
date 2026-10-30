// SPDX-License-Identifier: Apache-2.0

package conv

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/multigres/testkit/assert"

	"go.graveland.dev/rafiki/pkg/msg"
)

// A read walks the chain head-first but must leave the head hottest: eviction
// has to drop the oldest part of the history, not the node every read starts
// at.
func TestCacheKeepsTheHeadHottest(t *testing.T) {
	c := assert.NewAborting(t)
	b := NewMemBackend()
	convID := uuid.Must(uuid.NewV7()).String()
	b.Ensure(convID)
	s := NewStore(b, Options{})
	h := s.Open(convID, AsOwner())

	text := func(s string) msg.Message {
		return msg.Message{Role: msg.User, Content: []msg.Block{msg.Text{Text: s}}}
	}
	root, err := h.Append(context.Background(), Append{FromHead: true, Nodes: []NewNode{{Msg: text("a")}}})
	c.NoError(err, "a")
	head, err := h.Append(context.Background(), Append{FromHead: true, Nodes: []NewNode{{Msg: text("b")}}})
	c.NoError(err, "b")

	_, err = h.Working(context.Background())
	c.NoError(err, "Working")

	s.mu.Lock()
	front := s.lru.Front().Value.(*Node).ID
	back := s.lru.Back().Value.(*Node).ID
	s.mu.Unlock()
	c.Eq(head.Nodes[0].ID, front, "the head is the hottest node")
	c.Eq(root.Nodes[0].ID, back, "the root is the coldest")
}

// A read that observed the head before a sweep must not install it into the
// entry that replaced the swept one, even when the replacement's generation has
// caught up to the one the read captured: entry identity, not only the
// generation, orders a read against a publish.
func TestReaderStateFromASweptEntryIsDropped(t *testing.T) {
	c := assert.NewAborting(t)
	mem := NewMemBackend()
	convID := uuid.Must(uuid.NewV7()).String()
	mem.Ensure(convID)
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	s := NewStore(mem, Options{Now: func() time.Time { return now }, IdleTTL: time.Minute})

	owner := s.Open(convID, AsOwner())
	root, err := owner.Append(context.Background(), Append{FromHead: true, Nodes: []NewNode{{
		Msg: msg.Message{Role: msg.User, Content: []msg.Block{msg.Text{Text: "a"}}},
	}}})
	c.NoError(err, "append")

	// A reader captures the entry and its generation before asking the backend,
	// exactly as Handle.State does.
	s.mu.Lock()
	swept := s.entries[convID]
	s.mu.Unlock()
	gen, live := s.generation(convID, swept)
	c.True(live, "the entry is live when the read captures its generation")

	// The entry idles past the TTL and is swept. The replacement's generation is
	// then driven up to the captured one, so only the entry's identity stands
	// between the stalled read and the replacement's head.
	now = now.Add(2 * time.Minute)
	replacement := s.touch(convID, true, "")
	c.True(replacement != swept, "the sweep left a replacement entry")
	fresh := headState{id: root.Nodes[0].ID, nextOrdinal: 1}
	for i := uint64(0); i < gen; i++ {
		s.storeHeadWrite(convID, replacement, fresh)
	}
	s.mu.Lock()
	caughtUp := replacement.gen == gen
	s.mu.Unlock()
	c.True(caughtUp, "the replacement's generation caught up to the read's")

	// The read's observation lands now: it belongs to an entry that is gone.
	s.storeHeadRead(convID, swept, gen, headState{id: "stale", nextOrdinal: 7})

	s.mu.Lock()
	head := replacement.head.Load()
	current := s.entries[convID]
	s.mu.Unlock()
	c.Eq(replacement, current, "the swept entry was not installed back")
	c.Require().NotNil(head, "the replacement has a head")
	c.Eq(root.Nodes[0].ID, head.id, "the replacement's head was not overwritten")
	c.Eq(1, head.nextOrdinal, "nor its next ordinal")
}

// A re-fetched older node must not displace the newest one in the provider
// index, and an evicted node's id must go with it while its ordinal stays: the
// mark is what keeps a re-fetched older duplicate from becoming the answer.
func TestProviderIndexPrefersTheNewestAndForgetsAnEvictedNode(t *testing.T) {
	c := assert.NewAborting(t)
	s := NewStore(NewMemBackend(), Options{})
	const convID = "conv"
	s.touch(convID, false, "")

	newer := &Node{Ref: Ref{ID: "newer", ConversationID: convID, Ordinal: 5}, Content: []byte("x"), ProviderMessageID: "msg_1"}
	older := &Node{Ref: Ref{ID: "older", ConversationID: convID, Ordinal: 2}, Content: []byte("x"), ProviderMessageID: "msg_1"}
	s.putNodes(convID, newer)
	s.putNodes(convID, older)

	s.mu.Lock()
	ref, ok := s.entries[convID].byProvider["msg_1"]
	s.mu.Unlock()
	c.True(ok, "indexed")
	c.Eq("newer", ref.id, "the newest ordinal wins")

	s.mu.Lock()
	for s.lru.Len() > 0 {
		s.evictLocked()
	}
	ref, ok = s.entries[convID].byProvider["msg_1"]
	s.mu.Unlock()
	c.True(ok, "the id stays indexed")
	c.Eq("", ref.id, "but no longer names an evicted node")
	c.Eq(5, ref.ordinal, "the ordinal it remembers is the newest it has seen")
}
