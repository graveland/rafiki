// SPDX-License-Identifier: Apache-2.0

package conv_test

import (
	"context"
	"testing"
	"time"

	"github.com/multigres/testkit/assert"

	"go.graveland.dev/rafiki/pkg/conv"
	"go.graveland.dev/rafiki/pkg/conv/convtest"
	"go.graveland.dev/rafiki/pkg/msg"
)

func TestOwnerReadIssuesNoBackendCalls(t *testing.T) {
	c := assert.NewAborting(t)
	b, convID := newConv(t)
	ctx := context.Background()
	counting := convtest.NewCounting(b)
	h := conv.NewStore(counting, conv.Options{}).Open(convID, conv.AsOwner())

	_, err := h.Append(ctx, conv.Append{FromHead: true, Nodes: []conv.NewNode{{Msg: textMsg(msg.User, "a")}}})
	c.NoError(err, "a")
	_, err = h.Append(ctx, conv.Append{FromHead: true, Nodes: []conv.NewNode{{Msg: textMsg(msg.Assistant, "b")}}})
	c.NoError(err, "b")

	counting.Reset()
	path, err := h.Working(ctx)
	c.NoError(err, "Working")
	c.Len(path, 2, "the working path")
	c.Empty(counting.Counts(), "an owner read with everything cached makes no backend call")
}

func TestReaderReadChecksStateOnce(t *testing.T) {
	c := assert.NewAborting(t)
	b, convID := newConv(t)
	ctx := context.Background()
	counting := convtest.NewCounting(b)
	s := conv.NewStore(counting, conv.Options{})
	owner := s.Open(convID, conv.AsOwner())
	_, err := owner.Append(ctx, conv.Append{FromHead: true, Nodes: []conv.NewNode{{Msg: textMsg(msg.User, "a")}}})
	c.NoError(err, "a")
	_, err = owner.Append(ctx, conv.Append{FromHead: true, Nodes: []conv.NewNode{{Msg: textMsg(msg.Assistant, "b")}}})
	c.NoError(err, "b")

	counting.Reset()
	path, err := s.Open(convID).Working(ctx)
	c.NoError(err, "Working")
	c.Len(path, 2, "the working path")
	counts := counting.Counts()
	c.Eq(1, counts["State"], "a reader checks the head once")
	c.Eq(0, counts["Walk"], "the cached path needs no walk")
	c.Eq(0, counts["Fetch"], "nor a fetch")
}

func TestEvictionRefetchesOnlyMissing(t *testing.T) {
	c := assert.NewAborting(t)
	b, convID := newConv(t)
	ctx := context.Background()
	seed := conv.NewStore(b, conv.Options{}).Open(convID, conv.AsOwner())
	var inserted []*conv.Node
	for _, text := range []string{"row 0", "row 1", "row 2"} {
		res, err := seed.Append(ctx, conv.Append{FromHead: true, Nodes: []conv.NewNode{{Msg: textMsg(msg.User, text)}}})
		c.NoError(err, "append %s", text)
		inserted = append(inserted, res.Nodes[0])
	}
	size := int64(len(inserted[0].Content))
	c.Eq(size, int64(len(inserted[2].Content)), "the fixture's nodes are the same size")

	rec := &recorder{Backend: b}
	reader := conv.NewStore(rec, conv.Options{CacheBytes: 2 * size}).Open(convID)
	first, err := reader.Working(ctx)
	c.NoError(err, "first Working")
	c.Len(first, 3, "the first read fetches everything")

	rec.reset()
	second, err := reader.Working(ctx)
	c.NoError(err, "second Working")
	c.Len(second, 3, "the path is complete")
	fetched := rec.fetched()
	c.Len(fetched, 1, "one fetch")
	c.Len(fetched[0], 1, "only the evicted node is re-fetched, not the whole path")
	c.Contains(nodeIDs(second), fetched[0][0], "the fetched node is part of the path")
}

func TestSnapshotSurvivesEviction(t *testing.T) {
	c := assert.NewAborting(t)
	b, convID := newConv(t)
	ctx := context.Background()
	h := conv.NewStore(b, conv.Options{CacheBytes: 1}).Open(convID, conv.AsOwner())
	_, err := h.Append(ctx, conv.Append{FromHead: true, Nodes: []conv.NewNode{{Msg: textMsg(msg.User, "a")}}})
	c.NoError(err, "a")
	_, err = h.Append(ctx, conv.Append{FromHead: true, Nodes: []conv.NewNode{{Msg: textMsg(msg.Assistant, "b")}}})
	c.NoError(err, "b")

	sn, err := h.Snapshot(ctx)
	c.NoError(err, "Snapshot")
	c.Len(historyIDs(sn), 2, "the snapshot holds both nodes")

	for i := 0; i < 4; i++ {
		_, err = h.Append(ctx, conv.Append{FromHead: true, Nodes: []conv.NewNode{{Msg: textMsg(msg.User, "more")}}})
		c.NoError(err, "append %d", i)
	}

	c.Len(historyIDs(sn), 2, "the snapshot is unchanged by eviction")
	for n := range sn.History() {
		c.NotEmpty(n.Content, "an evicted node is still readable through the snapshot")
		c.NotEmpty(n.Msg.Content, "and its decoded message")
	}
}

func TestSnapshotUnaffectedByLaterAppend(t *testing.T) {
	c := assert.NewAborting(t)
	b, convID := newConv(t)
	ctx := context.Background()
	h := conv.NewStore(b, conv.Options{}).Open(convID, conv.AsOwner())
	first, err := h.Append(ctx, conv.Append{FromHead: true, Nodes: []conv.NewNode{{Msg: textMsg(msg.User, "a")}}})
	c.NoError(err, "a")
	head, err := h.Append(ctx, conv.Append{FromHead: true, Nodes: []conv.NewNode{{Msg: textMsg(msg.Assistant, "b")}}})
	c.NoError(err, "b")

	sn, err := h.Snapshot(ctx)
	c.NoError(err, "Snapshot")
	before := historyIDs(sn)

	_, err = h.Append(ctx, conv.Append{FromHead: true, Nodes: []conv.NewNode{{Msg: textMsg(msg.User, "c")}}})
	c.NoError(err, "c")

	c.EqDeep(before, historyIDs(sn), "the snapshot did not grow")
	c.Eq(head.Nodes[0].ID, sn.Head().ID, "nor did its head move")
	c.Eq(first.Nodes[0].ID, before[0], "the snapshot still starts at the root")
}

func TestEntryExpiresAfterIdleTTL(t *testing.T) {
	c := assert.NewAborting(t)
	b, convID := newConv(t)
	ctx := context.Background()
	clock := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	counting := convtest.NewCounting(b)
	h := conv.NewStore(counting, conv.Options{Now: func() time.Time { return clock }}).Open(convID, conv.AsOwner())
	_, err := h.Append(ctx, conv.Append{FromHead: true, Nodes: []conv.NewNode{{Msg: textMsg(msg.User, "a")}}})
	c.NoError(err, "a")

	counting.Reset()
	_, err = h.Working(ctx)
	c.NoError(err, "warm read")
	c.Eq(0, counting.Counts()["State"], "a warm owner entry answers from memory")

	clock = clock.Add(5*time.Minute + time.Second)
	_, err = h.Working(ctx)
	c.NoError(err, "read after the TTL")
	c.Eq(1, counting.Counts()["State"], "an expired entry re-checks the head")
}

// A provider message id is not unique, so the cache never answers a lookup from
// memory: every handle, owner or reader, asks the backend, which orders by
// ordinal.
func TestProviderLookupAlwaysAsksTheBackend(t *testing.T) {
	c := assert.NewAborting(t)
	b, convID := newConv(t)
	ctx := context.Background()
	counting := convtest.NewCounting(b)
	s := conv.NewStore(counting, conv.Options{})
	owner := s.Open(convID, conv.AsOwner())
	res, err := owner.Append(ctx, conv.Append{FromHead: true, Nodes: []conv.NewNode{{
		Msg:               textMsg(msg.Assistant, "b"),
		ProviderMessageID: "msg_indexed",
	}}})
	c.NoError(err, "append")

	counting.Reset()
	node, ok, err := owner.NodeByProviderMessageID(ctx, "msg_indexed")
	c.NoError(err, "owner lookup")
	c.True(ok, "found")
	c.Eq(res.Nodes[0].ID, node.ID, "the node the provider id names")
	c.Eq(1, counting.Counts()["ByProviderMessageID"], "an owner asks the backend too")

	counting.Reset()
	node, ok, err = s.Open(convID).NodeByProviderMessageID(ctx, "msg_indexed")
	c.NoError(err, "reader lookup")
	c.True(ok, "found")
	c.Eq(res.Nodes[0].ID, node.ID, "the node the provider id names")
	c.Eq(1, counting.Counts()["ByProviderMessageID"], "and so does a reader")

	counting.Reset()
	_, ok, err = owner.NodeByProviderMessageID(ctx, "msg_absent")
	c.NoError(err, "absent")
	c.False(ok, "not found")
	c.Eq(1, counting.Counts()["ByProviderMessageID"], "an unknown id is a backend miss")
}

// A writer that pauses between committing its head and publishing it must not
// resurrect the entry the TTL swept out from under it: the publish is dropped,
// and the owner read that follows serves the head the conversation actually has.
func TestSweptEntryIsNeverResurrected(t *testing.T) {
	c := assert.NewAborting(t)
	b, convID := newConv(t)
	ctx := context.Background()
	clk := newClock()
	counting := convtest.NewCounting(b)
	gate := newCommitGate()
	gated := &gatedCommit{Backend: counting, gate: gate}
	s := conv.NewStore(gated, conv.Options{Now: clk.Now, IdleTTL: time.Minute})

	owner := s.Open(convID, conv.AsOwner())
	root, err := owner.Append(ctx, conv.Append{FromHead: true, Nodes: []conv.NewNode{{Msg: textMsg(msg.User, "a")}}})
	c.NoError(err, "a")

	// Writer A commits its head and pauses before publishing it.
	release := pausedAppend(gate, owner, conv.Append{FromHead: true, Nodes: []conv.NewNode{{Msg: textMsg(msg.Assistant, "b")}}})

	// The entry idles past the TTL, so the next handle sweeps it.
	clk.Advance(2 * time.Minute)

	// Writer B resolves a NEW entry, reads the committed head, and commits and
	// publishes its own on top of it.
	other := s.Open(convID, conv.AsOwner())
	newer, err := other.Append(ctx, conv.Append{FromHead: true, Nodes: []conv.NewNode{{Msg: textMsg(msg.Assistant, "c")}}})
	c.NoError(err, "c")

	// A resumes and publishes into an entry that is no longer the conversation's.
	paused, err := release()
	c.NoError(err, "the paused writer")
	c.Eq(1, paused.Inserted, "the paused writer inserted its row")

	counting.Reset()
	path, err := s.Open(convID, conv.AsOwner()).Working(ctx)
	c.NoError(err, "Working")
	c.EqDeep(
		[]string{root.Nodes[0].ID, paused.Nodes[0].ID, newer.Nodes[0].ID},
		nodeIDs(path),
		"the owner read serves the newest committed head",
	)
	c.Empty(counting.Counts(), "and answered from the live entry, not from a resurrected one")
}

// A publish that lands after its entry was swept leaves the live entry's head
// where the live entry put it. A retract makes the two heads differ, so a stale
// publish would be visible as a retracted row coming back as the head.
func TestPublishAfterSweepIsDropped(t *testing.T) {
	c := assert.NewAborting(t)
	b, convID := newConv(t)
	ctx := context.Background()
	clk := newClock()
	counting := convtest.NewCounting(b)
	gate := newCommitGate()
	gated := &gatedCommit{Backend: counting, gate: gate}
	s := conv.NewStore(gated, conv.Options{Now: clk.Now, IdleTTL: time.Minute})

	owner := s.Open(convID, conv.AsOwner())
	root, err := owner.Append(ctx, conv.Append{FromHead: true, Nodes: []conv.NewNode{{Msg: textMsg(msg.User, "a")}}})
	c.NoError(err, "a")

	// Writer A commits a user row and pauses before publishing it.
	release := pausedAppend(gate, owner, conv.Append{FromHead: true, Nodes: []conv.NewNode{{Msg: textMsg(msg.User, "b")}}})

	// The entry idles past the TTL and is swept; B resolves the replacement and
	// moves the head back to the root, which it publishes there.
	clk.Advance(2 * time.Minute)
	other := s.Open(convID, conv.AsOwner())
	st, err := other.State(ctx)
	c.NoError(err, "State")
	c.NotEmpty(st.HeadID, "the paused writer's row is committed")
	c.NoError(other.RetractHead(ctx, st.HeadID), "retract")

	// A resumes: its publish must not move the live entry's head back.
	paused, err := release()
	c.NoError(err, "the paused writer")
	c.Eq(st.HeadID, paused.Nodes[0].ID, "the retracted row is the one A committed")

	counting.Reset()
	path, err := s.Open(convID, conv.AsOwner()).Working(ctx)
	c.NoError(err, "Working")
	c.EqDeep([]string{root.Nodes[0].ID}, nodeIDs(path), "the retracted row did not come back as the head")
	c.Eq(1, counting.Counts()["State"], "and the same-ordinal position was re-read rather than guessed")
}

// A commit that lands after its writer's entry was swept must still reach the
// conversation's live entry. Dropping it would leave that entry loaded and
// permanently stale, answering a head older than the one committed.
func TestCommitAfterSweepRefreshesTheLiveEntry(t *testing.T) {
	c := assert.NewAborting(t)
	b, convID := newConv(t)
	ctx := context.Background()
	clk := newClock()
	counting := convtest.NewCounting(b)
	gate := newCommitGate()
	blocked := &blockedCommit{Backend: counting, gate: gate}
	s := conv.NewStore(blocked, conv.Options{Now: clk.Now, IdleTTL: time.Minute})

	owner := s.Open(convID, conv.AsOwner())
	root, err := owner.Append(ctx, conv.Append{FromHead: true, Nodes: []conv.NewNode{{Msg: textMsg(msg.User, "a")}}})
	c.NoError(err, "a")

	// Writer A resolves H0 and pauses before its commit reaches the backend.
	release := pausedAppend(gate, owner, conv.Append{FromHead: true, Nodes: []conv.NewNode{{Msg: textMsg(msg.Assistant, "b")}}})

	// The entry idles past the TTL and is swept; an owner read creates the
	// replacement and loads H0 into it.
	clk.Advance(2 * time.Minute)
	other := s.Open(convID, conv.AsOwner())
	before, err := other.Working(ctx)
	c.NoError(err, "Working")
	c.EqDeep([]string{root.Nodes[0].ID}, nodeIDs(before), "the live entry starts at H0")

	// A's commit lands, with the entry it resolved already gone.
	committed, err := release()
	c.NoError(err, "the paused writer")
	c.Eq(1, committed.Inserted, "the paused writer inserted its row")

	counting.Reset()
	after, err := other.Working(ctx)
	c.NoError(err, "Working after the commit")
	c.EqDeep([]string{root.Nodes[0].ID, committed.Nodes[0].ID}, nodeIDs(after), "the committed head reached the live entry")
	c.Empty(counting.Counts(), "and was published to it, not left for a backend read")
}

// A retract that lands after its writer's entry was swept must reach the
// conversation's live entry too. A head move leaves the ordinal alone, so a
// replacement created before it holds the row being retracted and would serve it
// as the head until the entry expired.
func TestRetractAfterSweepRefreshesTheLiveEntry(t *testing.T) {
	c := assert.NewAborting(t)
	b, convID := newConv(t)
	ctx := context.Background()
	clk := newClock()
	counting := convtest.NewCounting(b)
	gate := newCommitGate()
	blocked := &blockedCommit{Backend: counting, gate: gate}
	s := conv.NewStore(blocked, conv.Options{Now: clk.Now, IdleTTL: time.Minute})

	owner := s.Open(convID, conv.AsOwner())
	root, err := owner.Append(ctx, conv.Append{FromHead: true, Nodes: []conv.NewNode{{Msg: textMsg(msg.User, "a")}}})
	c.NoError(err, "a")
	retracted, err := owner.Append(ctx, conv.Append{FromHead: true, Nodes: []conv.NewNode{{Msg: textMsg(msg.User, "b")}}})
	c.NoError(err, "b")

	// Writer A resolves the retract and pauses before its commit lands.
	release := pausedWrite(gate, func() error { return owner.RetractHead(ctx, retracted.Nodes[0].ID) })

	// The entry idles past the TTL and is swept; an owner read creates the
	// replacement, which loads the position as it stands — the row to retract is
	// still the head.
	clk.Advance(2 * time.Minute)
	other := s.Open(convID, conv.AsOwner())
	before, err := other.Working(ctx)
	c.NoError(err, "Working")
	c.EqDeep([]string{root.Nodes[0].ID, retracted.Nodes[0].ID}, nodeIDs(before), "the replacement starts at the row to retract")

	// The retract commits, with the entry it resolved already gone.
	c.NoError(release(), "the paused retract")

	after, err := other.Working(ctx)
	c.NoError(err, "Working after the retract")
	c.EqDeep([]string{root.Nodes[0].ID}, nodeIDs(after), "the replacement serves the retracted row's parent")
}

// The answer is always the newest ordinal, even when the cache holds an older
// duplicate of the same provider id: the cache cannot know which duplicates
// exist, so it never answers a lookup itself.
func TestProviderLookupAnswersTheNewestOrdinal(t *testing.T) {
	c := assert.NewAborting(t)
	b, convID := newConv(t)
	ctx := context.Background()
	counting := convtest.NewCounting(b)
	owner := conv.NewStore(counting, conv.Options{}).Open(convID, conv.AsOwner())

	older, err := owner.Append(ctx, conv.Append{FromHead: true, Nodes: []conv.NewNode{{
		Msg:               textMsg(msg.Assistant, "older"),
		ProviderMessageID: "msg_dup",
	}}})
	c.NoError(err, "older")
	newer, err := owner.Append(ctx, conv.Append{FromHead: true, Nodes: []conv.NewNode{{
		Msg:               textMsg(msg.Assistant, "newer"),
		ProviderMessageID: "msg_dup",
	}}})
	c.NoError(err, "newer")
	c.Eq(older.Nodes[0].Ordinal+1, newer.Nodes[0].Ordinal, "the duplicate is the newer row")

	// Roots caches the older duplicate — it is the root — which is the only one
	// a cache could see.
	roots, err := owner.Roots(ctx)
	c.NoError(err, "Roots")
	c.Require().Len(roots, 1, "one root")
	c.Eq(older.Nodes[0].ID, roots[0].ID, "the older duplicate is cached")

	counting.Reset()
	node, ok, err := owner.NodeByProviderMessageID(ctx, "msg_dup")
	c.NoError(err, "NodeByProviderMessageID")
	c.True(ok, "found")
	c.Eq(newer.Nodes[0].ID, node.ID, "the newest ordinal, not the cached older duplicate")
	c.Eq(1, counting.Counts()["ByProviderMessageID"], "the backend answered it")
}

func TestStateIsNeverCached(t *testing.T) {
	c := assert.NewAborting(t)
	b, convID := newConv(t)
	ctx := context.Background()
	counting := convtest.NewCounting(b)
	h := conv.NewStore(counting, conv.Options{}).Open(convID, conv.AsOwner())
	_, err := h.Append(ctx, conv.Append{FromHead: true, Nodes: []conv.NewNode{{Msg: textMsg(msg.User, "a")}}})
	c.NoError(err, "a")

	counting.Reset()
	_, err = h.State(ctx)
	c.NoError(err, "first State")
	_, err = h.State(ctx)
	c.NoError(err, "second State")
	c.Eq(2, counting.Counts()["State"], "every State call is a backend call, even for an owner")
}

func TestMemBackendConformance(t *testing.T) {
	convtest.RunBackendSuite(t, func(t *testing.T) (conv.Backend, func(string)) {
		t.Helper()
		m := conv.NewMemBackend()
		return m, m.Ensure
	})
}

// A reader's observation that lands after a retract carries the SAME next
// ordinal as the state the retract published, so only a generation can order
// them: the retracted row must not come back as the head.
func TestReaderStateCannotResurrectARetractedHead(t *testing.T) {
	c := assert.NewAborting(t)
	b, convID := newConv(t)
	ctx := context.Background()
	stalled := &stalledState{Backend: b, entered: make(chan struct{}), release: make(chan struct{})}
	s := conv.NewStore(stalled, conv.Options{})
	owner := s.Open(convID, conv.AsOwner())

	first, err := owner.Append(ctx, conv.Append{FromHead: true, Nodes: []conv.NewNode{{Msg: textMsg(msg.User, "one")}}})
	c.NoError(err, "one")
	second, err := owner.Append(ctx, conv.Append{FromHead: true, Nodes: []conv.NewNode{{Msg: textMsg(msg.User, "two")}}})
	c.NoError(err, "two")

	// The reader reads the pre-retract head, then stalls until the retract has
	// committed and published.
	stalled.arm()
	reader := s.Open(convID)
	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, err := reader.Working(ctx); err != nil {
			c.Errorf("reader Working: %v", err)
		}
	}()
	<-stalled.entered
	c.NoError(owner.RetractHead(ctx, second.Nodes[0].ID), "retract")
	close(stalled.release)
	<-done

	path, err := owner.Working(ctx)
	c.NoError(err, "Working")
	c.EqDeep([]string{first.Nodes[0].ID}, nodeIDs(path), "the retracted row did not come back")
}

// A head a reader loaded must not be inherited by the owner that takes over.
func TestOwnerAttachInvalidatesCachedHead(t *testing.T) {
	c := assert.NewAborting(t)
	b, convID := newConv(t)
	ctx := context.Background()
	_, err := conv.NewStore(b, conv.Options{}).Open(convID, conv.AsOwner()).Append(ctx, conv.Append{
		FromHead: true,
		Nodes:    []conv.NewNode{{Msg: textMsg(msg.User, "a")}},
	})
	c.NoError(err, "append")

	// Only a reader has touched this store's entry: the head it loaded must not
	// be inherited by the owner that attaches next.
	counting := convtest.NewCounting(b)
	s := conv.NewStore(counting, conv.Options{})
	_, err = s.Open(convID).Working(ctx)
	c.NoError(err, "reader read")

	counting.Reset()
	owner := s.Open(convID, conv.AsOwner())
	_, err = owner.Working(ctx)
	c.NoError(err, "owner read")
	c.Eq(1, counting.Counts()["State"], "the owner transition re-reads the head")

	counting.Reset()
	_, err = owner.Working(ctx)
	c.NoError(err, "second owner read")
	c.Eq(0, counting.Counts()["State"], "and then trusts it")
}

// A read in flight against an entry that holds no position yet must not store
// its answer once a commit has published into that entry: the commit's publish
// bumps the generation even though there was no position to replace.
func TestCommitPublishesIntoAnEmptyEntry(t *testing.T) {
	c := assert.NewAborting(t)
	b, convID := newConv(t)
	ctx := context.Background()
	clk := newClock()
	gate := newCommitGate()
	stalled := &stalledState{Backend: &blockedCommit{Backend: b, gate: gate}, entered: make(chan struct{}), release: make(chan struct{})}
	s := conv.NewStore(stalled, conv.Options{Now: clk.Now, IdleTTL: time.Minute})

	owner := s.Open(convID, conv.AsOwner())
	root, err := owner.Append(ctx, conv.Append{FromHead: true, Nodes: []conv.NewNode{{Msg: textMsg(msg.User, "a")}}})
	c.NoError(err, "a")

	// A resolves E and pauses before its commit.
	releaseA := pausedAppend(gate, owner, conv.Append{FromHead: true, Nodes: []conv.NewNode{{Msg: textMsg(msg.Assistant, "b")}}})

	// E expires; owner B creates E2, captures its generation, reads H0 and
	// pauses before returning.
	clk.Advance(2 * time.Minute)
	stalled.arm()
	other := s.Open(convID, conv.AsOwner())
	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, err := other.Working(ctx); err != nil {
			c.Errorf("B Working: %v", err)
		}
	}()
	<-stalled.entered

	// A commits H1 while E2 has no position.
	committed, err := releaseA()
	c.NoError(err, "A")

	close(stalled.release)
	<-done

	after, err := other.Working(ctx)
	c.NoError(err, "owner Working")
	c.EqDeep([]string{root.Nodes[0].ID, committed.Nodes[0].ID}, nodeIDs(after), "the owner read answers the committed head, not H0")
}

// A writer's own State observation, made while resolving, must not overwrite a
// newer committed head that was published into the entry in the meantime.
func TestWriterObservationCannotOverwriteANewerCommit(t *testing.T) {
	c := assert.NewAborting(t)
	b, convID := newConv(t)
	ctx := context.Background()
	clk := newClock()
	counting := convtest.NewCounting(b)
	gateB := newCommitGate()
	inner := &blockedCommit{Backend: counting, gate: gateB}
	gateA := newCommitGate()
	outer := &blockedCommit{Backend: inner, gate: gateA}
	stalled := &stalledState{Backend: outer, entered: make(chan struct{}), release: make(chan struct{})}
	s := conv.NewStore(stalled, conv.Options{Now: clk.Now, IdleTTL: time.Minute})

	owner := s.Open(convID, conv.AsOwner())
	root, err := owner.Append(ctx, conv.Append{FromHead: true, Nodes: []conv.NewNode{{Msg: textMsg(msg.User, "a")}}})
	c.NoError(err, "a")

	// A resolves E and pauses before committing H1.
	releaseA := pausedAppend(gateA, owner, conv.Append{FromHead: true, Nodes: []conv.NewNode{{Msg: textMsg(msg.Assistant, "b")}}})

	// E expires; B loads H0 into E2, then starts a RetractHead whose State reads
	// H0 and pauses.
	clk.Advance(2 * time.Minute)
	other := s.Open(convID, conv.AsOwner())
	_, err = other.Working(ctx)
	c.NoError(err, "B loads H0")
	stalled.arm()
	retractDone := make(chan error, 1)
	go func() { retractDone <- other.RetractHead(ctx, root.Nodes[0].ID) }()
	<-stalled.entered

	// A commits H1; its publish reaches E2.
	committed, err := releaseA()
	c.NoError(err, "A")
	gateB.arm()

	// B resumes and runs on to its own commit, which is held so the cache can be
	// inspected right after the observation was stored or dropped.
	close(stalled.release)
	<-gateB.entered

	counting.Reset()
	during, err := other.Working(ctx)
	c.NoError(err, "owner Working")
	c.EqDeep([]string{root.Nodes[0].ID, committed.Nodes[0].ID}, nodeIDs(during), "the stalled observation did not overwrite the committed head")
	c.Eq(0, counting.Counts()["State"], "and the entry still answers from the published head")

	close(gateB.release)
	c.Require().NotNil(<-retractDone, "the retract of a row that is no longer the head is refused")
}

// A retract leaves the ordinal alone, so the ordinal cannot order it against an
// earlier position: the next read must see it, and an observation that was in
// flight before it must not undo it.
func TestEqualOrdinalPublishOrdersByGeneration(t *testing.T) {
	c := assert.NewAborting(t)
	b, convID := newConv(t)
	ctx := context.Background()
	counting := convtest.NewCounting(b)
	stalled := &stalledState{Backend: counting, entered: make(chan struct{}), release: make(chan struct{})}
	s := conv.NewStore(stalled, conv.Options{})
	owner := s.Open(convID, conv.AsOwner())

	first, err := owner.Append(ctx, conv.Append{FromHead: true, Nodes: []conv.NewNode{{Msg: textMsg(msg.User, "one")}}})
	c.NoError(err, "one")
	second, err := owner.Append(ctx, conv.Append{FromHead: true, Nodes: []conv.NewNode{{Msg: textMsg(msg.User, "two")}}})
	c.NoError(err, "two")

	stalled.arm()
	reader := s.Open(convID)
	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, err := reader.Working(ctx); err != nil {
			c.Errorf("reader Working: %v", err)
		}
	}()
	<-stalled.entered
	c.NoError(owner.RetractHead(ctx, second.Nodes[0].ID), "retract")

	counting.Reset()
	seen, err := owner.Working(ctx)
	c.NoError(err, "Working after the retract")
	c.EqDeep([]string{first.Nodes[0].ID}, nodeIDs(seen), "the next read sees the retract")
	c.Eq(1, counting.Counts()["State"], "by asking the backend")

	close(stalled.release)
	<-done

	counting.Reset()
	later, err := owner.Working(ctx)
	c.NoError(err, "Working after the stalled observation")
	c.EqDeep([]string{first.Nodes[0].ID}, nodeIDs(later), "the stalled observation did not undo the retract")
	c.Eq(0, counting.Counts()["State"], "and the entry still answers from the position the read stored")
}
