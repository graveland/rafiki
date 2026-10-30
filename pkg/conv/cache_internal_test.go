// SPDX-License-Identifier: Apache-2.0

package conv

import (
	"context"
	"sync"
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

// racingState replaces the entry's head on every State call, standing in for a
// concurrent installer that wins each swap.
type racingState struct {
	Backend
	mu     sync.Mutex
	calls  int
	onCall func()
}

func (b *racingState) State(ctx context.Context, convID string) (State, error) {
	st, err := b.Backend.State(ctx, convID)
	b.mu.Lock()
	b.calls++
	hook := b.onCall
	b.mu.Unlock()
	if hook != nil {
		hook()
	}
	return st, err
}

func (b *racingState) reset() {
	b.mu.Lock()
	b.calls = 0
	b.mu.Unlock()
}

func (b *racingState) count() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.calls
}

// A refresh that keeps losing its install gives up after a bounded number of
// attempts and answers with the state it read, uninstalled; a write whose
// refresh did not install invalidates the head instead.
func TestRefreshIsBoundedUnderContendedInstalls(t *testing.T) {
	c := assert.NewAborting(t)
	b := NewMemBackend()
	convID := uuid.Must(uuid.NewV7()).String()
	b.Ensure(convID)
	racing := &racingState{Backend: b}
	s := NewStore(racing, Options{})
	text := msg.Message{Role: msg.User, Content: []msg.Block{msg.Text{Text: "a"}}}
	res, err := s.Open(convID, AsOwner()).Append(context.Background(), Append{FromHead: true, Nodes: []NewNode{{Msg: text}}})
	c.NoError(err, "append")

	racing.onCall = func() { s.invalidate(convID) }
	racing.reset()
	st, installed, err := s.refresh(context.Background(), convID)
	c.NoError(err, "refresh")
	c.False(installed, "every swap was lost")
	c.Eq(res.Nodes[0].ID, st.HeadID, "the state last read is still the right answer")
	c.Eq(maxRefreshAttempts, racing.count(), "and the loop stopped at its bound")

	racing.reset()
	path, err := s.Open(convID).Working(context.Background())
	c.NoError(err, "reader Working")
	c.Len(path, 1, "a reader that cannot win the swap still gets a correct answer")
	c.Eq(maxRefreshAttempts, racing.count(), "within the same bound")

	racing.onCall = nil
	_, installed, err = s.refresh(context.Background(), convID)
	c.NoError(err, "uncontended refresh")
	c.True(installed, "an uncontended refresh installs")
	c.True(s.touch(convID).head.Load().cached, "and caches the head")
	racing.onCall = func() { s.invalidate(convID) }
	s.refreshAfterCommit(context.Background(), convID)
	racing.onCall = nil
	racing.reset()
	_, err = s.Open(convID, AsOwner()).Working(context.Background())
	c.NoError(err, "owner Working")
	c.Eq(1, racing.count(), "a write whose refresh did not install leaves the next owner read to ask the backend")
}
