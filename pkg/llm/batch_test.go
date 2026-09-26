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
	if err != nil {
		t.Fatal(err)
	}
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
	b := &fakeBatcher{scripts: []func() (*anthropic.Message, error){func() (*anthropic.Message, error) { return batchReply(), nil }}}
	sender := &scriptedSender{scripts: []func(anthropic.MessageNewParams) (*anthropic.Message, error){
		respondText("must not be reached"),
	}}
	c := newBatchClient(t, b, sender)

	resp, err := c.SendParams(context.Background(), SendMeta{ConversationID: "01a0d5e6-2636-7afa-b356-cf9441b16e31", Ordinal: 12}, batchParams())
	if err != nil {
		t.Fatalf("SendParams: %v", err)
	}
	if got := resp.Content[0].Text; got != "batched" {
		t.Errorf("response = %q, want the batch result", got)
	}
	if b.calls != 1 {
		t.Fatalf("batcher called %d times, want 1", b.calls)
	}
	if got := b.lastMods[0]; got != "z-ai/glm-5.3-flash:batch" {
		t.Errorf("batcher model = %q, want the resolved :batch id z-ai/glm-5.3-flash:batch", got)
	}
	if got := b.lastIDs[0]; got != "01a0d5e6-2636-7afa-b356-cf9441b16e31-12" {
		t.Errorf("batcher customID = %q, want <conversation>-<ordinal>", got)
	}
	if sender.calls != 0 {
		t.Errorf("live sender called %d times, want 0 (first call parks)", sender.calls)
	}
}

func TestBatchLaterCallStripsSuffix(t *testing.T) {
	b := &fakeBatcher{scripts: []func() (*anthropic.Message, error){func() (*anthropic.Message, error) {
		t.Error("batcher must not be called on a later call")
		return batchReply(), nil
	}}}
	sender := &scriptedSender{scripts: []func(anthropic.MessageNewParams) (*anthropic.Message, error){
		respondText("live"),
	}}
	c := newBatchClient(t, b, sender)

	resp, err := c.SendParams(context.Background(), SendMeta{ConversationID: "conv"}, laterCallParams())
	if err != nil {
		t.Fatalf("SendParams: %v", err)
	}
	if got := resp.Content[0].Text; got != "live" {
		t.Errorf("response = %q, want the live sender's", got)
	}
	if sender.calls != 1 {
		t.Fatalf("live sender called %d times, want 1", sender.calls)
	}
	if got := string(sender.lastReq[0].Model); got != "z-ai/glm-5.3-flash" {
		t.Errorf("live model = %q, want the suffix-stripped z-ai/glm-5.3-flash", got)
	}
	if b.calls != 0 {
		t.Errorf("batcher called %d times, want 0", b.calls)
	}
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
	if err == nil || !strings.Contains(err.Error(), "served only by an anthropic-openrouter provider") {
		t.Fatalf("error = %v, want the anthropic-openrouter-only refusal", err)
	}
	if sender.calls != 0 || b.calls != 0 {
		t.Errorf("nothing may be sent: sender=%d batcher=%d, want 0/0", sender.calls, b.calls)
	}
}

func TestBatchNoBatcherErrors(t *testing.T) {
	sender := &scriptedSender{scripts: []func(anthropic.MessageNewParams) (*anthropic.Message, error){
		respondText("must not be reached"),
	}}
	c, err := NewClient(
		WithProviderSender("openrouter", sender),
		WithCatalog(seededCatalog(t)),
		WithLogger(testLogger(t)),
	)
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.SendParams(context.Background(), SendMeta{ConversationID: "conv"}, batchParams())
	if err == nil || !strings.Contains(err.Error(), "no batcher configured") {
		t.Fatalf("error = %v, want 'no batcher configured'", err)
	}
	if sender.calls != 0 {
		t.Errorf("live sender called %d times, want 0", sender.calls)
	}
}

func TestBatchNeedsConversationID(t *testing.T) {
	b := &fakeBatcher{}
	sender := &scriptedSender{}
	c := newBatchClient(t, b, sender)

	_, err := c.SendParams(context.Background(), SendMeta{}, batchParams())
	if err == nil || !strings.Contains(err.Error(), "needs a conversation id") {
		t.Fatalf("error = %v, want 'needs a conversation id'", err)
	}
	if b.calls != 0 {
		t.Errorf("batcher called %d times, want 0", b.calls)
	}
}

func TestBatchStreamingBypassed(t *testing.T) {
	b := &fakeBatcher{scripts: []func() (*anthropic.Message, error){func() (*anthropic.Message, error) { return batchReply(), nil }}}
	streamer := newFakeStreamingSender(textStreamEvents("must not stream")...)
	c := newBatchClient(t, b, streamer)

	handlerFired := false
	conv, err := c.Conversation(context.Background(),
		NewConversation("", "test"), Model("openrouter/z-ai/glm-5.3-flash:batch"))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := conv.Send(context.Background(), UserText("hi"),
		WithStreamHandler(func(anthropic.MessageStreamEventUnion) { handlerFired = true }))
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if handlerFired {
		t.Error("stream handler must never fire on the batch path")
	}
	if streamer.streamCalls != 0 || streamer.newCalls != 0 {
		t.Errorf("streaming sender reached (stream=%d new=%d), want never", streamer.streamCalls, streamer.newCalls)
	}
	if b.calls != 1 {
		t.Errorf("batcher called %d times, want 1", b.calls)
	}
	if got := resp.Content[0].Text; got != "batched" {
		t.Errorf("response = %q, want the batch result", got)
	}
}

func TestBatchWaitHookBrackets(t *testing.T) {
	b := &fakeBatcher{scripts: []func() (*anthropic.Message, error){func() (*anthropic.Message, error) { return batchReply(), nil }}}
	c := newBatchClient(t, b, &scriptedSender{})

	var seen []bool
	conv, err := c.Conversation(context.Background(),
		NewConversation("", "test"), Model("openrouter/z-ai/glm-5.3-flash:batch"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conv.Send(context.Background(), UserText("hi"),
		WithBatchWait(func(waiting bool) { seen = append(seen, waiting) })); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if len(seen) != 2 || !seen[0] || seen[1] {
		t.Fatalf("hook sequence = %v, want [true false]", seen)
	}

	// On a batcher error the hook must still see false.
	bErr := &fakeBatcher{scripts: []func() (*anthropic.Message, error){func() (*anthropic.Message, error) {
		return nil, errors.New("llm: batch failed")
	}}}
	c2 := newBatchClient(t, bErr, &scriptedSender{})
	conv2, err := c2.Conversation(context.Background(),
		NewConversation("", "test"), Model("openrouter/z-ai/glm-5.3-flash:batch"))
	if err != nil {
		t.Fatal(err)
	}
	seen = nil
	if _, err := conv2.Send(context.Background(), UserText("hi"),
		WithBatchWait(func(waiting bool) { seen = append(seen, waiting) })); err == nil {
		t.Fatal("Send with a failing batcher must error")
	}
	if len(seen) != 2 || !seen[0] || seen[1] {
		t.Fatalf("hook sequence on error = %v, want [true false]", seen)
	}
}

func TestBatchCustomIDCharset(t *testing.T) {
	got := BatchCustomID("01a0d5e6-2636-7afa-b356-cf9441b16e31", 12)
	want := "01a0d5e6-2636-7afa-b356-cf9441b16e31-12"
	if got != want {
		t.Fatalf("BatchCustomID = %q, want %q", got, want)
	}
	if !regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`).MatchString(got) {
		t.Errorf("custom_id %q violates Anthropic's ^[A-Za-z0-9_-]{1,64}$ charset", got)
	}
}

// TestBatchTurnCaptured pins the capture bookkeeping on the batch path: with
// a DB-backed capture store, the parked turn is begun and completed on
// success and failed on a batcher error — one turn row per send, resolved
// either way.
func TestBatchTurnCaptured(t *testing.T) {
	pool := convTestPool(t)
	ctx := context.Background()

	newConv := func(t *testing.T, b Batcher) (*Client, *Conversation) {
		t.Helper()
		c, err := NewClient(
			WithProviderSender("openrouter", &scriptedSender{}),
			WithBatcher(b),
			WithStore(pool),
			WithCatalog(seededCatalog(t)),
			WithLogger(testLogger(t)),
		)
		if err != nil {
			t.Fatal(err)
		}
		conv, err := c.Conversation(ctx, NewConversation("", "test"),
			Model("openrouter/z-ai/glm-5.3-flash:batch"))
		if err != nil {
			t.Fatal(err)
		}
		return c, conv
	}

	turnStatus := func(t *testing.T, c *Client, convID string) string {
		t.Helper()
		rows, err := pool.Query(ctx,
			`SELECT status FROM conversations.conversation_turn WHERE conversation_id=$1::uuid ORDER BY ordinal`,
			convID)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var statuses []string
		for rows.Next() {
			var s string
			if err := rows.Scan(&s); err != nil {
				t.Fatal(err)
			}
			statuses = append(statuses, s)
		}
		if len(statuses) != 1 {
			t.Fatalf("turn rows = %v, want exactly one", statuses)
		}
		return statuses[0]
	}

	// Success path.
	b := &fakeBatcher{scripts: []func() (*anthropic.Message, error){func() (*anthropic.Message, error) { return batchReply(), nil }}}
	c, conv := newConv(t, b)
	if _, err := conv.Send(ctx, UserText("hi")); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if got := turnStatus(t, c, conv.ID); got != "complete" {
		t.Errorf("turn status = %q, want complete", got)
	}

	// Error path.
	bErr := &fakeBatcher{scripts: []func() (*anthropic.Message, error){func() (*anthropic.Message, error) {
		return nil, errors.New("llm: batch failed")
	}}}
	c2, conv2 := newConv(t, bErr)
	if _, err := conv2.Send(ctx, UserText("hi")); err == nil {
		t.Fatal("Send with a failing batcher must error")
	}
	if got := turnStatus(t, c2, conv2.ID); got != "error" {
		t.Errorf("turn status = %q, want error", got)
	}
}
