// SPDX-License-Identifier: Apache-2.0

package conv_test

import (
	"context"
	"testing"

	"github.com/multigres/testkit/assert"

	"go.graveland.dev/rafiki/pkg/conv"
	"go.graveland.dev/rafiki/pkg/conv/convtest"
	"go.graveland.dev/rafiki/pkg/msg"
)

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
// ordinal as the state the retract installed, so only the version can order
// them: the retracted row must not come back as the head.
func TestReaderStateCannotResurrectARetractedHead(t *testing.T) {
	c := assert.NewAborting(t)
	b, convID := newConv(t)
	ctx := context.Background()
	seed := conv.NewStore(b, conv.Options{}).Open(convID, conv.AsOwner())
	stalled := &stalledState{Backend: b, entered: make(chan struct{}), release: make(chan struct{})}
	s := conv.NewStore(stalled, conv.Options{})
	owner := s.Open(convID, conv.AsOwner())

	first, err := seed.Append(ctx, conv.Append{FromHead: true, Nodes: []conv.NewNode{{Msg: textMsg(msg.User, "one")}}})
	c.NoError(err, "one")
	second, err := seed.Append(ctx, conv.Append{FromHead: true, Nodes: []conv.NewNode{{Msg: textMsg(msg.User, "two")}}})
	c.NoError(err, "two")

	// The reader reads the pre-retract head, then stalls until the retract has
	// committed and refreshed.
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

func TestNodeCacheStillAvoidsFetches(t *testing.T) {
	c := assert.NewAborting(t)
	b, convID := newConv(t)
	ctx := context.Background()
	seed := conv.NewStore(b, conv.Options{}).Open(convID, conv.AsOwner())
	_, err := seed.Append(ctx, conv.Append{FromHead: true, Nodes: []conv.NewNode{{Msg: textMsg(msg.User, "a")}}})
	c.NoError(err, "a")
	_, err = seed.Append(ctx, conv.Append{FromHead: true, Nodes: []conv.NewNode{{Msg: textMsg(msg.Assistant, "b")}}})
	c.NoError(err, "b")

	counting := convtest.NewCounting(b)
	h := conv.NewStore(counting, conv.Options{}).Open(convID)
	_, err = h.Working(ctx)
	c.NoError(err, "cold Working")
	c.True(counting.Counts()["Walk"] > 0, "the cold read walks")

	counting.Reset()
	path, err := h.Working(ctx)
	c.NoError(err, "warm Working")
	c.Len(path, 2, "the working path")
	counts := counting.Counts()
	c.Eq(0, counts["Walk"], "the warm read needs no walk")
	c.Eq(0, counts["Fetch"], "nor a fetch")
}

func TestCommitIsVisibleToTheNextRead(t *testing.T) {
	c := assert.NewAborting(t)
	b, convID := newConv(t)
	ctx := context.Background()
	s := conv.NewStore(b, conv.Options{})
	writer := s.Open(convID, conv.AsOwner())
	reader := s.Open(convID)

	first, err := writer.Append(ctx, conv.Append{FromHead: true, Nodes: []conv.NewNode{{Msg: textMsg(msg.User, "a")}}})
	c.NoError(err, "a")
	path, err := reader.Working(ctx)
	c.NoError(err, "Working")
	c.EqDeep([]string{first.Nodes[0].ID}, nodeIDs(path), "the reader sees the first commit")

	second, err := writer.Append(ctx, conv.Append{FromHead: true, Nodes: []conv.NewNode{{Msg: textMsg(msg.User, "b")}}})
	c.NoError(err, "b")
	path, err = reader.Working(ctx)
	c.NoError(err, "Working")
	c.EqDeep([]string{first.Nodes[0].ID, second.Nodes[0].ID}, nodeIDs(path), "the next read sees the new head")

	c.NoError(writer.RetractHead(ctx, second.Nodes[0].ID), "retract")
	path, err = reader.Working(ctx)
	c.NoError(err, "Working")
	c.EqDeep([]string{first.Nodes[0].ID}, nodeIDs(path), "and the retract")
}
