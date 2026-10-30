// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/multigres/testkit/assert"

	"go.graveland.dev/rafiki/pkg/conv"
	"go.graveland.dev/rafiki/pkg/conv/convtest"
	"go.graveland.dev/rafiki/pkg/convtree"
	msganthropic "go.graveland.dev/rafiki/pkg/msg/anthropic"
)

// The backend tests share one disposable database and never assert on global
// counts: each builds its own conversation and drops it again.

// createConversation inserts a conversation under an id the caller chose and
// removes everything it holds when the test ends. The suite mints its own ids,
// so the id cannot be left to the column default.
func createConversation(t *testing.T, pool *pgxpool.Pool, id string) {
	t.Helper()
	c := assert.NewAborting(t)
	_, err := pool.Exec(context.Background(), `
		INSERT INTO conversations.conversation (id, origin_entrypoint, driven_by)
		VALUES ($1::uuid, 'test', 'server')`, id)
	c.NoError(err, "insert conversation %s", id)
	t.Cleanup(func() { dropConversation(t, pool, id) })
}

// cleanConversation creates a conversation and registers its removal.
func cleanConversation(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	id := newConversation(t, pool)
	t.Cleanup(func() { dropConversation(t, pool, id) })
	return id
}

// dropConversation removes a conversation and everything this package's tests
// wrote into it. Nodes go leaf-first because parent_id and derived_from_id are
// ON DELETE RESTRICT, and head_id has to be cleared before its row can go.
func dropConversation(t *testing.T, pool *pgxpool.Pool, id string) {
	t.Helper()
	ctx := context.Background()
	if _, err := pool.Exec(ctx,
		`UPDATE conversations.conversation SET head_id = NULL WHERE id = $1::uuid`, id); err != nil {
		t.Errorf("clear head of %s: %v", id, err)
		return
	}
	for {
		tag, err := pool.Exec(ctx, `
			DELETE FROM conversations.conversation_message m
			 WHERE m.conversation_id = $1::uuid
			   AND NOT EXISTS (SELECT 1 FROM conversations.conversation_message k
			                    WHERE k.parent_id = m.id OR k.derived_from_id = m.id)`, id)
		if err != nil {
			t.Errorf("delete nodes of %s: %v", id, err)
			return
		}
		if tag.RowsAffected() == 0 {
			break
		}
	}
	if _, err := pool.Exec(ctx,
		`DELETE FROM conversations.conversation WHERE id = $1::uuid`, id); err != nil {
		t.Errorf("delete conversation %s: %v", id, err)
	}
}

// countNodes is how many rows the conversation's tree holds.
func countNodes(t *testing.T, pool *pgxpool.Pool, convID string) int {
	t.Helper()
	var n int
	err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM conversations.conversation_message WHERE conversation_id = $1::uuid`,
		convID).Scan(&n)
	assert.NewAborting(t).NoError(err, "count nodes")
	return n
}

// convPair builds the same message for both write paths from one stored
// content, so AppendChain and the core hash identical bytes.
func convPair(t *testing.T, role, text string) (ChainNode, conv.NewNode) {
	t.Helper()
	c := assert.NewAborting(t)
	chain := chainText(role, text)
	decoded, err := msganthropic.Decode(role, chain.Content)
	c.NoError(err, "decode %q", text)
	return chain, conv.NewNode{Msg: decoded, Content: chain.Content}
}

// commitRow builds a root node carrying text, the shape a Commit from the core
// carries.
func commitRow(t *testing.T, convID, role, text string, ordinal, depth int) *conv.Node {
	t.Helper()
	c := assert.NewAborting(t)
	chain := chainText(role, text)
	hashes, err := convtree.Chain(convtree.RootSeed(convtree.RootKindRoot, nil),
		[]convtree.Msg{{Role: role, Content: chain.Content}})
	c.NoError(err, "hash %q", text)
	decoded, err := msganthropic.Decode(role, chain.Content)
	c.NoError(err, "decode %q", text)
	return &conv.Node{
		Ref: conv.Ref{
			ID:             uuid.Must(uuid.NewV7()).String(),
			ConversationID: convID,
			Ordinal:        ordinal,
			Depth:          depth,
			PathHash:       hashes[0],
		},
		Msg:     decoded,
		Content: chain.Content,
	}
}

// refIDs returns refs' ids in the order given.
func refIDs(refs []conv.Ref) []string {
	out := make([]string, len(refs))
	for i, r := range refs {
		out[i] = r.ID
	}
	return out
}

func TestConvBackendConformance(t *testing.T) {
	pool := leasePool(t)
	convtest.RunBackendSuite(t, func(t *testing.T) (conv.Backend, func(string)) {
		return NewConvBackend(pool), func(convID string) { createConversation(t, pool, convID) }
	})
}

func TestConvBackendConflictOnHeadMove(t *testing.T) {
	c := assert.NewAborting(t)
	pool := leasePool(t)
	ctx := context.Background()
	convID := cleanConversation(t, pool)
	b := NewConvBackend(pool)

	handle := conv.NewStore(b, conv.Options{}).Open(convID, conv.AsOwner())
	_, first := convPair(t, "user", "hello")
	res, err := handle.Append(ctx, conv.Append{FromHead: true, Nodes: []conv.NewNode{first}})
	c.NoError(err, "append")

	row := commitRow(t, convID, "user", "a stale writer", 1, 1)
	_, err = b.Commit(ctx, conv.Commit{
		ConversationID:    convID,
		ExpectHead:        "",
		ExpectNextOrdinal: 0,
		Rows:              []*conv.Node{row},
		NewHead:           row.ID,
	})
	c.ErrorIs(err, conv.ErrConflict, "a stale head is a conflict")

	c.Eq(1, countNodes(t, pool, convID), "the refused commit inserted nothing")
	st, err := b.State(ctx, convID)
	c.NoError(err, "State")
	c.Eq(res.Nodes[0].ID, st.HeadID, "the head did not move")
	c.Eq(1, st.NextOrdinal, "no ordinal was allocated")
}

func TestConvBackendInsertConflictIsErrConflict(t *testing.T) {
	c := assert.NewAborting(t)
	pool := leasePool(t)
	ctx := context.Background()
	convID := cleanConversation(t, pool)
	b := NewConvBackend(pool)

	handle := conv.NewStore(b, conv.Options{}).Open(convID, conv.AsOwner())
	_, first := convPair(t, "user", "hello")
	res, err := handle.Append(ctx, conv.Append{FromHead: true, Nodes: []conv.NewNode{first}})
	c.NoError(err, "append")

	st, err := b.State(ctx, convID)
	c.NoError(err, "State")

	dup := *res.Nodes[0]
	dup.ID = uuid.Must(uuid.NewV7()).String()
	dup.Ordinal = st.NextOrdinal
	_, err = b.Commit(ctx, conv.Commit{
		ConversationID:    convID,
		ExpectHead:        st.HeadID,
		ExpectNextOrdinal: st.NextOrdinal,
		Rows:              []*conv.Node{&dup},
		NewHead:           dup.ID,
	})
	c.ErrorIs(err, conv.ErrConflict, "an already-stored path hash is a conflict")

	c.Eq(1, countNodes(t, pool, convID), "the refused commit inserted nothing")
	after, err := b.State(ctx, convID)
	c.NoError(err, "State after the conflict")
	c.Eq(st.HeadID, after.HeadID, "the head did not move")
}

func TestConvBackendCommitFencesLease(t *testing.T) {
	c := assert.NewAborting(t)
	pool := leasePool(t)
	ctx := context.Background()
	convID := cleanConversation(t, pool)
	b := NewConvBackend(pool)

	expired := expiredLease(t, pool, convID, "daemon-expired")
	row := commitRow(t, convID, "user", "behind a dead lease", 0, 0)
	_, err := b.Commit(ctx, conv.Commit{
		ConversationID:    convID,
		ExpectHead:        "",
		ExpectNextOrdinal: 0,
		Rows:              []*conv.Node{row},
		NewHead:           row.ID,
		Lease:             expired,
	})
	c.ErrorIs(err, ErrLeaseLost, "an expired lease is a lost lease")
	c.True(errors.Is(err, conv.ErrLeaseLost), "the fence fails with conv's error")
	c.Eq(0, countNodes(t, pool, convID), "the fenced commit inserted nothing")
}

func TestConvBackendCountersMatchAppendChain(t *testing.T) {
	c := assert.NewAborting(t)
	pool := leasePool(t)
	ctx := context.Background()
	chainConv := cleanConversation(t, pool)
	coreConv := cleanConversation(t, pool)

	aChain, aCore := convPair(t, "user", "a")
	bChain, bCore := convPair(t, "assistant", "b")
	cChain, cCore := convPair(t, "user", "c")
	forkChain, forkCore := convPair(t, "assistant", "d")
	fallChain, fallCore := convPair(t, "user", "e")

	m := NewMessages(pool)
	chainRoot, err := m.AppendChain(ctx, ChainAppend{
		ConversationID: chainConv,
		Root:           convtree.RootKindRoot,
		Nodes:          []ChainNode{aChain, bChain, cChain},
	})
	c.NoError(err, "AppendChain root")
	_, err = m.AppendChain(ctx, ChainAppend{
		ConversationID: chainConv,
		ParentID:       chainRoot.Nodes[1].ID,
		Nodes:          []ChainNode{forkChain},
	})
	c.NoError(err, "AppendChain fork")
	_, err = m.AppendChain(ctx, ChainAppend{
		ConversationID: chainConv,
		ParentFromHead: true,
		Nodes:          []ChainNode{fallChain},
		Fallback:       true,
	})
	c.NoError(err, "AppendChain fallback")

	handle := conv.NewStore(NewConvBackend(pool), conv.Options{}).Open(coreConv, conv.AsOwner())
	coreRoot, err := handle.Append(ctx, conv.Append{
		FromHead: true,
		Nodes:    []conv.NewNode{aCore, bCore, cCore},
	})
	c.NoError(err, "conv root")
	_, err = handle.Append(ctx, conv.Append{
		ParentID: coreRoot.Nodes[1].ID,
		Nodes:    []conv.NewNode{forkCore},
	})
	c.NoError(err, "conv fork")
	_, err = handle.Append(ctx, conv.Append{
		FromHead: true,
		Nodes:    []conv.NewNode{fallCore},
		Fallback: true,
	})
	c.NoError(err, "conv fallback")

	viaChain := readConvState(t, pool, chainConv)
	viaCore := readConvState(t, pool, coreConv)
	c.Eq(viaChain.nodeCount, viaCore.nodeCount, "node_count")
	c.Eq(viaChain.forkCount, viaCore.forkCount, "fork_count")
	c.Eq(viaChain.fallback, viaCore.fallback, "fallback_count")
	c.Eq(viaChain.payloadBytes, viaCore.payloadBytes, "payload_bytes")
	c.Eq(viaChain.nextOrdinal, viaCore.nextOrdinal, "next_ordinal")
	c.Eq(5, viaCore.nodeCount, "the fixture appends five nodes")
	c.Eq(1, viaCore.forkCount, "the fixture forks once")
	c.Eq(1, viaCore.fallback, "the fixture falls back once")

	for ordinal := 0; ordinal < viaCore.nodeCount; ordinal++ {
		c.EqDeep(readTreeRow(t, pool, chainConv, ordinal).hash,
			readTreeRow(t, pool, coreConv, ordinal).hash,
			"node %d has the same path hash on both write paths", ordinal)
	}
}

func TestConvBackendWalkCrossesDerivedFrom(t *testing.T) {
	c := assert.NewAborting(t)
	pool := leasePool(t)
	ctx := context.Background()
	convID := cleanConversation(t, pool)
	b := NewConvBackend(pool)
	handle := conv.NewStore(b, conv.Options{}).Open(convID, conv.AsOwner())

	_, hello := convPair(t, "user", "hello")
	root, err := handle.Append(ctx, conv.Append{FromHead: true, Nodes: []conv.NewNode{hello}})
	c.NoError(err, "root append")
	_, hi := convPair(t, "assistant", "hi")
	tail, err := handle.Append(ctx, conv.Append{FromHead: true, Nodes: []conv.NewNode{hi}})
	c.NoError(err, "tail append")
	_, summary := convPair(t, "user", "summary")
	boundary, err := handle.Append(ctx, conv.Append{
		Root:          conv.KindCompactionSummary,
		DerivedFromID: tail.Nodes[0].ID,
		Nodes:         []conv.NewNode{summary},
	})
	c.NoError(err, "compaction root")

	stopped, err := b.Walk(ctx, convID, boundary.Nodes[0].ID, false)
	c.NoError(err, "Walk without crossing roots")
	c.EqDeep([]string{boundary.Nodes[0].ID}, refIDs(stopped), "a root ends the walk")

	crossed, err := b.Walk(ctx, convID, boundary.Nodes[0].ID, true)
	c.NoError(err, "Walk crossing roots")
	c.EqDeep([]string{boundary.Nodes[0].ID, tail.Nodes[0].ID, root.Nodes[0].ID},
		refIDs(crossed), "head first, across the derived-from edge")
	c.Eq(conv.KindCompactionSummary, crossed[0].Kind, "the boundary's kind")
	c.Eq("", crossed[0].ParentID, "the boundary is a root")
	c.Eq(tail.Nodes[0].ID, crossed[0].DerivedFromID, "the boundary derives from the tail")
	c.EqDeep([]int{0, 1, 0}, []int{crossed[0].Depth, crossed[1].Depth, crossed[2].Depth}, "depths")
}

// leaseSeen takes a store.Lease where a conv.Lease is required: the two names
// are one type, which is what lets a Backend's fence be the store's fence.
func leaseSeen(l conv.Lease) conv.Lease { return l }

// leaseHeld takes a conv.Lease where a store.Lease is required, the other
// direction of the same alias.
func leaseHeld(l Lease) bool { return l.Held() }

func TestLeaseAliases(t *testing.T) {
	c := assert.NewAborting(t)
	c.True(errors.Is(fmt.Errorf("append chain: %w", ErrLeaseLost), conv.ErrLeaseLost),
		"a store fence failure is conv.ErrLeaseLost")
	c.True(errors.Is(fmt.Errorf("conv commit: %w", conv.ErrLeaseLost), ErrLeaseLost),
		"a conv fence failure is store.ErrLeaseLost")

	now := time.Now()
	lease := leaseSeen(Lease{
		ConversationID: "conv",
		Holder:         "daemon",
		Token:          "token",
		ExpiresAt:      now,
	})
	c.Eq("conv", lease.ConversationID, "the literal assigns field for field")
	c.Eq("daemon", lease.Holder, "holder")
	c.Eq("token", lease.Token, "token")
	c.True(lease.ExpiresAt.Equal(now), "expiry")
	c.True(leaseHeld(lease), "a filled lease fences")
	c.False(leaseHeld(leaseSeen(Lease{})), "the zero lease does not")
}

// TestReaderUnderLeaseTakeover is the fence and the read path together: a
// writer that lost its lease cannot write, and a reader never serves a head it
// did not re-check. The reader's snapshots are validated while the takeover
// happens, so a torn read — a head whose nodes are not there, or a chain that
// does not link up — fails the test rather than passing on the final state.
func TestReaderUnderLeaseTakeover(t *testing.T) {
	c := assert.NewAborting(t)
	pool := leasePool(t)
	ctx := context.Background()
	convID := cleanConversation(t, pool)

	writerA := conv.NewStore(NewConvBackend(pool), conv.Options{})
	leaseA, ok, err := NewLeases(pool).Acquire(ctx, convID, "daemon-a", 5*time.Minute)
	c.NoError(err, "acquire A's lease")
	c.True(ok, "A holds the lease")
	handleA := writerA.Open(convID, conv.AsOwner(), conv.WithLease(leaseA))

	nodes := make([]conv.NewNode, 0, 3)
	for _, text := range []string{"a", "b", "c"} {
		_, n := convPair(t, "user", text)
		nodes = append(nodes, n)
	}
	first, err := handleA.Append(ctx, conv.Append{FromHead: true, Nodes: nodes})
	c.NoError(err, "A appends three nodes")

	var (
		mu        sync.Mutex
		committed = make(map[string]bool, 5)
		failures  []string
		snapshots int
	)
	for _, n := range first.Nodes {
		committed[n.ID] = true
	}

	reader := conv.NewStore(NewConvBackend(pool), conv.Options{})
	readerHandle := reader.Open(convID)
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
			}
			sn, err := readerHandle.Snapshot(ctx)
			mu.Lock()
			snapshots++
			if err != nil {
				failures = append(failures, fmt.Sprintf("snapshot: %v", err))
				mu.Unlock()
				return
			}
			if problem := snapshotProblem(sn, committed); problem != "" {
				failures = append(failures, problem)
			}
			mu.Unlock()
			time.Sleep(time.Millisecond)
		}
	}()

	_, err = pool.Exec(ctx, `
		UPDATE conversations.conversation_lease SET expires_at = now() - interval '1 second'
		 WHERE conversation_id = $1::uuid`, convID)
	c.NoError(err, "expire A's lease")

	leaseC, ok, err := NewLeases(pool).Acquire(ctx, convID, "daemon-c", 5*time.Minute)
	c.NoError(err, "acquire C's lease")
	c.True(ok, "C takes the expired lease over")

	writerC := conv.NewStore(NewConvBackend(pool), conv.Options{})
	handleC := writerC.Open(convID, conv.AsOwner(), conv.WithLease(leaseC))
	_, afterOne := convPair(t, "assistant", "after the takeover")
	_, afterTwo := convPair(t, "user", "and again")
	second, err := handleC.Append(ctx, conv.Append{
		FromHead: true,
		Nodes:    []conv.NewNode{afterOne, afterTwo},
	})
	c.NoError(err, "C appends after the takeover")
	mu.Lock()
	for _, n := range second.Nodes {
		committed[n.ID] = true
	}
	mu.Unlock()

	_, stale := convPair(t, "assistant", "a stale writer")
	_, err = handleA.Append(ctx, conv.Append{FromHead: true, Nodes: []conv.NewNode{stale}})
	c.ErrorIs(err, ErrLeaseLost, "the writer that lost its lease is fenced")

	close(stop)
	<-done

	mu.Lock()
	ran, problems := snapshots, append([]string(nil), failures...)
	mu.Unlock()
	for _, problem := range problems {
		t.Errorf("reader snapshot: %s", problem)
	}
	c.Greater(0, ran, "the reader took snapshots while the takeover ran")

	sn, err := readerHandle.Snapshot(ctx)
	c.NoError(err, "the reader's next snapshot")
	head := sn.Head()
	c.Require().NotNil(head, "the snapshot has a head")
	c.Eq(second.Nodes[len(second.Nodes)-1].ID, head.ID, "a reader re-checks the head and sees C's")
}

// snapshotProblem reports what is inconsistent about one reader snapshot, or ""
// when it is a view some writer actually committed: strictly increasing
// ordinals, every node hanging off its predecessor, and a head that was
// committed.
func snapshotProblem(sn *conv.Snapshot, committed map[string]bool) string {
	var prev *conv.Node
	head := ""
	for n := range sn.History() {
		if prev != nil {
			if n.Ordinal <= prev.Ordinal {
				return fmt.Sprintf("ordinal %d follows %d", n.Ordinal, prev.Ordinal)
			}
			if n.ParentID != prev.ID {
				return fmt.Sprintf("node %s hangs off %q, not %s", n.ID, n.ParentID, prev.ID)
			}
		}
		prev = n
		head = n.ID
	}
	if head == "" {
		return "empty snapshot"
	}
	if !committed[head] {
		return fmt.Sprintf("head %s was never committed", head)
	}
	return ""
}
