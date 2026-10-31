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
	msgs, rowStart, err := mergeForRequestRows(history, nil)
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
	msgs2, rowStart2, err := mergeForRequestRows(mergedAnchor, nil)
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
			msgs, rowStart, err := mergeForRequestRows(history, nil)
			c.Require().NoError(err, "merge")
			merged, err := mergeForRequest(history, nil)
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

// The reminders a Claude Code user message carries, in the order Claude Code
// writes them: the running model's identity (false under fundi), then a date.
const (
	modelReminderBlock = "<system-reminder>\nYou are powered by the model named Haiku 4.5. The exact model ID is claude-haiku-4-5-20251001.\n</system-reminder>"
	dateReminderBlock  = "<system-reminder>\nToday's date is 2026-10-31.\n</system-reminder>"
)

// originRow builds one fixture node written by origin.
func originRow(ordinal int, origin convpkg.Origin, role msg.Role, blocks ...msg.Block) *convpkg.Node {
	return &convpkg.Node{
		Ref:    convpkg.Ref{Ordinal: ordinal},
		Msg:    msg.Message{Role: role, Content: blocks},
		Origin: origin,
	}
}

// TestForRequestDropsModelReminderFromClaudeCodeNodes pins that a node another
// harness wrote is replayed through that harness before it reaches a request:
// Claude Code's model reminder is false under fundi and is dropped, and the
// remaining reminder is marked for the encoder in the order it was written.
func TestForRequestDropsModelReminderFromClaudeCodeNodes(t *testing.T) {
	c := assert.NewAborting(t)

	history := []*convpkg.Node{
		originRow(0, convpkg.OriginClaudeCode, msg.User,
			msg.Text{Text: modelReminderBlock},
			msg.Text{Text: dateReminderBlock},
			msg.Text{Text: "hello"},
		),
	}

	msgs, _, err := mergeForRequestRows(history, nil)
	c.Require().NoError(err, "merge")
	c.Require().Len(msgs, 1, "one user message")
	c.Eq(msg.User, msgs[0].Role, "role")
	c.EqDeep([]msg.Block{
		msg.Text{Text: dateReminderBlock, Injected: true},
		msg.Text{Text: "hello"},
	}, msgs[0].Content, "the model reminder is dropped, the rest kept in order")
}

// TestForRequestLeavesFundiAndUnclassifiedNodesUntouched pins the other side:
// content pkg/llm wrote itself, or wrote before origins existed, is sent as
// stored — every block survives and none is marked.
func TestForRequestLeavesFundiAndUnclassifiedNodesUntouched(t *testing.T) {
	c := assert.NewAborting(t)

	blocks := []msg.Block{
		msg.Text{Text: modelReminderBlock},
		msg.Text{Text: dateReminderBlock},
		msg.Text{Text: "hello"},
	}

	for _, origin := range []convpkg.Origin{convpkg.OriginFundi, convpkg.OriginUnclassified} {
		got := forRequest(originRow(0, origin, msg.User, blocks...), nil)
		c.Eq(msg.User, got.Role, "%q: role", origin)
		c.EqDeep(blocks, got.Content, "%q: every block survives, none marked", origin)
	}
}

// TestForRequestIsDeterministic pins replay as a pure function of the stored
// node: two passes over the same history agree, and the node is not written to
// — a node's Msg is shared by the cache, so a mutation here would change every
// later turn's prefix.
func TestForRequestIsDeterministic(t *testing.T) {
	c := assert.NewAborting(t)

	history := []*convpkg.Node{
		originRow(0, convpkg.OriginClaudeCode, msg.User,
			msg.Text{Text: modelReminderBlock},
			msg.Text{Text: dateReminderBlock},
			msg.Text{Text: "hello"},
		),
	}
	wantStored := msg.Message{Role: msg.User, Content: []msg.Block{
		msg.Text{Text: modelReminderBlock},
		msg.Text{Text: dateReminderBlock},
		msg.Text{Text: "hello"},
	}}

	first, _, err := mergeForRequestRows(history, nil)
	c.Require().NoError(err, "first merge")
	second, _, err := mergeForRequestRows(history, nil)
	c.Require().NoError(err, "second merge")

	c.EqDeep(first, second, "two passes over the same history agree")
	c.EqDeep(wantStored, history[0].Msg, "the stored node is unchanged")
}

// TestMergeForRequestFiltersBeforeMerging pins WHERE the replay happens: per
// node, before consecutive user rows fuse. A merged run would otherwise carry
// the dropped reminder from the Claude Code row into the fundi row's message.
func TestMergeForRequestFiltersBeforeMerging(t *testing.T) {
	c := assert.NewAborting(t)

	history := []*convpkg.Node{
		originRow(0, convpkg.OriginClaudeCode, msg.User,
			msg.Text{Text: modelReminderBlock},
			msg.Text{Text: "first"},
		),
		originRow(1, convpkg.OriginFundi, msg.User,
			msg.Text{Text: "second"},
		),
	}

	msgs, _, err := mergeForRequestRows(history, nil)
	c.Require().NoError(err, "merge")
	c.Require().Len(msgs, 1, "the two user rows merge into one message")
	c.EqDeep([]msg.Block{
		msg.Text{Text: "first"},
		msg.Text{Text: "second"},
	}, msgs[0].Content, "the Claude Code row was filtered before the merge")
}

// nodeRow builds one fixture node written by origin in conversation convID.
func nodeRow(convID string, ordinal int, origin convpkg.Origin, role msg.Role, blocks ...msg.Block) *convpkg.Node {
	return &convpkg.Node{
		Ref:    convpkg.Ref{ConversationID: convID, Ordinal: ordinal},
		Msg:    msg.Message{Role: role, Content: blocks},
		Origin: origin,
	}
}

// toolDefs builds the send's tool set from names alone.
func toolDefs(names ...string) []msg.ToolDef {
	out := make([]msg.ToolDef, 0, len(names))
	for _, n := range names {
		out = append(out, msg.ToolDef{Name: n})
	}
	return out
}

// claudeToolHistory is a borrowed Claude Code turn: a call to Read and a call
// to Task, each answered in the next user row.
func claudeToolHistory(convID string) []*convpkg.Node {
	return []*convpkg.Node{
		nodeRow(convID, 0, convpkg.OriginClaudeCode, msg.Assistant,
			msg.ToolUse{ID: "t_read", Name: "Read", Input: json.RawMessage(`{"file_path":"/tmp/a"}`)},
			msg.ToolUse{ID: "t_task", Name: "Task", Input: json.RawMessage(`{"description":"find it"}`)},
		),
		nodeRow(convID, 1, convpkg.OriginClaudeCode, msg.User,
			msg.ToolResult{ToolUseID: "t_read", Content: []msg.Block{msg.Text{Text: "file body"}}},
			msg.ToolResult{ToolUseID: "t_task", Content: []msg.Block{msg.Text{Text: "subagent report"}}},
		),
	}
}

// TestForRequestReplaysClaudeToolsAgainstTheSendsTools pins that a borrowed
// Claude Code call becomes a call to the fundi tool of the same name when the
// send offers it, and that its result stays a tool_result.
func TestForRequestReplaysClaudeToolsAgainstTheSendsTools(t *testing.T) {
	c := assert.NewAborting(t)

	msgs, _, err := mergeForRequestRows(claudeToolHistory("src"), newReplayContext("fork", toolDefs("read")))
	c.Require().NoError(err, "merge")
	c.Require().Len(msgs, 2, "assistant and user")

	use, ok := msgs[0].Content[0].(msg.ToolUse)
	c.Require().True(ok, "the Read call is still a tool_use")
	c.Eq("read", use.Name, "renamed to the fundi tool")
	c.Eq("t_read", use.ID, "the id is untouched")
	c.Eq(`{"file_path":"/tmp/a"}`, string(use.Input), "the input is untouched")

	result, ok := msgs[1].Content[0].(msg.ToolResult)
	c.Require().True(ok, "the result of a renamed call stays a tool_result")
	c.Eq("t_read", result.ToolUseID, "same id")

	_, ok = msgs[0].Content[1].(msg.Text)
	c.True(ok, "the Task call the send cannot serve became text")
	_, ok = msgs[1].Content[1].(msg.Text)
	c.True(ok, "and so did its result")
}

// TestForRequestLeavesFundiOriginUntouched pins the other side of the replay:
// a fundi node's tool calls are fundi's already, whatever names they carry.
func TestForRequestLeavesFundiOriginUntouched(t *testing.T) {
	c := assert.NewAborting(t)

	blocks := []msg.Block{
		msg.ToolUse{ID: "t_read", Name: "Read", Input: json.RawMessage(`{"file_path":"/tmp/a"}`)},
	}
	got := forRequest(nodeRow("fork", 0, convpkg.OriginFundi, msg.Assistant, blocks...),
		newReplayContext("fork", toolDefs("read")))
	c.EqDeep(msg.Message{Role: msg.Assistant, Content: blocks}, got, "a fundi node is sent as stored")
}

// TestBorrowedThinkingIsStripped pins that reasoning another conversation wrote
// never reaches the request: its signature belongs to the upstream that minted
// it, and a fork routes independently of its source.
func TestBorrowedThinkingIsStripped(t *testing.T) {
	c := assert.NewAborting(t)

	history := []*convpkg.Node{
		nodeRow("src", 0, convpkg.OriginClaudeCode, msg.Assistant,
			msg.Thinking{Text: "reasoning", Sig: msg.Signature{Dialect: msg.DialectAnthropic, Value: "sig"}},
			msg.Text{Text: "the answer"},
		),
	}

	msgs, _, err := mergeForRequestRows(history, newReplayContext("fork", nil))
	c.Require().NoError(err, "merge")
	c.Require().Len(msgs, 1, "one assistant message")
	c.EqDeep([]msg.Block{msg.Text{Text: "the answer"}}, msgs[0].Content,
		"the borrowed thinking is gone and the rest is kept")
}

// TestBorrowedThinkingOnlyMessageKeepsPlaceholder pins that stripping never
// empties a message: a borrowed assistant row that was only reasoning keeps a
// placeholder in its place.
func TestBorrowedThinkingOnlyMessageKeepsPlaceholder(t *testing.T) {
	c := assert.NewAborting(t)

	history := []*convpkg.Node{
		nodeRow("src", 0, convpkg.OriginFundi, msg.Assistant,
			msg.Thinking{Text: "reasoning", Sig: msg.Signature{Dialect: msg.DialectAnthropic, Value: "sig"}},
		),
	}

	msgs, _, err := mergeForRequestRows(history, newReplayContext("fork", nil))
	c.Require().NoError(err, "merge")
	c.Require().Len(msgs, 1, "the message survives")
	c.EqDeep([]msg.Block{msg.Text{Text: "(borrowed reasoning omitted)"}}, msgs[0].Content,
		"a placeholder stands in for the reasoning")
}

// TestOwnThinkingIsNeverStripped pins the boundary: the same block on a node of
// the requesting conversation is that conversation's own reasoning and stays.
func TestOwnThinkingIsNeverStripped(t *testing.T) {
	c := assert.NewAborting(t)

	thinking := msg.Thinking{Text: "reasoning", Sig: msg.Signature{Dialect: msg.DialectAnthropic, Value: "sig"}}
	history := []*convpkg.Node{
		nodeRow("fork", 0, convpkg.OriginClaudeCode, msg.Assistant, thinking, msg.Text{Text: "the answer"}),
	}

	msgs, _, err := mergeForRequestRows(history, newReplayContext("fork", nil))
	c.Require().NoError(err, "merge")
	c.Require().Len(msgs, 1, "one assistant message")
	c.EqDeep([]msg.Block{thinking, msg.Text{Text: "the answer"}}, msgs[0].Content,
		"own thinking is kept")
}
