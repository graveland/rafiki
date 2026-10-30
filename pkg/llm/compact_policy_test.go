// SPDX-License-Identifier: Apache-2.0

package llm

import (
	"encoding/json"
	"strings"
	"testing"

	convpkg "go.graveland.dev/rafiki/pkg/conv"
	"go.graveland.dev/rafiki/pkg/msg"
	msganthropic "go.graveland.dev/rafiki/pkg/msg/anthropic"

	"github.com/multigres/testkit/assert"
)

func cpUserText(text string) *convpkg.Node {
	return &convpkg.Node{Msg: msg.Message{Role: msg.User, Content: []msg.Block{msg.Text{Text: text}}}}
}

func cpAssistantText(text string) *convpkg.Node {
	return &convpkg.Node{Msg: msg.Message{Role: msg.Assistant, Content: []msg.Block{msg.Text{Text: text}}}}
}

func cpAssistantToolUse(id string) *convpkg.Node {
	return &convpkg.Node{Msg: msg.Message{Role: msg.Assistant, Content: []msg.Block{
		msg.ToolUse{ID: id, Name: "bash", Input: json.RawMessage(`{"command":"ls"}`)},
	}}}
}

func cpUserToolResult(id string) *convpkg.Node {
	return &convpkg.Node{Msg: msg.Message{Role: msg.User, Content: []msg.Block{
		msg.ToolResult{ToolUseID: id, Content: []msg.Block{msg.Text{Text: "output"}}},
	}}}
}

func TestCompactionPolicyZeroValueMeansDefaults(t *testing.T) {
	c := assert.NewAborting(t)
	p := CompactionPolicy{}.withDefaults()
	c.Eq(40_000, p.HeadroomBuffer)
	c.Eq(10, p.TailPercent)
	c.Eq(50_000, p.TailCap)
	c.Eq(int64(16_000), p.SummaryMaxTokens)
	c.False(p.NoTail)
	c.Eq(4, minCompactableRows)
}

func TestCompactionPolicyBufferCoversSummary(t *testing.T) {
	c := assert.NewAborting(t)
	p := CompactionPolicy{SummaryMaxTokens: 60_000, HeadroomBuffer: 10_000}.withDefaults()
	c.Eq(64_000, p.HeadroomBuffer)
}

func TestCompactionShouldCompactBoundary(t *testing.T) {
	c := assert.NewAborting(t)
	p := CompactionPolicy{ContextWindowFn: func() int { return 1_000_000 }}.withDefaults()
	c.True(p.shouldCompact(960_000), "remaining 40_000 equals the buffer")
	c.False(p.shouldCompact(959_999), "remaining 40_001 exceeds the buffer")
}

func TestCompactionShouldCompactUnknownWindowNever(t *testing.T) {
	c := assert.NewAborting(t)
	c.False(CompactionPolicy{}.withDefaults().shouldCompact(10_000_000))
	c.False(CompactionPolicy{ContextWindowFn: func() int { return 0 }}.withDefaults().shouldCompact(10_000_000))
	c.False(CompactionPolicy{ContextWindowFn: func() int { return -5 }}.withDefaults().shouldCompact(10_000_000))
}

func TestCompactionTailBudget(t *testing.T) {
	c := assert.NewAborting(t)
	c.Eq(20_000, CompactionPolicy{ContextWindowFn: func() int { return 200_000 }}.withDefaults().tailBudget())
	c.Eq(50_000, CompactionPolicy{ContextWindowFn: func() int { return 1_000_000 }}.withDefaults().tailBudget())
	c.Eq(0, CompactionPolicy{NoTail: true, ContextWindowFn: func() int { return 200_000 }}.withDefaults().tailBudget())
	c.Eq(50_000, CompactionPolicy{}.withDefaults().tailBudget())
}

func TestCompactionCutTailNeverSplitsToolPair(t *testing.T) {
	c := assert.NewAborting(t)
	history := []*convpkg.Node{
		cpUserText("start"),
		cpAssistantToolUse("A"),
		cpUserToolResult("A"),
		cpAssistantText("done"),
	}
	c.False(isLegalCut(history[2]), "a tool_result row is never a legal cut")

	// A budget that would otherwise start the tail at index 2 (its suffix
	// fits exactly): the tool_result row is skipped, so the tail starts at 3.
	budget := estimateHistoryTokens(history[2:])
	c.Greater(budget, estimateHistoryTokens(history[1:]))
	c.Eq(3, cutTail(history, budget))
}

func TestCompactionCutTailLegalStarts(t *testing.T) {
	c := assert.NewAborting(t)
	history := []*convpkg.Node{
		cpUserText("first"),
		cpAssistantText("second"),
		cpUserText("third"),
	}
	// Index 0 is never a tail start, even when everything fits.
	c.Eq(1, cutTail(history, estimateHistoryTokens(history)+1_000_000))
	// A cut may start at an assistant row.
	c.Eq(1, cutTail(history, estimateHistoryTokens(history[1:])))
	// A cut may start at a user text row.
	c.Eq(2, cutTail(history, estimateHistoryTokens(history[2:])))
}

func TestCompactionCutTailEarliestFit(t *testing.T) {
	c := assert.NewAborting(t)
	history := []*convpkg.Node{
		cpUserText("zero"),
		cpAssistantText("one"),
		cpUserText("two"),
		cpAssistantText("three"),
	}
	c.True(isLegalCut(history[1]) && isLegalCut(history[2]) && isLegalCut(history[3]))

	// Only the middle start's suffix fits; the earliest that fits wins.
	budget := estimateHistoryTokens(history[2:])
	c.Greater(budget, estimateHistoryTokens(history[1:]))
	c.Eq(2, cutTail(history, budget))
}

func TestCompactionCutTailEmptyWhenNothingFits(t *testing.T) {
	c := assert.NewAborting(t)
	history := []*convpkg.Node{
		cpUserText("zero"),
		cpAssistantText("one"),
		cpUserText("two"),
	}
	lastSuffix := estimateHistoryTokens(history[len(history)-1:])
	c.Eq(len(history), cutTail(history, 0))
	c.Eq(len(history), cutTail(history, lastSuffix-1))
}

func TestCompactionPromptForbidsTools(t *testing.T) {
	c := assert.NewAborting(t)
	c.StrContains(compactionPrompt, "Respond with TEXT ONLY")
	c.StrContains(compactionPrompt, "Do NOT call any tools")
	c.StrContains(compactionPrompt, "<summary>")
}

func TestCompactionExtractSummary(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
		ok   bool
	}{
		{"well-formed", "<summary>the summary</summary>", "the summary", true},
		{"analysis then summary", "<analysis>deliberation</analysis><summary>the summary</summary>", "the summary", true},
		{"unclosed summary", "<summary>no closing tag", "no closing tag", true},
		{"analysis only", "<analysis>deliberation only</analysis>", "", false},
		{"no tags", "  plain response  ", "plain response", true},
		{"empty", "   ", "", false},
		{"empty summary", "<summary></summary>", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := assert.NewAborting(t)
			got, ok := extractSummary(tt.in)
			c.Eq(tt.want, got)
			c.Eq(tt.ok, ok)
		})
	}
}

func TestCompactionSummaryMessageIsUserText(t *testing.T) {
	c := assert.NewAborting(t)
	m := summaryMessage("the summary")
	c.Eq(msg.User, m.Role)
	c.Len(m.Content, 1)
	text, ok := m.Content[0].(msg.Text)
	c.Require().True(ok, "the summary is not a text block")
	c.Eq(summaryFraming+"the summary", text.Text)
	c.True(strings.HasPrefix(text.Text, summaryFraming))
}

// TestEstimateTokensNative pins the byte estimate: stored content plus the
// role plus the request envelope {"role":"","content":}, four bytes per token.
// It is computed from the stored bytes rather than by re-marshalling the
// message, so it costs nothing on a node the cache already holds.
func TestEstimateTokensNative(t *testing.T) {
	c := assert.NewAborting(t)
	c.Eq(24, envelopeBytes, "the request envelope is {\"role\":\"\",\"content\":}")

	short := &convpkg.Node{
		Msg:     msg.Message{Role: msg.User},
		Content: []byte(`[{"type":"text","text":"hi"}]`),
	}
	c.Eq(15, estimateTokens(short), "user, 29 stored bytes")

	empty := &convpkg.Node{Msg: msg.Message{Role: msg.User}}
	c.Eq(7, estimateTokens(empty), "user, no stored bytes")

	long := &convpkg.Node{
		Msg:     msg.Message{Role: msg.Assistant},
		Content: []byte(strings.Repeat("x", 400)),
	}
	c.Eq(109, estimateTokens(long), "assistant, 400 stored bytes")
}

// TestCompactionCutTailTreatsOpaqueToolResultAsToolResult pins the boundary rule
// against the shapes production actually stores: a tool_result carrying a field
// this model does not (a caller) keeps its typed form with that key in Ext, and
// one whose core fields cannot be read is kept verbatim as msg.Opaque. A tail
// that started on either would separate it from the tool_use that produced it,
// so the typed, Ext-carrying and Opaque forms must be classified identically.
func TestCompactionCutTailTreatsOpaqueToolResultAsToolResult(t *testing.T) {
	c := assert.NewAborting(t)

	// A caller is real extra data the core does not model, so it rides along in
	// Ext rather than degrading the block.
	withExt, err := msganthropic.Decode("user", []byte(
		`[{"type":"tool_result","tool_use_id":"tu_1","content":[{"type":"text","text":"out"}],"caller":{"type":"direct"}}]`))
	c.Require().NoError(err, "decode stored tool_result")
	c.Require().Len(withExt.Content, 1)
	result, ok := withExt.Content[0].(msg.ToolResult)
	c.Require().True(ok, "the block must keep its typed form, got %T", withExt.Content[0])
	c.Eq(`{"caller":{"type":"direct"}}`, result.Ext.Raw, "the caller must ride along in Ext")

	// A block whose core fields cannot be read is still kept verbatim, and still
	// declares the type it answers as.
	unreadable, err := msganthropic.Decode("user", []byte(
		`[{"type":"tool_result","tool_use_id":"tu_1","content":[{"type":"text","text":"out"}],"is_error":"yes"}]`))
	c.Require().NoError(err, "decode an unreadable tool_result")
	c.Require().Len(unreadable.Content, 1)
	opaque, ok := unreadable.Content[0].(msg.Opaque)
	c.Require().True(ok, "an unreadable tool_result must degrade to Opaque, got %T", unreadable.Content[0])
	c.Eq("tool_result", msg.OpaqueType(opaque), "the Opaque block must still declare its type")

	typedRow := &convpkg.Node{Msg: msg.Message{Role: msg.User, Content: []msg.Block{
		msg.ToolResult{ToolUseID: "tu_1", Content: []msg.Block{msg.Text{Text: "out"}}},
	}}}
	extRow := &convpkg.Node{Msg: withExt}
	opaqueRow := &convpkg.Node{Msg: unreadable}

	c.False(isLegalCut(typedRow), "a typed tool_result row is not a legal cut")
	c.False(isLegalCut(extRow), "a tool_result row carrying Ext must not be a legal cut")
	c.False(isLegalCut(opaqueRow), "an Opaque tool_result row must not be a legal cut")

	// The same history with each shape: the cut skips the result row and starts
	// at the assistant row above it.
	for name, resultRow := range map[string]*convpkg.Node{"typed": typedRow, "ext": extRow, "opaque": opaqueRow} {
		history := []*convpkg.Node{
			cpUserText("start"),
			cpAssistantToolUse("tu_1"),
			resultRow,
			cpAssistantText("done"),
		}
		budget := estimateHistoryTokens(history[2:])
		c.Greater(budget, estimateHistoryTokens(history[1:]))
		c.Eq(3, cutTail(history, budget), "%s tool_result row must be skipped by the cut", name)
	}
}
