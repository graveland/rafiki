// SPDX-License-Identifier: Apache-2.0

package conv_test

import (
	"context"
	"testing"

	"github.com/multigres/testkit/assert"

	"go.graveland.dev/rafiki/pkg/conv"
	"go.graveland.dev/rafiki/pkg/msg"
)

func TestWorkingStopsAtNearestRoot(t *testing.T) {
	c := assert.NewAborting(t)
	b, convID := newConv(t)
	ctx := context.Background()
	h := conv.NewStore(b, conv.Options{}).Open(convID, conv.AsOwner())

	first, err := h.Append(ctx, conv.Append{FromHead: true, Nodes: []conv.NewNode{{Msg: textMsg(msg.User, "a")}}})
	c.NoError(err, "a")
	second, err := h.Append(ctx, conv.Append{FromHead: true, Nodes: []conv.NewNode{{Msg: textMsg(msg.Assistant, "b")}}})
	c.NoError(err, "b")
	boundary, err := h.Append(ctx, conv.Append{
		Root:          conv.KindCompactionSummary,
		DerivedFromID: second.Nodes[0].ID,
		Nodes:         []conv.NewNode{{Msg: textMsg(msg.User, "summary")}},
	})
	c.NoError(err, "boundary")
	last, err := h.Append(ctx, conv.Append{FromHead: true, Nodes: []conv.NewNode{{Msg: textMsg(msg.Assistant, "d")}}})
	c.NoError(err, "d")

	working, err := h.Working(ctx)
	c.NoError(err, "Working")
	c.EqDeep([]string{boundary.Nodes[0].ID, last.Nodes[0].ID}, nodeIDs(working), "the suffix from the nearest root")

	sn, err := h.Snapshot(ctx)
	c.NoError(err, "Snapshot")
	fromSnapshot := []string{}
	for n := range sn.Working() {
		fromSnapshot = append(fromSnapshot, n.ID)
	}
	c.EqDeep(nodeIDs(working), fromSnapshot, "the snapshot's working view agrees")
	c.EqDeep([]string{first.Nodes[0].ID, second.Nodes[0].ID, boundary.Nodes[0].ID, last.Nodes[0].ID},
		historyIDs(sn), "history is the whole tree")
	c.Eq(last.Nodes[0].ID, sn.Head().ID, "the head")
}

func TestHistoryCrossesDerivedFrom(t *testing.T) {
	c := assert.NewAborting(t)
	b, convID := newConv(t)
	ctx := context.Background()
	h := conv.NewStore(b, conv.Options{}).Open(convID, conv.AsOwner())

	first, err := h.Append(ctx, conv.Append{FromHead: true, Nodes: []conv.NewNode{{Msg: textMsg(msg.User, "a")}}})
	c.NoError(err, "a")
	second, err := h.Append(ctx, conv.Append{FromHead: true, Nodes: []conv.NewNode{{Msg: textMsg(msg.Assistant, "b")}}})
	c.NoError(err, "b")
	compaction, err := h.Append(ctx, conv.Append{
		Root:          conv.KindCompactionSummary,
		DerivedFromID: second.Nodes[0].ID,
		Nodes:         []conv.NewNode{{Msg: textMsg(msg.User, "summary")}},
	})
	c.NoError(err, "compaction")
	cleared, err := h.Append(ctx, conv.Append{
		Root:          conv.KindClear,
		DerivedFromID: compaction.Nodes[0].ID,
		Nodes:         []conv.NewNode{{Msg: textMsg(msg.User, "cleared")}},
	})
	c.NoError(err, "clear")

	// A cold store proves the walk itself crosses both links.
	sn, err := conv.NewStore(b, conv.Options{}).Open(convID).Snapshot(ctx)
	c.NoError(err, "Snapshot")
	c.EqDeep([]string{first.Nodes[0].ID, second.Nodes[0].ID, compaction.Nodes[0].ID, cleared.Nodes[0].ID},
		historyIDs(sn), "history crosses every derived-from link")

	working, err := h.Working(ctx)
	c.NoError(err, "Working")
	c.EqDeep([]string{cleared.Nodes[0].ID}, nodeIDs(working), "working stops at the clear")
}

func TestTranscriptDropsCompactionTail(t *testing.T) {
	c := assert.NewAborting(t)
	b, convID := newConv(t)
	ctx := context.Background()
	h := conv.NewStore(b, conv.Options{}).Open(convID, conv.AsOwner())

	root, err := h.Append(ctx, conv.Append{FromHead: true, Nodes: []conv.NewNode{{Msg: textMsg(msg.User, "a")}}})
	c.NoError(err, "a")
	tail, err := h.Append(ctx, conv.Append{FromHead: true, Nodes: []conv.NewNode{{
		Msg:  textMsg(msg.User, "a"),
		Kind: conv.KindCompactionTail,
	}}})
	c.NoError(err, "tail")
	last, err := h.Append(ctx, conv.Append{FromHead: true, Nodes: []conv.NewNode{{Msg: textMsg(msg.Assistant, "b")}}})
	c.NoError(err, "b")

	sn, err := h.Snapshot(ctx)
	c.NoError(err, "Snapshot")
	c.EqDeep([]string{root.Nodes[0].ID, tail.Nodes[0].ID, last.Nodes[0].ID}, historyIDs(sn), "history keeps the tail")
	c.EqDeep([]string{root.Nodes[0].ID, last.Nodes[0].ID}, transcriptIDs(sn), "transcript drops it")
	c.Eq(conv.KindCompactionTail, tail.Nodes[0].Kind, "the tail's kind")

	page, err := sn.Page(conv.PageQuery{})
	c.NoError(err, "Page")
	c.EqDeep([]int{0, 2}, ordinals(page), "the tail is not paged")
}

func TestTranscriptOmitsDeadBranch(t *testing.T) {
	c := assert.NewAborting(t)
	b, convID := newConv(t)
	ctx := context.Background()
	h := conv.NewStore(b, conv.Options{}).Open(convID, conv.AsOwner())

	root, err := h.Append(ctx, conv.Append{FromHead: true, Nodes: []conv.NewNode{{Msg: textMsg(msg.User, "a")}}})
	c.NoError(err, "a")
	retracted, err := h.Append(ctx, conv.Append{FromHead: true, Nodes: []conv.NewNode{{Msg: textMsg(msg.User, "b")}}})
	c.NoError(err, "b")
	c.NoError(h.RetractHead(ctx, retracted.Nodes[0].ID), "retract")
	retry, err := h.Append(ctx, conv.Append{FromHead: true, Nodes: []conv.NewNode{{Msg: textMsg(msg.User, "b again")}}})
	c.NoError(err, "retry")

	sn, err := conv.NewStore(b, conv.Options{}).Open(convID).Snapshot(ctx)
	c.NoError(err, "Snapshot")
	history := historyIDs(sn)
	c.EqDeep([]string{root.Nodes[0].ID, retry.Nodes[0].ID}, history, "the retracted row is unreachable")
	c.NotContains(history, retracted.Nodes[0].ID, "the dead branch")
	c.EqDeep(history, transcriptIDs(sn), "the transcript agrees")
}

func TestRequestAtDropsTrimRange(t *testing.T) {
	c := assert.NewAborting(t)
	b, convID := newConv(t)
	ctx := context.Background()
	h := conv.NewStore(b, conv.Options{}).Open(convID, conv.AsOwner())

	first, err := h.Append(ctx, conv.Append{FromHead: true, Nodes: []conv.NewNode{{Msg: textMsg(msg.User, "a")}}})
	c.NoError(err, "a")
	second, err := h.Append(ctx, conv.Append{FromHead: true, Nodes: []conv.NewNode{{Msg: textMsg(msg.Assistant, "b")}}})
	c.NoError(err, "b")
	third, err := h.Append(ctx, conv.Append{FromHead: true, Nodes: []conv.NewNode{{Msg: textMsg(msg.User, "c")}}})
	c.NoError(err, "c")
	head, err := h.Append(ctx, conv.Append{FromHead: true, Nodes: []conv.NewNode{{Msg: textMsg(msg.Assistant, "d")}}})
	c.NoError(err, "d")

	all, err := h.RequestAt(ctx, head.Nodes[0].ID, nil, nil)
	c.NoError(err, "RequestAt at the head")
	c.EqDeep([]string{first.Nodes[0].ID, second.Nodes[0].ID, third.Nodes[0].ID, head.Nodes[0].ID}, nodeIDs(all), "no trim")

	trimmed, err := h.RequestAt(ctx, head.Nodes[0].ID, intPtr(1), intPtr(3))
	c.NoError(err, "RequestAt with a trim range")
	c.EqDeep([]string{first.Nodes[0].ID, head.Nodes[0].ID}, nodeIDs(trimmed), "positions 1 and 2 dropped")

	earlier, err := h.RequestAt(ctx, second.Nodes[0].ID, nil, nil)
	c.NoError(err, "RequestAt at an earlier head")
	c.EqDeep([]string{first.Nodes[0].ID, second.Nodes[0].ID}, nodeIDs(earlier), "the path ends at the named head")

	oneSided, err := h.RequestAt(ctx, head.Nodes[0].ID, intPtr(1), nil)
	c.NoError(err, "a one-sided trim is ignored")
	c.EqDeep(nodeIDs(all), nodeIDs(oneSided), "nothing dropped")
}
