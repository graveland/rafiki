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
