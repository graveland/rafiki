// SPDX-License-Identifier: Apache-2.0

package llm

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
	"go.opentelemetry.io/otel/trace"

	"go.graveland.dev/rafiki/pkg/msg"

	"github.com/multigres/testkit/assert"
)

// fakeBatcher records Park's arguments and replays a queued result. The last
// script repeats (mirrors scriptedSender's convention).
type fakeBatcher struct {
	mu       sync.Mutex
	calls    int
	lastIDs  []string
	lastMods []string
	// lastBody is the request body the batcher parked, as it received it.
	lastBody json.RawMessage
	scripts  []func() (json.RawMessage, error)
}

func (b *fakeBatcher) Park(_ context.Context, customID, model string, body json.RawMessage, _ json.RawMessage) (json.RawMessage, error) {
	b.mu.Lock()
	b.calls++
	b.lastIDs = append(b.lastIDs, customID)
	b.lastMods = append(b.lastMods, model)
	b.lastBody = body
	b.mu.Unlock()
	i := b.calls - 1
	if i >= len(b.scripts) {
		i = len(b.scripts) - 1
	}
	return b.scripts[i]()
}

func batchReply() json.RawMessage {
	return json.RawMessage(`{"id":"msg_b","type":"message","role":"assistant","model":"z-ai/glm-5.3-flash:batch",
		"content":[{"type":"text","text":"batched"}],
		"stop_reason":"end_turn","usage":{"input_tokens":7,"output_tokens":3}}`)
}

// newBatchClient builds a store-less client whose "openrouter" provider is
// backed by sender (never expected to be called on the park path) and whose
// batcher is b.
func newBatchClient(t *testing.T, b Batcher, sender SDKSender) *Client {
	t.Helper()
	c, err := NewClient(
		WithProviderSender("anthropic", sdkFake(sender)),
		WithProviderSender("openrouter", sdkFake(sender)),
		WithBatcher(b),
		WithCatalog(seededCatalog(t)),
		WithLogger(testLogger(t)),
	)
	assert.NewAborting(t).NoError(err)
	return c
}

func batchParams() anthropic.MessageNewParams {
	return anthropic.MessageNewParams{Model: "openrouter/z-ai/glm-5.3-flash:batch", MaxTokens: 16,
		Messages: []anthropic.MessageParam{anthropic.NewUserMessage(anthropic.NewTextBlock("hi"))}}
}

// laterCallParams is a request containing an assistant message — a later call.
func laterCallParams() anthropic.MessageNewParams {
	p := batchParams()
	p.Messages = append(p.Messages, anthropic.NewAssistantMessage(anthropic.NewTextBlock("earlier reply")),
		anthropic.NewUserMessage(anthropic.NewTextBlock("next")))
	return p
}

func TestBatchFirstCallParks(t *testing.T) {
	ck := assert.NewCollecting(t)
	b := &fakeBatcher{scripts: []func() (json.RawMessage, error){func() (json.RawMessage, error) { return batchReply(), nil }}}
	sender := &scriptedSender{scripts: []func(anthropic.MessageNewParams) (*anthropic.Message, error){
		respondText("must not be reached"),
	}}
	c := newBatchClient(t, b, sender)

	resp, err := c.SendParams(context.Background(), SendMeta{ConversationID: "01a0d5e6-2636-7afa-b356-cf9441b16e31", Ordinal: 12}, batchParams())
	ck.Require().NoError(err, "SendParams")
	ck.Eq("batched", resp.Content[0].Text, "response")
	ck.Require().Eq(1, b.calls, "batcher called")
	ck.Eq("z-ai/glm-5.3-flash:batch", b.lastMods[0], "batcher model")
	ck.Eq("01a0d5e6-2636-7afa-b356-cf9441b16e31-12", b.lastIDs[0], "batcher customID")
	ck.Eq(0, sender.calls, "live sender called")
}

func TestBatchLaterCallStripsSuffix(t *testing.T) {
	ck := assert.NewCollecting(t)
	b := &fakeBatcher{scripts: []func() (json.RawMessage, error){func() (json.RawMessage, error) {
		t.Error("batcher must not be called on a later call")
		return batchReply(), nil
	}}}
	sender := &scriptedSender{scripts: []func(anthropic.MessageNewParams) (*anthropic.Message, error){
		respondText("live"),
	}}
	c := newBatchClient(t, b, sender)

	resp, err := c.SendParams(context.Background(), SendMeta{ConversationID: "conv"}, laterCallParams())
	ck.Require().NoError(err, "SendParams")
	ck.Eq("live", resp.Content[0].Text, "response")
	ck.Require().Eq(1, sender.calls, "live sender called")
	ck.Eq("z-ai/glm-5.3-flash", string(sender.lastReq[0].Model), "live model")
	ck.Eq(0, b.calls, "batcher called")
}

func TestBatchNonOpenRouterProviderRefused(t *testing.T) {
	b := &fakeBatcher{}
	sender := &scriptedSender{scripts: []func(anthropic.MessageNewParams) (*anthropic.Message, error){
		respondText("must not be reached"),
	}}
	c := newBatchClient(t, b, sender)

	params := batchParams()
	params.Model = "anthropic/claude-haiku-4-5:batch"
	_, err := c.SendParams(context.Background(), SendMeta{ConversationID: "conv"}, params)
	assert.NewAborting(t).False(err == nil || !strings.Contains(err.Error(), "served only by an anthropic-openrouter provider"), "error = %v, want the anthropic-openrouter-only refusal", err)
	if sender.calls != 0 || b.calls != 0 {
		t.Errorf("nothing may be sent: sender=%d batcher=%d, want 0/0", sender.calls, b.calls)
	}
}

func TestBatchNoBatcherErrors(t *testing.T) {
	ck := assert.NewCollecting(t)
	sender := &scriptedSender{scripts: []func(anthropic.MessageNewParams) (*anthropic.Message, error){
		respondText("must not be reached"),
	}}
	c, err := NewClient(
		WithProviderSender("openrouter", sdkFake(sender)),
		WithCatalog(seededCatalog(t)),
		WithLogger(testLogger(t)),
	)
	ck.Require().NoError(err)
	_, err = c.SendParams(context.Background(), SendMeta{ConversationID: "conv"}, batchParams())
	ck.Require().False(err == nil || !strings.Contains(err.Error(), "no batcher configured"), "error = %v, want 'no batcher configured'", err)
	ck.Eq(0, sender.calls, "live sender called")
}

func TestBatchNeedsConversationID(t *testing.T) {
	ck := assert.NewCollecting(t)
	b := &fakeBatcher{}
	sender := &scriptedSender{}
	c := newBatchClient(t, b, sender)

	_, err := c.SendParams(context.Background(), SendMeta{}, batchParams())
	ck.Require().False(err == nil || !strings.Contains(err.Error(), "needs a conversation id"), "error = %v, want 'needs a conversation id'", err)
	ck.Eq(0, b.calls, "batcher called")
}

func TestBatchStreamingBypassed(t *testing.T) {
	ck := assert.NewCollecting(t)
	b := &fakeBatcher{scripts: []func() (json.RawMessage, error){func() (json.RawMessage, error) { return batchReply(), nil }}}
	streamer := newFakeStreamingSender(textStreamEvents("must not stream")...)
	c := newBatchClient(t, b, streamer)

	handlerFired := false
	conv, err := c.Conversation(context.Background(),
		NewConversation("", "test"), Model("openrouter/z-ai/glm-5.3-flash:batch"))
	ck.Require().NoError(err)
	resp, err := conv.Send(context.Background(), UserText("hi"),
		WithStreamHandler(func(anthropic.MessageStreamEventUnion) { handlerFired = true }))
	ck.Require().NoError(err, "Send")
	ck.False(handlerFired, "stream handler must never fire on the batch path")
	if streamer.streamCalls != 0 || streamer.newCalls != 0 {
		t.Errorf("streaming sender reached (stream=%d new=%d), want never", streamer.streamCalls, streamer.newCalls)
	}
	ck.Eq(1, b.calls, "batcher called")
	ck.Eq("batched", resp.Content[0].Text, "response")
}

func TestBatchWaitHookBrackets(t *testing.T) {
	ck := assert.NewAborting(t)
	b := &fakeBatcher{scripts: []func() (json.RawMessage, error){func() (json.RawMessage, error) { return batchReply(), nil }}}
	c := newBatchClient(t, b, &scriptedSender{})

	var seen []bool
	conv, err := c.Conversation(context.Background(),
		NewConversation("", "test"), Model("openrouter/z-ai/glm-5.3-flash:batch"))
	ck.NoError(err)
	if _, err := conv.Send(context.Background(), UserText("hi"),
		WithBatchWait(func(waiting bool) { seen = append(seen, waiting) })); err != nil {
		t.Fatalf("Send: %v", err)
	}
	ck.False(len(seen) != 2 || !seen[0] || seen[1], "hook sequence = %v, want [true false]", seen)

	// On a batcher error the hook must still see false.
	bErr := &fakeBatcher{scripts: []func() (json.RawMessage, error){func() (json.RawMessage, error) {
		return nil, errors.New("llm: batch failed")
	}}}
	c2 := newBatchClient(t, bErr, &scriptedSender{})
	conv2, err := c2.Conversation(context.Background(),
		NewConversation("", "test"), Model("openrouter/z-ai/glm-5.3-flash:batch"))
	ck.NoError(err)
	seen = nil
	if _, err := conv2.Send(context.Background(), UserText("hi"),
		WithBatchWait(func(waiting bool) { seen = append(seen, waiting) })); err == nil {
		t.Fatal("Send with a failing batcher must error")
	}
	ck.False(len(seen) != 2 || !seen[0] || seen[1], "hook sequence on error = %v, want [true false]", seen)
}

func TestBatchCustomIDCharset(t *testing.T) {
	c := assert.NewCollecting(t)
	got := BatchCustomID("01a0d5e6-2636-7afa-b356-cf9441b16e31", 12)
	want := "01a0d5e6-2636-7afa-b356-cf9441b16e31-12"
	c.Require().Eq(want, got, "BatchCustomID")
	c.True(regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`).MatchString(got), "custom_id %q violates Anthropic's ^[A-Za-z0-9_-]{1,64}$ charset", got)
}

// TestBatchTurnCaptured pins the capture bookkeeping on the batch path: with
// a DB-backed capture store, the parked turn is begun and completed on
// success and failed on a batcher error — one turn row per send, resolved
// either way.
func TestBatchTurnCaptured(t *testing.T) {
	ck := assert.NewCollecting(t)
	pool := convTestPool(t)
	ctx := context.Background()

	newConv := func(t *testing.T, b Batcher) (*Client, *Conversation) {
		t.Helper()
		ck := assert.NewAborting(t)
		c, err := NewClient(
			WithProviderSender("openrouter", sdkFake(&scriptedSender{})),
			WithBatcher(b),
			WithStore(pool),
			WithCatalog(seededCatalog(t)),
			WithLogger(testLogger(t)),
		)
		ck.NoError(err)
		conv, err := c.Conversation(ctx, NewConversation("", "test"),
			Model("openrouter/z-ai/glm-5.3-flash:batch"))
		ck.NoError(err)
		return c, conv
	}

	turnStatus := func(t *testing.T, c *Client, convID string) string {
		t.Helper()
		ck := assert.NewAborting(t)
		rows, err := pool.Query(ctx,
			`SELECT status FROM conversations.conversation_turn WHERE conversation_id=$1::uuid ORDER BY ordinal`,
			convID)
		ck.NoError(err)
		defer rows.Close()
		var statuses []string
		for rows.Next() {
			var s string
			ck.NoError(rows.Scan(&s))
			statuses = append(statuses, s)
		}
		ck.Len(statuses, 1, "turn rows")
		return statuses[0]
	}

	turnUpstream := func(t *testing.T, convID string) string {
		t.Helper()
		ck := assert.NewAborting(t)
		var upstream *string
		ck.NoError(pool.QueryRow(ctx,
			`SELECT upstream FROM conversations.conversation_turn WHERE conversation_id=$1::uuid ORDER BY ordinal LIMIT 1`,
			convID).Scan(&upstream))
		ck.NotNil(upstream, "captured turn upstream is NULL, want the primary provider name")
		return *upstream
	}

	// Success path.
	b := &fakeBatcher{scripts: []func() (json.RawMessage, error){func() (json.RawMessage, error) { return batchReply(), nil }}}
	c, conv := newConv(t, b)
	if _, err := conv.Send(ctx, UserText("hi")); err != nil {
		t.Fatalf("Send: %v", err)
	}
	ck.Eq("complete", turnStatus(t, c, conv.ID), "turn status")
	// The batch turn's upstream must be the PRIMARY PROVIDER NAME (what a
	// live turn records, and what insights' failover joins read), never the
	// model id the request carries.
	ck.Eq("openrouter", turnUpstream(t, conv.ID), "turn upstream")

	// Error path.
	bErr := &fakeBatcher{scripts: []func() (json.RawMessage, error){func() (json.RawMessage, error) {
		return nil, errors.New("llm: batch failed")
	}}}
	c2, conv2 := newConv(t, bErr)
	_, err := conv2.Send(ctx, UserText("hi"))
	ck.Require().Error(err, "Send with a failing batcher must error")
	ck.Eq("error", turnStatus(t, c2, conv2.ID), "turn status")
}

// TestBatchParkStripsForeignThinking is the :batch twin of the sender-level
// strip. A conversation that started on OpenAI Responses carries reasoning
// signatures in its thinking blocks, and this path never reaches a sender, so
// without the codec's own foreign-block rule a parked request would carry a
// signature the batch provider cannot verify.
//
// It calls parkSend DIRECTLY rather than through send, and that is the point:
// prepareSend parks only a first call (no assistant message), so today a parked
// request cannot carry a thinking block. The strip is defence in depth for the
// day that rule relaxes, and this pins parkSend's own contract instead of a
// caller's.
func TestBatchParkStripsForeignThinking(t *testing.T) {
	ck := assert.NewCollecting(t)
	b := &fakeBatcher{scripts: []func() (json.RawMessage, error){func() (json.RawMessage, error) { return batchReply(), nil }}}
	sender := &scriptedSender{scripts: []func(anthropic.MessageNewParams) (*anthropic.Message, error){
		respondText("must not be reached"),
	}}
	c := newBatchClient(t, b, sender)

	req := &msg.Request{
		Model: "z-ai/glm-5.3-flash:batch", MaxTokens: 16,
		Messages: []msg.Message{
			{Role: msg.User, Content: []msg.Block{msg.Text{Text: "hi"}}},
			{Role: msg.Assistant, Content: []msg.Block{
				msg.Thinking{Text: "reasoning text", Sig: msg.Signature{
					Dialect: msg.DialectResponsesAPI, Value: "openai-responses:api:rs_1:encrypted-blob"}},
				msg.Text{Text: "reply"},
			}},
		},
	}
	ctx := context.Background()
	p := &preparedSend{ctx: ctx, span: trace.SpanFromContext(ctx), primary: "openrouter", req: req}
	_, err := c.parkSend(ctx, p.span, SendMeta{ConversationID: "conv"}, p)
	ck.Require().NoError(err, "parkSend")
	ck.Require().Eq(1, b.calls, "batcher called")

	var body struct {
		Messages []struct {
			Role    string `json:"role"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"messages"`
	}
	ck.Require().NoError(json.Unmarshal(b.lastBody, &body), "decode the parked body")
	last := body.Messages[len(body.Messages)-1]
	ck.Require().Len(last.Content, 1, "content after the strip")
	ck.Eq("text", last.Content[0].Type, "the surviving block is text")
	ck.Eq("reply", last.Content[0].Text, "the text block survives")
}

// TestBatchParkSendsAnthropicBody pins what actually leaves for the provider:
// the parked call's per-call body is the ANTHROPIC wire form of the native
// request — model, system, messages, tools, tool_choice and the cache policy's
// breakpoint — and it never asks for a stream (a batch call cannot stream).
func TestBatchParkSendsAnthropicBody(t *testing.T) {
	ck := assert.NewCollecting(t)
	b := &fakeBatcher{scripts: []func() (json.RawMessage, error){func() (json.RawMessage, error) { return batchReply(), nil }}}
	c := newBatchClient(t, b, &scriptedSender{})

	req := &msg.Request{
		Model: "z-ai/glm-5.3-flash:batch", MaxTokens: 32,
		System:   []msg.Text{{Text: "sys"}},
		Messages: []msg.Message{{Role: msg.User, Content: []msg.Block{msg.Text{Text: "hi"}}}},
		Tools: []msg.ToolDef{
			{Name: "bash", Description: "run", InputSchema: json.RawMessage(`{"type":"object"}`)},
		},
		ForceTool: "bash",
		Cache:     CachePolicy{SystemTTL: Cache5m, MessagesTTL: CacheOff, Breakpoints: 1},
	}
	ctx := context.Background()
	p := &preparedSend{ctx: ctx, span: trace.SpanFromContext(ctx), primary: "openrouter", req: req}
	_, err := c.parkSend(ctx, p.span, SendMeta{ConversationID: "conv", Ordinal: 3}, p)
	ck.Require().NoError(err, "parkSend")
	ck.Require().Eq(1, b.calls, "batcher called")

	var body struct {
		Model     string `json:"model"`
		MaxTokens int64  `json:"max_tokens"`
		Stream    *bool  `json:"stream"`
		System    []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"system"`
		Messages []struct {
			Role    string `json:"role"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"messages"`
		Tools []struct {
			Name        string         `json:"name"`
			Description string         `json:"description"`
			InputSchema map[string]any `json:"input_schema"`
		} `json:"tools"`
		ToolChoice *struct {
			Type string `json:"type"`
			Name string `json:"name"`
		} `json:"tool_choice"`
	}
	ck.Require().NoError(json.Unmarshal(b.lastBody, &body), "the parked body must be Anthropic JSON: %s", b.lastBody)
	ck.Eq("z-ai/glm-5.3-flash:batch", body.Model, "model")
	ck.Eq(int64(32), body.MaxTokens, "max_tokens")
	ck.Nil(body.Stream, "a parked call must never ask for a stream")
	ck.Require().Len(body.System, 1, "system blocks")
	ck.Eq("sys", body.System[0].Text, "system text")
	ck.Require().Len(body.Messages, 1, "messages")
	ck.Require().Len(body.Messages[0].Content, 1, "content blocks")
	ck.Eq("hi", body.Messages[0].Content[0].Text, "message text")
	ck.Require().Len(body.Tools, 1, "tools")
	ck.Eq("bash", body.Tools[0].Name, "tool name")
	ck.Eq("run", body.Tools[0].Description, "tool description")
	ck.Require().NotNil(body.ToolChoice, "tool_choice")
	ck.Eq("bash", body.ToolChoice.Name, "forced tool")
	ck.StrContains(string(b.lastBody), `"cache_control"`, "the cache policy's breakpoint must ride the parked body")
}
