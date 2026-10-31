// SPDX-License-Identifier: Apache-2.0

package llm

import (
	"encoding/json"
	"fmt"
	"reflect"
	"testing"

	convpkg "go.graveland.dev/rafiki/pkg/conv"
	"go.graveland.dev/rafiki/pkg/msg"

	"github.com/multigres/testkit/assert"
)

// TestTrimRangeMapsMergedMessagesToRows pins what a trim range is measured in:
// rowStart is indexed by wire message and holds PATH positions, so a merged run
// of rows shifts every later position and the anchor's end is where the trimmed
// range begins.
func TestTrimRangeMapsMergedMessagesToRows(t *testing.T) {
	c := assert.NewAborting(t)

	// Rows 4 and 5 are both user, so they merge into one wire message.
	history := []*convpkg.Node{
		textRow(0, msg.User, "u0"),
		textRow(1, msg.Assistant, "a1"),
		textRow(2, msg.User, "u2"),
		textRow(3, msg.Assistant, "a3"),
		textRow(4, msg.User, "u4a"),
		textRow(5, msg.User, "u4b"),
		textRow(6, msg.Assistant, "a6"),
		textRow(7, msg.User, "u7"),
	}
	msgs, rowStart, err := mergeForRequestRows(history)
	c.Require().NoError(err, "merge")
	c.Eq(7, len(msgs), "wire messages")
	c.EqDeep([]int{0, 1, 2, 3, 4, 6, 7}, rowStart, "rowStart")
	c.Eq(2, len(msgs[4].Content), "rows 4 and 5 merged into one message")

	// The policy shape: the anchor plus the last two messages.
	trimmed := []msg.Message{msgs[0], msgs[5], msgs[6]}
	to, keptRowStart, ok := trimDropped(msgs, trimmed, rowStart)
	c.Require().True(ok, "anchor-plus-suffix trim not recognised")
	c.Eq(6, to, "first kept suffix position")
	c.EqDeep([]int{0, 6, 7}, keptRowStart, "keptRowStart")
	// The turn records anchorEnd..to: rows 1 through 5 were dropped.
	c.Eq(1, rowStart[1], "anchorEnd")
	c.Eq(5, to-rowStart[1], "positions dropped by the recorded range")

	// A merged ANCHOR: rows 0 and 1 both user, so the anchor message spans two
	// path positions and the dropped range starts at 2.
	mergedAnchor := []*convpkg.Node{
		textRow(0, msg.User, "u0"),
		textRow(1, msg.User, "u1"),
		textRow(2, msg.Assistant, "a2"),
		textRow(3, msg.User, "u3"),
		textRow(4, msg.Assistant, "a4"),
	}
	msgs2, rowStart2, err := mergeForRequestRows(mergedAnchor)
	c.Require().NoError(err, "merge")
	c.Eq(4, len(msgs2), "wire messages with a merged anchor")
	c.EqDeep([]int{0, 2, 3, 4}, rowStart2, "rowStart with a merged anchor")
	c.Eq(2, rowStart2[1], "anchorEnd with a merged anchor")

	trimmed2 := []msg.Message{msgs2[0], msgs2[2], msgs2[3]}
	to2, kept2, ok := trimDropped(msgs2, trimmed2, rowStart2)
	c.Require().True(ok, "trim with a merged anchor not recognised")
	c.Eq(3, to2, "first kept suffix position with a merged anchor")
	c.EqDeep([]int{0, 3, 4}, kept2, "keptRowStart with a merged anchor")
}

// TestTrimDroppedRejectsNonAnchorSuffixShape holds trimDropped to the ONLY
// shape it may interpret: everything else records no range, because a range
// derived from an unrecognised cut would name rows the request did not drop.
func TestTrimDroppedRejectsNonAnchorSuffixShape(t *testing.T) {
	c := assert.NewAborting(t)

	orig := make([]msg.Message, 5)
	for i := range orig {
		orig[i] = msg.Message{Role: msg.User, Content: []msg.Block{msg.Text{Text: fmt.Sprintf("m%d", i)}}}
	}
	rowStart := []int{0, 1, 2, 3, 4}

	if _, _, ok := trimDropped(orig, []msg.Message{orig[1], orig[3], orig[4]}, rowStart); ok {
		t.Error("a trim that dropped the anchor was accepted")
	}
	if _, _, ok := trimDropped(orig, []msg.Message{orig[0], orig[4], orig[3]}, rowStart); ok {
		t.Error("a reordered trim was accepted")
	}
	if _, _, ok := trimDropped(orig, orig, rowStart); ok {
		t.Error("a trim that dropped nothing was accepted")
	}
	// A row mapping that does not cover the original list cannot be indexed.
	if _, _, ok := trimDropped(orig, []msg.Message{orig[0], orig[3], orig[4]}, rowStart[:2]); ok {
		t.Error("a short row mapping was accepted")
	}

	// The policy shape itself is accepted, so the guards above are what reject
	// the others rather than an unconditional false.
	to, keptRowStart, ok := trimDropped(orig, []msg.Message{orig[0], orig[3], orig[4]}, rowStart)
	c.Require().True(ok, "anchor-plus-suffix trim not recognised")
	c.Eq(3, to, "first kept suffix position")
	c.EqDeep([]int{0, 3, 4}, keptRowStart, "keptRowStart")
}

// TestMergeForRequestRowsMatchesMergeForRequest pins that the row-aware merge
// is the same merge: its message list must stay byte-identical to
// mergeForRequest's for every fixture the merge tests use.
func TestMergeForRequestRowsMatchesMergeForRequest(t *testing.T) {
	brokenToolUse := msg.ToolUse{ID: "id_broken", Name: "", Input: json.RawMessage(`{"command":"whoami"}`)}
	goodToolUse := msg.ToolUse{ID: "id_good", Name: "bash", Input: json.RawMessage(`{"command":"ls"}`)}
	toolResult := func(id string) msg.ToolResult {
		return msg.ToolResult{ToolUseID: id, Content: []msg.Block{msg.Text{Text: "out"}}}
	}
	cases := map[string][]*convpkg.Node{
		"empty-name tool_use replaced": {
			{Msg: msg.Message{Role: msg.Assistant, Content: []msg.Block{goodToolUse, brokenToolUse, msg.Text{Text: "note"}}}},
			{Msg: msg.Message{Role: msg.User, Content: []msg.Block{toolResult("id_good"), toolResult("id_broken")}}},
		},
		"good tool_uses untouched": {
			{Msg: msg.Message{Role: msg.Assistant, Content: []msg.Block{goodToolUse, goodToolUse}}},
		},
		"consecutive user rows merge": {
			{Msg: msg.Message{Role: msg.User, Content: []msg.Block{msg.Text{Text: "first"}}}},
			{Msg: msg.Message{Role: msg.User, Content: []msg.Block{msg.Text{Text: "second"}}}},
			{Msg: msg.Message{Role: msg.Assistant, Content: []msg.Block{msg.Text{Text: "reply"}}}},
			{Msg: msg.Message{Role: msg.User, Content: []msg.Block{msg.Text{Text: "third"}}}},
		},
	}

	for name, history := range cases {
		t.Run(name, func(t *testing.T) {
			c := assert.NewAborting(t)
			msgs, rowStart, err := mergeForRequestRows(history)
			c.Require().NoError(err, "merge")
			merged, err := mergeForRequest(history)
			c.Require().NoError(err, "mergeForRequest")
			if !reflect.DeepEqual(merged, msgs) {
				t.Fatalf("mergeForRequestRows output differs from mergeForRequest:\n%#v\n%#v",
					merged, msgs)
			}
			c.Eq(len(msgs), len(rowStart), "rowStart entries vs messages")
			// A merged row appends no start, so each start is the first row of
			// its message and the starts ascend.
			prev := -1
			for i, start := range rowStart {
				if start <= prev || start >= len(history) {
					t.Fatalf("rowStart[%d] = %d out of range/order (history %d rows)", i, start, len(history))
				}
				prev = start
			}
		})
	}
}
