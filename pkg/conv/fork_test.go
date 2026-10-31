// SPDX-License-Identifier: Apache-2.0

package conv_test

import (
	"context"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/multigres/testkit/assert"

	"go.graveland.dev/rafiki/pkg/conv"
	"go.graveland.dev/rafiki/pkg/conv/convtest"
	"go.graveland.dev/rafiki/pkg/msg"
)

// seedForkChain appends one user message per text under the conversation's head
// and returns the nodes.
func seedForkChain(t *testing.T, s *conv.Store, convID string, texts ...string) []*conv.Node {
	t.Helper()
	c := assert.NewAborting(t)
	h := s.Open(convID, conv.AsOwner())
	var out []*conv.Node
	for _, text := range texts {
		res, err := h.Append(context.Background(), conv.Append{FromHead: true, Nodes: []conv.NewNode{{Msg: textMsg(msg.User, text)}}})
		c.NoError(err, "append %q", text)
		out = append(out, res.Nodes[0])
	}
	return out
}

// newForkEnv returns a mem backend holding a source conversation with a two-node
// chain, plus that conversation and its chain.
func newForkEnv(t *testing.T) (*conv.MemBackend, string, []*conv.Node) {
	t.Helper()
	m, src := newConv(t)
	chain := seedForkChain(t, conv.NewStore(m, conv.Options{}), src, "source one", "source two")
	return m, src, chain
}

// forkAt makes a fork of fromConvID at fromNodeID and returns its id.
func forkAt(t *testing.T, m *conv.MemBackend, fromConvID, fromNodeID string) string {
	t.Helper()
	id := uuid.Must(uuid.NewV7()).String()
	assert.NewAborting(t).NoError(m.EnsureFork(id, fromConvID, fromNodeID), "EnsureFork")
	return id
}

// A fork's borrowed segment is another conversation's, and its nodes are cached
// under that conversation: once the edges and the head are warm, an owner read
// of the fork asks the backend for nothing at all.
func TestWarmOwnerForkReadMakesNoBackendCall(t *testing.T) {
	c := assert.NewAborting(t)
	ctx := context.Background()
	m, src, chain := newForkEnv(t)
	forkID := forkAt(t, m, src, chain[1].ID)
	seedForkChain(t, conv.NewStore(m, conv.Options{}), forkID, "fork row")

	counting := convtest.NewCounting(m)
	h := conv.NewStore(counting, conv.Options{}).Open(forkID, conv.AsOwner())
	_, err := h.Snapshot(ctx)
	c.NoError(err, "cold Snapshot")

	counting.Reset()
	_, err = h.Snapshot(ctx)
	c.NoError(err, "warm Snapshot")
	counts := counting.Counts()
	c.Eq(0, counts["State"], "a warm owner fork read asks for no head")
	c.Eq(0, counts["Walk"], "and walks nothing")
	c.Eq(0, counts["Fetch"], "and fetches nothing")
}

// A handle that is not the owner still asks for the fork's own head on every
// read, and for nothing else: the borrowed segment is cached under the
// conversation it belongs to, and both edges are cached on their entries.
func TestWarmNonOwnerForkReadOnlyAsksForTheForkHead(t *testing.T) {
	c := assert.NewAborting(t)
	ctx := context.Background()
	m, src, chain := newForkEnv(t)
	forkID := forkAt(t, m, src, chain[1].ID)
	seedForkChain(t, conv.NewStore(m, conv.Options{}), forkID, "fork row")

	counting := convtest.NewCounting(m)
	s := conv.NewStore(counting, conv.Options{})
	_, err := s.Open(forkID).Snapshot(ctx)
	c.NoError(err, "cold Snapshot")

	counting.Reset()
	_, err = s.Open(forkID).Snapshot(ctx)
	c.NoError(err, "second Snapshot")
	c.EqDeep(map[string]int{"State": 1}, counting.Counts(), "only the fork's own head is asked for")
}

// stateRecorder records the conversations a State read was issued for.
type stateRecorder struct {
	conv.Backend
	mu    sync.Mutex
	convs []string
}

func (r *stateRecorder) State(ctx context.Context, convID string) (conv.State, error) {
	r.mu.Lock()
	r.convs = append(r.convs, convID)
	r.mu.Unlock()
	return r.Backend.State(ctx, convID)
}

// stateReads is how many State reads convID received.
func (r *stateRecorder) stateReads(convID string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, id := range r.convs {
		if id == convID {
			n++
		}
	}
	return n
}

// A fork's edge cannot change, so it is read once per entry however many reads
// cross it: a fork of a fork asks each source conversation for its edge once,
// while the head of the conversation being read is still asked for every time.
func TestEdgeIsReadOnceFromState(t *testing.T) {
	c := assert.NewAborting(t)
	ctx := context.Background()
	m, aID, a := newForkEnv(t)
	bID := forkAt(t, m, aID, a[1].ID)
	bChain := seedForkChain(t, conv.NewStore(m, conv.Options{}), bID, "b one")
	cID := forkAt(t, m, bID, bChain[0].ID)
	seedForkChain(t, conv.NewStore(m, conv.Options{}), cID, "c one")

	rec := &stateRecorder{Backend: m}
	s := conv.NewStore(rec, conv.Options{})
	for i := 0; i < 2; i++ {
		if _, err := s.Open(cID).Snapshot(ctx); err != nil {
			t.Fatalf("Snapshot %d: %v", i, err)
		}
	}
	c.Eq(1, rec.stateReads(aID), "A's edge was read once, by the first walk")
	c.Eq(1, rec.stateReads(bID), "B's edge was read once")
	c.Eq(2, rec.stateReads(cID), "C's own head is read on every non-owner read")
}
