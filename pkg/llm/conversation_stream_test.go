// SPDX-License-Identifier: Apache-2.0

package llm

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/packages/ssestream"

	"go.graveland.dev/rafiki/pkg/routing"
)

// newTestConversation builds a store-less (in-memory) Conversation around
// sender — fast unit-test scaffolding for the streaming path, no DB needed.
func newTestConversation(t *testing.T, sender Sender) *Conversation {
	t.Helper()
	c, err := NewClient(
		WithUpstream(UpstreamAnthropic, sender),
		WithDefaultModel("claude-haiku-4-5"),
		WithLogger(testLogger(t)),
	)
	if err != nil {
		t.Fatal(err)
	}
	conv, err := c.Conversation(context.Background(),
		NewConversation("brent", "test"), Model("claude-haiku-4-5"), SystemText("sys"))
	if err != nil {
		t.Fatal(err)
	}
	return conv
}

// newTestConversationWithBreaker builds a Conversation with a real breaker
// AND a fallback chain configured on the primary — the shape needed for
// sendStreaming to genuinely consult and update the primary's breaker
// (mirrors TestSend_StreamEngagesWithFallbackAndBreakerConfigured's setup).
func newTestConversationWithBreaker(t *testing.T, primary, fallback Sender) (*Conversation, *Client) {
	t.Helper()
	c, err := NewClient(
		WithUpstream(UpstreamAnthropic, primary),
		WithUpstream(UpstreamOpenRouter, fallback),
		WithBreaker(15*time.Minute),
		WithCatalog(seededCatalog(t)),
		WithDefaultModel("claude-haiku-4-5"),
		WithLogger(testLogger(t)),
	)
	if err != nil {
		t.Fatal(err)
	}
	conv, err := c.Conversation(context.Background(),
		NewConversation("brent", "test"), Model("claude-haiku-4-5"), SystemText("sys"),
		Fallback(UpstreamOpenRouter))
	if err != nil {
		t.Fatal(err)
	}
	return conv, c
}

// newTestConversationWithOpenBreaker is newTestConversationWithBreaker with
// the primary's breaker pre-tripped open by a prior retryable failure —
// models the sustained-primary-degradation state the breaker exists to
// detect, independent of anything sendStreaming itself does in the test.
func newTestConversationWithOpenBreaker(t *testing.T, primary, fallback Sender) (*Conversation, *Client) {
	t.Helper()
	conv, c := newTestConversationWithBreaker(t, primary, fallback)
	c.Breaker(UpstreamAnthropic).RecordResult(time.Now(), true)
	if !c.Breaker(UpstreamAnthropic).Open() {
		t.Fatal("test setup: breaker did not open")
	}
	return conv, c
}

// breakerSawFailure reports whether the breaker is currently open. Open() is
// the only state routing.Breaker exposes, so this is the sole hook available
// to prove a failure was recorded via the breaker's own API rather than by
// reaching into a private field.
func breakerSawFailure(b *routing.Breaker) bool {
	return b.Open()
}

// textOf extracts the delta text from a text_delta content_block_delta
// event, "" for anything else — the handler-side probe the tests use to
// reconstruct what was streamed.
func textOf(ev anthropic.MessageStreamEventUnion) string {
	if ev.Type == "content_block_delta" && ev.Delta.Type == "text_delta" {
		return ev.Delta.Text
	}
	return ""
}

// textOfMessage concatenates all text content blocks of an accumulated
// message.
func textOfMessage(msg *anthropic.Message) string {
	var sb strings.Builder
	for _, c := range msg.Content {
		sb.WriteString(c.Text)
	}
	return sb.String()
}

func sseEvent(typ, raw string) ssestream.Event {
	return ssestream.Event{Type: typ, Data: []byte(raw)}
}

func messageStartEvent() ssestream.Event {
	return sseEvent("message_start", `{"type":"message_start","message":{"id":"msg_stream","type":"message",
		"role":"assistant","model":"claude-haiku-4-5","content":[],"usage":{"input_tokens":10,"output_tokens":0}}}`)
}

func contentBlockStartEvent() ssestream.Event {
	return sseEvent("content_block_start",
		`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`)
}

func textDeltaEvent(s string) ssestream.Event {
	return sseEvent("content_block_delta",
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"`+s+`"}}`)
}

func contentBlockStopEvent() ssestream.Event {
	return sseEvent("content_block_stop", `{"type":"content_block_stop","index":0}`)
}

func messageDeltaEvent() ssestream.Event {
	return sseEvent("message_delta",
		`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":5}}`)
}

func messageStopEvent() ssestream.Event {
	return sseEvent("message_stop", `{"type":"message_stop"}`)
}

// textStreamEvents composes a complete, valid single-text-block streaming
// response out of the given deltas (message_start..message_stop) — the
// realistic event sequence anthropic.Message.Accumulate expects.
func textStreamEvents(parts ...string) []ssestream.Event {
	events := []ssestream.Event{messageStartEvent(), contentBlockStartEvent()}
	for _, p := range parts {
		events = append(events, textDeltaEvent(p))
	}
	events = append(events, contentBlockStopEvent(), messageDeltaEvent(), messageStopEvent())
	return events
}

// fakeDecoder replays a fixed event slice, then (once exhausted) surfaces
// trailErr from Err() — modeling a stream that fails after already
// delivering zero or more real events (ssestream.Stream.Next() sets its
// internal err from decoder.Err() exactly when decoder.Next() returns
// false).
type fakeDecoder struct {
	events   []ssestream.Event
	idx      int
	trailErr error
}

func (d *fakeDecoder) Next() bool {
	if d.idx >= len(d.events) {
		return false
	}
	d.idx++
	return true
}
func (d *fakeDecoder) Event() ssestream.Event { return d.events[d.idx-1] }
func (d *fakeDecoder) Close() error           { return nil }
func (d *fakeDecoder) Err() error             { return d.trailErr }

// streamScript is one scripted NewStreaming call: openErr models the
// request-level rejection the real SDK plumbs straight into
// ssestream.NewStream's err argument (e.g. a real prompt-too-large 400,
// which the API returns before any event is ever emitted); trailErr models a
// decoder-surfaced error once events are exhausted (used to prove, for
// testing purposes only, that the library's trim-retry guard does not rely
// on errors always arriving before content — see
// TestSend_StreamTrimRetryNeverRetriesAfterDelivery).
type streamScript struct {
	events   []ssestream.Event
	openErr  error
	trailErr error
	// newErr, when set, is returned by New() independent of openErr — models
	// the non-streaming retry (SendParams/callModel, driven by sendAttempt on
	// attempted=false) hitting the same failing upstream after a streaming
	// attempt that got further than "rejected before any event" (which
	// openErr alone models). openErr can't do double duty here: NewStream
	// wires it straight into the stream's terminal err, so Stream.Next()
	// would bail before ever surfacing a queued event.
	newErr error
}

// fakeStreamingSender implements llm.StreamingSender, replaying queued
// streamScripts in order (the last repeats, mirroring scriptedSender in
// llm_test.go).
type fakeStreamingSender struct {
	scripts     []streamScript
	calls       int // indexes scripts for NewStreaming
	streamCalls int
	newCalls    int // indexes scripts for New, independent of calls/streamCalls
	lastParams  []anthropic.MessageNewParams
}

func newFakeStreamingSender(events ...ssestream.Event) *fakeStreamingSender {
	return &fakeStreamingSender{scripts: []streamScript{{events: events}}}
}

// New mirrors NewStreaming's script selection (openErr fails the call
// exactly as it fails NewStreaming, modeling a primary whose non-streaming
// retry — e.g. from callModel during a SendParams fallback handoff — hits
// the identical rejection) but indexes independently via newCalls, since a
// pre-delivery-failover test drives New and NewStreaming as separate call
// sequences on the same sender.
func (f *fakeStreamingSender) New(_ context.Context, params anthropic.MessageNewParams) (*anthropic.Message, error) {
	f.lastParams = append(f.lastParams, params)
	i := f.newCalls
	if i >= len(f.scripts) {
		i = len(f.scripts) - 1
	}
	f.newCalls++
	s := f.scripts[i]
	if s.newErr != nil {
		return nil, s.newErr
	}
	if s.openErr != nil {
		return nil, s.openErr
	}
	acc := anthropic.Message{}
	for _, e := range s.events {
		var ev anthropic.MessageStreamEventUnion
		if err := (&ev).UnmarshalJSON(e.Data); err == nil {
			_ = acc.Accumulate(ev)
		}
	}
	return &acc, nil
}

func (f *fakeStreamingSender) NewStreaming(_ context.Context, params anthropic.MessageNewParams) (*ssestream.Stream[anthropic.MessageStreamEventUnion], error) {
	f.streamCalls++
	f.lastParams = append(f.lastParams, params)
	i := f.calls
	if i >= len(f.scripts) {
		i = len(f.scripts) - 1
	}
	f.calls++
	s := f.scripts[i]
	return ssestream.NewStream[anthropic.MessageStreamEventUnion](
		&fakeDecoder{events: s.events, trailErr: s.trailErr}, s.openErr), nil
}

// nonStreamingFake satisfies Sender only (no NewStreaming) — proves the
// silent-fallback path: WithStreamHandler must never fire its handler, and
// Send must still return the plain reply, when the resolved sender can't
// stream.
type nonStreamingFake struct {
	reply string
	calls int
}

func (f *nonStreamingFake) New(_ context.Context, _ anthropic.MessageNewParams) (*anthropic.Message, error) {
	f.calls++
	return cannedMessage(`{"id":"m","type":"message","role":"assistant","model":"m",
		"content":[{"type":"text","text":"` + f.reply + `"}],
		"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`), nil
}

// closeTrackingDecoder wraps fakeDecoder to record whether Close was called
// — the leak-detection probe for sendStreaming's defer-hoist fix below.
type closeTrackingDecoder struct {
	fakeDecoder
	closed bool
}

func (d *closeTrackingDecoder) Close() error {
	d.closed = true
	return nil
}

// leakySender's NewStreaming returns a non-nil stream ALONGSIDE a non-nil
// error — the shape no first-party implementation produces (sdkSender's
// NewStreaming, and every other fake in this package, always return a nil
// error; any failure is instead plumbed into the returned stream's own
// Err()), but which StreamingSender's contract does not forbid. This is the
// one shape that actually exercises sendStreaming's NewStreaming-error
// branch via the real return value rather than stream.Err() — the "dead
// code trap" TestSend_StreamFailsOverOnPreDeliveryPrimaryFailure and its
// siblings fall into (see streamScript's openErr doc): those wire openErr
// into ssestream.NewStream's err argument, so they always exercise
// stream.Err(), never NewStreaming's own error return.
type leakySender struct {
	decoder *closeTrackingDecoder
}

func (s *leakySender) New(_ context.Context, _ anthropic.MessageNewParams) (*anthropic.Message, error) {
	return nil, errors.New("leakySender: New called, want NewStreaming")
}

func (s *leakySender) NewStreaming(_ context.Context, _ anthropic.MessageNewParams) (*ssestream.Stream[anthropic.MessageStreamEventUnion], error) {
	return ssestream.NewStream[anthropic.MessageStreamEventUnion](s.decoder, nil), errors.New("boom")
}

// TestSendStreaming_ClosesStreamEvenWhenNewStreamingReturnsBothStreamAndError
// pins the leak fix: sendStreaming's defer that closes the stream must be
// registered BEFORE the NewStreaming-error checks, not after, so a
// StreamingSender that returns (non-nil stream, non-nil err) still gets its
// decoder closed instead of leaked. leakySender has no fallback or breaker
// configured (newTestConversation), so sendStreaming takes the breaker-nil,
// attempted=true return — a single NewStreaming call, no retry loop to
// confound the assertion.
func TestSendStreaming_ClosesStreamEvenWhenNewStreamingReturnsBothStreamAndError(t *testing.T) {
	decoder := &closeTrackingDecoder{}
	sender := &leakySender{decoder: decoder}
	conv := newTestConversation(t, sender)

	_, err := conv.Send(context.Background(), UserText("hi"),
		WithStreamHandler(func(anthropic.MessageStreamEventUnion) {}))
	if err == nil {
		t.Fatal("Send must fail: leakySender's NewStreaming always errors")
	}
	if !decoder.closed {
		t.Error("stream must be closed even when NewStreaming returns a non-nil error alongside a non-nil stream")
	}
}

func TestSend_StreamHandlerReceivesEventsAndAccumulates(t *testing.T) {
	sender := newFakeStreamingSender(textStreamEvents("Hel", "lo")...)
	conv := newTestConversation(t, sender)

	var seen []string
	msg, err := conv.Send(context.Background(), UserText("hi"),
		WithStreamHandler(func(ev anthropic.MessageStreamEventUnion) {
			if d := textOf(ev); d != "" {
				seen = append(seen, d)
			}
		}))
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if got := strings.Join(seen, ""); got != "Hello" {
		t.Errorf("handler saw %q, want Hello", got)
	}
	if got := textOfMessage(msg); got != "Hello" {
		t.Errorf("accumulated message = %q, want Hello", got)
	}
	if sender.streamCalls != 1 {
		t.Errorf("streamCalls = %d, want 1", sender.streamCalls)
	}
}

func TestSend_FallsBackWhenSenderCannotStream(t *testing.T) {
	sender := &nonStreamingFake{reply: "Hello"}
	conv := newTestConversation(t, sender)

	called := false
	msg, err := conv.Send(context.Background(), UserText("hi"),
		WithStreamHandler(func(anthropic.MessageStreamEventUnion) { called = true }))
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if called {
		t.Error("handler must not fire for a non-streaming sender")
	}
	if got := textOfMessage(msg); got != "Hello" {
		t.Errorf("non-streaming fallback message = %q, want Hello", got)
	}
	if sender.calls != 1 {
		t.Errorf("New calls = %d, want exactly 1 (no duplicate attempt)", sender.calls)
	}
}

func TestSend_NoHandlerUsesNonStreamingPath(t *testing.T) {
	sender := newFakeStreamingSender(textStreamEvents("x")...)
	conv := newTestConversation(t, sender)

	if _, err := conv.Send(context.Background(), UserText("hi")); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if sender.streamCalls != 0 {
		t.Error("no handler means no streaming call")
	}
}

// seedBigHistory writes n large messages so the default TrimPolicy has
// something to drop, mirroring TestConversationTrimRetryKeepsPrefixAndRows.
func seedBigHistory(t *testing.T, conv *Conversation, n int) {
	t.Helper()
	big := strings.Repeat("y", 80*1024)
	for i := range n {
		role := anthropic.MessageParamRoleUser
		if i%2 == 1 {
			role = anthropic.MessageParamRoleAssistant
		}
		msg := anthropic.MessageParam{Role: role,
			Content: []anthropic.ContentBlockParamUnion{anthropic.NewTextBlock(big)}}
		if err := conv.appendMessage(context.Background(), i, msg, nil); err != nil {
			t.Fatal(err)
		}
	}
}

// TestSend_StreamTrimRetrySucceedsWhenNoEventsDelivered is the streaming
// analog of TestConversationTrimRetryKeepsPrefixAndRows: the first attempt
// is rejected as prompt-too-large before any event is delivered (the
// realistic API shape — see streamScript's openErr doc), so the retry must
// still happen and must still stream through the handler.
func TestSend_StreamTrimRetrySucceedsWhenNoEventsDelivered(t *testing.T) {
	sender := &fakeStreamingSender{scripts: []streamScript{
		{openErr: promptTooLargeErr()},
		{events: textStreamEvents("after trim")},
	}}
	conv := newTestConversation(t, sender)
	seedBigHistory(t, conv, 5)

	var seen []string
	msg, err := conv.Send(context.Background(), UserText("question"),
		WithStreamHandler(func(ev anthropic.MessageStreamEventUnion) {
			if d := textOf(ev); d != "" {
				seen = append(seen, d)
			}
		}))
	if err != nil {
		t.Fatalf("Send with trim-retry: %v", err)
	}
	if got := textOfMessage(msg); got != "after trim" {
		t.Errorf("message = %q, want %q", got, "after trim")
	}
	if got := strings.Join(seen, ""); got != "after trim" {
		t.Errorf("handler saw %q, want only the retry's content", got)
	}
	if sender.streamCalls != 2 {
		t.Errorf("streamCalls = %d, want 2 (failed attempt + successful retry)", sender.streamCalls)
	}
	if len(sender.lastParams) != 2 || len(sender.lastParams[1].Messages) >= len(sender.lastParams[0].Messages) {
		t.Errorf("retry not trimmed: attempts=%d", len(sender.lastParams))
	}
}

// TestSend_StreamTrimRetryNeverRetriesAfterDelivery is the structural
// guarantee behind THE SECOND CONSTRAINT: once ANY event has reached the
// caller's handler for an attempt, sendWithTrim must never retry — even
// when the eventual error looks exactly like the ordinary
// isPromptTooLarge trim-retry trigger. This does not rely on the API's
// actual before-any-content error timing: the first scripted attempt
// deliberately delivers a real content event and THEN errors
// prompt-too-large, to prove the guard holds regardless.
func TestSend_StreamTrimRetryNeverRetriesAfterDelivery(t *testing.T) {
	sender := &fakeStreamingSender{scripts: []streamScript{
		{
			events:   []ssestream.Event{messageStartEvent(), contentBlockStartEvent(), textDeltaEvent("partial")},
			trailErr: promptTooLargeErr(),
		},
		{events: textStreamEvents("SHOULD NEVER BE DELIVERED")},
	}}
	conv := newTestConversation(t, sender)
	seedBigHistory(t, conv, 5)

	var seen []string
	handlerCalls := 0
	_, err := conv.Send(context.Background(), UserText("question"),
		WithStreamHandler(func(ev anthropic.MessageStreamEventUnion) {
			handlerCalls++
			if d := textOf(ev); d != "" {
				seen = append(seen, d)
			}
		}))
	if err == nil {
		t.Fatal("Send must fail: a retry after partial delivery would double-deliver events")
	}
	if !isPromptTooLarge(err) {
		t.Errorf("expected the underlying prompt-too-large error to surface, got: %v", err)
	}
	if sender.streamCalls != 1 {
		t.Errorf("streamCalls = %d, want 1 (must NOT retry once events were delivered)", sender.streamCalls)
	}
	if handlerCalls != 3 {
		t.Errorf("handler invoked %d times, want exactly 3 (the first attempt's events only)", handlerCalls)
	}
	if strings.Join(seen, "") != "partial" {
		t.Errorf("handler saw %q, want only %q (never the second script)", strings.Join(seen, ""), "partial")
	}
}

// TestSend_StreamEngagesWithFallbackAndBreakerConfigured guards the
// regression a narrower guard shipped: fundi enables Fallback(OpenRouter)
// AND WithBreaker together whenever an OpenRouter key is configured (the
// common case), and a guard that bailed out of streaming whenever BOTH were
// present made WithStreamHandler silently dead in exactly that deployment.
// Streaming must engage here even though a fallback chain and an active
// breaker are both configured, as long as nothing has actually failed.
func TestSend_StreamEngagesWithFallbackAndBreakerConfigured(t *testing.T) {
	primary := newFakeStreamingSender(textStreamEvents("Hi", " there")...)
	fallback := &nonStreamingFake{reply: "must not be used"}
	c, err := NewClient(
		WithUpstream(UpstreamAnthropic, primary),
		WithUpstream(UpstreamOpenRouter, fallback),
		WithBreaker(15*time.Minute),
		WithCatalog(seededCatalog(t)),
		WithDefaultModel("claude-haiku-4-5"),
		WithLogger(testLogger(t)),
	)
	if err != nil {
		t.Fatal(err)
	}
	conv, err := c.Conversation(context.Background(),
		NewConversation("brent", "test"), Model("claude-haiku-4-5"), SystemText("sys"),
		Fallback(UpstreamOpenRouter))
	if err != nil {
		t.Fatal(err)
	}

	var seen []string
	msg, err := conv.Send(context.Background(), UserText("hi"),
		WithStreamHandler(func(ev anthropic.MessageStreamEventUnion) {
			if d := textOf(ev); d != "" {
				seen = append(seen, d)
			}
		}))
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if primary.streamCalls != 1 {
		t.Errorf("streamCalls = %d, want 1: streaming must engage with a fallback chain + breaker configured", primary.streamCalls)
	}
	if got := strings.Join(seen, ""); got != "Hi there" {
		t.Errorf("handler saw %q, want %q", got, "Hi there")
	}
	if got := textOfMessage(msg); got != "Hi there" {
		t.Errorf("message = %q, want %q", got, "Hi there")
	}
	if fallback.calls != 0 {
		t.Errorf("fallback called %d times, want 0 (primary succeeded)", fallback.calls)
	}
}

// TestSend_StreamFailsOverOnPreDeliveryPrimaryFailure proves the other half
// of the fix: when the primary's streaming request fails BEFORE delivering
// any event, sendStreaming must report attempted=false so the caller's
// SendParams retry can reach the fallback chain - nothing was delivered, so
// nothing can be double-delivered by that retry.
func TestSend_StreamFailsOverOnPreDeliveryPrimaryFailure(t *testing.T) {
	// One script entry: NewStreaming AND New (the callModel retry SendParams
	// makes) both fail the same retryable way, modeling a primary that's
	// genuinely down rather than one that behaves differently per call.
	primary := &fakeStreamingSender{scripts: []streamScript{{openErr: overloadedErr()}}}
	fallback := &nonStreamingFake{reply: "fallback done"}
	c, err := NewClient(
		WithUpstream(UpstreamAnthropic, primary),
		WithUpstream(UpstreamOpenRouter, fallback),
		WithBreaker(15*time.Minute),
		WithCatalog(seededCatalog(t)),
		WithDefaultModel("claude-haiku-4-5"),
		WithLogger(testLogger(t)),
	)
	if err != nil {
		t.Fatal(err)
	}
	conv, err := c.Conversation(context.Background(),
		NewConversation("brent", "test"), Model("claude-haiku-4-5"), SystemText("sys"),
		Fallback(UpstreamOpenRouter))
	if err != nil {
		t.Fatal(err)
	}

	handlerCalls := 0
	msg, err := conv.Send(context.Background(), UserText("hi"),
		WithStreamHandler(func(anthropic.MessageStreamEventUnion) { handlerCalls++ }))
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if got := textOfMessage(msg); got != "fallback done" {
		t.Errorf("message = %q, want %q (fallback must be reached)", got, "fallback done")
	}
	if handlerCalls != 0 {
		t.Errorf("handler invoked %d times, want 0 (nothing streamed: primary rejected pre-delivery, fallback doesn't stream)", handlerCalls)
	}
	if primary.streamCalls != 1 {
		t.Errorf("primary.streamCalls = %d, want 1 (the failed streaming attempt)", primary.streamCalls)
	}
	if primary.newCalls != 1 {
		t.Errorf("primary.newCalls = %d, want 1 (SendParams/callModel retrying primary non-streamed before failing over)", primary.newCalls)
	}
	if fallback.calls != 1 {
		t.Errorf("fallback.calls = %d, want 1", fallback.calls)
	}
	if !c.Breaker(UpstreamAnthropic).Open() {
		t.Error("breaker must be open after the retryable primary failure")
	}
}

// TestSend_FailsOverWhenStreamDiesAfterMessageStartButBeforeContent is the
// C4 regression: a stream that is ACCEPTED (message_start arrives) and then
// dies before any content_block event has delivered nothing to the handler,
// so failing over is exactly as safe as the pre-delivery case above — and
// the non-streaming path would have recovered here. Gating `delivered` on
// message_start (the pre-fix behavior) makes this unreachable: attempted
// would already be true, so the error would surface directly instead of
// failing over.
//
// primary's non-streaming retry (newErr) also fails retryably, modeling the
// same-upstream non-streaming attempt SendParams/callModel makes after
// attempted=false — without it, that retry would silently "succeed" with an
// empty message reconstructed from message_start alone and the test would
// never reach the fallback at all.
func TestSend_FailsOverWhenStreamDiesAfterMessageStartButBeforeContent(t *testing.T) {
	primary := &fakeStreamingSender{scripts: []streamScript{{
		events:   []ssestream.Event{messageStartEvent()},
		trailErr: overloadedErr(),
		newErr:   overloadedErr(),
	}}}
	fallback := &nonStreamingFake{reply: "recovered"}
	c, err := NewClient(
		WithUpstream(UpstreamAnthropic, primary),
		WithUpstream(UpstreamOpenRouter, fallback),
		WithBreaker(15*time.Minute),
		WithCatalog(seededCatalog(t)),
		WithDefaultModel("claude-haiku-4-5"),
		WithLogger(testLogger(t)),
	)
	if err != nil {
		t.Fatal(err)
	}
	conv, err := c.Conversation(context.Background(),
		NewConversation("brent", "test"), Model("claude-haiku-4-5"), SystemText("sys"),
		Fallback(UpstreamOpenRouter))
	if err != nil {
		t.Fatal(err)
	}

	var seen []string
	msg, err := conv.Send(context.Background(), UserText("hi"),
		WithStreamHandler(func(ev anthropic.MessageStreamEventUnion) {
			if d := textOf(ev); d != "" {
				seen = append(seen, d)
			}
		}))
	if err != nil {
		t.Fatalf("expected failover to the fallback, got error: %v", err)
	}
	if got := textOfMessage(msg); got != "recovered" {
		t.Errorf("message = %q, want %q — did not fail over", got, "recovered")
	}
	if len(seen) != 0 {
		t.Errorf("handler saw content %v from a stream that delivered none", seen)
	}
	if primary.streamCalls != 1 {
		t.Errorf("primary.streamCalls = %d, want 1", primary.streamCalls)
	}
	if primary.newCalls != 1 {
		t.Errorf("primary.newCalls = %d, want 1 (SendParams/callModel retrying primary non-streamed before failing over)", primary.newCalls)
	}
	if fallback.calls != 1 {
		t.Errorf("fallback.calls = %d, want 1", fallback.calls)
	}
}

// TestSendStreaming_StepsAsideWhenBreakerIsOpen is the C3 regression: an open
// breaker means the primary is known-bad. sendStreaming must not issue to it
// at all — that is precisely the latency (a full timeout against a dead
// primary, every turn) the breaker exists to avoid. The scripted primary
// events are deliberately something that would be plainly visible if
// streamed, proving the sender is never touched rather than merely that its
// output goes unobserved.
func TestSendStreaming_StepsAsideWhenBreakerIsOpen(t *testing.T) {
	primary := newFakeStreamingSender(textDeltaEvent("should not be reached"), messageStopEvent())
	fallback := &nonStreamingFake{reply: "via fallback"}
	conv, _ := newTestConversationWithOpenBreaker(t, primary, fallback)

	msg, err := conv.Send(context.Background(), UserText("hi"),
		WithStreamHandler(func(anthropic.MessageStreamEventUnion) {
			t.Error("handler fired despite an open breaker on the primary")
		}))
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if got := textOfMessage(msg); got != "via fallback" {
		t.Errorf("message = %q, want the fallback's reply", got)
	}
	if primary.calls != 0 {
		t.Errorf("primary called %d times with breaker open, want 0", primary.calls)
	}
	if fallback.calls != 1 {
		t.Errorf("fallback called %d times, want 1", fallback.calls)
	}
}

// TestSendStreaming_RecordsResultIntoBreaker is the other half of C3: a
// streamed failure must trip the breaker itself, or the breaker can only ever
// learn from the non-streaming handoff path and streaming quietly never
// contributes at all.
//
// The scripted failure delivers real content (a text delta) before dying
// retryably — i.e. the SAME "delivered, so no failover is possible" shape as
// TestSend_StreamTrimRetryNeverRetriesAfterDelivery — deliberately, so that
// sendStreaming's own result IS the final one for this send: no
// SendParams/callModel retry ever follows (attempted is always true once
// content has delivered), so there is no other code path that could have
// recorded this failure. If the breaker ends up open, sendStreaming itself
// must be what did it.
func TestSendStreaming_RecordsResultIntoBreaker(t *testing.T) {
	primary := &fakeStreamingSender{scripts: []streamScript{{
		events:   []ssestream.Event{messageStartEvent(), contentBlockStartEvent(), textDeltaEvent("partial")},
		trailErr: overloadedErr(),
	}}}
	fallback := &nonStreamingFake{reply: "ok"}
	conv, client := newTestConversationWithBreaker(t, primary, fallback)

	_, err := conv.Send(context.Background(), UserText("hi"),
		WithStreamHandler(func(anthropic.MessageStreamEventUnion) {}))
	if err == nil {
		t.Fatal("Send must fail: content already delivered, so no failover is possible")
	}
	if fallback.calls != 0 {
		t.Errorf("fallback called %d times, want 0 (no failover once content has delivered)", fallback.calls)
	}

	b := client.Breaker(UpstreamAnthropic)
	if b == nil {
		t.Fatal("no breaker configured")
	}
	if !breakerSawFailure(b) {
		t.Error("streamed failure was not recorded — the breaker cannot open from the streaming path")
	}
}
