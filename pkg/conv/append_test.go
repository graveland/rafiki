// SPDX-License-Identifier: Apache-2.0

package conv_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/multigres/testkit/assert"

	"go.graveland.dev/rafiki/pkg/conv"
	"go.graveland.dev/rafiki/pkg/convtree"
	"go.graveland.dev/rafiki/pkg/msg"
	msganthropic "go.graveland.dev/rafiki/pkg/msg/anthropic"
	"go.graveland.dev/rafiki/pkg/slashcmd"
)

// Each case here is one rule of the append validation, so deleting a rule
// turns exactly one of them red.
func TestAppendValidateParentMode(t *testing.T) {
	c := assert.NewAborting(t)
	b, convID := newConv(t)
	h := conv.NewStore(b, conv.Options{}).Open(convID)
	nodes := []conv.NewNode{{Msg: textMsg(msg.User, "hi")}}

	_, err := h.Append(context.Background(), conv.Append{Nodes: nodes})
	c.ErrorContains(err, "conv: append: exactly one of ParentID, FromHead or Root is required", "no parent mode")

	_, err = h.Append(context.Background(), conv.Append{
		FromHead: true,
		Root:     convtree.RootKindRoot,
		Nodes:    nodes,
	})
	c.ErrorContains(err, "conv: append: exactly one of ParentID, FromHead or Root is required", "two parent modes")
}

func TestAppendValidateNoNodes(t *testing.T) {
	c := assert.NewAborting(t)
	b, convID := newConv(t)
	h := conv.NewStore(b, conv.Options{}).Open(convID)

	_, err := h.Append(context.Background(), conv.Append{FromHead: true})
	c.ErrorContains(err, "conv: append: no nodes to append", "from-head append with no nodes")

	_, err = h.Append(context.Background(), conv.Append{Root: convtree.RootKindClear})
	c.ErrorContains(err, "conv: append: no nodes to append", "root append with no nodes")
}

func TestAppendValidateRootKind(t *testing.T) {
	c := assert.NewAborting(t)
	b, convID := newConv(t)
	h := conv.NewStore(b, conv.Options{}).Open(convID)

	_, err := h.Append(context.Background(), conv.Append{
		Root:  "bogus",
		Nodes: []conv.NewNode{{Msg: textMsg(msg.User, "hi")}},
	})
	c.ErrorContains(err, `conv: append: unknown root kind "bogus"`, "unknown root kind")
}

func TestAppendValidateDerivedRequiresRoot(t *testing.T) {
	c := assert.NewAborting(t)
	b, convID := newConv(t)
	h := conv.NewStore(b, conv.Options{}).Open(convID)

	_, err := h.Append(context.Background(), conv.Append{
		ParentID:      "some-node",
		DerivedFromID: "another-node",
		Nodes:         []conv.NewNode{{Msg: textMsg(msg.User, "hi")}},
	})
	c.ErrorContains(err, "conv: append: derived-from requires a root", "derived-from without a root")
}

func TestAppendValidateDerivedBoth(t *testing.T) {
	c := assert.NewAborting(t)
	b, convID := newConv(t)
	h := conv.NewStore(b, conv.Options{}).Open(convID)

	_, err := h.Append(context.Background(), conv.Append{
		Root:            convtree.RootKindClear,
		DerivedFromID:   "some-node",
		DerivedFromHead: true,
		Nodes:           []conv.NewNode{{Msg: textMsg(msg.User, "hi")}},
	})
	c.ErrorContains(err, "conv: append: derived-from is DerivedFromID or DerivedFromHead, not both", "both derived-from forms")
}

func TestAppendDedupsExistingPrefix(t *testing.T) {
	c := assert.NewAborting(t)
	b, convID := newConv(t)
	ctx := context.Background()
	h := conv.NewStore(b, conv.Options{}).Open(convID, conv.AsOwner())

	root, err := h.Append(ctx, conv.Append{FromHead: true, Nodes: []conv.NewNode{{Msg: textMsg(msg.User, "one")}}})
	c.NoError(err, "root")
	child, err := h.Append(ctx, conv.Append{FromHead: true, Nodes: []conv.NewNode{{Msg: textMsg(msg.Assistant, "two")}}})
	c.NoError(err, "child")

	again, err := h.Append(ctx, conv.Append{
		Root: convtree.RootKindRoot,
		Nodes: []conv.NewNode{
			{Msg: textMsg(msg.User, "one")},
			{Msg: textMsg(msg.Assistant, "two")},
		},
	})
	c.NoError(err, "replay")
	c.Eq(0, again.Inserted, "a replayed chain inserts nothing")
	c.EqDeep([]string{root.Nodes[0].ID, child.Nodes[0].ID}, nodeIDs(again.Nodes), "the existing rows are returned")

	st, err := h.State(ctx)
	c.NoError(err, "State")
	c.Eq(child.Nodes[0].ID, st.HeadID, "the head still ends on the replayed chain")
	c.Eq(2, st.NextOrdinal, "a replay allocates no ordinal")
}

func TestAppendRefusesGap(t *testing.T) {
	c := assert.NewAborting(t)
	b, convID := newConv(t)
	ctx := context.Background()
	h := conv.NewStore(b, conv.Options{}).Open(convID, conv.AsOwner())

	// The chain the append will ask for, and the hash of its LAST element: the
	// stored tree will hold that one without the middle one, which is the shape
	// a hand-written row leaves behind.
	seed := convtree.RootSeed(convtree.RootKindRoot, nil)
	var chain []convtree.Msg
	for _, text := range []string{"one", "two", "three"} {
		content, err := msganthropic.EncodeContent(textMsg(msg.User, text))
		c.NoError(err, "encode %s", text)
		chain = append(chain, convtree.Msg{Role: "user", Content: content})
	}
	hashes, err := convtree.Chain(seed, chain)
	c.NoError(err, "hash chain")

	root, err := h.Append(ctx, conv.Append{FromHead: true, Nodes: []conv.NewNode{{Msg: textMsg(msg.User, "one")}}})
	c.NoError(err, "root")
	orphan := storedNode(t, convID, root.Nodes[0].ID, root.Nodes[0].PathHash, msg.User, "three", 1, 1)
	orphan.PathHash = hashes[2]
	_, err = b.Commit(ctx, conv.Commit{
		ConversationID:    convID,
		ExpectHead:        root.Nodes[0].ID,
		ExpectNextOrdinal: 1,
		Rows:              []*conv.Node{orphan},
		NewHead:           orphan.ID,
	})
	c.NoError(err, "hand insert the later node")

	_, err = h.Append(ctx, conv.Append{
		Root: convtree.RootKindRoot,
		Nodes: []conv.NewNode{
			{Msg: textMsg(msg.User, "one")},
			{Msg: textMsg(msg.User, "two")},
			{Msg: textMsg(msg.User, "three")},
		},
	})
	c.ErrorContains(err, "conv: append: tree invariant broken: node 2 exists but node 1 does not", "gap")
}

func TestAppendFromHeadOnEmptyStartsRoot(t *testing.T) {
	c := assert.NewAborting(t)
	b, convID := newConv(t)
	ctx := context.Background()
	h := conv.NewStore(b, conv.Options{}).Open(convID, conv.AsOwner())

	res, err := h.Append(ctx, conv.Append{FromHead: true, Nodes: []conv.NewNode{{Msg: textMsg(msg.User, "hi")}}})
	c.NoError(err, "append")
	node := res.Nodes[0]
	c.Eq("", node.ParentID, "no parent")
	c.Eq("", node.Kind, "an ordinary root carries no kind")
	c.Eq(0, node.Ordinal, "first ordinal")
	c.Eq(0, node.Depth, "first depth")

	content, err := msganthropic.EncodeContent(textMsg(msg.User, "hi"))
	c.NoError(err, "encode")
	want, err := convtree.Chain(convtree.RootSeed(convtree.RootKindRoot, nil),
		[]convtree.Msg{{Role: "user", Content: content}})
	c.NoError(err, "hash")
	c.EqDeep(want[0], node.PathHash, "the root hashes from the root seed")
}

func TestAppendRootDerivedFromHead(t *testing.T) {
	c := assert.NewAborting(t)
	b, convID := newConv(t)
	ctx := context.Background()
	h := conv.NewStore(b, conv.Options{}).Open(convID, conv.AsOwner())

	first, err := h.Append(ctx, conv.Append{FromHead: true, Nodes: []conv.NewNode{{Msg: textMsg(msg.User, "hi")}}})
	c.NoError(err, "first")
	boundary, err := h.Append(ctx, conv.Append{
		Root:            conv.KindCompactionSummary,
		DerivedFromHead: true,
		Nodes:           []conv.NewNode{{Msg: textMsg(msg.User, "summary")}},
	})
	c.NoError(err, "compaction root")

	node := boundary.Nodes[0]
	c.Eq("", node.ParentID, "a derived root has no parent")
	c.Eq(first.Nodes[0].ID, node.DerivedFromID, "derived from the head")
	c.Eq(conv.KindCompactionSummary, node.Kind, "the root kind is recorded on the boundary row")

	content, err := msganthropic.EncodeContent(textMsg(msg.User, "summary"))
	c.NoError(err, "encode")
	seed := convtree.RootSeed(convtree.RootKindCompaction, &first.Nodes[0].PathHash)
	want, err := convtree.Chain(seed, []convtree.Msg{{Role: "user", Content: content}})
	c.NoError(err, "hash")
	c.EqDeep(want[0], node.PathHash, "a compaction root hashes from its seed and derived-from")
}

func TestTwoClearsAreDistinctRoots(t *testing.T) {
	c := assert.NewAborting(t)
	b, convID := newConv(t)
	ctx := context.Background()
	h := conv.NewStore(b, conv.Options{}).Open(convID, conv.AsOwner())

	first, err := h.Append(ctx, conv.Append{FromHead: true, Nodes: []conv.NewNode{{Msg: textMsg(msg.User, "one")}}})
	c.NoError(err, "first")
	second, err := h.Append(ctx, conv.Append{FromHead: true, Nodes: []conv.NewNode{{Msg: textMsg(msg.Assistant, "two")}}})
	c.NoError(err, "second")

	fromFirst, err := h.Append(ctx, conv.Append{
		Root:          convtree.RootKindClear,
		DerivedFromID: first.Nodes[0].ID,
		Nodes:         []conv.NewNode{{Msg: textMsg(msg.User, "cleared")}},
	})
	c.NoError(err, "clear from the first node")
	fromSecond, err := h.Append(ctx, conv.Append{
		Root:          convtree.RootKindClear,
		DerivedFromID: second.Nodes[0].ID,
		Nodes:         []conv.NewNode{{Msg: textMsg(msg.User, "cleared")}},
	})
	c.NoError(err, "clear from the second node")
	c.NotEq(fromFirst.Nodes[0].ID, fromSecond.Nodes[0].ID, "clears from different nodes are different roots")
	c.NotEq(fromFirst.Nodes[0].PathHash, fromSecond.Nodes[0].PathHash, "their hashes differ too")

	replay, err := h.Append(ctx, conv.Append{
		Root:          convtree.RootKindClear,
		DerivedFromID: first.Nodes[0].ID,
		Nodes:         []conv.NewNode{{Msg: textMsg(msg.User, "cleared")}},
	})
	c.NoError(err, "replay")
	c.Eq(0, replay.Inserted, "a clear from the same node is the same root")
	c.Eq(fromFirst.Nodes[0].ID, replay.Nodes[0].ID, "the existing clear")
}

func TestAppendCountsFork(t *testing.T) {
	c := assert.NewAborting(t)
	b, convID := newConv(t)
	ctx := context.Background()
	rec := &commitRecorder{MemBackend: b}
	h := conv.NewStore(rec, conv.Options{}).Open(convID, conv.AsOwner())

	root, err := h.Append(ctx, conv.Append{FromHead: true, Nodes: []conv.NewNode{{Msg: textMsg(msg.User, "root")}}})
	c.NoError(err, "root")
	_, err = h.Append(ctx, conv.Append{
		ParentID: root.Nodes[0].ID,
		Nodes:    []conv.NewNode{{Msg: textMsg(msg.Assistant, "a")}},
	})
	c.NoError(err, "first child")
	c.Eq(0, rec.last().Forks, "the first child of a node is not a fork")

	_, err = h.Append(ctx, conv.Append{
		ParentID: root.Nodes[0].ID,
		Nodes:    []conv.NewNode{{Msg: textMsg(msg.Assistant, "b")}},
	})
	c.NoError(err, "second child")
	c.Eq(1, rec.last().Forks, "a second child of a node is a fork")
}

func TestAppendEmptyNodesMovesHead(t *testing.T) {
	c := assert.NewAborting(t)
	b, convID := newConv(t)
	ctx := context.Background()
	h := conv.NewStore(b, conv.Options{}).Open(convID, conv.AsOwner())

	root, err := h.Append(ctx, conv.Append{FromHead: true, Nodes: []conv.NewNode{{Msg: textMsg(msg.User, "one")}}})
	c.NoError(err, "root")
	_, err = h.Append(ctx, conv.Append{FromHead: true, Nodes: []conv.NewNode{{Msg: textMsg(msg.Assistant, "two")}}})
	c.NoError(err, "child")

	moved, err := h.Append(ctx, conv.Append{ParentID: root.Nodes[0].ID})
	c.NoError(err, "head move")
	c.Eq(0, moved.Inserted, "no rows inserted")
	c.Empty(moved.Nodes, "no rows returned")

	st, err := h.State(ctx)
	c.NoError(err, "State")
	c.Eq(root.Nodes[0].ID, st.HeadID, "the head moved to the named node")

	path, err := h.Working(ctx)
	c.NoError(err, "Working")
	c.EqDeep([]string{root.Nodes[0].ID}, nodeIDs(path), "the working path shrank")
}

func TestAppendOrdinalsFollowNextOrdinal(t *testing.T) {
	c := assert.NewAborting(t)
	b, convID := newConv(t)
	ctx := context.Background()
	h := conv.NewStore(b, conv.Options{}).Open(convID, conv.AsOwner())

	one, err := h.Append(ctx, conv.Append{FromHead: true, Nodes: []conv.NewNode{{Msg: textMsg(msg.User, "one")}}})
	c.NoError(err, "one")
	two, err := h.Append(ctx, conv.Append{FromHead: true, Nodes: []conv.NewNode{{Msg: textMsg(msg.Assistant, "two")}}})
	c.NoError(err, "two")
	three, err := h.Append(ctx, conv.Append{FromHead: true, Nodes: []conv.NewNode{{Msg: textMsg(msg.User, "three")}}})
	c.NoError(err, "three")
	c.EqDeep([]int{0, 1, 2}, []int{one.Nodes[0].Ordinal, two.Nodes[0].Ordinal, three.Nodes[0].Ordinal},
		"ordinals follow insertion order")

	c.NoError(h.RetractHead(ctx, three.Nodes[0].ID), "retract")
	retry, err := h.Append(ctx, conv.Append{FromHead: true, Nodes: []conv.NewNode{{Msg: textMsg(msg.User, "three again")}}})
	c.NoError(err, "retry")
	c.Eq(3, retry.Nodes[0].Ordinal, "a retracted ordinal is never reused")
	c.Eq(two.Nodes[0].ID, retry.Nodes[0].ParentID, "the retry hangs where the retracted row did")
}

func TestAppendHashesStorableContent(t *testing.T) {
	c := assert.NewAborting(t)
	b, convID := newConv(t)
	ctx := context.Background()
	h := conv.NewStore(b, conv.Options{}).Open(convID, conv.AsOwner())

	withNUL := []byte(`[{"type":"text","text":"a\u0000b"}]`)
	res, err := h.Append(ctx, conv.Append{FromHead: true, Nodes: []conv.NewNode{{
		Msg:     textMsg(msg.User, "ab"),
		Content: withNUL,
	}}})
	c.NoError(err, "append")
	node := res.Nodes[0]

	c.NotStrContains(string(node.Content), `\u0000`, "U+0000 is stripped from the stored content")
	c.StrContains(string(node.Content), `"text":"ab"`, "the text survives the strip")

	hashes, err := convtree.Chain(convtree.RootSeed(convtree.RootKindRoot, nil),
		[]convtree.Msg{{Role: "user", Content: node.Content}})
	c.NoError(err, "hash")
	c.EqDeep(hashes[0], node.PathHash, "the node is hashed as it is stored")

	// The same message written WITHOUT the NUL is the same node, which is what
	// makes the strip a hashing rule rather than a cosmetic one.
	replay, err := h.Append(ctx, conv.Append{
		Root:  convtree.RootKindRoot,
		Nodes: []conv.NewNode{{Msg: textMsg(msg.User, "ab"), Content: []byte(`[{"type":"text","text":"ab"}]`)}},
	})
	c.NoError(err, "replay")
	c.Eq(0, replay.Inserted, "the stripped content dedups against the NUL-bearing append")
	c.Eq(node.ID, replay.Nodes[0].ID, "the same node")
}

func TestAppendRetriesOnConflict(t *testing.T) {
	c := assert.NewAborting(t)
	b, convID := newConv(t)
	ctx := context.Background()
	h := conv.NewStore(b, conv.Options{}).Open(convID, conv.AsOwner())
	root, err := h.Append(ctx, conv.Append{FromHead: true, Nodes: []conv.NewNode{{Msg: textMsg(msg.User, "one")}}})
	c.NoError(err, "root")

	// Another writer moves the head between this append's resolve and its
	// commit.
	competing := storedNode(t, convID, root.Nodes[0].ID, root.Nodes[0].PathHash, msg.Assistant, "competing", 1, 1)
	wrapped := &conflictOnce{Backend: b, hooks: []func() error{func() error {
		_, err := b.Commit(ctx, conv.Commit{
			ConversationID:    convID,
			ExpectHead:        root.Nodes[0].ID,
			ExpectNextOrdinal: 1,
			Rows:              []*conv.Node{competing},
			NewHead:           competing.ID,
		})
		return err
	}}}
	retry := conv.NewStore(wrapped, conv.Options{}).Open(convID, conv.AsOwner())
	res, err := retry.Append(ctx, conv.Append{FromHead: true, Nodes: []conv.NewNode{{Msg: textMsg(msg.Assistant, "two")}}})
	c.NoError(err, "the append succeeds on the new head")
	c.Eq(1, res.Inserted, "one node inserted")
	c.Eq(competing.ID, res.Nodes[0].ParentID, "the retry chained under the node that won")
	c.Eq(2, res.Nodes[0].Ordinal, "the retry took the next ordinal")
}

func TestAppendGivesUpAfterFiveConflicts(t *testing.T) {
	c := assert.NewAborting(t)
	b, convID := newConv(t)
	failing := &failingCommit{Backend: b, failure: conv.ErrConflict}
	h := conv.NewStore(failing, conv.Options{}).Open(convID)

	_, err := h.Append(context.Background(), conv.Append{FromHead: true, Nodes: []conv.NewNode{{Msg: textMsg(msg.User, "hi")}}})
	c.ErrorIs(err, conv.ErrConflict, "a conflict is still a conflict")
	c.ErrorContains(err, "conv: append: conv: conversation head moved after 5 attempts", "gives up after five")
	c.Eq(5, failing.count(), "five attempts")
}

func TestAppendLeaseLostNotRetried(t *testing.T) {
	c := assert.NewAborting(t)
	b, convID := newConv(t)
	failing := &failingCommit{Backend: b, failure: conv.ErrLeaseLost}
	h := conv.NewStore(failing, conv.Options{}).Open(convID)

	_, err := h.Append(context.Background(), conv.Append{FromHead: true, Nodes: []conv.NewNode{{Msg: textMsg(msg.User, "hi")}}})
	c.ErrorIs(err, conv.ErrLeaseLost, "a lost lease is returned as-is")
	c.False(errors.Is(err, conv.ErrConflict), "not a conflict")
	c.Eq(1, failing.count(), "a lost lease is never retried")
}

func TestRetractHeadRefusesNonHeadAndNonUser(t *testing.T) {
	c := assert.NewAborting(t)
	b, convID := newConv(t)
	ctx := context.Background()
	h := conv.NewStore(b, conv.Options{}).Open(convID, conv.AsOwner())

	user, err := h.Append(ctx, conv.Append{FromHead: true, Nodes: []conv.NewNode{{Msg: textMsg(msg.User, "one")}}})
	c.NoError(err, "user")
	reply, err := h.Append(ctx, conv.Append{FromHead: true, Nodes: []conv.NewNode{{Msg: textMsg(msg.Assistant, "two")}}})
	c.NoError(err, "reply")

	err = h.RetractHead(ctx, user.Nodes[0].ID)
	c.ErrorContains(err, "conv: retract head: "+user.Nodes[0].ID+" is not the head user row", "not the head")

	err = h.RetractHead(ctx, reply.Nodes[0].ID)
	c.ErrorContains(err, "conv: retract head: "+reply.Nodes[0].ID+" is not the head user row", "not a user row")

	err = h.RetractHead(ctx, "0b8f7c1e-0000-0000-0000-000000000000")
	c.ErrorContains(err, "is not the head user row", "an unknown node")

	path, err := h.Working(ctx)
	c.NoError(err, "Working")
	c.Len(path, 2, "nothing was retracted")
}

func TestIsClearBoundary(t *testing.T) {
	c := assert.NewAborting(t)
	boundary := &conv.Node{
		Ref: conv.Ref{Kind: conv.KindClear},
		Msg: msg.Message{Role: msg.User, Content: []msg.Block{msg.Text{Text: slashcmd.ClearBoundaryText}}},
	}
	c.True(conv.IsClearBoundary(boundary), "a clear root's boundary row")

	wrongKind := *boundary
	wrongKind.Kind = ""
	c.False(conv.IsClearBoundary(&wrongKind), "kind")

	wrongRole := *boundary
	wrongRole.Msg = msg.Message{Role: msg.Assistant, Content: boundary.Msg.Content}
	c.False(conv.IsClearBoundary(&wrongRole), "role")

	wrongText := *boundary
	wrongText.Msg = textMsg(msg.User, "something else")
	c.False(conv.IsClearBoundary(&wrongText), "text")

	twoBlocks := *boundary
	twoBlocks.Msg = msg.Message{Role: msg.User, Content: []msg.Block{
		msg.Text{Text: slashcmd.ClearBoundaryText},
		msg.Text{Text: "more"},
	}}
	c.False(conv.IsClearBoundary(&twoBlocks), "more than one block")

	c.False(conv.IsClearBoundary(nil), "nil")
}

// commitRecorder keeps the commits a store issued.
type commitRecorder struct {
	*conv.MemBackend
	mu      sync.Mutex
	commits []conv.Commit
}

func (r *commitRecorder) Commit(ctx context.Context, commit conv.Commit) (time.Time, error) {
	r.mu.Lock()
	r.commits = append(r.commits, commit)
	r.mu.Unlock()
	return r.MemBackend.Commit(ctx, commit)
}

func (r *commitRecorder) last() conv.Commit {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.commits) == 0 {
		return conv.Commit{}
	}
	return r.commits[len(r.commits)-1]
}

// A node id names a node in ONE conversation. A parent, a derived-from or a
// replayed head belonging to another conversation is refused, never adopted
// from the cache: the id space is global, a tree is not.
func TestAppendRejectsForeignNode(t *testing.T) {
	c := assert.NewAborting(t)
	ctx := context.Background()
	b := conv.NewMemBackend()
	otherID := uuid.Must(uuid.NewV7()).String()
	b.Ensure(otherID)
	convID := uuid.Must(uuid.NewV7()).String()
	b.Ensure(convID)

	// One store, so the foreign node is in this cache: the danger is a cache
	// hit, not a backend miss.
	s := conv.NewStore(b, conv.Options{})
	foreign, err := s.Open(otherID, conv.AsOwner()).Append(ctx, conv.Append{
		FromHead: true,
		Nodes:    []conv.NewNode{{Msg: textMsg(msg.User, "elsewhere")}},
	})
	c.NoError(err, "foreign append")
	foreignID := foreign.Nodes[0].ID

	h := s.Open(convID, conv.AsOwner())
	_, err = h.Append(ctx, conv.Append{
		ParentID: foreignID,
		Nodes:    []conv.NewNode{{Msg: textMsg(msg.Assistant, "here")}},
	})
	c.ErrorContains(err, "conv: append: node "+foreignID+" not in conversation "+convID, "a foreign parent")

	_, err = h.Append(ctx, conv.Append{
		Root:          convtree.RootKindClear,
		DerivedFromID: foreignID,
		Nodes:         []conv.NewNode{{Msg: textMsg(msg.User, "cleared")}},
	})
	c.ErrorContains(err, "conv: append: derived-from: node "+foreignID+" not in conversation "+convID, "a foreign derived-from")

	_, err = h.RequestAt(ctx, foreignID, nil, nil)
	c.ErrorContains(err, "not in conversation "+convID, "a foreign head")

	st, err := h.State(ctx)
	c.NoError(err, "State")
	c.Eq("", st.HeadID, "nothing was inserted")
	path, err := h.Working(ctx)
	c.NoError(err, "Working")
	c.Empty(path, "the conversation is still empty")
}

// A published node owns its slices and pointed-to values.
func TestPublishedNodeDoesNotAliasCallerMemory(t *testing.T) {
	c := assert.NewAborting(t)
	b, convID := newConv(t)
	ctx := context.Background()
	h := conv.NewStore(b, conv.Options{}).Open(convID, conv.AsOwner())

	in, out := int64(11), int64(22)
	ids := []string{"tool_1"}
	res, err := h.Append(ctx, conv.Append{FromHead: true, Nodes: []conv.NewNode{{
		Msg: msg.Message{Role: msg.Assistant, Content: []msg.Block{
			msg.ToolUse{ID: "tool_1", Name: "bash", Input: json.RawMessage(`{}`)},
		}},
		ToolUseIDs:   ids,
		InputTokens:  &in,
		OutputTokens: &out,
	}}})
	c.NoError(err, "append")
	node := res.Nodes[0]

	ids[0] = "mutated"
	in, out = 99, 98
	c.EqDeep([]string{"tool_1"}, node.ToolUseIDs, "the node's tool-use ids are its own")
	c.Eq(int64(11), *node.InputTokens, "the node's input tokens are its own")
	c.Eq(int64(22), *node.OutputTokens, "the node's output tokens are its own")
}

// The head and role checks are pre-checks: a writer that has lost the
// conversation learns that, not that its head is wrong.
func TestRetractHeadFencesBeforeRefusing(t *testing.T) {
	c := assert.NewAborting(t)
	b, convID := newConv(t)
	ctx := context.Background()
	h := conv.NewStore(b, conv.Options{}).Open(convID, conv.AsOwner())
	first, err := h.Append(ctx, conv.Append{FromHead: true, Nodes: []conv.NewNode{{Msg: textMsg(msg.User, "one")}}})
	c.NoError(err, "one")
	_, err = h.Append(ctx, conv.Append{FromHead: true, Nodes: []conv.NewNode{{Msg: textMsg(msg.User, "two")}}})
	c.NoError(err, "two")

	fenced := conv.NewStore(&failingCommit{Backend: b, failure: conv.ErrLeaseLost}, conv.Options{}).
		Open(convID, conv.WithLease(conv.Lease{ConversationID: convID, Holder: "gone", Token: "stale"}))

	err = fenced.RetractHead(ctx, first.Nodes[0].ID)
	c.ErrorIs(err, conv.ErrLeaseLost, "a lost lease wins over the head check")
	c.NotStrContains(err.Error(), "not the head user row", "the refusal is not reported instead")
}
