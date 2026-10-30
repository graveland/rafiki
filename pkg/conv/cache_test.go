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

func TestProviderIndexResolvesWithoutQuery(t *testing.T) {
	c := assert.NewAborting(t)
	b, convID := newConv(t)
	ctx := context.Background()
	counting := convtest.NewCounting(b)
	h := conv.NewStore(counting, conv.Options{}).Open(convID, conv.AsOwner())
	res, err := h.Append(ctx, conv.Append{FromHead: true, Nodes: []conv.NewNode{{
		Msg:               textMsg(msg.Assistant, "b"),
		ProviderMessageID: "msg_indexed",
	}}})
	c.NoError(err, "append")

	counting.Reset()
	node, ok, err := h.NodeByProviderMessageID(ctx, "msg_indexed")
	c.NoError(err, "NodeByProviderMessageID")
	c.True(ok, "found")
	c.Eq(res.Nodes[0].ID, node.ID, "the indexed node")
	c.Empty(counting.Counts(), "the conversation's own index answered it")

	counting.Reset()
	_, ok, err = h.NodeByProviderMessageID(ctx, "msg_absent")
	c.NoError(err, "absent")
	c.False(ok, "not found")
	c.Eq(1, counting.Counts()["ByProviderMessageID"], "an unknown id falls through to the backend")
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
	gated := &gatedCommit{Backend: counting, entered: make(chan struct{}), release: make(chan struct{})}
	s := conv.NewStore(gated, conv.Options{Now: clk.Now, IdleTTL: time.Minute})

	owner := s.Open(convID, conv.AsOwner())
	root, err := owner.Append(ctx, conv.Append{FromHead: true, Nodes: []conv.NewNode{{Msg: textMsg(msg.User, "a")}}})
	c.NoError(err, "a")

	// Writer A commits its head and pauses before publishing it.
	release := pausedAppend(gated, owner, conv.Append{FromHead: true, Nodes: []conv.NewNode{{Msg: textMsg(msg.Assistant, "b")}}})

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
	gated := &gatedCommit{Backend: counting, entered: make(chan struct{}), release: make(chan struct{})}
	s := conv.NewStore(gated, conv.Options{Now: clk.Now, IdleTTL: time.Minute})

	owner := s.Open(convID, conv.AsOwner())
	root, err := owner.Append(ctx, conv.Append{FromHead: true, Nodes: []conv.NewNode{{Msg: textMsg(msg.User, "a")}}})
	c.NoError(err, "a")

	// Writer A commits a user row and pauses before publishing it.
	release := pausedAppend(gated, owner, conv.Append{FromHead: true, Nodes: []conv.NewNode{{Msg: textMsg(msg.User, "b")}}})

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
	c.Empty(counting.Counts(), "and the live entry answered it")
}

// The provider index is an owner-only optimisation: a reader handle asks the
// backend even with a warm index, because a provider id is not unique and only
// the backend orders by ordinal.
func TestProviderIndexIsOwnerOnly(t *testing.T) {
	c := assert.NewAborting(t)
	b, convID := newConv(t)
	ctx := context.Background()
	counting := convtest.NewCounting(b)
	s := conv.NewStore(counting, conv.Options{})
	owner := s.Open(convID, conv.AsOwner())
	res, err := owner.Append(ctx, conv.Append{FromHead: true, Nodes: []conv.NewNode{{
		Msg:               textMsg(msg.Assistant, "b"),
		ProviderMessageID: "msg_shared",
	}}})
	c.NoError(err, "append")

	counting.Reset()
	node, ok, err := s.Open(convID).NodeByProviderMessageID(ctx, "msg_shared")
	c.NoError(err, "reader lookup")
	c.True(ok, "found")
	c.Eq(res.Nodes[0].ID, node.ID, "the node the provider id names")
	c.Eq(1, counting.Counts()["ByProviderMessageID"], "a reader asks the backend with the index warm")

	counting.Reset()
	node, ok, err = owner.NodeByProviderMessageID(ctx, "msg_shared")
	c.NoError(err, "owner lookup")
	c.True(ok, "found")
	c.Eq(res.Nodes[0].ID, node.ID, "the node the provider id names")
	c.Empty(counting.Counts(), "an owner answers from its own index")
}

// The index must never answer with an older duplicate. Once the newest node
// carrying a provider id has been evicted, the id it remembered is forgotten but
// its ordinal is not, so re-fetching the older node cannot become the answer.
func TestProviderIndexNeverAnswersOlderThanNewest(t *testing.T) {
	c := assert.NewAborting(t)
	b, convID := newConv(t)
	ctx := context.Background()

	// One node's worth of cache, measured on a scratch conversation.
	scratch := convID + "-size"
	b.Ensure(scratch)
	sized, err := conv.NewStore(b, conv.Options{}).Open(scratch, conv.AsOwner()).Append(ctx, conv.Append{
		FromHead: true,
		Nodes:    []conv.NewNode{{Msg: textMsg(msg.Assistant, "older")}},
	})
	c.NoError(err, "size probe")
	budget := int64(len(sized.Nodes[0].Content))

	counting := convtest.NewCounting(b)
	owner := conv.NewStore(counting, conv.Options{CacheBytes: budget}).Open(convID, conv.AsOwner())
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
	c.Eq(budget, int64(len(older.Nodes[0].Content)), "the older node fills the cache on its own")
	c.Eq(budget, int64(len(newer.Nodes[0].Content)), "so does the newer one")

	// Evict the newer node by caching a third, then re-fetch the older root: the
	// older duplicate is now the only node in the cache carrying the id.
	third, err := owner.Append(ctx, conv.Append{FromHead: true, Nodes: []conv.NewNode{{Msg: textMsg(msg.Assistant, "third")}}})
	c.NoError(err, "third")
	c.Eq(budget, int64(len(third.Nodes[0].Content)), "and so does the third, which is what evicts the newer node")
	roots, err := owner.Roots(ctx)
	c.NoError(err, "Roots")
	c.Require().Len(roots, 1, "one root")
	c.Eq(older.Nodes[0].ID, roots[0].ID, "the older node is the root, and is cached again")

	counting.Reset()
	node, ok, err := owner.NodeByProviderMessageID(ctx, "msg_dup")
	c.NoError(err, "NodeByProviderMessageID")
	c.True(ok, "found")
	c.Eq(newer.Nodes[0].ID, node.ID, "the newest ordinal, not the older duplicate")
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
