// SPDX-License-Identifier: Apache-2.0

package llm

import (
	"fmt"
	"reflect"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"

	"go.graveland.dev/rafiki/pkg/store"

	"github.com/multigres/testkit/assert"
)

// TestTrimRangeMapsMergedMessagesToRows pins what a trim range is measured in:
// rowStart is indexed by wire message and holds PATH positions, so a merged run
// of rows shifts every later position and the anchor's end is where the trimmed
// range begins.
func TestTrimRangeMapsMergedMessagesToRows(t *testing.T) {
	c := assert.NewAborting(t)

	// Rows 4 and 5 are both user, so they merge into one wire message.
	history := []store.Message{
		textRow(0, anthropic.MessageParamRoleUser, "u0"),
		textRow(1, anthropic.MessageParamRoleAssistant, "a1"),
		textRow(2, anthropic.MessageParamRoleUser, "u2"),
		textRow(3, anthropic.MessageParamRoleAssistant, "a3"),
		textRow(4, anthropic.MessageParamRoleUser, "u4a"),
		textRow(5, anthropic.MessageParamRoleUser, "u4b"),
		textRow(6, anthropic.MessageParamRoleAssistant, "a6"),
		textRow(7, anthropic.MessageParamRoleUser, "u7"),
	}
	msgs, rowStart := mergeForRequestRows(history)
	c.Eq(7, len(msgs), "wire messages")
	c.EqDeep([]int{0, 1, 2, 3, 4, 6, 7}, rowStart, "rowStart")
	c.Eq(2, len(msgs[4].Content), "rows 4 and 5 merged into one message")

	// The policy shape: the anchor plus the last two messages.
	trimmed := []Message{msgs[0], msgs[5], msgs[6]}
	to, keptRowStart, ok := trimDropped(msgs, trimmed, rowStart)
	c.Require().True(ok, "anchor-plus-suffix trim not recognised")
	c.Eq(6, to, "first kept suffix position")
	c.EqDeep([]int{0, 6, 7}, keptRowStart, "keptRowStart")
	// The turn records anchorEnd..to: rows 1 through 5 were dropped.
	c.Eq(1, rowStart[1], "anchorEnd")
	c.Eq(5, to-rowStart[1], "positions dropped by the recorded range")

	// A merged ANCHOR: rows 0 and 1 both user, so the anchor message spans two
	// path positions and the dropped range starts at 2.
	mergedAnchor := []store.Message{
		textRow(0, anthropic.MessageParamRoleUser, "u0"),
		textRow(1, anthropic.MessageParamRoleUser, "u1"),
		textRow(2, anthropic.MessageParamRoleAssistant, "a2"),
		textRow(3, anthropic.MessageParamRoleUser, "u3"),
		textRow(4, anthropic.MessageParamRoleAssistant, "a4"),
	}
	msgs2, rowStart2 := mergeForRequestRows(mergedAnchor)
	c.Eq(4, len(msgs2), "wire messages with a merged anchor")
	c.EqDeep([]int{0, 2, 3, 4}, rowStart2, "rowStart with a merged anchor")
	c.Eq(2, rowStart2[1], "anchorEnd with a merged anchor")

	trimmed2 := []Message{msgs2[0], msgs2[2], msgs2[3]}
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

	orig := make([]Message, 5)
	for i := range orig {
		orig[i] = anthropic.NewUserMessage(anthropic.NewTextBlock(fmt.Sprintf("m%d", i)))
	}
	rowStart := []int{0, 1, 2, 3, 4}

	if _, _, ok := trimDropped(orig, []Message{orig[1], orig[3], orig[4]}, rowStart); ok {
		t.Error("a trim that dropped the anchor was accepted")
	}
	if _, _, ok := trimDropped(orig, []Message{orig[0], orig[4], orig[3]}, rowStart); ok {
		t.Error("a reordered trim was accepted")
	}
	if _, _, ok := trimDropped(orig, orig, rowStart); ok {
		t.Error("a trim that dropped nothing was accepted")
	}
	// A row mapping that does not cover the original list cannot be indexed.
	if _, _, ok := trimDropped(orig, []Message{orig[0], orig[3], orig[4]}, rowStart[:2]); ok {
		t.Error("a short row mapping was accepted")
	}

	// The policy shape itself is accepted, so the guards above are what reject
	// the others rather than an unconditional false.
	to, keptRowStart, ok := trimDropped(orig, []Message{orig[0], orig[3], orig[4]}, rowStart)
	c.Require().True(ok, "anchor-plus-suffix trim not recognised")
	c.Eq(3, to, "first kept suffix position")
	c.EqDeep([]int{0, 3, 4}, keptRowStart, "keptRowStart")
}

// TestMergeForRequestRowsMatchesMergeForRequest pins that the row-aware merge
// is the same merge: its message list must stay byte-identical to
// mergeForRequest's for every fixture the merge tests use.
func TestMergeForRequestRowsMatchesMergeForRequest(t *testing.T) {
	brokenToolUse := anthropic.ContentBlockParamUnion{
		OfToolUse: &anthropic.ToolUseBlockParam{ID: "id_broken", Name: "", Input: map[string]any{"command": "whoami"}},
	}
	goodToolUse := anthropic.ContentBlockParamUnion{
		OfToolUse: &anthropic.ToolUseBlockParam{ID: "id_good", Name: "bash", Input: map[string]any{"command": "ls"}},
	}
	toolResult := func(id string) anthropic.ContentBlockParamUnion {
		return anthropic.ContentBlockParamUnion{OfToolResult: &anthropic.ToolResultBlockParam{
			ToolUseID: id,
			Content:   []anthropic.ToolResultBlockParamContentUnion{{OfText: &anthropic.TextBlockParam{Text: "out"}}},
		}}
	}
	cases := map[string][]store.Message{
		"empty-name tool_use replaced": {
			{Ordinal: 0, Param: anthropic.MessageParam{
				Role:    anthropic.MessageParamRoleAssistant,
				Content: []anthropic.ContentBlockParamUnion{goodToolUse, brokenToolUse, anthropic.NewTextBlock("note")},
			}},
			{Ordinal: 1, Param: anthropic.MessageParam{
				Role:    anthropic.MessageParamRoleUser,
				Content: []anthropic.ContentBlockParamUnion{toolResult("id_good"), toolResult("id_broken")},
			}},
		},
		"good tool_uses untouched": {
			{Ordinal: 0, Param: anthropic.MessageParam{
				Role:    anthropic.MessageParamRoleAssistant,
				Content: []anthropic.ContentBlockParamUnion{goodToolUse, goodToolUse},
			}},
		},
		"consecutive user rows merge": {
			{Ordinal: 0, Param: anthropic.NewUserMessage(anthropic.NewTextBlock("first"))},
			{Ordinal: 1, Param: anthropic.NewUserMessage(anthropic.NewTextBlock("second"))},
			{Ordinal: 2, Param: anthropic.MessageParam{
				Role:    anthropic.MessageParamRoleAssistant,
				Content: []anthropic.ContentBlockParamUnion{anthropic.NewTextBlock("reply")},
			}},
			{Ordinal: 3, Param: anthropic.NewUserMessage(anthropic.NewTextBlock("third"))},
		},
	}

	for name, history := range cases {
		t.Run(name, func(t *testing.T) {
			c := assert.NewAborting(t)
			msgs, rowStart := mergeForRequestRows(history)
			if !reflect.DeepEqual(mergeForRequest(history), msgs) {
				t.Fatalf("mergeForRequestRows output differs from mergeForRequest:\n%#v\n%#v",
					mergeForRequest(history), msgs)
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
