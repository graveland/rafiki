// SPDX-License-Identifier: Apache-2.0

// Package convtest is the conformance suite every conv.Backend must pass, plus
// a counting wrapper for tests that assert on backend traffic.
package convtest

import (
	"context"
	"errors"
	"sort"
	"strconv"
	"sync"
	"testing"

	"github.com/google/uuid"
	"github.com/multigres/testkit/assert"

	"go.graveland.dev/rafiki/pkg/conv"
	"go.graveland.dev/rafiki/pkg/convtree"
	"go.graveland.dev/rafiki/pkg/msg"
)

// RunBackendSuite exercises every Backend method through a conv.Store. The
// second return value of newBackend creates a conversation.
//
// The two conflict subtests call Commit directly: a head or path-hash conflict
// is a backend contract that a Store hides behind its retry loop, so it can
// only be observed at the interface.
func RunBackendSuite(t *testing.T, newBackend func(t *testing.T) (conv.Backend, func(convID string))) {
	t.Helper()

	// setup returns a backend and a freshly created conversation id.
	setup := func(t *testing.T) (conv.Backend, string) {
		t.Helper()
		b, create := newBackend(t)
		convID := uuid.Must(uuid.NewV7()).String()
		create(convID)
		return b, convID
	}

	// setupTwo returns a backend and two conversations, for the cases where a
	// backend must refuse something that belongs to the other one.
	setupTwo := func(t *testing.T) (conv.Backend, string, string) {
		t.Helper()
		b, create := newBackend(t)
		mine := uuid.Must(uuid.NewV7()).String()
		other := uuid.Must(uuid.NewV7()).String()
		create(mine)
		create(other)
		return b, mine, other
	}

	text := func(role msg.Role, s string) msg.Message {
		return msg.Message{Role: role, Content: []msg.Block{msg.Text{Text: s}}}
	}

	t.Run("EmptyState", func(t *testing.T) {
		c := assert.NewAborting(t)
		b, convID := setup(t)
		h := conv.NewStore(b, conv.Options{}).Open(convID, conv.AsOwner())

		st, err := h.State(context.Background())
		c.NoError(err, "State")
		c.Eq("", st.HeadID, "an empty conversation has no head")
		c.Eq(0, st.NextOrdinal, "an empty conversation's next ordinal is 0")
		c.False(st.ClearPending, "an empty conversation has no pending clear")

		path, err := h.Working(context.Background())
		c.NoError(err, "Working")
		c.Empty(path, "an empty conversation has an empty working path")
	})

	t.Run("AppendChainsUnderHead", func(t *testing.T) {
		c := assert.NewAborting(t)
		b, convID := setup(t)
		ctx := context.Background()
		h := conv.NewStore(b, conv.Options{}).Open(convID, conv.AsOwner())

		first, err := h.Append(ctx, conv.Append{FromHead: true, Nodes: []conv.NewNode{{Msg: text(msg.User, "hello")}}})
		c.NoError(err, "first append")
		c.Eq(1, first.Inserted, "the first append inserts its node")
		c.Len(first.Nodes, 1, "one node")
		root := first.Nodes[0]
		c.Eq("", root.ParentID, "the first node of an empty conversation is a root")
		c.Eq(0, root.Ordinal, "root ordinal")
		c.Eq(0, root.Depth, "root depth")

		second, err := h.Append(ctx, conv.Append{FromHead: true, Nodes: []conv.NewNode{{Msg: text(msg.Assistant, "hi")}}})
		c.NoError(err, "second append")
		reply := second.Nodes[0]
		c.Eq(root.ID, reply.ParentID, "the second node chains under the first")
		c.Eq(1, reply.Ordinal, "second ordinal")
		c.Eq(1, reply.Depth, "second depth")

		st, err := h.State(ctx)
		c.NoError(err, "State")
		c.Eq(reply.ID, st.HeadID, "the head is the last node appended")
		c.Eq(2, st.NextOrdinal, "two rows inserted")

		path, err := h.Working(ctx)
		c.NoError(err, "Working")
		c.Len(path, 2, "working path")
		c.EqDeep([]string{root.ID, reply.ID}, []string{path[0].ID, path[1].ID}, "chronological")
	})

	t.Run("WalkCrossesRoots", func(t *testing.T) {
		c := assert.NewAborting(t)
		b, convID := setup(t)
		ctx := context.Background()
		store := conv.NewStore(b, conv.Options{})
		h := store.Open(convID, conv.AsOwner())

		root, err := h.Append(ctx, conv.Append{FromHead: true, Nodes: []conv.NewNode{{Msg: text(msg.User, "hello")}}})
		c.NoError(err, "root append")
		tail, err := h.Append(ctx, conv.Append{FromHead: true, Nodes: []conv.NewNode{{Msg: text(msg.Assistant, "hi")}}})
		c.NoError(err, "tail append")
		boundary, err := h.Append(ctx, conv.Append{
			Root:          conv.KindCompactionSummary,
			DerivedFromID: tail.Nodes[0].ID,
			Nodes:         []conv.NewNode{{Msg: text(msg.User, "summary")}},
		})
		c.NoError(err, "compaction root")
		c.Eq(tail.Nodes[0].ID, boundary.Nodes[0].DerivedFromID, "derived from the tail")
		c.Eq("", boundary.Nodes[0].ParentID, "a compaction boundary is a root")
		c.Eq(conv.KindCompactionSummary, boundary.Nodes[0].Kind, "root kind")

		// A fresh store has an empty cache, so this read is a real Walk.
		fresh := conv.NewStore(b, conv.Options{}).Open(convID)
		sn, err := fresh.Snapshot(ctx)
		c.NoError(err, "Snapshot")
		c.EqDeep([]string{root.Nodes[0].ID, tail.Nodes[0].ID, boundary.Nodes[0].ID},
			historyIDs(sn), "history crosses the derived-from link")

		working, err := fresh.Working(ctx)
		c.NoError(err, "Working")
		c.Len(working, 1, "the working path stops at the compaction root")
		c.Eq(boundary.Nodes[0].ID, working[0].ID, "working path is the new root")
	})

	t.Run("WalkRejectsAForeignNode", func(t *testing.T) {
		c := assert.NewAborting(t)
		b, mine, other := setupTwo(t)
		ctx := context.Background()
		h := conv.NewStore(b, conv.Options{}).Open(mine, conv.AsOwner())
		res, err := h.Append(ctx, conv.Append{FromHead: true, Nodes: []conv.NewNode{{Msg: text(msg.User, "hello")}}})
		c.NoError(err, "append")

		// A node id from another conversation is not found: every read is
		// conversation-scoped, and a backend that answered with an empty walk
		// instead would hide a head pointing out of its conversation.
		_, err = b.Walk(ctx, other, res.Nodes[0].ID, true)
		c.ErrorIs(err, conv.ErrNotFound, "a node of another conversation is not found")
	})

	t.Run("FetchAndLookup", func(t *testing.T) {
		c := assert.NewAborting(t)
		b, convID := setup(t)
		ctx := context.Background()
		h := conv.NewStore(b, conv.Options{}).Open(convID, conv.AsOwner())
		res, err := h.Append(ctx, conv.Append{FromHead: true, Nodes: []conv.NewNode{{Msg: text(msg.User, "hello")}}})
		c.NoError(err, "append")
		node := res.Nodes[0]

		fresh := conv.NewStore(b, conv.Options{}).Open(convID)
		path, err := fresh.Working(ctx)
		c.NoError(err, "Working through a cold cache")
		c.Len(path, 1, "one node")

		// Content is each backend's own rendering of the stored JSON, so the
		// bytes cannot be compared: Postgres hands back what jsonb renders,
		// which sorts object keys and normalises whitespace. What must agree is
		// the canonical form — the thing a node is hashed and re-stored as.
		want, err := convtree.Canon(node.Msg.Role.String(), node.Content)
		c.NoError(err, "canonicalise the appended node's content")
		got, err := convtree.Canon(path[0].Msg.Role.String(), path[0].Content)
		c.NoError(err, "canonicalise the fetched node's content")
		c.EqDeep(want, got, "the fetched node is the appended one, up to the backend's rendering")

		refs, err := fresh.NodesByHash(ctx, []convtree.Hash{node.PathHash})
		c.NoError(err, "NodesByHash")
		ref, ok := refs[node.PathHash]
		c.True(ok, "the node's hash is held")
		c.Eq(node.ID, ref.ID, "ref id")
		c.Eq(node.Ordinal, ref.Ordinal, "ref ordinal")
	})

	t.Run("Roots", func(t *testing.T) {
		c := assert.NewAborting(t)
		b, convID := setup(t)
		ctx := context.Background()
		h := conv.NewStore(b, conv.Options{}).Open(convID, conv.AsOwner())
		first, err := h.Append(ctx, conv.Append{FromHead: true, Nodes: []conv.NewNode{{Msg: text(msg.User, "one")}}})
		c.NoError(err, "first root")
		second, err := h.Append(ctx, conv.Append{
			Root:          conv.KindClear,
			DerivedFromID: first.Nodes[0].ID,
			Nodes:         []conv.NewNode{{Msg: text(msg.User, "two")}},
		})
		c.NoError(err, "second root")

		roots, err := h.Roots(ctx)
		c.NoError(err, "Roots")
		c.Len(roots, 2, "two roots")
		c.EqDeep([]string{second.Nodes[0].ID, first.Nodes[0].ID}, nodeIDs(roots), "newest ordinal first")
	})

	t.Run("ByProviderMessageID", func(t *testing.T) {
		c := assert.NewAborting(t)
		b, convID := setup(t)
		ctx := context.Background()
		h := conv.NewStore(b, conv.Options{}).Open(convID, conv.AsOwner())
		res, err := h.Append(ctx, conv.Append{FromHead: true, Nodes: []conv.NewNode{{
			Msg:               text(msg.Assistant, "hi"),
			ProviderMessageID: "msg_conformance",
		}}})
		c.NoError(err, "append")
		c.Eq("msg_conformance", res.Nodes[0].ProviderMessageID, "the node carries the provider id")

		fresh := conv.NewStore(b, conv.Options{}).Open(convID)
		node, ok, err := fresh.NodeByProviderMessageID(ctx, "msg_conformance")
		c.NoError(err, "NodeByProviderMessageID")
		c.True(ok, "found")
		c.Eq(res.Nodes[0].ID, node.ID, "the node the provider id names")

		_, ok, err = fresh.NodeByProviderMessageID(ctx, "msg_absent")
		c.NoError(err, "absent lookup")
		c.False(ok, "an unknown provider id is not found")
	})

	t.Run("HasChild", func(t *testing.T) {
		c := assert.NewAborting(t)
		b, convID := setup(t)
		ctx := context.Background()
		counting := NewCounting(b)
		h := conv.NewStore(counting, conv.Options{}).Open(convID, conv.AsOwner())

		root, err := h.Append(ctx, conv.Append{FromHead: true, Nodes: []conv.NewNode{{Msg: text(msg.User, "hello")}}})
		c.NoError(err, "root")
		first, err := h.Append(ctx, conv.Append{
			ParentID: root.Nodes[0].ID,
			Nodes:    []conv.NewNode{{Msg: text(msg.Assistant, "a")}},
		})
		c.NoError(err, "first child")
		counting.Reset()
		second, err := h.Append(ctx, conv.Append{
			ParentID: root.Nodes[0].ID,
			Nodes:    []conv.NewNode{{Msg: text(msg.Assistant, "b")}},
		})
		c.NoError(err, "second child")
		c.NotEq(first.Nodes[0].ID, second.Nodes[0].ID, "a second child is a distinct node")
		c.Eq(1, counting.Counts()["HasChild"], "the fork check asked the backend")
	})

	t.Run("CommitRejectsStaleHead", func(t *testing.T) {
		c := assert.NewAborting(t)
		b, convID := setup(t)
		ctx := context.Background()
		h := conv.NewStore(b, conv.Options{}).Open(convID, conv.AsOwner())
		_, err := h.Append(ctx, conv.Append{FromHead: true, Nodes: []conv.NewNode{{Msg: text(msg.User, "hello")}}})
		c.NoError(err, "append")

		_, err = b.Commit(ctx, conv.Commit{ConversationID: convID, ExpectHead: "", ExpectNextOrdinal: 0})
		c.ErrorIs(err, conv.ErrConflict, "a stale head is a conflict")

		_, err = b.Commit(ctx, conv.Commit{ConversationID: convID, ExpectHead: "", ExpectNextOrdinal: 1})
		c.ErrorIs(err, conv.ErrConflict, "a stale next ordinal is a conflict")
	})

	t.Run("CommitRejectsDuplicateHash", func(t *testing.T) {
		c := assert.NewAborting(t)
		b, convID := setup(t)
		ctx := context.Background()
		h := conv.NewStore(b, conv.Options{}).Open(convID, conv.AsOwner())
		res, err := h.Append(ctx, conv.Append{FromHead: true, Nodes: []conv.NewNode{{Msg: text(msg.User, "hello")}}})
		c.NoError(err, "append")
		node := res.Nodes[0]

		dup := *node
		dup.ID = uuid.Must(uuid.NewV7()).String()
		st, err := h.State(ctx)
		c.NoError(err, "State")
		_, err = b.Commit(ctx, conv.Commit{
			ConversationID:    convID,
			ExpectHead:        st.HeadID,
			ExpectNextOrdinal: st.NextOrdinal,
			Rows:              []*conv.Node{&dup},
			NewHead:           dup.ID,
		})
		c.ErrorIs(err, conv.ErrConflict, "a duplicate path hash is a conflict")
	})

	t.Run("CommitRejectsAForeignParent", func(t *testing.T) {
		c := assert.NewAborting(t)
		b, mine, other := setupTwo(t)
		ctx := context.Background()
		h := conv.NewStore(b, conv.Options{}).Open(mine, conv.AsOwner())
		root, err := h.Append(ctx, conv.Append{FromHead: true, Nodes: []conv.NewNode{{Msg: text(msg.User, "hello")}}})
		c.NoError(err, "append in the first conversation")

		// Both rows hang off a node that lives in the OTHER conversation. A
		// row id is globally unique, so a foreign key would accept it; the
		// backend itself has to refuse it, or a write can splice one
		// conversation's tree into another.
		child := foreignRow(t, other, "hello", root.Nodes[0].ID, "")
		_, err = b.Commit(ctx, conv.Commit{
			ConversationID: other, Rows: []*conv.Node{child}, NewHead: child.ID,
		})
		c.ErrorIs(err, conv.ErrNotFound, "a parent from another conversation is not found")
		c.NotErrorIs(err, conv.ErrConflict, "and it is not retried as a conflict")

		derived := foreignRow(t, other, "summary", "", root.Nodes[0].ID)
		derived.Kind = conv.KindCompactionSummary
		_, err = b.Commit(ctx, conv.Commit{
			ConversationID: other, Rows: []*conv.Node{derived}, NewHead: derived.ID,
		})
		c.ErrorIs(err, conv.ErrNotFound, "a derived-from from another conversation is not found")

		st, err := b.State(ctx, other)
		c.NoError(err, "State of the refused conversation")
		c.Eq("", st.HeadID, "the refused commits left no head")
		c.Eq(0, st.NextOrdinal, "the refused commits allocated no ordinal")
	})

	t.Run("RetractMovesHead", func(t *testing.T) {
		c := assert.NewAborting(t)
		b, convID := setup(t)
		ctx := context.Background()
		h := conv.NewStore(b, conv.Options{}).Open(convID, conv.AsOwner())
		first, err := h.Append(ctx, conv.Append{FromHead: true, Nodes: []conv.NewNode{{Msg: text(msg.User, "one")}}})
		c.NoError(err, "first")
		second, err := h.Append(ctx, conv.Append{FromHead: true, Nodes: []conv.NewNode{{Msg: text(msg.User, "two")}}})
		c.NoError(err, "second")

		c.NoError(h.RetractHead(ctx, second.Nodes[0].ID), "retract the head user row")

		path, err := h.Working(ctx)
		c.NoError(err, "Working")
		c.Len(path, 1, "the retracted row is unreachable")
		c.Eq(first.Nodes[0].ID, path[0].ID, "the head moved back one")

		st, err := h.State(ctx)
		c.NoError(err, "State")
		c.Eq(2, st.NextOrdinal, "a retract allocates no ordinal")
	})

	t.Run("ConcurrentAppendsGetDisjointOrdinals", func(t *testing.T) {
		c := assert.NewAborting(t)
		b, convID := setup(t)
		ctx := context.Background()
		convs := conv.NewStore(b, conv.Options{})

		const writers = 2
		var wg sync.WaitGroup
		results := make([][]int, writers)
		errs := make([]error, writers)
		for i := range writers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				chain := []conv.NewNode{
					{Msg: text(msg.User, strconv.Itoa(i)+" a")},
					{Msg: text(msg.Assistant, strconv.Itoa(i)+" b")},
				}
				res, err := convs.Open(convID, conv.AsOwner()).Append(ctx, conv.Append{Root: convtree.RootKindRoot, Nodes: chain})
				errs[i] = err
				for _, n := range res.Nodes {
					results[i] = append(results[i], n.Ordinal)
				}
			}()
		}
		wg.Wait()

		var all []int
		for i := range writers {
			c.NoError(errs[i], "writer %d", i)
			all = append(all, results[i]...)
		}
		sort.Ints(all)
		c.EqDeep([]int{0, 1, 2, 3}, all, "two chains allocate four gap-free ordinals")
	})

	t.Run("ConcurrentSameChainLeavesOneCopy", func(t *testing.T) {
		c := assert.NewAborting(t)
		b, convID := setup(t)
		ctx := context.Background()
		convs := conv.NewStore(b, conv.Options{})

		chain := []conv.NewNode{{Msg: text(msg.User, "same")}, {Msg: text(msg.Assistant, "chain")}}
		const writers = 2
		var wg sync.WaitGroup
		ids := make([][]string, writers)
		errs := make([]error, writers)
		for i := range writers {
			wg.Add(1)
			go func() {
				defer wg.Done()
				res, err := convs.Open(convID, conv.AsOwner()).Append(ctx, conv.Append{Root: convtree.RootKindRoot, Nodes: chain})
				errs[i] = err
				for _, n := range res.Nodes {
					ids[i] = append(ids[i], n.ID)
				}
			}()
		}
		wg.Wait()

		for i := range writers {
			c.NoError(errs[i], "writer %d", i)
			c.EqDeep(ids[0], ids[i], "writer %d returned the same ids", i)
		}
		nodes, err := convs.Transcript(ctx, convID)
		c.NoError(err, "Transcript")
		c.Len(nodes, 2, "one copy of the chain")
	})

	t.Run("CompactionRootWithTail", func(t *testing.T) {
		c := assert.NewAborting(t)
		b, convID := setup(t)
		ctx := context.Background()
		h := conv.NewStore(b, conv.Options{}).Open(convID, conv.AsOwner())

		root, err := h.Append(ctx, conv.Append{FromHead: true, Nodes: []conv.NewNode{
			{Msg: text(msg.User, "a")}, {Msg: text(msg.Assistant, "b")},
		}})
		c.NoError(err, "root")
		replaced := int64(150)
		boundary, err := h.Append(ctx, conv.Append{
			Root:          conv.KindCompactionSummary,
			DerivedFromID: root.Nodes[1].ID,
			Nodes: []conv.NewNode{
				{Msg: text(msg.User, "summary"), InputTokens: &replaced},
				{Msg: text(msg.User, "a"), Kind: conv.KindCompactionTail},
				{Msg: text(msg.Assistant, "b"), Kind: conv.KindCompactionTail},
			},
		})
		c.NoError(err, "compaction")
		c.Len(boundary.Nodes, 3, "summary + two tail copies")
		c.Eq(conv.KindCompactionSummary, boundary.Nodes[0].Kind, "the summary is a root")
		c.Eq("", boundary.Nodes[0].ParentID, "the summary has no parent")
		c.Eq(root.Nodes[1].ID, boundary.Nodes[0].DerivedFromID, "the summary derives from the old head")
		c.Eq(conv.KindCompactionTail, boundary.Nodes[1].Kind, "tail copy kind")
		c.Eq(boundary.Nodes[0].ID, boundary.Nodes[1].ParentID, "the tail hangs under the summary")
		c.Require().NotNil(boundary.Nodes[0].InputTokens, "the summary carries the replaced size")
		c.Eq(replaced, *boundary.Nodes[0].InputTokens, "the replaced size")

		working, err := h.Working(ctx)
		c.NoError(err, "Working")
		c.Len(working, 3, "working set = summary + tail")
		c.Eq(conv.KindCompactionSummary, working[0].Kind, "the working path restarts at the summary")

		sn, err := h.Snapshot(ctx)
		c.NoError(err, "Snapshot")
		var kinds []string
		for n := range sn.Transcript() {
			kinds = append(kinds, n.Kind)
		}
		c.EqDeep([]string{"", "", conv.KindCompactionSummary}, kinds, "transcript: pre-compaction rows, then the summary; tails dropped")
	})

	t.Run("UnknownConversation", func(t *testing.T) {
		c := assert.NewAborting(t)
		b, _ := setup(t)
		h := conv.NewStore(b, conv.Options{}).Open(uuid.Must(uuid.NewV7()).String())
		_, err := h.State(context.Background())
		c.ErrorIs(err, conv.ErrNotFound, "an unknown conversation is ErrNotFound")
		c.False(errors.Is(err, conv.ErrConflict), "not a conflict")
	})
}

// foreignRow builds a node whose parent, or derived-from, names a node of
// ANOTHER conversation. Its path hash is a real one, so a backend that skipped
// the check would store the row rather than trip over the hash.
func foreignRow(t *testing.T, convID, content, parentID, derivedFromID string) *conv.Node {
	t.Helper()
	c := assert.NewAborting(t)
	body := []byte(strconv.Quote(content))
	hashes, err := convtree.Chain(convtree.RootSeed(convtree.RootKindRoot, nil),
		[]convtree.Msg{{Role: "user", Content: body}})
	c.NoError(err, "hash the foreign row")
	return &conv.Node{
		Ref: conv.Ref{
			ID:             uuid.Must(uuid.NewV7()).String(),
			ConversationID: convID,
			ParentID:       parentID,
			DerivedFromID:  derivedFromID,
			PathHash:       hashes[0],
		},
		Msg:     msg.Message{Role: msg.User, Content: []msg.Block{msg.Text{Text: content}}},
		Content: body,
	}
}

// historyIDs returns a snapshot's node ids in chronological order.
func historyIDs(sn *conv.Snapshot) []string {
	out := []string{}
	for n := range sn.History() {
		out = append(out, n.ID)
	}
	return out
}

// nodeIDs returns nodes' ids in the order given.
func nodeIDs(nodes []*conv.Node) []string {
	out := make([]string, len(nodes))
	for i, n := range nodes {
		out[i] = n.ID
	}
	return out
}
