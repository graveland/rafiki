// SPDX-License-Identifier: Apache-2.0

package convtest

import (
	"context"
	"strconv"
	"testing"

	"github.com/google/uuid"
	"github.com/multigres/testkit/assert"

	"go.graveland.dev/rafiki/pkg/conv"
	"go.graveland.dev/rafiki/pkg/convtree"
	"go.graveland.dev/rafiki/pkg/msg"
)

// UndeclaredParentPlanter is implemented by a test harness that can plant the
// one row no Commit accepts: a row whose parent is another conversation's node
// that is not the conversation's fork-from. The walk's refusal to follow an
// undeclared edge can only be observed on such a row, because Commit refuses to
// write one; the planted row is also the conversation's head, so a read reaches
// it.
type UndeclaredParentPlanter interface {
	PlantUndeclaredParentRow(convID, parentID, content string) (string, error)
}

// RunForkSuite exercises the fork edge through a conv.Store. create makes an
// ordinary conversation; fork makes convID a fork of fromNodeID (in fromConvID)
// with next_ordinal = that node's ordinal + 1.
func RunForkSuite(t *testing.T, newBackend func(t *testing.T) (b conv.Backend,
	create func(convID string), fork func(convID, fromConvID, fromNodeID string))) {
	t.Helper()

	text := func(role msg.Role, s string) msg.Message {
		return msg.Message{Role: role, Content: []msg.Block{msg.Text{Text: s}}}
	}

	// source creates a conversation holding a three-node chain.
	source := func(t *testing.T, b conv.Backend, create func(string)) (string, []*conv.Node) {
		t.Helper()
		c := assert.NewAborting(t)
		convID := uuid.Must(uuid.NewV7()).String()
		create(convID)
		res, err := conv.NewStore(b, conv.Options{}).Open(convID, conv.AsOwner()).Append(
			context.Background(), conv.Append{FromHead: true, Nodes: []conv.NewNode{
				{Msg: text(msg.User, "source one")},
				{Msg: text(msg.Assistant, "source two")},
				{Msg: text(msg.User, "source three")},
			}})
		c.NoError(err, "source chain")
		c.Require().Len(res.Nodes, 3, "three source nodes")
		return convID, res.Nodes
	}

	// appendChain appends one message per node under the conversation's head.
	appendChain := func(t *testing.T, b conv.Backend, convID string, nodes ...conv.NewNode) []*conv.Node {
		t.Helper()
		c := assert.NewAborting(t)
		res, err := conv.NewStore(b, conv.Options{}).Open(convID, conv.AsOwner()).Append(
			context.Background(), conv.Append{FromHead: true, Nodes: nodes})
		c.NoError(err, "append to %s", convID)
		return res.Nodes
	}

	t.Run("ForeignParentOtherThanForkFromRefused", func(t *testing.T) {
		c := assert.NewAborting(t)
		b, create, fork := newBackend(t)
		srcID, chain := source(t, b, create)
		_, third := source(t, b, create)
		forkID := uuid.Must(uuid.NewV7()).String()
		fork(forkID, srcID, chain[1].ID)

		row := forkCommitRow(t, forkID, "hangs off a third conversation", third[0].ID, "", 2, 2)
		err := commitDirectly(t, b, forkID, []*conv.Node{row}, row.ID)
		c.ErrorIs(err, conv.ErrNotFound, "only the declared fork-from node is a legal foreign parent")
		c.NotErrorIs(err, conv.ErrConflict, "and it is not retried as a conflict")
	})

	t.Run("ForkFromParentAccepted", func(t *testing.T) {
		c := assert.NewAborting(t)
		b, create, fork := newBackend(t)
		srcID, chain := source(t, b, create)
		forkID := uuid.Must(uuid.NewV7()).String()
		fork(forkID, srcID, chain[1].ID)

		nodes := appendChain(t, b, forkID, conv.NewNode{Msg: text(msg.Assistant, "forked reply")})
		c.Require().Len(nodes, 1, "one fork row")
		c.Eq(chain[1].ID, nodes[0].ParentID, "the first fork row hangs under the borrowed node")
		c.Eq(chain[1].Depth+1, nodes[0].Depth, "one level deeper than the borrowed node")
		c.Eq(chain[1].Ordinal+1, nodes[0].Ordinal, "the fork's first ordinal follows the borrowed node's")
	})

	t.Run("DerivedFromForkFromAccepted", func(t *testing.T) {
		c := assert.NewAborting(t)
		b, create, fork := newBackend(t)
		srcID, chain := source(t, b, create)
		forkID := uuid.Must(uuid.NewV7()).String()
		fork(forkID, srcID, chain[1].ID)

		res, err := conv.NewStore(b, conv.Options{}).Open(forkID, conv.AsOwner()).Append(
			context.Background(), conv.Append{
				Root:            convtree.RootKindRoot,
				DerivedFromHead: true,
				Nodes:           []conv.NewNode{{Msg: text(msg.User, "a fresh start")}},
			})
		c.NoError(err, "a root derived from the head of an empty fork")
		c.Require().Len(res.Nodes, 1, "one root")
		c.Eq(chain[1].ID, res.Nodes[0].DerivedFromID, "derived from the borrowed node")
		c.Eq("", res.Nodes[0].ParentID, "a root has no parent")
	})

	t.Run("WalkStopsAtForkFrom", func(t *testing.T) {
		c := assert.NewAborting(t)
		b, create, fork := newBackend(t)
		srcID, chain := source(t, b, create)
		forkID := uuid.Must(uuid.NewV7()).String()
		fork(forkID, srcID, chain[1].ID)
		own := appendChain(t, b, forkID,
			conv.NewNode{Msg: text(msg.Assistant, "one")},
			conv.NewNode{Msg: text(msg.User, "two")})

		refs, err := b.Walk(context.Background(), forkID, own[1].ID, false)
		c.NoError(err, "a walk that reaches the fork-from node ends there")
		c.EqDeep([]string{own[1].ID, own[0].ID}, refIDs(refs), "the fork's own refs, and no borrowed one")
	})

	t.Run("WalkRefusesUndeclaredForeignParent", func(t *testing.T) {
		c := assert.NewAborting(t)
		b, create, fork := newBackend(t)
		planter, ok := b.(UndeclaredParentPlanter)
		if !ok {
			t.Skip("backend cannot plant a row Commit refuses")
		}
		srcID, chain := source(t, b, create)
		_, third := source(t, b, create)
		forkID := uuid.Must(uuid.NewV7()).String()
		fork(forkID, srcID, chain[1].ID)

		rowID, err := planter.PlantUndeclaredParentRow(forkID, third[0].ID, "undeclared parent")
		c.NoError(err, "plant the row")
		refs, err := b.Walk(context.Background(), forkID, rowID, false)
		c.ErrorIs(err, conv.ErrNotFound, "a parent outside the conversation is not a silent truncation")
		c.Empty(refs, "and the walk answers no partial chain")
	})

	t.Run("ViewsCrossTheEdge", func(t *testing.T) {
		c := assert.NewAborting(t)
		ctx := context.Background()
		b, create, fork := newBackend(t)
		srcID, chain := source(t, b, create)
		forkID := uuid.Must(uuid.NewV7()).String()
		fork(forkID, srcID, chain[1].ID)
		own := appendChain(t, b, forkID,
			conv.NewNode{Msg: text(msg.Assistant, "fork reply one")},
			conv.NewNode{Msg: text(msg.User, "fork two")})

		h := conv.NewStore(b, conv.Options{}).Open(forkID)
		want := []string{chain[0].ID, chain[1].ID, own[0].ID, own[1].ID}
		sn, err := h.Snapshot(ctx)
		c.NoError(err, "Snapshot")
		c.EqDeep(want, historyIDs(sn), "History: the borrowed chain, then the fork's rows")
		c.EqDeep(want, transcriptIDs(sn), "Transcript crosses the edge")
		working, err := h.Working(ctx)
		c.NoError(err, "Working")
		c.EqDeep(want, nodeIDs(working), "Working crosses the edge")
		page, err := sn.Page(conv.PageQuery{})
		c.NoError(err, "Page")
		c.EqDeep(want, nodeIDs(page), "Page crosses the edge")
	})

	t.Run("EmptyForkReadsBorrowedSegment", func(t *testing.T) {
		c := assert.NewAborting(t)
		b, create, fork := newBackend(t)
		srcID, chain := source(t, b, create)
		forkID := uuid.Must(uuid.NewV7()).String()
		fork(forkID, srcID, chain[1].ID)

		working, err := conv.NewStore(b, conv.Options{}).Open(forkID).Working(context.Background())
		c.NoError(err, "Working of an empty fork")
		c.EqDeep([]string{chain[0].ID, chain[1].ID}, nodeIDs(working), "the borrowed segment, ending at the fork-from node")
	})

	t.Run("ForkOfForkCrossesTwoEdges", func(t *testing.T) {
		c := assert.NewAborting(t)
		b, create, fork := newBackend(t)
		aID, a := source(t, b, create)
		bID := uuid.Must(uuid.NewV7()).String()
		fork(bID, aID, a[1].ID)
		bOwn := appendChain(t, b, bID,
			conv.NewNode{Msg: text(msg.Assistant, "b one")},
			conv.NewNode{Msg: text(msg.User, "b two")})
		cID := uuid.Must(uuid.NewV7()).String()
		fork(cID, bID, bOwn[1].ID)
		cOwn := appendChain(t, b, cID, conv.NewNode{Msg: text(msg.Assistant, "c one")})

		sn, err := conv.NewStore(b, conv.Options{}).Open(cID).Snapshot(context.Background())
		c.NoError(err, "Snapshot")
		c.EqDeep([]string{a[0].ID, a[1].ID, bOwn[0].ID, bOwn[1].ID, cOwn[0].ID},
			historyIDs(sn), "A's prefix, then B's, then C's")
	})

	t.Run("PageAfterOrdinalCrossesMonotonically", func(t *testing.T) {
		c := assert.NewAborting(t)
		ctx := context.Background()
		b, create, fork := newBackend(t)
		srcID, chain := source(t, b, create)
		forkID := uuid.Must(uuid.NewV7()).String()
		fork(forkID, srcID, chain[1].ID)
		own := appendChain(t, b, forkID,
			conv.NewNode{Msg: text(msg.Assistant, "fork reply")},
			conv.NewNode{Msg: text(msg.User, "fork follow-up")})

		sn, err := conv.NewStore(b, conv.Options{}).Open(forkID).Snapshot(ctx)
		c.NoError(err, "Snapshot")
		after := chain[1].Ordinal - 1
		page, err := sn.Page(conv.PageQuery{AfterOrdinal: &after})
		c.NoError(err, "Page")
		c.EqDeep([]string{chain[1].ID, own[0].ID, own[1].ID}, nodeIDs(page), "the fork-from node, then the fork's rows")
		for i := 1; i < len(page); i++ {
			c.True(page[i].Ordinal > page[i-1].Ordinal, "ordinals strictly increase across the edge")
		}
	})

	t.Run("RetractFirstForkRowRestoresEmptyHead", func(t *testing.T) {
		c := assert.NewAborting(t)
		ctx := context.Background()
		b, create, fork := newBackend(t)
		srcID, chain := source(t, b, create)
		forkID := uuid.Must(uuid.NewV7()).String()
		fork(forkID, srcID, chain[1].ID)
		h := conv.NewStore(b, conv.Options{}).Open(forkID, conv.AsOwner())
		own := appendChain(t, b, forkID, conv.NewNode{Msg: text(msg.User, "the only fork row")})

		c.NoError(h.RetractHead(ctx, own[0].ID), "retract the fork's lone user row")
		st, err := h.State(ctx)
		c.NoError(err, "State")
		c.Eq("", st.HeadID, "the head is empty again, not the borrowed node")
		working, err := h.Working(ctx)
		c.NoError(err, "Working")
		c.EqDeep([]string{chain[0].ID, chain[1].ID}, nodeIDs(working), "back to the borrowed segment")
	})

	t.Run("RequestAtForkFromHead", func(t *testing.T) {
		c := assert.NewAborting(t)
		b, create, fork := newBackend(t)
		srcID, chain := source(t, b, create)
		forkID := uuid.Must(uuid.NewV7()).String()
		fork(forkID, srcID, chain[1].ID)

		path, err := conv.NewStore(b, conv.Options{}).Open(forkID).RequestAt(context.Background(), chain[1].ID, nil, nil)
		c.NoError(err, "RequestAt the borrowed node on a fork")
		c.EqDeep([]string{chain[0].ID, chain[1].ID}, nodeIDs(path), "the source chain, not a foreign miss")
	})
}

// forkCommitRow builds a row the way a Commit carries one. Its path hash is a
// real one for a root, so a backend that skipped its parent check would store
// the row rather than trip over the hash.
func forkCommitRow(t *testing.T, convID, content, parentID, derivedFromID string, ordinal, depth int) *conv.Node {
	t.Helper()
	c := assert.NewAborting(t)
	body := []byte(strconv.Quote(content))
	hashes, err := convtree.Chain(convtree.RootSeed(convtree.RootKindRoot, nil),
		[]convtree.Msg{{Role: "user", Content: body}})
	c.NoError(err, "hash the row")
	return &conv.Node{
		Ref: conv.Ref{
			ID:             uuid.Must(uuid.NewV7()).String(),
			ConversationID: convID,
			ParentID:       parentID,
			DerivedFromID:  derivedFromID,
			Ordinal:        ordinal,
			Depth:          depth,
			PathHash:       hashes[0],
		},
		Msg:     msg.Message{Role: msg.User, Content: []msg.Block{msg.Text{Text: content}}},
		Content: body,
	}
}

// commitDirectly commits rows into convID against the state the backend reports.
func commitDirectly(t *testing.T, b conv.Backend, convID string, rows []*conv.Node, newHead string) error {
	t.Helper()
	c := assert.NewAborting(t)
	ctx := context.Background()
	st, err := b.State(ctx, convID)
	c.NoError(err, "State of %s", convID)
	_, err = b.Commit(ctx, conv.Commit{
		ConversationID:    convID,
		ExpectHead:        st.HeadID,
		ExpectNextOrdinal: st.NextOrdinal,
		Rows:              rows,
		NewHead:           newHead,
	})
	return err
}

// refIDs returns refs' ids in the order given.
func refIDs(refs []conv.Ref) []string {
	out := make([]string, len(refs))
	for i, r := range refs {
		out[i] = r.ID
	}
	return out
}

// transcriptIDs returns a snapshot's transcript ids, chronological.
func transcriptIDs(sn *conv.Snapshot) []string {
	out := []string{}
	for n := range sn.Transcript() {
		out = append(out, n.ID)
	}
	return out
}
