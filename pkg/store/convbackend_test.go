// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
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

	// The fence runs before the comparison, so a LIVE lease must leave the
	// conflict a conflict: this writer still holds the conversation.
	live, ok, err := NewLeases(pool).Acquire(ctx, convID, "daemon-holder", 5*time.Minute)
	c.NoError(err, "acquire a live lease")
	c.True(ok, "the conversation is free")
	row = commitRow(t, convID, "user", "a stale head under a live lease", 1, 1)
	_, err = b.Commit(ctx, conv.Commit{
		ConversationID:    convID,
		ExpectHead:        "",
		ExpectNextOrdinal: 0,
		Rows:              []*conv.Node{row},
		NewHead:           row.ID,
		Lease:             live,
	})
	c.ErrorIs(err, conv.ErrConflict, "a stale head under a live lease is a conflict, not a lease loss")
	c.Eq(1, countNodes(t, pool, convID), "the refused commit inserted nothing either")
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

	// A taken-over writer usually has a stale head as well. The fence is checked
	// first, so it hears "you lost the lease" — the answer that ends it — rather
	// than a conflict the caller's CAS loop would retry and then misreport.
	handle := conv.NewStore(b, conv.Options{}).Open(convID)
	_, first := convPair(t, "user", "appended while unfenced")
	_, err = handle.Append(ctx, conv.Append{FromHead: true, Nodes: []conv.NewNode{first}})
	c.NoError(err, "append")

	stale := commitRow(t, convID, "user", "a stale head behind a dead lease", 1, 1)
	_, err = b.Commit(ctx, conv.Commit{
		ConversationID:    convID,
		ExpectHead:        "",
		ExpectNextOrdinal: 0,
		Rows:              []*conv.Node{stale},
		NewHead:           stale.ID,
		Lease:             expired,
	})
	c.ErrorIs(err, ErrLeaseLost, "a lost lease outranks a stale head")
	c.False(errors.Is(err, conv.ErrConflict), "so the caller cannot retry it as a conflict")
	c.Eq(1, countNodes(t, pool, convID), "the fenced commit inserted nothing")
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

// TestConvBackendFetchReturnsTheStoredRendering pins Content to the column's
// own text. jsonb re-renders what it was given, so a backend that answered with
// a re-encode of the decoded message would look identical through convtree.Canon
// while quietly normalising away whatever the codec had written — and the
// compaction tail re-stores Content verbatim, so a codec bug must stay visible.
func TestConvBackendFetchReturnsTheStoredRendering(t *testing.T) {
	c := assert.NewAborting(t)
	pool := leasePool(t)
	ctx := context.Background()
	convID := cleanConversation(t, pool)
	b := NewConvBackend(pool)
	handle := conv.NewStore(b, conv.Options{}).Open(convID, conv.AsOwner())

	_, hello := convPair(t, "user", "hello")
	res, err := handle.Append(ctx, conv.Append{FromHead: true, Nodes: []conv.NewNode{hello}})
	c.NoError(err, "append")
	node := res.Nodes[0]

	var stored string
	err = pool.QueryRow(ctx, `
		SELECT content::text FROM conversations.conversation_message WHERE id = $1::uuid`,
		node.ID).Scan(&stored)
	c.NoError(err, "read the stored content")

	fetched, err := b.Fetch(ctx, convID, []string{node.ID})
	c.NoError(err, "Fetch")
	c.Len(fetched, 1, "one node")
	c.Eq(stored, string(fetched[0].Content), "Content is the column's rendering")
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
// writer that lost its lease cannot write, a takeover waits for a writer
// transaction already in flight, and a reader never serves a head it did not
// re-check.
//
// The ordering is forced rather than hoped for: the reader must complete a
// snapshot before the lease is expired, and the decisive snapshot must have
// BEGUN after C's append returned. A reader that answered from a cached head
// would hand back A's head for that snapshot and fail, however the timing fell.
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

	// committed is read only by the test goroutine, which is what lets the
	// reader's snapshots be validated in the order they were taken.
	committed := make(map[string]bool, 5)
	for _, n := range first.Nodes {
		committed[n.ID] = true
	}

	reader := conv.NewStore(NewConvBackend(pool), conv.Options{})
	readerHandle := reader.Open(convID)
	reports := make(chan snapshotReport, 256)
	stop := make(chan struct{})
	done := make(chan struct{})
	var begun atomic.Int64
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
			}
			started := begun.Add(1)
			sn, err := readerHandle.Snapshot(ctx)
			select {
			case reports <- snapshotReport{started: started, snapshot: sn, err: err}:
			case <-stop:
				return
			}
			time.Sleep(time.Millisecond)
		}
	}()

	// The reader must have taken a real snapshot BEFORE the takeover, or a
	// reader that simply started late would satisfy the assertion at the end.
	select {
	case rep := <-reports:
		checkSnapshot(t, rep, committed)
	case <-time.After(10 * time.Second):
		t.Fatal("the reader took no snapshot before the takeover")
	}

	// A Commit already in flight holds the takeover out: the fence's FOR SHARE
	// on the lease row is what the takeover's UPDATE waits for. Held here as a
	// direct transaction, with the two statements Commit opens with.
	tx, err := pool.Begin(ctx)
	c.NoError(err, "begin the in-flight writer transaction")
	var (
		heldHead  string
		heldNext  int
		fencedOne int
	)
	c.NoError(tx.QueryRow(ctx, `
		SELECT coalesce(head_id::text, ''), next_ordinal
		  FROM conversations.conversation WHERE id = $1::uuid FOR UPDATE`, convID).
		Scan(&heldHead, &heldNext), "lock the conversation")
	c.NoError(tx.QueryRow(ctx, `
		SELECT 1 FROM conversations.conversation_lease
		 WHERE conversation_id = $1::uuid AND holder = $2 AND token = $3::uuid
		   AND expires_at > now() FOR SHARE`, convID, leaseA.Holder, leaseA.Token).
		Scan(&fencedOne), "fence under FOR SHARE")

	takeover := make(chan takeoverResult, 1)
	go func() { takeover <- takeLease(ctx, pool, convID, "daemon-c") }()
	select {
	case res := <-takeover:
		t.Fatalf("the takeover did not wait for the writer transaction: %v", res.err)
	case <-time.After(250 * time.Millisecond):
	}
	c.NoError(tx.Commit(ctx), "the in-flight writer transaction ends")

	var leaseC Lease
	select {
	case res := <-takeover:
		c.NoError(res.err, "the takeover completes once the writer transaction ends")
		leaseC = res.lease
	case <-time.After(10 * time.Second):
		t.Fatal("the takeover never completed after the writer transaction ended")
	}

	writerC := conv.NewStore(NewConvBackend(pool), conv.Options{})
	handleC := writerC.Open(convID, conv.AsOwner(), conv.WithLease(leaseC))
	_, afterOne := convPair(t, "assistant", "after the takeover")
	_, afterTwo := convPair(t, "user", "and again")
	second, err := handleC.Append(ctx, conv.Append{
		FromHead: true,
		Nodes:    []conv.NewNode{afterOne, afterTwo},
	})
	c.NoError(err, "C appends after the takeover")
	for _, n := range second.Nodes {
		committed[n.ID] = true
	}

	_, stale := convPair(t, "assistant", "a stale writer")
	_, err = handleA.Append(ctx, conv.Append{FromHead: true, Nodes: []conv.NewNode{stale}})
	c.ErrorIs(err, ErrLeaseLost, "the writer that lost its lease is fenced")

	// The decisive snapshot has to have BEGUN after C's append returned, so it
	// cannot be one that was already in flight, and its head is C's.
	mark := begun.Load()
	var decisive *conv.Snapshot
	deadline := time.After(10 * time.Second)
	for decisive == nil {
		select {
		case rep := <-reports:
			sn := checkSnapshot(t, rep, committed)
			if rep.started > mark {
				decisive = sn
			}
		case <-deadline:
			t.Fatal("the reader took no snapshot after the takeover")
		}
	}
	c.Eq(second.Nodes[len(second.Nodes)-1].ID, decisive.Head().ID,
		"a reader re-checks the head and sees C's")

	close(stop)
	<-done
}

// snapshotReport is one reader snapshot, stamped with the sequence number of
// the iteration that took it.
type snapshotReport struct {
	started  int64
	snapshot *conv.Snapshot
	err      error
}

// takeoverResult is what the takeover goroutine hands back.
type takeoverResult struct {
	lease Lease
	err   error
}

// takeLease expires the conversation's lease and acquires it for holder: what a
// second daemon does once it decides the first is gone.
func takeLease(ctx context.Context, pool *pgxpool.Pool, convID, holder string) takeoverResult {
	if _, err := pool.Exec(ctx, `
		UPDATE conversations.conversation_lease SET expires_at = now() - interval '1 second'
		 WHERE conversation_id = $1::uuid`, convID); err != nil {
		return takeoverResult{err: fmt.Errorf("expire the lease: %w", err)}
	}
	lease, ok, err := NewLeases(pool).Acquire(ctx, convID, holder, 5*time.Minute)
	if err != nil {
		return takeoverResult{err: fmt.Errorf("acquire %s: %w", holder, err)}
	}
	if !ok {
		return takeoverResult{err: fmt.Errorf("%s did not get the lease", holder)}
	}
	return takeoverResult{lease: lease}
}

// checkSnapshot fails the test when one reader snapshot is inconsistent: the
// chain must link up, ordinals must strictly increase, and the head must be a
// node some writer actually committed.
func checkSnapshot(t *testing.T, rep snapshotReport, committed map[string]bool) *conv.Snapshot {
	t.Helper()
	c := assert.NewAborting(t)
	c.NoError(rep.err, "the reader's snapshot")
	var prev *conv.Node
	head := ""
	for n := range rep.snapshot.History() {
		if prev != nil {
			c.Eq(prev.ID, n.ParentID, "node %s hangs off its predecessor", n.ID)
			c.Greater(prev.Ordinal, n.Ordinal, "ordinals strictly increase")
		}
		prev = n
		head = n.ID
	}
	c.NotEq("", head, "the snapshot is not empty")
	c.True(committed[head], "head %s was committed by a writer", head)
	return rep.snapshot
}
