// SPDX-License-Identifier: Apache-2.0

package llm

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"

	"go.graveland.dev/rafiki/pkg/store"

	"github.com/multigres/testkit/assert"
)

// memClient builds a store-less client whose conversations live in memory.
func memClient(t *testing.T, sender Sender) *Client {
	t.Helper()
	c, err := NewClient(
		WithProviderSender("anthropic", sender),
		WithCatalog(seededCatalog(t)),
		WithDefaultModel("haiku-latest"),
		WithLogger(testLogger(t)),
	)
	assert.NewAborting(t).NoError(err)
	return c
}

// respondMsg builds a canned response with an explicit content JSON array,
// stop reason and usage.
func respondMsg(stop string, in, out, cacheRead, cacheCreate int64, contentJSON string) func(anthropic.MessageNewParams) (*anthropic.Message, error) {
	return func(anthropic.MessageNewParams) (*anthropic.Message, error) {
		return cannedMessage(fmt.Sprintf(`{"id":"msg_c","type":"message","role":"assistant","model":"claude-haiku-4-5",
			"content":%s,"stop_reason":%q,
			"usage":{"input_tokens":%d,"output_tokens":%d,"cache_read_input_tokens":%d,"cache_creation_input_tokens":%d}}`,
			contentJSON, stop, in, out, cacheRead, cacheCreate)), nil
	}
}

// respondUsage builds a plain text response reporting the given usage.
func respondUsage(in, out, cacheRead, cacheCreate int64, text string) func(anthropic.MessageNewParams) (*anthropic.Message, error) {
	return respondMsg("end_turn", in, out, cacheRead, cacheCreate,
		fmt.Sprintf(`[{"type":"text","text":%q}]`, text))
}

// isSummaryReq reports whether p is a compaction summary call: its last user
// content block carries the compaction prompt.
func isSummaryReq(p anthropic.MessageNewParams) bool {
	if len(p.Messages) == 0 {
		return false
	}
	last := p.Messages[len(p.Messages)-1]
	if len(last.Content) == 0 {
		return false
	}
	b := last.Content[len(last.Content)-1]
	return b.OfText != nil && b.OfText.Text == compactionPrompt
}

func countSummaryReqs(sender *scriptedSender) int {
	n := 0
	for _, p := range sender.lastReq {
		if isSummaryReq(p) {
			n++
		}
	}
	return n
}

func textRow(ordinal int, role anthropic.MessageParamRole, text string) store.Message {
	return store.Message{Ordinal: ordinal, Param: anthropic.MessageParam{
		Role:    role,
		Content: []anthropic.ContentBlockParamUnion{anthropic.NewTextBlock(text)},
	}}
}

// stripCC returns a copy of msgs with every cache_control marker cleared, so
// two requests can be compared without the moving-breakpoint noise.
func stripCC(msgs []anthropic.MessageParam) []anthropic.MessageParam {
	out := make([]anthropic.MessageParam, len(msgs))
	for i, m := range msgs {
		nc := make([]anthropic.ContentBlockParamUnion, len(m.Content))
		for j, b := range m.Content {
			nc[j] = blockWithCacheControl(b, anthropic.CacheControlEphemeralParam{})
		}
		out[i] = anthropic.MessageParam{Role: m.Role, Content: nc}
	}
	return out
}

// countingTrim is a TrimPolicy that records how many times it ran and never
// shrinks — the signal that compaction happened (or did not) before trim.
type countingTrim struct{ calls *int }

func (c countingTrim) Trim(msgs []Message, _ int) ([]Message, bool) {
	*c.calls++
	return msgs, false
}

func hasKind(msgs []store.Message, kind string) int {
	n := 0
	for _, m := range msgs {
		if m.Kind != nil && *m.Kind == kind {
			n++
		}
	}
	return n
}

func TestCompactionProactiveFiresWhenHeadroomLow(t *testing.T) {
	ck := assert.NewCollecting(t)
	sender := &scriptedSender{scripts: []func(anthropic.MessageNewParams) (*anthropic.Message, error){
		respondUsage(80_000, 0, 0, 0, "first reply"),
		respondUsage(100, 50, 0, 0, "<summary>handover</summary>"),
		respondUsage(200, 20, 0, 0, "second reply"),
	}}
	c := memClient(t, sender)
	ctx := context.Background()
	conv, err := c.Conversation(ctx, NewConversation("", "test"),
		WithCompaction(CompactionPolicy{ContextWindowFn: func() int { return 100_000 }}))
	ck.Require().NoError(err)
	conv.mem = append(conv.mem,
		textRow(0, anthropic.MessageParamRoleUser, "a"),
		textRow(1, anthropic.MessageParamRoleAssistant, "b"),
		textRow(2, anthropic.MessageParamRoleUser, "c"),
		textRow(3, anthropic.MessageParamRoleAssistant, "d"))

	if _, err := conv.Send(ctx, UserText("first")); err != nil {
		t.Fatalf("first send: %v", err)
	}
	ck.Eq(80_000, conv.usedTokens, "usedTokens after first response")

	if _, err := conv.Send(ctx, UserText("second")); err != nil {
		t.Fatalf("second send: %v", err)
	}
	ck.Require().Len(sender.lastReq, 3, "requests")
	ck.True(isSummaryReq(sender.lastReq[1]), "request 1 is not the summary call")
	ck.False(isSummaryReq(sender.lastReq[2]), "request 2 should be the real call")
}

func TestCompactionProactiveSilentWithPlentyOfHeadroom(t *testing.T) {
	ck := assert.NewCollecting(t)
	sender := &scriptedSender{scripts: []func(anthropic.MessageNewParams) (*anthropic.Message, error){
		respondUsage(10_000, 0, 0, 0, "r1"),
		respondUsage(10_000, 0, 0, 0, "r2"),
	}}
	c := memClient(t, sender)
	ctx := context.Background()
	conv, err := c.Conversation(ctx, NewConversation("", "test"),
		WithCompaction(CompactionPolicy{ContextWindowFn: func() int { return 100_000 }}))
	ck.Require().NoError(err)
	conv.mem = append(conv.mem,
		textRow(0, anthropic.MessageParamRoleUser, "a"),
		textRow(1, anthropic.MessageParamRoleAssistant, "b"),
		textRow(2, anthropic.MessageParamRoleUser, "c"),
		textRow(3, anthropic.MessageParamRoleAssistant, "d"))

	if _, err := conv.Send(ctx, UserText("one")); err != nil {
		t.Fatalf("send 1: %v", err)
	}
	if _, err := conv.Send(ctx, UserText("two")); err != nil {
		t.Fatalf("send 2: %v", err)
	}
	ck.Require().Len(sender.lastReq, 2, "requests")
	ck.Eq(0, countSummaryReqs(sender), "summary calls")
}

func TestCompactionProactiveSilentWhenWindowUnknown(t *testing.T) {
	ck := assert.NewCollecting(t)
	sender := &scriptedSender{scripts: []func(anthropic.MessageNewParams) (*anthropic.Message, error){
		respondUsage(10, 5, 0, 0, "r"),
	}}
	c := memClient(t, sender)
	ctx := context.Background()
	conv, err := c.Conversation(ctx, NewConversation("", "test"),
		WithCompaction(CompactionPolicy{})) // nil ContextWindowFn
	ck.Require().NoError(err)
	conv.mem = append(conv.mem,
		textRow(0, anthropic.MessageParamRoleUser, "a"),
		textRow(1, anthropic.MessageParamRoleAssistant, "b"),
		textRow(2, anthropic.MessageParamRoleUser, "c"),
		textRow(3, anthropic.MessageParamRoleAssistant, "d"))
	conv.usedTokens = 999_000

	if _, err := conv.Continue(ctx); err != nil {
		t.Fatalf("continue: %v", err)
	}
	ck.Eq(0, countSummaryReqs(sender), "summary calls with an unknown window")
}

func TestCompactionProactiveSilentBelowMinRows(t *testing.T) {
	ck := assert.NewCollecting(t)
	sender := &scriptedSender{scripts: []func(anthropic.MessageNewParams) (*anthropic.Message, error){
		respondUsage(10, 5, 0, 0, "r"),
	}}
	c := memClient(t, sender)
	ctx := context.Background()
	conv, err := c.Conversation(ctx, NewConversation("", "test"),
		WithCompaction(CompactionPolicy{ContextWindowFn: func() int { return 1_000_000 }}))
	ck.Require().NoError(err)
	conv.mem = append(conv.mem,
		textRow(0, anthropic.MessageParamRoleUser, "a"),
		textRow(1, anthropic.MessageParamRoleAssistant, "b"),
		textRow(2, anthropic.MessageParamRoleUser, "c"))
	conv.usedTokens = 990_000

	if _, err := conv.Continue(ctx); err != nil {
		t.Fatalf("continue: %v", err)
	}
	ck.Eq(0, countSummaryReqs(sender), "summary calls below min rows")
}

func TestCompactionUsedTokensCountsCacheTokens(t *testing.T) {
	ck := assert.NewCollecting(t)
	sender := &scriptedSender{scripts: []func(anthropic.MessageNewParams) (*anthropic.Message, error){
		respondUsage(1, 100, 80_000, 5_000, "r"),
	}}
	c := memClient(t, sender)
	ctx := context.Background()
	conv, err := c.Conversation(ctx, NewConversation("", "test"))
	ck.Require().NoError(err)

	if _, err := conv.Send(ctx, UserText("hi")); err != nil {
		t.Fatalf("send: %v", err)
	}
	ck.Eq(85_101, conv.usedTokens, "usedTokens must count cache read/creation and output")
}

func TestCompactionProactiveCountsPendingRows(t *testing.T) {
	ck := assert.NewCollecting(t)
	sender := &scriptedSender{scripts: []func(anthropic.MessageNewParams) (*anthropic.Message, error){
		respondUsage(900_000, 0, 0, 0, "r1"),
		respondUsage(10, 5, 0, 0, "<summary>s</summary>"),
		respondUsage(10, 5, 0, 0, "r2"),
	}}
	c := memClient(t, sender)
	ctx := context.Background()
	conv, err := c.Conversation(ctx, NewConversation("", "test"),
		WithCompaction(CompactionPolicy{ContextWindowFn: func() int { return 1_000_000 }}))
	ck.Require().NoError(err)
	conv.mem = append(conv.mem,
		textRow(0, anthropic.MessageParamRoleUser, "a"),
		textRow(1, anthropic.MessageParamRoleAssistant, "b"),
		textRow(2, anthropic.MessageParamRoleUser, "c"),
		textRow(3, anthropic.MessageParamRoleAssistant, "d"))

	if _, err := conv.Send(ctx, UserText("first")); err != nil {
		t.Fatalf("send 1: %v", err)
	}
	ck.Eq(900_000, conv.usedTokens, "usedTokens after first response")

	// Two tool-result rows appended since the last response, ~70k tokens
	// between them: without the pending term the remaining 100k headroom is
	// above the 40k buffer and nothing would fire.
	big := strings.Repeat("z", 140_000)
	for i := 0; i < 2; i++ {
		content := []anthropic.ContentBlockParamUnion{anthropic.NewToolResultBlock("toolu_1", big, false)}
		if err := conv.AppendUser(ctx, content); err != nil {
			t.Fatalf("append tool result %d: %v", i, err)
		}
	}

	if _, err := conv.Continue(ctx); err != nil {
		t.Fatalf("continue: %v", err)
	}
	ck.Require().Len(sender.lastReq, 3, "requests")
	ck.True(isSummaryReq(sender.lastReq[1]), "compaction did not count the pending tool-result rows")
}

func TestCompactionSummaryMaxTokensClampedToConversationCap(t *testing.T) {
	ck := assert.NewCollecting(t)
	ctx := context.Background()

	run := func(t *testing.T, opts ...ConvOption) int64 {
		sender := &scriptedSender{scripts: []func(anthropic.MessageNewParams) (*anthropic.Message, error){
			respondUsage(10, 5, 0, 0, "<summary>s</summary>"),
			respondUsage(10, 5, 0, 0, "r"),
		}}
		c := memClient(t, sender)
		base := []ConvOption{
			NewConversation("", "test"),
			MaxTokens(8_000),
			WithCompaction(CompactionPolicy{ContextWindowFn: func() int { return 1_000_000 }, SummaryMaxTokens: 16_000}),
		}
		conv, err := c.Conversation(ctx, append(base, opts...)...)
		ck.Require().NoError(err)
		conv.mem = append(conv.mem,
			textRow(0, anthropic.MessageParamRoleUser, "a"),
			textRow(1, anthropic.MessageParamRoleAssistant, "b"),
			textRow(2, anthropic.MessageParamRoleUser, "c"),
			textRow(3, anthropic.MessageParamRoleAssistant, "d"))
		conv.usedTokens = 999_000
		if _, err := conv.Continue(ctx); err != nil {
			t.Fatalf("continue: %v", err)
		}
		ck.Require().True(isSummaryReq(sender.lastReq[0]), "first request is not the summary call")
		return sender.lastReq[0].MaxTokens
	}

	ck.Eq(int64(8_000), run(t), "summary max_tokens clamped to the conversation cap")
	ck.Eq(int64(10_000), run(t, ThinkingBudget(2_000)), "summary max_tokens must add the thinking budget")
}

func TestCompactionUsedTokensEstimatedWhenUnset(t *testing.T) {
	ck := assert.NewCollecting(t)
	sender := &scriptedSender{scripts: []func(anthropic.MessageNewParams) (*anthropic.Message, error){
		respondUsage(10, 5, 0, 0, "<summary>s</summary>"),
		respondUsage(10, 5, 0, 0, "r"),
	}}
	c := memClient(t, sender)
	ctx := context.Background()
	conv, err := c.Conversation(ctx, NewConversation("", "test"),
		WithCompaction(CompactionPolicy{ContextWindowFn: func() int { return 50_000 }}))
	ck.Require().NoError(err)

	// A large history with no reported usage: the byte estimate alone must
	// exceed the window's headroom.
	pad := strings.Repeat("q", 12_000)
	conv.mem = append(conv.mem,
		textRow(0, anthropic.MessageParamRoleUser, pad),
		textRow(1, anthropic.MessageParamRoleAssistant, pad),
		textRow(2, anthropic.MessageParamRoleUser, pad),
		textRow(3, anthropic.MessageParamRoleAssistant, pad))

	if _, err := conv.Continue(ctx); err != nil {
		t.Fatalf("continue: %v", err)
	}
	ck.True(isSummaryReq(sender.lastReq[0]), "compaction did not fire from the byte estimate")
}

func TestCompactionSummaryRequestPrefixIsByteIdentical(t *testing.T) {
	ck := assert.NewCollecting(t)
	// The overflow path: the rejected real request and the summary request are
	// built from the SAME working set, so they must share the cached prefix
	// byte for byte.
	sender := &scriptedSender{scripts: []func(anthropic.MessageNewParams) (*anthropic.Message, error){
		respondErr(promptTooLargeErr()),
		respondUsage(10, 5, 0, 0, "<summary>s</summary>"),
		respondUsage(10, 5, 0, 0, "real"),
	}}
	c := memClient(t, sender)
	ctx := context.Background()
	conv, err := c.Conversation(ctx, NewConversation("", "test"),
		ThinkingBudget(1_000),
		SystemText("you are a test"),
		WithCompaction(CompactionPolicy{})) // nil window: overflow only
	ck.Require().NoError(err)
	conv.mem = append(conv.mem,
		textRow(0, anthropic.MessageParamRoleUser, "a"),
		textRow(1, anthropic.MessageParamRoleAssistant, "b"),
		textRow(2, anthropic.MessageParamRoleUser, "c"),
		textRow(3, anthropic.MessageParamRoleAssistant, "d"),
		textRow(4, anthropic.MessageParamRoleUser, "e"))

	tools := []anthropic.ToolUnionParam{
		anthropic.ToolUnionParamOfTool(anthropic.ToolInputSchemaParam{Type: "object"}, "report_findings"),
	}
	if _, err := conv.Continue(ctx, WithTools(tools)); err != nil {
		t.Fatalf("continue: %v", err)
	}
	ck.Require().Len(sender.lastReq, 3, "requests")
	prev, sum := sender.lastReq[0], sender.lastReq[1]
	ck.Require().True(isSummaryReq(sum), "request 1 is not the summary call")

	ck.Eq(mustJSON(prev.Tools), mustJSON(sum.Tools), "tools bytes differ")
	ck.Eq(mustJSON(prev.System), mustJSON(sum.System), "system bytes differ")
	ck.Eq(mustJSON(prev.Thinking), mustJSON(sum.Thinking), "thinking bytes differ")

	want := stripCC(prev.Messages)
	last := want[len(want)-1]
	last.Content = append(slices.Clone(last.Content), anthropic.NewTextBlock(compactionPrompt))
	want[len(want)-1] = last
	ck.Eq(mustJSON(want), mustJSON(stripCC(sum.Messages)), "summary messages must be the previous messages plus one prompt block")
}

func TestCompactionSummaryRequestHasNoToolChoice(t *testing.T) {
	ck := assert.NewCollecting(t)
	sender := &scriptedSender{scripts: []func(anthropic.MessageNewParams) (*anthropic.Message, error){
		respondUsage(999_000, 0, 0, 0, "r1"),
		respondUsage(10, 5, 0, 0, "<summary>s</summary>"),
		respondUsage(10, 5, 0, 0, "r2"),
	}}
	c := memClient(t, sender)
	ctx := context.Background()
	conv, err := c.Conversation(ctx, NewConversation("", "test"),
		WithCompaction(CompactionPolicy{ContextWindowFn: func() int { return 1_000_000 }}))
	ck.Require().NoError(err)
	conv.mem = append(conv.mem,
		textRow(0, anthropic.MessageParamRoleUser, "a"),
		textRow(1, anthropic.MessageParamRoleAssistant, "b"),
		textRow(2, anthropic.MessageParamRoleUser, "c"),
		textRow(3, anthropic.MessageParamRoleAssistant, "d"))

	tools := []anthropic.ToolUnionParam{
		anthropic.ToolUnionParamOfTool(anthropic.ToolInputSchemaParam{Type: "object"}, "report_findings"),
	}
	if _, err := conv.Send(ctx, UserText("one"), WithTools(tools), WithToolChoice("report_findings")); err != nil {
		t.Fatalf("send 1: %v", err)
	}
	if _, err := conv.Continue(ctx, WithTools(tools), WithToolChoice("report_findings")); err != nil {
		t.Fatalf("continue: %v", err)
	}
	ck.Require().Len(sender.lastReq, 3, "requests")
	ck.Require().True(isSummaryReq(sender.lastReq[1]), "request 1 is not the summary call")
	ck.Nil(sender.lastReq[1].ToolChoice.OfTool, "summary request must carry no tool_choice, got")
}

func TestCompactionDiscardsToolUseBlocksInSummaryResponse(t *testing.T) {
	ck := assert.NewCollecting(t)
	sender := &scriptedSender{scripts: []func(anthropic.MessageNewParams) (*anthropic.Message, error){
		respondMsg("end_turn", 10, 5, 0, 0,
			`[{"type":"tool_use","id":"toolu_1","name":"Read","input":{}},{"type":"text","text":"<summary>X</summary>"}]`),
		respondUsage(10, 5, 0, 0, "real"),
	}}
	c := memClient(t, sender)
	ctx := context.Background()
	conv, err := c.Conversation(ctx, NewConversation("", "test"),
		WithCompaction(CompactionPolicy{ContextWindowFn: func() int { return 1_000_000 }}))
	ck.Require().NoError(err)
	conv.mem = append(conv.mem,
		textRow(0, anthropic.MessageParamRoleUser, "a"),
		textRow(1, anthropic.MessageParamRoleAssistant, "b"),
		textRow(2, anthropic.MessageParamRoleUser, "c"),
		textRow(3, anthropic.MessageParamRoleAssistant, "d"))
	conv.usedTokens = 999_000

	if _, err := conv.Continue(ctx); err != nil {
		t.Fatalf("continue: %v", err)
	}
	ck.Require().NotZero(conv.memHorizon, "no summary row was written")
	row := conv.mem[conv.memHorizon]
	ck.Require().Len(row.Param.Content, 1, "summary row content blocks")
	ck.Require().NotNil(row.Param.Content[0].OfText, "summary row is not a text block")
	ck.StrContains(row.Param.Content[0].OfText.Text, "X", "summary text")
	ck.Nil(row.Param.Content[0].OfToolUse, "a tool_use block was stored in the summary")
}

func TestCompactionTruncatedSummaryWritesNothing(t *testing.T) {
	ck := assert.NewCollecting(t)
	sender := &scriptedSender{scripts: []func(anthropic.MessageNewParams) (*anthropic.Message, error){
		respondMsg("max_tokens", 10, 5, 0, 0, `[{"type":"text","text":"<summary>partial"}]`),
		respondUsage(10, 5, 0, 0, "real"),
	}}
	c := memClient(t, sender)
	ctx := context.Background()
	conv, err := c.Conversation(ctx, NewConversation("", "test"),
		WithCompaction(CompactionPolicy{ContextWindowFn: func() int { return 1_000_000 }}))
	ck.Require().NoError(err)
	conv.mem = append(conv.mem,
		textRow(0, anthropic.MessageParamRoleUser, "a"),
		textRow(1, anthropic.MessageParamRoleAssistant, "b"),
		textRow(2, anthropic.MessageParamRoleUser, "c"),
		textRow(3, anthropic.MessageParamRoleAssistant, "d"))
	conv.usedTokens = 999_000

	resp, err := conv.Continue(ctx)
	ck.Require().NoError(err, "the turn must still proceed")
	ck.Eq("real", resp.Content[0].Text, "response")
	ck.Eq(0, conv.memHorizon, "horizon moved on a truncated summary")
	ck.Eq(0, hasKind(conv.mem, store.KindCompactionSummary), "a truncated summary was stored")
}

func TestCompactionEmptySummaryWritesNothing(t *testing.T) {
	ck := assert.NewCollecting(t)
	sender := &scriptedSender{scripts: []func(anthropic.MessageNewParams) (*anthropic.Message, error){
		respondUsage(10, 5, 0, 0, "<analysis>just thinking</analysis>"),
		respondUsage(10, 5, 0, 0, "real"),
	}}
	c := memClient(t, sender)
	ctx := context.Background()
	conv, err := c.Conversation(ctx, NewConversation("", "test"),
		WithCompaction(CompactionPolicy{ContextWindowFn: func() int { return 1_000_000 }}))
	ck.Require().NoError(err)
	conv.mem = append(conv.mem,
		textRow(0, anthropic.MessageParamRoleUser, "a"),
		textRow(1, anthropic.MessageParamRoleAssistant, "b"),
		textRow(2, anthropic.MessageParamRoleUser, "c"),
		textRow(3, anthropic.MessageParamRoleAssistant, "d"))
	conv.usedTokens = 999_000

	if _, err := conv.Continue(ctx); err != nil {
		t.Fatalf("continue: %v", err)
	}
	ck.Eq(0, conv.memHorizon, "horizon moved on an empty summary")
	ck.Eq(0, hasKind(conv.mem, store.KindCompactionSummary), "an empty summary was stored")
}

func TestCompactionSummaryCallErrorLetsTurnProceed(t *testing.T) {
	ck := assert.NewCollecting(t)
	sender := &scriptedSender{scripts: []func(anthropic.MessageNewParams) (*anthropic.Message, error){
		respondErr(errors.New("summary backend down")),
		respondUsage(10, 5, 0, 0, "real"),
	}}
	c := memClient(t, sender)
	ctx := context.Background()
	conv, err := c.Conversation(ctx, NewConversation("", "test"),
		WithCompaction(CompactionPolicy{ContextWindowFn: func() int { return 1_000_000 }}))
	ck.Require().NoError(err)
	conv.mem = append(conv.mem,
		textRow(0, anthropic.MessageParamRoleUser, "a"),
		textRow(1, anthropic.MessageParamRoleAssistant, "b"),
		textRow(2, anthropic.MessageParamRoleUser, "c"),
		textRow(3, anthropic.MessageParamRoleAssistant, "d"))
	conv.usedTokens = 999_000

	resp, err := conv.Continue(ctx)
	ck.Require().NoError(err, "the turn must proceed on the unchanged history")
	ck.Eq("real", resp.Content[0].Text, "response")
	ck.Eq(0, hasKind(conv.mem, store.KindCompactionSummary), "a summary was written after a failed call")
}

func TestCompactionContextCancelledPropagates(t *testing.T) {
	ck := assert.NewCollecting(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sender := &scriptedSender{scripts: []func(anthropic.MessageNewParams) (*anthropic.Message, error){
		func(anthropic.MessageNewParams) (*anthropic.Message, error) { cancel(); return nil, ctx.Err() },
		respondUsage(10, 5, 0, 0, "real"),
	}}
	c := memClient(t, sender)
	conv, err := c.Conversation(ctx, NewConversation("", "test"),
		WithCompaction(CompactionPolicy{ContextWindowFn: func() int { return 1_000_000 }}))
	ck.Require().NoError(err)
	conv.mem = append(conv.mem,
		textRow(0, anthropic.MessageParamRoleUser, "a"),
		textRow(1, anthropic.MessageParamRoleAssistant, "b"),
		textRow(2, anthropic.MessageParamRoleUser, "c"),
		textRow(3, anthropic.MessageParamRoleAssistant, "d"))
	conv.usedTokens = 999_000

	_, err = conv.Continue(ctx)
	ck.ErrorIs(err, context.Canceled, "Continue must surface the cancellation")
}

func TestCompactionWorkingSetAfterCompaction(t *testing.T) {
	ck := assert.NewCollecting(t)
	sender := &scriptedSender{scripts: []func(anthropic.MessageNewParams) (*anthropic.Message, error){
		respondUsage(10, 5, 0, 0, "<summary>handover</summary>"),
		respondUsage(10, 5, 0, 0, "reply"),
	}}
	c := memClient(t, sender)
	ctx := context.Background()
	conv, err := c.Conversation(ctx, NewConversation("", "test"),
		WithCompaction(CompactionPolicy{ContextWindowFn: func() int { return 1_000_000 }, TailCap: 50}))
	ck.Require().NoError(err)
	conv.mem = append(conv.mem,
		textRow(0, anthropic.MessageParamRoleUser, "a"),
		textRow(1, anthropic.MessageParamRoleAssistant, "b"),
		textRow(2, anthropic.MessageParamRoleUser, strings.Repeat("x", 400)),
		textRow(3, anthropic.MessageParamRoleAssistant, "c"),
		textRow(4, anthropic.MessageParamRoleUser, "d"))
	conv.usedTokens = 999_000

	if _, err := conv.Continue(ctx); err != nil {
		t.Fatalf("continue: %v", err)
	}
	ck.Require().Len(sender.lastReq, 2, "requests")
	ck.Require().True(isSummaryReq(sender.lastReq[0]), "request 0 is not the summary call")

	real := sender.lastReq[1]
	ck.Require().Len(real.Messages, 3, "real request messages = [summary, tail…]")
	ck.Require().NotNil(real.Messages[0].Content[0].OfText, "first request message is not text")
	ck.StrContains(real.Messages[0].Content[0].OfText.Text, "handover", "working set does not start at the summary")
	ck.Eq("d", real.Messages[2].Content[0].OfText.Text, "the tail's last row is missing from the request")

	ck.Eq(5, conv.memHorizon, "memHorizon")
	ck.Require().Len(conv.mem, 9, "mem rows")
	ck.Require().NotNil(conv.mem[8].Param.Content[0].OfText, "assistant reply is not text")
	ck.Eq("reply", conv.mem[8].Param.Content[0].OfText.Text, "assistant reply ordinal")
	ck.Eq(anthropic.MessageParamRoleAssistant, conv.mem[8].Param.Role, "assistant reply role")

	hist, err := conv.History(ctx)
	ck.Require().NoError(err)
	ck.Require().Len(hist, 4, "History() must return the working set")
	ck.Require().NotNil(hist[0].Kind, "working set does not start at the summary")
	ck.Eq(store.KindCompactionSummary, *hist[0].Kind, "working set row 0 kind")
	ck.Eq(5, hist[0].Ordinal, "summary ordinal")
	ck.Eq(8, hist[3].Ordinal, "assistant ordinal")
}

func TestCompactionSecondCompactionComposes(t *testing.T) {
	ck := assert.NewCollecting(t)
	sender := &scriptedSender{scripts: []func(anthropic.MessageNewParams) (*anthropic.Message, error){
		respondUsage(10, 5, 0, 0, "<summary>first</summary>"),
		respondUsage(10, 5, 0, 0, "r1"),
		respondUsage(10, 5, 0, 0, "<summary>second</summary>"),
		respondUsage(10, 5, 0, 0, "r2"),
	}}
	c := memClient(t, sender)
	ctx := context.Background()
	conv, err := c.Conversation(ctx, NewConversation("", "test"),
		WithCompaction(CompactionPolicy{ContextWindowFn: func() int { return 1_000_000 }}))
	ck.Require().NoError(err)
	conv.mem = append(conv.mem,
		textRow(0, anthropic.MessageParamRoleUser, "a"),
		textRow(1, anthropic.MessageParamRoleAssistant, "b"),
		textRow(2, anthropic.MessageParamRoleUser, "c"),
		textRow(3, anthropic.MessageParamRoleAssistant, "d"))

	conv.usedTokens = 999_000
	if _, err := conv.Continue(ctx); err != nil {
		t.Fatalf("continue 1: %v", err)
	}
	conv.usedTokens = 999_000
	if _, err := conv.Continue(ctx); err != nil {
		t.Fatalf("continue 2: %v", err)
	}

	ck.Require().Len(sender.lastReq, 4, "requests")
	ck.Require().True(isSummaryReq(sender.lastReq[2]), "request 2 is not the second summary call")
	ck.StrContains(mustJSON(sender.lastReq[2].Messages), "first", "the second summary request must contain the first summary")

	row := conv.mem[conv.memHorizon]
	ck.Require().NotNil(row.Kind, "working set does not start at the second summary")
	ck.Eq(store.KindCompactionSummary, *row.Kind, "working set row kind")
	ck.StrContains(row.Param.Content[0].OfText.Text, "second", "working set does not hold the second summary")
}

func TestCompactionNoTailIsSummaryOnly(t *testing.T) {
	ck := assert.NewCollecting(t)
	sender := &scriptedSender{scripts: []func(anthropic.MessageNewParams) (*anthropic.Message, error){
		respondUsage(10, 5, 0, 0, "<summary>only</summary>"),
		respondUsage(10, 5, 0, 0, "reply"),
	}}
	c := memClient(t, sender)
	ctx := context.Background()
	conv, err := c.Conversation(ctx, NewConversation("", "test"),
		WithCompaction(CompactionPolicy{ContextWindowFn: func() int { return 1_000_000 }, NoTail: true}))
	ck.Require().NoError(err)
	conv.mem = append(conv.mem,
		textRow(0, anthropic.MessageParamRoleUser, "a"),
		textRow(1, anthropic.MessageParamRoleAssistant, "b"),
		textRow(2, anthropic.MessageParamRoleUser, "c"),
		textRow(3, anthropic.MessageParamRoleAssistant, "d"))
	conv.usedTokens = 999_000

	var workingAtEnd int
	conv.OnCompaction(func(ev CompactionEvent) {
		if ev.Phase == CompactionEnd && ev.PreTokens > 0 {
			h, _ := conv.History(ctx)
			workingAtEnd = len(h)
		}
	})
	if _, err := conv.Continue(ctx); err != nil {
		t.Fatalf("continue: %v", err)
	}
	ck.Eq(1, workingAtEnd, "NoTail working set must be the summary alone")
}

func TestCompactionOverflowNetCompactsOnceBeforeTrim(t *testing.T) {
	ck := assert.NewCollecting(t)
	sender := &scriptedSender{scripts: []func(anthropic.MessageNewParams) (*anthropic.Message, error){
		respondErr(promptTooLargeErr()),
		respondUsage(10, 5, 0, 0, "<summary>s</summary>"),
		respondUsage(10, 5, 0, 0, "real"),
	}}
	c := memClient(t, sender)
	ctx := context.Background()
	trimCalls := 0
	conv, err := c.Conversation(ctx, NewConversation("", "test"),
		WithTrimPolicy(countingTrim{calls: &trimCalls}),
		WithCompaction(CompactionPolicy{})) // nil window: overflow only
	ck.Require().NoError(err)
	conv.mem = append(conv.mem,
		textRow(0, anthropic.MessageParamRoleUser, "a"),
		textRow(1, anthropic.MessageParamRoleAssistant, "b"),
		textRow(2, anthropic.MessageParamRoleUser, "c"),
		textRow(3, anthropic.MessageParamRoleAssistant, "d"))

	resp, err := conv.Continue(ctx)
	ck.Require().NoError(err, "the retried call must succeed")
	ck.Eq("real", resp.Content[0].Text, "response")
	ck.Require().Len(sender.lastReq, 3, "requests")
	ck.True(isSummaryReq(sender.lastReq[1]), "no summary call happened on overflow")
	ck.Eq(0, trimCalls, "trim must not run before compaction")
}

func TestCompactionOverflowCompactsAtMostOnce(t *testing.T) {
	ck := assert.NewCollecting(t)
	sender := &scriptedSender{scripts: []func(anthropic.MessageNewParams) (*anthropic.Message, error){
		respondErr(promptTooLargeErr()),
		respondUsage(10, 5, 0, 0, "<summary>s</summary>"),
		respondErr(promptTooLargeErr()),
	}}
	c := memClient(t, sender)
	ctx := context.Background()
	trimCalls := 0
	conv, err := c.Conversation(ctx, NewConversation("", "test"),
		WithTrimPolicy(countingTrim{calls: &trimCalls}),
		WithCompaction(CompactionPolicy{}))
	ck.Require().NoError(err)
	conv.mem = append(conv.mem,
		textRow(0, anthropic.MessageParamRoleUser, "a"),
		textRow(1, anthropic.MessageParamRoleAssistant, "b"),
		textRow(2, anthropic.MessageParamRoleUser, "c"),
		textRow(3, anthropic.MessageParamRoleAssistant, "d"))

	_, err = conv.Continue(ctx)
	ck.Error(err, "the still-too-large retry must surface an error")
	ck.Eq(1, countSummaryReqs(sender), "compaction must run at most once")
	ck.Eq(1, trimCalls, "trim must run after the single compaction")
}

func TestCompactionOverflowSkippedWhenTooFewRows(t *testing.T) {
	ck := assert.NewCollecting(t)
	sender := &scriptedSender{scripts: []func(anthropic.MessageNewParams) (*anthropic.Message, error){
		respondErr(promptTooLargeErr()),
	}}
	c := memClient(t, sender)
	ctx := context.Background()
	conv, err := c.Conversation(ctx, NewConversation("", "test"),
		WithCompaction(CompactionPolicy{}))
	ck.Require().NoError(err)
	conv.mem = append(conv.mem,
		textRow(0, anthropic.MessageParamRoleUser, "a"),
		textRow(1, anthropic.MessageParamRoleAssistant, "b"))

	_, err = conv.Continue(ctx)
	ck.Require().Error(err, "the original error must reach the caller")
	ck.True(IsPromptTooLarge(err), "the error is not the prompt-too-large rejection")
	ck.Eq(0, countSummaryReqs(sender), "compaction must not run below min rows")
}

func TestCompactionOverflowSummaryTooLargeFallsToTrim(t *testing.T) {
	ck := assert.NewCollecting(t)
	sender := &scriptedSender{scripts: []func(anthropic.MessageNewParams) (*anthropic.Message, error){
		respondErr(promptTooLargeErr()),
		respondErr(promptTooLargeErr()),
	}}
	c := memClient(t, sender)
	ctx := context.Background()
	trimCalls := 0
	conv, err := c.Conversation(ctx, NewConversation("", "test"),
		WithTrimPolicy(countingTrim{calls: &trimCalls}),
		WithCompaction(CompactionPolicy{}))
	ck.Require().NoError(err)
	conv.mem = append(conv.mem,
		textRow(0, anthropic.MessageParamRoleUser, "a"),
		textRow(1, anthropic.MessageParamRoleAssistant, "b"),
		textRow(2, anthropic.MessageParamRoleUser, "c"),
		textRow(3, anthropic.MessageParamRoleAssistant, "d"))

	_, err = conv.Continue(ctx)
	ck.Error(err, "the too-large error must surface")
	ck.Eq(0, hasKind(conv.mem, store.KindCompactionSummary), "nothing must be written when the summary itself overflows")
	ck.Eq(1, trimCalls, "trim must run after the failed summary")
}

func TestCompactionStartEndEventsBalanced(t *testing.T) {
	ctx := context.Background()

	run := func(t *testing.T, summary func(anthropic.MessageNewParams) (*anthropic.Message, error)) []CompactionEvent {
		ck := assert.NewCollecting(t)
		sender := &scriptedSender{scripts: []func(anthropic.MessageNewParams) (*anthropic.Message, error){
			summary,
			respondUsage(10, 5, 0, 0, "real"),
		}}
		c := memClient(t, sender)
		conv, err := c.Conversation(ctx, NewConversation("", "test"),
			WithCompaction(CompactionPolicy{ContextWindowFn: func() int { return 1_000_000 }}))
		ck.Require().NoError(err)
		conv.mem = append(conv.mem,
			textRow(0, anthropic.MessageParamRoleUser, "a"),
			textRow(1, anthropic.MessageParamRoleAssistant, "b"),
			textRow(2, anthropic.MessageParamRoleUser, "c"),
			textRow(3, anthropic.MessageParamRoleAssistant, "d"))
		conv.usedTokens = 999_000
		var events []CompactionEvent
		conv.OnCompaction(func(ev CompactionEvent) { events = append(events, ev) })
		if _, err := conv.Continue(ctx); err != nil {
			t.Fatalf("continue: %v", err)
		}
		return events
	}

	ok := run(t, respondUsage(10, 5, 0, 0, "<summary>s</summary>"))
	ck := assert.NewCollecting(t)
	ck.Require().Len(ok, 2, "events on success")
	ck.Eq(CompactionStart, ok[0].Phase, "event 0 phase")
	ck.Eq("threshold", ok[0].Trigger, "event 0 trigger")
	ck.Eq(CompactionEnd, ok[1].Phase, "event 1 phase")
	ck.Greater(0, ok[1].PreTokens, "PreTokens on success")

	failed := run(t, respondErr(errors.New("boom")))
	ck.Require().Len(failed, 2, "events on failure")
	ck.Eq(CompactionStart, failed[0].Phase, "failed event 0 phase")
	ck.Eq(CompactionEnd, failed[1].Phase, "failed event 1 phase")
	ck.Eq(0, failed[1].PreTokens, "failed PreTokens")
	ck.Eq(0, failed[1].PostTokens, "failed PostTokens")
}

func TestCompactionStoreLoadStaysFull(t *testing.T) {
	ck := assert.NewCollecting(t)
	pool := convTestPool(t)
	ctx := context.Background()
	sender := &scriptedSender{scripts: []func(anthropic.MessageNewParams) (*anthropic.Message, error){
		respondUsage(10, 5, 0, 0, "<summary>handover</summary>"),
		respondUsage(10, 5, 0, 0, "reply"),
	}}
	c := testClient(t, pool, sender)
	conv, err := c.Conversation(ctx, NewConversation("", "test"),
		WithCompaction(CompactionPolicy{ContextWindowFn: func() int { return 1_000_000 }, NoTail: true}))
	ck.Require().NoError(err)

	msgs := store.NewMessages(pool)
	for i, txt := range []string{"seed0", "seed1", "seed2", "seed3"} {
		role := anthropic.MessageParamRoleUser
		if i%2 == 1 {
			role = anthropic.MessageParamRoleAssistant
		}
		ck.Require().NoError(msgs.Append(ctx, conv.ID, i,
			anthropic.MessageParam{Role: role, Content: []anthropic.ContentBlockParamUnion{anthropic.NewTextBlock(txt)}}, nil))
	}
	conv.usedTokens = 999_000

	if _, err := conv.Continue(ctx); err != nil {
		t.Fatalf("continue: %v", err)
	}

	full, err := msgs.Load(ctx, conv.ID)
	ck.Require().NoError(err)
	ck.Require().Len(full, 6, "Load must return every row: 4 seeds + summary + assistant")
	for i, want := range []string{"seed0", "seed1", "seed2", "seed3"} {
		ck.Eq(want, full[i].Param.Content[0].OfText.Text, "pre-compaction row %d", i)
	}
	working, err := msgs.LoadWorking(ctx, conv.ID)
	ck.Require().NoError(err)
	ck.Require().Len(working, 2, "working set = summary + assistant")
	ck.Require().NotNil(working[0].Kind, "working set does not start at the summary")
	ck.Eq(store.KindCompactionSummary, *working[0].Kind, "working set row 0 kind")
}

func TestCompactionSummaryTurnRecordedAndNextTurnRecorded(t *testing.T) {
	ck := assert.NewCollecting(t)
	pool := convTestPool(t)
	ctx := context.Background()
	sender := &scriptedSender{scripts: []func(anthropic.MessageNewParams) (*anthropic.Message, error){
		respondUsage(10, 5, 0, 0, "<summary>handover</summary>"),
		respondUsage(10, 5, 0, 0, "reply"),
	}}
	c := testClient(t, pool, sender)
	conv, err := c.Conversation(ctx, NewConversation("", "test"),
		WithCompaction(CompactionPolicy{ContextWindowFn: func() int { return 1_000_000 }, NoTail: true}))
	ck.Require().NoError(err)

	msgs := store.NewMessages(pool)
	for i := 0; i < 4; i++ {
		role := anthropic.MessageParamRoleUser
		if i%2 == 1 {
			role = anthropic.MessageParamRoleAssistant
		}
		ck.Require().NoError(msgs.Append(ctx, conv.ID, i,
			anthropic.MessageParam{Role: role, Content: []anthropic.ContentBlockParamUnion{anthropic.NewTextBlock("x")}}, nil))
	}
	conv.usedTokens = 999_000

	if _, err := conv.Continue(ctx); err != nil {
		t.Fatalf("continue: %v", err)
	}

	var total, compaction int
	ck.Require().NoError(pool.QueryRow(ctx, `SELECT count(*), count(*) FILTER (WHERE source = 'compaction')
		FROM conversations.conversation_turn WHERE conversation_id = $1::uuid`, conv.ID).Scan(&total, &compaction),
		"read turn rows")
	ck.Eq(2, total, "turn rows: the summary call and the following real call")
	ck.Eq(1, compaction, "summary turns")
}
