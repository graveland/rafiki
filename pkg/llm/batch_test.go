// SPDX-License-Identifier: Apache-2.0

package llm

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"

	"github.com/multigres/testkit/assert"
)

// fakeBatcher records Park's arguments and replays a queued result. The last
// script repeats (mirrors scriptedSender's convention).
type fakeBatcher struct {
	mu       sync.Mutex
	calls    int
	lastIDs  []string
	lastMods []string
	scripts  []func() (*anthropic.Message, error)
}

func (b *fakeBatcher) Park(_ context.Context, customID, model string, _ anthropic.MessageNewParams) (*anthropic.Message, error) {
	b.mu.Lock()
	b.calls++
	b.lastIDs = append(b.lastIDs, customID)
	b.lastMods = append(b.lastMods, model)
	b.mu.Unlock()
	i := b.calls - 1
	if i >= len(b.scripts) {
		i = len(b.scripts) - 1
	}
	return b.scripts[i]()
}

func batchReply() *anthropic.Message {
	return cannedMessage(`{"id":"msg_b","type":"message","role":"assistant","model":"z-ai/glm-5.3-flash:batch",
		"content":[{"type":"text","text":"batched"}],
		"stop_reason":"end_turn","usage":{"input_tokens":7,"output_tokens":3}}`)
}

// newBatchClient builds a store-less client whose "openrouter" provider is
// backed by sender (never expected to be called on the park path) and whose
// batcher is b.
func newBatchClient(t *testing.T, b Batcher, sender Sender) *Client {
	t.Helper()
	c, err := NewClient(
		WithProviderSender("anthropic", sender),
		WithProviderSender("openrouter", sender),
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
	b := &fakeBatcher{scripts: []func() (*anthropic.Message, error){func() (*anthropic.Message, error) { return batchReply(), nil }}}
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
	b := &fakeBatcher{scripts: []func() (*anthropic.Message, error){func() (*anthropic.Message, error) {
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
		WithProviderSender("openrouter", sender),
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
	b := &fakeBatcher{scripts: []func() (*anthropic.Message, error){func() (*anthropic.Message, error) { return batchReply(), nil }}}
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
	b := &fakeBatcher{scripts: []func() (*anthropic.Message, error){func() (*anthropic.Message, error) { return batchReply(), nil }}}
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
	bErr := &fakeBatcher{scripts: []func() (*anthropic.Message, error){func() (*anthropic.Message, error) {
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
			WithProviderSender("openrouter", &scriptedSender{}),
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
	b := &fakeBatcher{scripts: []func() (*anthropic.Message, error){func() (*anthropic.Message, error) { return batchReply(), nil }}}
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
	bErr := &fakeBatcher{scripts: []func() (*anthropic.Message, error){func() (*anthropic.Message, error) {
		return nil, errors.New("llm: batch failed")
	}}}
	c2, conv2 := newConv(t, bErr)
	_, err := conv2.Send(ctx, UserText("hi"))
	ck.Require().Error(err, "Send with a failing batcher must error")
	ck.Eq("error", turnStatus(t, c2, conv2.ID), "turn status")
}
