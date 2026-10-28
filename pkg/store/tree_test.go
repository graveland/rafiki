// SPDX-License-Identifier: Apache-2.0

package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"go.graveland.dev/rafiki/pkg/convtree"

	"github.com/multigres/testkit/assert"
)

// The tree tests share one disposable database and never assert on global
// counts: each builds its own conversation.

// chainText builds a chain node carrying one text block, the shape capture
// writes.
func chainText(role, text string) ChainNode {
	content, err := json.Marshal([]map[string]any{{"type": "text", "text": text}})
	if err != nil {
		panic(err)
	}
	return ChainNode{Role: role, Content: jsonbSafe(content)}
}

// treeRow is one stored node as the assertions read it.
type treeRow struct {
	id      string
	parent  string
	derived string
	depth   int
	ordinal int
	kind    *string
	hash    []byte
}

func readTreeRow(t *testing.T, pool *pgxpool.Pool, convID string, ordinal int) treeRow {
	t.Helper()
	var r treeRow
	err := pool.QueryRow(context.Background(), `
		SELECT id::text, coalesce(parent_id::text, ''), coalesce(derived_from_id::text, ''),
		       coalesce(depth, 0), ordinal, kind, path_hash
		  FROM conversations.conversation_message
		 WHERE conversation_id = $1::uuid AND ordinal = $2`, convID, ordinal).
		Scan(&r.id, &r.parent, &r.derived, &r.depth, &r.ordinal, &r.kind, &r.hash)
	assert.NewAborting(t).NoError(err, "read node at ordinal %d", ordinal)
	return r
}

// convState is the conversation row's tree statistics.
type convState struct {
	head         string
	nextOrdinal  int
	nodeCount    int
	forkCount    int
	fallback     int
	payloadBytes int64
	clearPending bool
}

func readConvState(t *testing.T, pool *pgxpool.Pool, convID string) convState {
	t.Helper()
	var s convState
	err := pool.QueryRow(context.Background(), `
		SELECT coalesce(head_id::text, ''), next_ordinal, node_count, fork_count,
		       fallback_count, payload_bytes, clear_pending
		  FROM conversations.conversation WHERE id = $1::uuid`, convID).
		Scan(&s.head, &s.nextOrdinal, &s.nodeCount, &s.forkCount, &s.fallback, &s.payloadBytes, &s.clearPending)
	assert.NewAborting(t).NoError(err, "read conversation state")
	return s
}

// storedPayloadBytes is the payload_bytes basis: the stored jsonb text, not
// the Go bytes that produced it.
func storedPayloadBytes(t *testing.T, pool *pgxpool.Pool, convID string) int64 {
	t.Helper()
	var n int64
	err := pool.QueryRow(context.Background(), `
		SELECT coalesce(sum(octet_length(content::text)), 0)
		  FROM conversations.conversation_message WHERE conversation_id = $1::uuid`, convID).Scan(&n)
	assert.NewAborting(t).NoError(err, "sum stored payload bytes")
	return n
}

func pathIDs(msgs []Message) []string {
	out := make([]string, len(msgs))
	for i, m := range msgs {
		out[i] = m.ID
	}
	return out
}

// expiredLease takes a lease on conv that is already expired. Acquire's
// negative TTL writes exactly that row, so the fence's `expires_at > now()`
// predicate — not a missing row — is what has to refuse it.
func expiredLease(t *testing.T, pool *pgxpool.Pool, conv, holder string) Lease {
	t.Helper()
	c := assert.NewAborting(t)
	ctx := context.Background()
	lease, ok, err := NewLeases(pool).Acquire(ctx, conv, holder, -1*time.Second)
	c.NoError(err, "acquire an expired lease")
	c.True(ok, "acquire an expired lease")
	var live bool
	c.NoError(pool.QueryRow(ctx, `
		SELECT expires_at > now() FROM conversations.conversation_lease
		 WHERE conversation_id = $1::uuid`, conv).Scan(&live), "read lease expiry")
	c.False(live, "the lease row must be expired for this test to mean anything")
	return lease
}

func TestAppendChainInsertsChainWithConsecutiveOrdinals(t *testing.T) {
	c := assert.NewAborting(t)
	pool := leasePool(t)
	ctx := context.Background()
	conv := newConversation(t, pool)

	res, err := NewMessages(pool).AppendChain(ctx, ChainAppend{
		ConversationID: conv,
		Root:           convtree.RootKindRoot,
		Nodes:          []ChainNode{chainText("user", "a"), chainText("assistant", "b"), chainText("user", "c")},
	})
	c.NoError(err, "AppendChain")
	c.Eq(3, res.Inserted, "inserted")
	c.Len(res.Nodes, 3, "result nodes")
	c.EqDeep([]int{0, 1, 2}, []int{res.Nodes[0].Ordinal, res.Nodes[1].Ordinal, res.Nodes[2].Ordinal}, "ordinals")
	c.EqDeep([]int{0, 1, 2}, []int{res.Nodes[0].Depth, res.Nodes[1].Depth, res.Nodes[2].Depth}, "depths")

	n0 := readTreeRow(t, pool, conv, 0)
	n1 := readTreeRow(t, pool, conv, 1)
	n2 := readTreeRow(t, pool, conv, 2)
	c.Eq("", n0.parent, "a root has no parent")
	c.Eq(n0.id, n1.parent, "node 1 hangs off node 0")
	c.Eq(n1.id, n2.parent, "node 2 hangs off node 1")
	c.EqDeep([]int{0, 1, 2}, []int{n0.depth, n1.depth, n2.depth}, "stored depths")

	s := readConvState(t, pool, conv)
	c.Eq(n2.id, s.head, "head is the last node")
	c.Eq(3, s.nodeCount, "node_count")
	c.Eq(3, s.nextOrdinal, "next_ordinal")
	c.Eq(storedPayloadBytes(t, pool, conv), s.payloadBytes, "payload_bytes is the stored jsonb text length")
}

func TestAppendChainPartialPrefixInsertsOnlyTheRest(t *testing.T) {
	c := assert.NewAborting(t)
	pool := leasePool(t)
	ctx := context.Background()
	conv := newConversation(t, pool)
	m := NewMessages(pool)

	a, b, cc, d := chainText("user", "a"), chainText("assistant", "b"), chainText("user", "c"), chainText("assistant", "d")
	first, err := m.AppendChain(ctx, ChainAppend{ConversationID: conv, Root: convtree.RootKindRoot, Nodes: []ChainNode{a, b}})
	c.NoError(err, "first append")
	c.Eq(2, first.Inserted, "first append inserted")

	second, err := m.AppendChain(ctx, ChainAppend{ConversationID: conv, Root: convtree.RootKindRoot, Nodes: []ChainNode{a, b, cc, d}})
	c.NoError(err, "second append")
	c.Eq(2, second.Inserted, "only the missing suffix is inserted")
	c.Eq(first.Nodes[0].ID, second.Nodes[0].ID, "existing node identity is reused")
	c.Eq(first.Nodes[1].ID, second.Nodes[1].ID, "existing node identity is reused")
	c.EqDeep([]int{2, 3}, []int{second.Nodes[2].Ordinal, second.Nodes[3].Ordinal}, "new ordinals continue the sequence")
	c.Eq(first.Nodes[1].ID, readTreeRow(t, pool, conv, 2).parent, "c hangs off b")
	c.Eq(4, readConvState(t, pool, conv).nodeCount, "node_count counts every node")
}

func TestAppendChainAllExistingMovesHead(t *testing.T) {
	c := assert.NewAborting(t)
	pool := leasePool(t)
	ctx := context.Background()
	conv := newConversation(t, pool)
	m := NewMessages(pool)

	a, b, cc := chainText("user", "a"), chainText("assistant", "b"), chainText("user", "c")
	first, err := m.AppendChain(ctx, ChainAppend{ConversationID: conv, Root: convtree.RootKindRoot, Nodes: []ChainNode{a, b}})
	c.NoError(err, "first append")
	_, err = m.AppendChain(ctx, ChainAppend{ConversationID: conv, Root: convtree.RootKindRoot, Nodes: []ChainNode{a, cc}})
	c.NoError(err, "sibling append")

	back, err := m.AppendChain(ctx, ChainAppend{ConversationID: conv, Root: convtree.RootKindRoot, Nodes: []ChainNode{a, b}})
	c.NoError(err, "re-append the original chain")
	c.Eq(0, back.Inserted, "nothing is inserted")
	c.Eq(first.Nodes[1].ID, back.Nodes[1].ID, "the existing node is returned")
	c.Eq(first.Nodes[1].ID, readConvState(t, pool, conv).head, "head moves even when nothing is inserted")
}

func TestAppendChainSiblingCountsFork(t *testing.T) {
	c := assert.NewAborting(t)
	pool := leasePool(t)
	ctx := context.Background()
	conv := newConversation(t, pool)
	m := NewMessages(pool)

	a, b, cc := chainText("user", "a"), chainText("assistant", "b"), chainText("user", "c")
	_, err := m.AppendChain(ctx, ChainAppend{ConversationID: conv, Root: convtree.RootKindRoot, Nodes: []ChainNode{a, b}})
	c.NoError(err, "first append")
	_, err = m.AppendChain(ctx, ChainAppend{ConversationID: conv, Root: convtree.RootKindRoot, Nodes: []ChainNode{a, cc}})
	c.NoError(err, "sibling append")
	c.Eq(1, readConvState(t, pool, conv).forkCount, "a second child of one parent is a fork")

	_, err = m.AppendChain(ctx, ChainAppend{ConversationID: conv, Root: convtree.RootKindRoot, Nodes: []ChainNode{a, cc}})
	c.NoError(err, "re-append the sibling")
	c.Eq(1, readConvState(t, pool, conv).forkCount, "re-appending an existing chain is not a new fork")
}

func TestAppendChainRootSeedsDiffer(t *testing.T) {
	c := assert.NewAborting(t)
	pool := leasePool(t)
	ctx := context.Background()
	conv := newConversation(t, pool)
	m := NewMessages(pool)

	base, err := m.AppendChain(ctx, ChainAppend{
		ConversationID: conv, Root: convtree.RootKindRoot, Nodes: []ChainNode{chainText("user", "a")},
	})
	c.NoError(err, "base root")

	clearNode := chainText("user", "/clear")
	first, err := m.AppendChain(ctx, ChainAppend{
		ConversationID: conv, Root: convtree.RootKindClear, DerivedFromHead: true, Nodes: []ChainNode{clearNode},
	})
	c.NoError(err, "first clear")
	c.Eq(1, first.Inserted, "first clear inserted")
	second, err := m.AppendChain(ctx, ChainAppend{
		ConversationID: conv, Root: convtree.RootKindClear, DerivedFromHead: true, Nodes: []ChainNode{clearNode},
	})
	c.NoError(err, "second clear")
	c.Eq(1, second.Inserted, "a clear cut from a different node is a different node")
	c.NotEq(first.Nodes[0].ID, second.Nodes[0].ID, "two clears with identical content are distinct rows")

	var clears int
	c.NoError(pool.QueryRow(ctx, `
		SELECT count(*) FROM conversations.conversation_message
		 WHERE conversation_id = $1::uuid AND kind = $2`, conv, KindClear).Scan(&clears), "count clears")
	c.Eq(2, clears, "two clear roots")

	summary := chainText("assistant", "summary")
	s1, err := m.AppendChain(ctx, ChainAppend{
		ConversationID: conv, Root: convtree.RootKindCompaction, DerivedFromHead: true, Nodes: []ChainNode{summary},
	})
	c.NoError(err, "compaction root")
	c.Eq(1, s1.Inserted, "compaction root inserted")
	s2, err := m.AppendChain(ctx, ChainAppend{
		ConversationID: conv, Root: convtree.RootKindCompaction, DerivedFromID: base.Nodes[0].ID, Nodes: []ChainNode{summary},
	})
	c.NoError(err, "second compaction root")
	c.Eq(0, s2.Inserted, "a compaction summary ignores derived-from, so an equal one is the same node")
	c.Eq(s1.Nodes[0].ID, s2.Nodes[0].ID, "the same compaction node")
}

func TestAppendChainParentFromHeadOnEmptyIsRoot(t *testing.T) {
	c := assert.NewAborting(t)
	pool := leasePool(t)
	ctx := context.Background()
	conv := newConversation(t, pool)

	node := chainText("user", "first")
	res, err := NewMessages(pool).AppendChain(ctx, ChainAppend{
		ConversationID: conv, ParentFromHead: true, Nodes: []ChainNode{node},
	})
	c.NoError(err, "AppendChain")
	c.Eq(1, res.Inserted, "inserted")

	row := readTreeRow(t, pool, conv, 0)
	c.Eq("", row.parent, "a root has no parent")
	c.True(row.kind == nil, "a plain root stores a NULL kind, got %v", row.kind)
	c.Eq(0, row.depth, "root depth")

	want, err := convtree.NodeHash(convtree.RootSeed(convtree.RootKindRoot, nil), convtree.Msg{Role: node.Role, Content: node.Content})
	c.NoError(err, "NodeHash")
	c.True(bytes.Equal(want[:], row.hash), "an empty-head append is seeded as a RootKindRoot root")
}

func TestAppendChainLeaseLost(t *testing.T) {
	c := assert.NewAborting(t)
	pool := leasePool(t)
	ctx := context.Background()
	conv := newConversation(t, pool)

	lease, ok, err := NewLeases(pool).Acquire(ctx, conv, "daemon-a", 5*time.Minute)
	c.NoError(err, "Acquire")
	c.True(ok, "Acquire on a free conversation")

	stale := lease
	stale.Token = uuid.NewString()
	_, err = AppendChain(ctx, pool, stale, ChainAppend{
		ConversationID: conv, Root: convtree.RootKindRoot, Nodes: []ChainNode{chainText("user", "a")},
	})
	c.ErrorIs(err, ErrLeaseLost, "a stale token is a lost lease")
	c.Eq(0, rowCount(t, pool, conv), "a refused append inserts nothing")

	_, err = AppendChain(ctx, pool, lease, ChainAppend{
		ConversationID: conv, Root: convtree.RootKindRoot, Nodes: []ChainNode{chainText("user", "a")},
	})
	c.NoError(err, "the live lease still writes")
	c.Eq(1, rowCount(t, pool, conv), "one row")
}

func TestAppendChainPrefixInvariant(t *testing.T) {
	c := assert.NewAborting(t)
	pool := leasePool(t)
	ctx := context.Background()
	conv := newConversation(t, pool)

	a, b := chainText("user", "a"), chainText("assistant", "b")
	hashes, err := convtree.Chain(convtree.RootSeed(convtree.RootKindRoot, nil), []convtree.Msg{
		{Role: a.Role, Content: a.Content}, {Role: b.Role, Content: b.Content},
	})
	c.NoError(err, "Chain")

	// Node 1 without node 0: the stored tree is not a prefix of the chain.
	_, err = pool.Exec(ctx, `
		INSERT INTO conversations.conversation_message
			(conversation_id, ordinal, role, content, path_hash, hash_version, depth)
		VALUES ($1::uuid, 0, $2, $3, $4, $5, 1)`, conv, b.Role, b.Content, hashes[1][:], convtree.HashVersion)
	c.NoError(err, "hand insert node 1")

	_, err = NewMessages(pool).AppendChain(ctx, ChainAppend{
		ConversationID: conv, Root: convtree.RootKindRoot, Nodes: []ChainNode{a, b},
	})
	c.ErrorContains(err, "tree invariant broken", "a gap in the stored chain is reported, never repaired")
	c.Eq(1, rowCount(t, pool, conv), "nothing was inserted")
}

func TestAppendChainConcurrentWritersGetDisjointGapFreeOrdinals(t *testing.T) {
	c := assert.NewAborting(t)
	pool := leasePool(t)
	ctx := context.Background()
	conv := newConversation(t, pool)
	m := NewMessages(pool)

	const writers = 8
	errs := make([]error, writers)
	var wg sync.WaitGroup
	for i := range writers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = m.AppendChain(ctx, ChainAppend{
				ConversationID: conv, ParentFromHead: true,
				Nodes: []ChainNode{chainText("user", fmt.Sprintf("writer-%d", i))},
			})
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		c.NoError(err, "writer %d", i)
	}
	c.EqDeep([]int{0, 1, 2, 3, 4, 5, 6, 7}, rowOrdinals(t, pool, conv), "ordinals are gap-free and disjoint")
	c.Eq(writers, readConvState(t, pool, conv).nodeCount, "node_count")
}

func TestAppendChainConcurrentSameChainLeavesOneCopy(t *testing.T) {
	c := assert.NewAborting(t)
	pool := leasePool(t)
	ctx := context.Background()
	conv := newConversation(t, pool)
	m := NewMessages(pool)

	const writers = 8
	nodes := []ChainNode{chainText("user", "a"), chainText("assistant", "b")}
	errs := make([]error, writers)
	results := make([]ChainResult, writers)
	var wg sync.WaitGroup
	for i := range writers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i], errs[i] = m.AppendChain(ctx, ChainAppend{
				ConversationID: conv, Root: convtree.RootKindRoot, Nodes: nodes,
			})
		}(i)
	}
	wg.Wait()

	c.Eq(2, rowCount(t, pool, conv), "one copy of the chain")
	for i := range writers {
		c.NoError(errs[i], "writer %d", i)
		c.Len(results[i].Nodes, 2, "writer %d nodes", i)
		c.EqDeep([]string{results[0].Nodes[0].ID, results[0].Nodes[1].ID},
			[]string{results[i].Nodes[0].ID, results[i].Nodes[1].ID}, "writer %d returned the same ids", i)
	}
	c.Eq(2, readConvState(t, pool, conv).nodeCount, "node_count counts each node once")
}

func TestAppendChainFallbackCounts(t *testing.T) {
	c := assert.NewAborting(t)
	pool := leasePool(t)
	ctx := context.Background()
	conv := newConversation(t, pool)

	_, err := NewMessages(pool).AppendChain(ctx, ChainAppend{
		ConversationID: conv, Root: convtree.RootKindRoot,
		Nodes: []ChainNode{chainText("user", "a")}, Fallback: true,
	})
	c.NoError(err, "AppendChain")
	c.Eq(1, readConvState(t, pool, conv).fallback, "fallback_count")
}

func TestAppendChainZeroNodesMovesHead(t *testing.T) {
	c := assert.NewAborting(t)
	pool := leasePool(t)
	ctx := context.Background()
	conv := newConversation(t, pool)
	m := NewMessages(pool)

	a, b, cc := chainText("user", "a"), chainText("assistant", "b"), chainText("user", "c")
	first, err := m.AppendChain(ctx, ChainAppend{ConversationID: conv, Root: convtree.RootKindRoot, Nodes: []ChainNode{a, b}})
	c.NoError(err, "first append")
	second, err := m.AppendChain(ctx, ChainAppend{ConversationID: conv, Root: convtree.RootKindRoot, Nodes: []ChainNode{a, cc}})
	c.NoError(err, "sibling append")
	c.Eq(second.Nodes[1].ID, readConvState(t, pool, conv).head, "head follows the sibling")

	res, err := m.AppendChain(ctx, ChainAppend{ConversationID: conv, ParentID: first.Nodes[1].ID})
	c.NoError(err, "zero-node append")
	c.Eq(0, res.Inserted, "nothing inserted")
	c.Len(res.Nodes, 0, "no nodes returned")
	s := readConvState(t, pool, conv)
	c.Eq(first.Nodes[1].ID, s.head, "head moved to the named parent")
	c.Eq(3, s.nodeCount, "node_count unchanged")
	c.Eq(3, rowCount(t, pool, conv), "rows unchanged")

	_, err = m.AppendChain(ctx, ChainAppend{ConversationID: conv, ParentID: uuid.NewString()})
	c.ErrorContains(err, "not in conversation", "a parent from another conversation is refused")

	lease, ok, lerr := NewLeases(pool).Acquire(ctx, conv, "daemon-a", 5*time.Minute)
	c.NoError(lerr, "Acquire")
	c.True(ok, "Acquire")
	stale := lease
	stale.Token = uuid.NewString()
	_, err = AppendChain(ctx, pool, stale, ChainAppend{ConversationID: conv, ParentID: second.Nodes[1].ID})
	c.ErrorIs(err, ErrLeaseLost, "a head move is fenced like an append")
	c.Eq(first.Nodes[1].ID, readConvState(t, pool, conv).head, "a refused move leaves the head")
}

func TestAppendChainConsumeClearPending(t *testing.T) {
	c := assert.NewAborting(t)
	pool := leasePool(t)
	ctx := context.Background()
	conv := newConversation(t, pool)
	m := NewMessages(pool)

	setClearPending := func(v bool) {
		t.Helper()
		_, err := pool.Exec(ctx, `UPDATE conversations.conversation SET clear_pending = $2 WHERE id = $1::uuid`, conv, v)
		c.NoError(err, "set clear_pending")
	}

	setClearPending(true)
	_, err := m.AppendChain(ctx, ChainAppend{
		ConversationID: conv, Root: convtree.RootKindRoot,
		Nodes: []ChainNode{chainText("user", "a")}, ConsumeClearPending: true,
	})
	c.NoError(err, "append consuming clear_pending")
	c.False(readConvState(t, pool, conv).clearPending, "clear_pending consumed")

	setClearPending(true)
	_, err = m.AppendChain(ctx, ChainAppend{
		ConversationID: conv, ParentFromHead: true, Nodes: []ChainNode{chainText("user", "b")},
	})
	c.NoError(err, "append leaving clear_pending")
	c.True(readConvState(t, pool, conv).clearPending, "clear_pending untouched when not consumed")
}

func TestLoadPathFollowsHeadNotOrdinal(t *testing.T) {
	c := assert.NewAborting(t)
	pool := leasePool(t)
	ctx := context.Background()
	conv := newConversation(t, pool)
	m := NewMessages(pool)

	empty, err := m.LoadPath(ctx, conv)
	c.NoError(err, "LoadPath on an empty conversation")
	c.Len(empty, 0, "no head, no path")

	a, b, cc := chainText("user", "a"), chainText("assistant", "b"), chainText("user", "c")
	first, err := m.AppendChain(ctx, ChainAppend{ConversationID: conv, Root: convtree.RootKindRoot, Nodes: []ChainNode{a, b}})
	c.NoError(err, "first append")
	second, err := m.AppendChain(ctx, ChainAppend{ConversationID: conv, Root: convtree.RootKindRoot, Nodes: []ChainNode{a, cc}})
	c.NoError(err, "sibling append")

	path, err := m.LoadPath(ctx, conv)
	c.NoError(err, "LoadPath")
	c.EqDeep([]string{first.Nodes[0].ID, second.Nodes[1].ID}, pathIDs(path), "the path follows the head, not the ordinals")

	_, err = m.AppendChain(ctx, ChainAppend{ConversationID: conv, Root: convtree.RootKindRoot, Nodes: []ChainNode{a, b}})
	c.NoError(err, "re-append the original chain")
	path, err = m.LoadPath(ctx, conv)
	c.NoError(err, "LoadPath after moving the head back")
	c.EqDeep([]string{first.Nodes[0].ID, first.Nodes[1].ID}, pathIDs(path), "the path follows the new head")
}

func TestRetractHeadMovesHeadAndKeepsRow(t *testing.T) {
	c := assert.NewAborting(t)
	pool := leasePool(t)
	ctx := context.Background()
	conv := newConversation(t, pool)
	m := NewMessages(pool)

	root, err := m.AppendChain(ctx, ChainAppend{
		ConversationID: conv, Root: convtree.RootKindRoot, Nodes: []ChainNode{chainText("user", "a")},
	})
	c.NoError(err, "root append")
	user, err := m.AppendChain(ctx, ChainAppend{
		ConversationID: conv, ParentFromHead: true, Nodes: []ChainNode{chainText("user", "u")},
	})
	c.NoError(err, "user append")
	c.Eq(user.Nodes[0].ID, readConvState(t, pool, conv).head, "the user row is the head")

	c.NoError(m.RetractHead(ctx, conv, user.Nodes[0].ID), "RetractHead")
	c.Eq(root.Nodes[0].ID, readConvState(t, pool, conv).head, "head moved to the parent")
	c.Eq(2, rowCount(t, pool, conv), "the retracted row survives")

	c.Error(m.RetractHead(ctx, conv, user.Nodes[0].ID), "retracting a non-head row")
	c.Eq(root.Nodes[0].ID, readConvState(t, pool, conv).head, "a refused retract leaves the head")
}

// TestAppendChainExpiredLeaseIsRefused pins the fence's expiry half: a lease
// row that exists but has lapsed is a lost lease, and a refused append must
// leave both the head and the ordinal allocator exactly as they were.
func TestAppendChainExpiredLeaseIsRefused(t *testing.T) {
	c := assert.NewAborting(t)
	pool := leasePool(t)
	ctx := context.Background()
	conv := newConversation(t, pool)
	m := NewMessages(pool)

	expired := expiredLease(t, pool, conv, "daemon-a")
	_, err := AppendChain(ctx, pool, expired, ChainAppend{
		ConversationID: conv, Root: convtree.RootKindRoot, Nodes: []ChainNode{chainText("user", "a")},
	})
	c.ErrorIs(err, ErrLeaseLost, "an expired lease is a lost lease")
	c.Eq(0, rowCount(t, pool, conv), "a refused append inserts nothing")
	c.Eq("", readConvState(t, pool, conv).head, "a refused append does not move the head")

	// A successful append first, then the expired attempt: the allocator must
	// not have moved either.
	_, err = m.AppendChain(ctx, ChainAppend{
		ConversationID: conv, Root: convtree.RootKindRoot, Nodes: []ChainNode{chainText("user", "b")},
	})
	c.NoError(err, "unfenced append")
	before := readConvState(t, pool, conv)

	_, err = AppendChain(ctx, pool, expired, ChainAppend{
		ConversationID: conv, ParentFromHead: true, Nodes: []ChainNode{chainText("user", "c")},
	})
	c.ErrorIs(err, ErrLeaseLost, "the expired lease is refused after a successful append too")
	after := readConvState(t, pool, conv)
	c.Eq(before.nextOrdinal, after.nextOrdinal, "next_ordinal untouched")
	c.Eq(before.head, after.head, "head untouched")
	c.Eq(before.nodeCount, after.nodeCount, "node_count untouched")
	c.Eq(1, rowCount(t, pool, conv), "still one row")
}

// TestRetractHeadExpiredLeaseIsRefused is the same fence on the head move: the
// row the retract would target is a legitimate user head, so only the lease can
// be what refuses it.
func TestRetractHeadExpiredLeaseIsRefused(t *testing.T) {
	c := assert.NewAborting(t)
	pool := leasePool(t)
	ctx := context.Background()
	conv := newConversation(t, pool)

	live, ok, err := NewLeases(pool).Acquire(ctx, conv, "daemon-a", 5*time.Minute)
	c.NoError(err, "Acquire")
	c.True(ok, "Acquire on a free conversation")
	m := NewMessages(pool).WithLease(live)

	_, err = m.AppendChain(ctx, ChainAppend{
		ConversationID: conv, Root: convtree.RootKindRoot, Nodes: []ChainNode{chainText("user", "a")},
	})
	c.NoError(err, "root append under a live lease")
	user, err := m.AppendChain(ctx, ChainAppend{
		ConversationID: conv, ParentFromHead: true, Nodes: []ChainNode{chainText("user", "u")},
	})
	c.NoError(err, "user append under a live lease")
	c.Eq(user.Nodes[0].ID, readConvState(t, pool, conv).head, "the user row is the head")

	expired := expiredLease(t, pool, conv, "daemon-a")
	c.NotEq(live.Token, expired.Token, "the reclaim minted a new token")
	err = m.WithLease(expired).RetractHead(ctx, conv, user.Nodes[0].ID)
	c.ErrorIs(err, ErrLeaseLost, "an expired lease cannot retract the head")
	c.Eq(user.Nodes[0].ID, readConvState(t, pool, conv).head, "the head did not move")
	c.Eq(2, rowCount(t, pool, conv), "nothing was deleted")
}

// TestRetractHeadRefusesANonUserHead pins the two refusals that are not about
// the lease: the head must be a user row, and it must be the head. A lease the
// caller does not hold refuses an otherwise valid retract as well.
func TestRetractHeadRefusesANonUserHead(t *testing.T) {
	c := assert.NewAborting(t)
	pool := leasePool(t)
	ctx := context.Background()
	conv := newConversation(t, pool)
	m := NewMessages(pool)

	chain, err := m.AppendChain(ctx, ChainAppend{
		ConversationID: conv, Root: convtree.RootKindRoot,
		Nodes: []ChainNode{chainText("user", "a"), chainText("assistant", "b")},
	})
	c.NoError(err, "root chain")
	c.Eq(chain.Nodes[1].ID, readConvState(t, pool, conv).head, "the assistant row is the head")

	c.Error(m.RetractHead(ctx, conv, chain.Nodes[1].ID), "an assistant head cannot be retracted")
	c.Eq(chain.Nodes[1].ID, readConvState(t, pool, conv).head, "a refused retract leaves the head")
	c.Error(m.RetractHead(ctx, conv, chain.Nodes[0].ID), "a user row that is not the head cannot be retracted")
	c.Eq(chain.Nodes[1].ID, readConvState(t, pool, conv).head, "a refused retract leaves the head")

	lease, ok, err := NewLeases(pool).Acquire(ctx, conv, "daemon-a", 5*time.Minute)
	c.NoError(err, "Acquire")
	c.True(ok, "Acquire on a free conversation")
	user, err := NewMessages(pool).WithLease(lease).AppendChain(ctx, ChainAppend{
		ConversationID: conv, ParentFromHead: true, Nodes: []ChainNode{chainText("user", "u")},
	})
	c.NoError(err, "append under a live lease")
	c.Eq(user.Nodes[0].ID, readConvState(t, pool, conv).head, "the user row is the head")

	stale := lease
	stale.Token = uuid.NewString()
	err = NewMessages(pool).WithLease(stale).RetractHead(ctx, conv, user.Nodes[0].ID)
	c.ErrorIs(err, ErrLeaseLost, "a wrong token cannot retract the head")
	c.Eq(user.Nodes[0].ID, readConvState(t, pool, conv).head, "a refused retract leaves the head")
}

// TestAppendChainLeaseFenceBlocksTakeover pins the LOCK MODE the fence depends
// on, not just its outcome. An AppendChain test cannot observe the block
// without a pause hook inside the transaction, so this drives fenceLease — the
// production fence — on one connection, holding its lock open, and runs the
// takeover's UPDATE on a second. Dropping FOR SHARE from fenceLease makes the
// UPDATE succeed here while every other test in this file stays green, which is
// exactly why this test exists.
func TestAppendChainLeaseFenceBlocksTakeover(t *testing.T) {
	c := assert.NewAborting(t)
	pool := leasePool(t)
	ctx := context.Background()
	conv := newConversation(t, pool)

	lease, ok, err := NewLeases(pool).Acquire(ctx, conv, "daemon-a", 5*time.Minute)
	c.NoError(err, "Acquire")
	c.True(ok, "Acquire on a free conversation")

	fence, err := pool.Begin(ctx)
	c.NoError(err, "begin the fence transaction")
	defer func() { _ = fence.Rollback(ctx) }()
	c.NoError(fenceLease(ctx, fence, conv, lease), "the fence finds and locks the lease row")

	// The takeover's UPDATE needs the row exclusively. lock_timeout is what
	// turns the block into a bounded, assertable 55P03; statement_timeout is
	// only a backstop so this test can never hang. (statement_timeout alone
	// reports 57014, query_canceled, not the lock error.)
	takeover, err := pool.Begin(ctx)
	c.NoError(err, "begin the takeover transaction")
	defer func() { _ = takeover.Rollback(ctx) }()
	_, err = takeover.Exec(ctx, `SET LOCAL lock_timeout = '150ms'`)
	c.NoError(err, "lock_timeout")
	_, err = takeover.Exec(ctx, `SET LOCAL statement_timeout = '2s'`)
	c.NoError(err, "statement_timeout")

	newToken := uuid.NewString()
	_, err = takeover.Exec(ctx, `
		UPDATE conversations.conversation_lease SET token = $2::uuid WHERE conversation_id = $1::uuid`,
		conv, newToken)
	c.Error(err, "a takeover must block on the fence's share lock")
	var pgErr *pgconn.PgError
	c.True(errors.As(err, &pgErr), "the takeover failed with a Postgres error, got %v", err)
	c.Eq("55P03", pgErr.Code, "lock_not_available: the takeover could not take the row")
	c.NoError(takeover.Rollback(ctx), "roll back the takeover")

	// Once the fence's transaction ends, the same takeover goes through.
	c.NoError(fence.Rollback(ctx), "release the fence")
	_, err = pool.Exec(ctx, `
		UPDATE conversations.conversation_lease SET token = $2::uuid WHERE conversation_id = $1::uuid`,
		conv, newToken)
	c.NoError(err, "the takeover succeeds once the fence transaction ends")
	var token string
	c.NoError(pool.QueryRow(ctx, `
		SELECT token::text FROM conversations.conversation_lease WHERE conversation_id = $1::uuid`,
		conv).Scan(&token), "read the lease token")
	c.Eq(newToken, token, "the takeover took the lease")
}
