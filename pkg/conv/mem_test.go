// SPDX-License-Identifier: Apache-2.0

package conv

import (
	"context"
	"strconv"
	"testing"

	"github.com/google/uuid"
	"github.com/multigres/testkit/assert"

	"go.graveland.dev/rafiki/pkg/convtree"
	"go.graveland.dev/rafiki/pkg/msg"
	msganthropic "go.graveland.dev/rafiki/pkg/msg/anthropic"
)

// seedMemChain appends one user message per text under the conversation's head.
func seedMemChain(t *testing.T, s *Store, convID string, texts ...string) []*Node {
	t.Helper()
	c := assert.NewAborting(t)
	h := s.Open(convID, AsOwner())
	var out []*Node
	for _, text := range texts {
		res, err := h.Append(context.Background(), Append{FromHead: true, Nodes: []NewNode{
			{Msg: msg.Message{Role: msg.User, Content: []msg.Block{msg.Text{Text: text}}}}}})
		c.NoError(err, "append %q", text)
		out = append(out, res.Nodes[0])
	}
	return out
}

// plantUndeclaredParentRow writes the one row Commit refuses — a row whose
// parent is another conversation's node that is not the conversation's fork-from
// — and makes it the conversation's head. It is the only way to observe a walk
// refusing an undeclared edge, because Commit will not write such a row.
func plantUndeclaredParentRow(t *testing.T, m *MemBackend, convID, parentID, content string) string {
	t.Helper()
	c := assert.NewAborting(t)
	body := []byte(strconv.Quote(content))
	hashes, err := convtree.Chain(convtree.RootSeed(convtree.RootKindRoot, nil),
		[]convtree.Msg{{Role: "user", Content: body}})
	c.NoError(err, "hash the row")
	message, err := msganthropic.Decode("user", body)
	c.NoError(err, "decode the row")

	m.mu.Lock()
	defer m.mu.Unlock()
	target, ok := m.convs[convID]
	c.True(ok, "conversation %s exists", convID)
	node := &Node{
		Ref: Ref{
			ID:             uuid.Must(uuid.NewV7()).String(),
			ConversationID: convID,
			ParentID:       parentID,
			Ordinal:        target.nextOrdinal,
			Depth:          1,
			PathHash:       hashes[0],
		},
		Msg:     message,
		Content: body,
	}
	target.nextOrdinal++
	target.nodes = append(target.nodes, node)
	target.byID[node.ID] = node
	target.byHash[node.PathHash] = node
	target.head = node.ID
	return node.ID
}

// TestMemWalkRefusesUndeclaredForeignParent: the walk ends at the declared
// fork-from node and nowhere else. A parent outside the conversation is reported
// rather than answered with the prefix that happened to be reachable.
func TestMemWalkRefusesUndeclaredForeignParent(t *testing.T) {
	c := assert.NewAborting(t)
	ctx := context.Background()
	m := NewMemBackend()
	s := NewStore(m, Options{})
	src := uuid.Must(uuid.NewV7()).String()
	m.Ensure(src)
	chain := seedMemChain(t, s, src, "source one", "source two")
	third := uuid.Must(uuid.NewV7()).String()
	m.Ensure(third)
	thirdChain := seedMemChain(t, s, third, "elsewhere")
	forkID := uuid.Must(uuid.NewV7()).String()
	c.NoError(m.EnsureFork(forkID, src, chain[1].ID), "EnsureFork")

	rowID := plantUndeclaredParentRow(t, m, forkID, thirdChain[0].ID, "undeclared parent")
	refs, err := m.Walk(ctx, forkID, rowID, false)
	c.ErrorIs(err, ErrNotFound, "a parent outside the conversation is not a silent truncation")
	c.Empty(refs, "and the walk answers no partial chain")
}

// TestUndeclaredForeignEdgeIsAnError: a conversation holding such a row fails
// the read rather than yielding a snapshot that silently begins somewhere else.
func TestUndeclaredForeignEdgeIsAnError(t *testing.T) {
	c := assert.NewAborting(t)
	ctx := context.Background()
	m := NewMemBackend()
	s := NewStore(m, Options{})
	src := uuid.Must(uuid.NewV7()).String()
	m.Ensure(src)
	chain := seedMemChain(t, s, src, "source one", "source two")
	third := uuid.Must(uuid.NewV7()).String()
	m.Ensure(third)
	thirdChain := seedMemChain(t, s, third, "elsewhere")
	forkID := uuid.Must(uuid.NewV7()).String()
	c.NoError(m.EnsureFork(forkID, src, chain[1].ID), "EnsureFork")
	plantUndeclaredParentRow(t, m, forkID, thirdChain[0].ID, "undeclared parent")

	sn, err := NewStore(m, Options{}).Open(forkID).Snapshot(ctx)
	c.ErrorIs(err, ErrNotFound, "an undeclared foreign parent fails the read")
	c.True(sn == nil, "and no partial snapshot is handed back")
}

// TestMemEnsureForkRefusals pins what EnsureFork will not do: it never replaces a
// conversation and never forks from a node its source does not hold.
func TestMemEnsureForkRefusals(t *testing.T) {
	c := assert.NewAborting(t)
	m := NewMemBackend()
	s := NewStore(m, Options{})
	src := uuid.Must(uuid.NewV7()).String()
	m.Ensure(src)
	chain := seedMemChain(t, s, src, "source one")

	forkID := uuid.Must(uuid.NewV7()).String()
	c.NoError(m.EnsureFork(forkID, src, chain[0].ID), "EnsureFork")
	c.Error(m.EnsureFork(forkID, src, chain[0].ID), "an existing conversation is never replaced")
	st, err := m.State(context.Background(), forkID)
	c.NoError(err, "State of the fork")
	c.Eq(chain[0].Ordinal+1, st.NextOrdinal, "the fork's ordinal counter follows the borrowed node")
	c.Eq(chain[0].ID, st.ForkFromID, "the fork records the node it borrows from")
	c.Eq(src, st.ForkFromConversationID, "and the conversation that node belongs to")

	other := uuid.Must(uuid.NewV7()).String()
	c.ErrorIs(m.EnsureFork(other, src, uuid.Must(uuid.NewV7()).String()), ErrNotFound,
		"a node the source does not hold is not a fork point")
}
