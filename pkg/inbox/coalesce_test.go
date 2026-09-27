// SPDX-License-Identifier: Apache-2.0

package inbox_test

import (
	"strings"
	"testing"

	"go.graveland.dev/rafiki/pkg/inbox"

	"github.com/multigres/testkit/assert"
)

func row(id, source, key, text string, mode inbox.Mode) inbox.Inbound {
	return inbox.Inbound{ID: id, ChildID: "c_1", Mode: mode, Source: source, Key: key, Text: text}
}

func TestCoalesceDirectMessagesAreOneBatchEach(t *testing.T) {
	c := assert.NewCollecting(t)
	got := inbox.Coalesce([]inbox.Inbound{
		row("m1", "", "", "first", inbox.ModePrompt),
		row("m2", "", "", "second", inbox.ModePrompt),
	}, inbox.BatchConfig{})

	c.Require().Len(got, 2, "want 2 batches, got %d", len(got))
	c.False(got[0].Frags[0] != "first" || got[1].Frags[0] != "second", "direct messages must keep arrival order, got %+v", got)
	if len(got[0].IDs) != 1 || got[0].IDs[0] != "m1" {
		t.Errorf("batch must carry its own row id, got %+v", got[0].IDs)
	}
}

func TestCoalesceFragmentsLastWriteWinsPerKey(t *testing.T) {
	c := assert.NewCollecting(t)
	got := inbox.Coalesce([]inbox.Inbound{
		row("f1", "subagents", "c_a", "agent c_a working", inbox.ModePrompt),
		row("f2", "subagents", "c_b", "agent c_b working", inbox.ModePrompt),
		row("f3", "subagents", "c_a", "agent c_a settled", inbox.ModePrompt),
	}, inbox.BatchConfig{})

	c.Require().Len(got, 1, "one source is one batch, got %d", len(got))
	want := []string{"agent c_a settled", "agent c_b working"}
	c.Eq(strings.Join(want, "|"), strings.Join(got[0].Frags, "|"), "frags = %v, want %v (keyed order is FIRST appearance, text is LAST write)", got[0].Frags, want)
	// f1 was superseded but is still accounted for: a row left pending
	// because its text lost a last-write-wins race is redelivered forever.
	c.Eq("f1,f2,f3", strings.Join(got[0].IDs, ","), "IDs = %v, want every row in the group including superseded ones", got[0].IDs)
}

func TestCoalesceAnySteerMakesTheBatchASteer(t *testing.T) {
	got := inbox.Coalesce([]inbox.Inbound{
		row("f1", "executor", "", "note", inbox.ModePrompt),
		row("f2", "executor", "", "EXECUTOR LOST", inbox.ModeSteer),
	}, inbox.BatchConfig{})

	assert.NewAborting(t).False(len(got) != 1 || got[0].Mode != inbox.ModeSteer, "a group containing a steer delivers as a steer, got %+v", got)
}

func TestCoalesceAppliesCapsWithAVisibleMarker(t *testing.T) {
	c := assert.NewCollecting(t)
	var rows []inbox.Inbound
	for i := range 5 {
		rows = append(rows, row(string(rune('a'+i)), "s", "", strings.Repeat("x", 10), inbox.ModePrompt))
	}
	got := inbox.Coalesce(rows, inbox.BatchConfig{MaxFragments: 3})
	c.Require().Len(got, 1, "want 1 batch, got %d", len(got))
	last := got[0].Frags[len(got[0].Frags)-1]
	c.StrContains(last, "omitted", "capped batch must end with an omission marker, got")
	c.Len(got[0].IDs, 5, "omitted rows are still acked, IDs =")
}

func TestCoalesceAbortIsItsOwnBatchWithNoBody(t *testing.T) {
	got := inbox.Coalesce([]inbox.Inbound{row("m1", "", "", "", inbox.ModeAbort)}, inbox.BatchConfig{})
	assert.NewAborting(t).False(len(got) != 1 || got[0].Mode != inbox.ModeAbort || len(got[0].Frags) != 0, "abort batch = %+v, want one empty-bodied abort batch", got)
}

// Attachments survive coalescing. A direct message is never coalesced, which is
// where they come from — but they are carried on the grouped path too, because
// silently discarding a payload is the failure shape this repo keeps fixing.
func TestCoalesceCarriesAttachments(t *testing.T) {
	c := assert.NewCollecting(t)
	img := inbox.Attachment{MediaType: "image/png", Data: []byte("bytes")}

	direct := inbox.Coalesce([]inbox.Inbound{
		{ID: "1", ChildID: "c_1", Mode: inbox.ModePrompt, Text: "look", Attachments: []inbox.Attachment{img}},
	}, inbox.BatchConfig{})
	c.Require().False(len(direct) != 1 || len(direct[0].Attachments) != 1, "direct message lost its attachment: %+v", direct)
	c.Eq("bytes", string(direct[0].Attachments[0].Data), "attachment bytes did not survive")

	grouped := inbox.Coalesce([]inbox.Inbound{
		{ID: "1", ChildID: "c_1", Mode: inbox.ModePrompt, Source: "s", Text: "a", Attachments: []inbox.Attachment{img}},
		{ID: "2", ChildID: "c_1", Mode: inbox.ModePrompt, Source: "s", Text: "b"},
	}, inbox.BatchConfig{})
	c.Require().False(len(grouped) != 1 || len(grouped[0].Attachments) != 1, "grouped batch lost its attachment: %+v", grouped)
}
