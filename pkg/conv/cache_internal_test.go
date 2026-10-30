// SPDX-License-Identifier: Apache-2.0

package conv

import (
	"context"
	"testing"

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
