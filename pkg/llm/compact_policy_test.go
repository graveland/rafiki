// SPDX-License-Identifier: Apache-2.0

package llm

import (
	"strings"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"

	"go.graveland.dev/rafiki/pkg/store"

	"github.com/multigres/testkit/assert"
)

func cpUserText(text string) anthropic.MessageParam {
	return anthropic.MessageParam{
		Role:    anthropic.MessageParamRoleUser,
		Content: []anthropic.ContentBlockParamUnion{{OfText: &anthropic.TextBlockParam{Text: text}}},
	}
}

func cpAssistantText(text string) anthropic.MessageParam {
	return anthropic.MessageParam{
		Role:    anthropic.MessageParamRoleAssistant,
		Content: []anthropic.ContentBlockParamUnion{{OfText: &anthropic.TextBlockParam{Text: text}}},
	}
}

func cpAssistantToolUse(id string) anthropic.MessageParam {
	return anthropic.MessageParam{
		Role: anthropic.MessageParamRoleAssistant,
		Content: []anthropic.ContentBlockParamUnion{{OfToolUse: &anthropic.ToolUseBlockParam{
			ID:    id,
			Name:  "bash",
			Input: map[string]any{"command": "ls"},
		}}},
	}
}

func cpUserToolResult(id string) anthropic.MessageParam {
	return anthropic.MessageParam{
		Role: anthropic.MessageParamRoleUser,
		Content: []anthropic.ContentBlockParamUnion{{OfToolResult: &anthropic.ToolResultBlockParam{
			ToolUseID: id,
			Content:   []anthropic.ToolResultBlockParamContentUnion{{OfText: &anthropic.TextBlockParam{Text: "output"}}},
		}}},
	}
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
	history := []store.Message{
		{Ordinal: 0, Param: cpUserText("start")},
		{Ordinal: 1, Param: cpAssistantToolUse("A")},
		{Ordinal: 2, Param: cpUserToolResult("A")},
		{Ordinal: 3, Param: cpAssistantText("done")},
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
	history := []store.Message{
		{Ordinal: 0, Param: cpUserText("first")},
		{Ordinal: 1, Param: cpAssistantText("second")},
		{Ordinal: 2, Param: cpUserText("third")},
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
	history := []store.Message{
		{Ordinal: 0, Param: cpUserText("zero")},
		{Ordinal: 1, Param: cpAssistantText("one")},
		{Ordinal: 2, Param: cpUserText("two")},
		{Ordinal: 3, Param: cpAssistantText("three")},
	}
	c.True(isLegalCut(history[1]) && isLegalCut(history[2]) && isLegalCut(history[3]))

	// Only the middle start's suffix fits; the earliest that fits wins.
	budget := estimateHistoryTokens(history[2:])
	c.Greater(budget, estimateHistoryTokens(history[1:]))
	c.Eq(2, cutTail(history, budget))
}

func TestCompactionCutTailEmptyWhenNothingFits(t *testing.T) {
	c := assert.NewAborting(t)
	history := []store.Message{
		{Ordinal: 0, Param: cpUserText("zero")},
		{Ordinal: 1, Param: cpAssistantText("one")},
		{Ordinal: 2, Param: cpUserText("two")},
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
	c.Eq(anthropic.MessageParamRoleUser, m.Role)
	c.Len(m.Content, 1)
	c.NotNil(m.Content[0].OfText)
	c.Eq(summaryFraming+"the summary", m.Content[0].OfText.Text)
	c.True(strings.HasPrefix(m.Content[0].OfText.Text, summaryFraming))
}
