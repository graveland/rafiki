// SPDX-License-Identifier: Apache-2.0

package llm

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"go.graveland.dev/rafiki/pkg/msg"
	"go.graveland.dev/rafiki/pkg/routing"

	"github.com/multigres/testkit/assert"
)

// newTestConversation builds a store-less (in-memory) Conversation around
// sender — fast unit-test scaffolding for the streaming path, no DB needed.
func newTestConversation(t *testing.T, sender Sender) *Conversation {
	t.Helper()
	ck := assert.NewAborting(t)
	c, err := NewClient(
		WithProviderSender("anthropic", sender),
		WithDefaultModel("claude-haiku-4-5"),
		WithLogger(testLogger(t)),
	)
	ck.NoError(err)
	conv, err := c.Conversation(context.Background(),
		NewConversation("", "test"), Model("claude-haiku-4-5"), SystemText("sys"))
	ck.NoError(err)
	return conv
}

// newTestConversationWithBreaker builds a Conversation with a real breaker
// AND a fallback chain configured on the primary — the shape needed for
// sendStreaming to genuinely consult and update the primary's breaker
// (mirrors TestSend_StreamEngagesWithFallbackAndBreakerConfigured's setup).
func newTestConversationWithBreaker(t *testing.T, primary, fallback Sender) (*Conversation, *Client) {
	t.Helper()
	ck := assert.NewAborting(t)
	c, err := NewClient(
		WithProviderSender("anthropic", primary),
		WithProviderSender("openrouter", fallback),
		WithBreaker(15*time.Minute),
		WithCatalog(seededCatalog(t)),
		WithDefaultModel("claude-haiku-4-5"),
		WithLogger(testLogger(t)),
	)
	ck.NoError(err)
	conv, err := c.Conversation(context.Background(),
		NewConversation("", "test"), Model("claude-haiku-4-5"), SystemText("sys"),
		Fallback("openrouter"))
	ck.NoError(err)
	return conv, c
}

// newTestConversationWithOpenBreaker is newTestConversationWithBreaker with
// the primary's breaker pre-tripped open by a prior retryable failure —
// models the sustained-primary-degradation state the breaker exists to
// detect, independent of anything sendStreaming itself does in the test.
func newTestConversationWithOpenBreaker(t *testing.T, primary, fallback Sender) (*Conversation, *Client) {
	t.Helper()
	conv, c := newTestConversationWithBreaker(t, primary, fallback)
	c.Breaker("anthropic").RecordResult(time.Now(), true)
	assert.NewAborting(t).True(c.Breaker("anthropic").Open(), "test setup: breaker did not open")
	return conv, c
}

// breakerSawFailure reports whether the breaker is currently open. Open() is
// the only state routing.Breaker exposes, so this is the sole hook available
// to prove a failure was recorded via the breaker's own API rather than by
// reaching into a private field.
func breakerSawFailure(b *routing.Breaker) bool {
	return b.Open()
}

// responseText concatenates every text block of an accumulated response.
func responseText(r *msg.Response) string {
	var sb strings.Builder
	for _, b := range r.Content {
		if tx, ok := b.(msg.Text); ok {
			sb.WriteString(tx.Text)
		}
	}
	return sb.String()
}

// probe is a stream handler that records how often it fired and what the last
// accumulated response's text was.
type probe struct {
	calls int
	last  string
}

func (p *probe) handler() StreamHandler {
	return func(r *msg.Response) {
		p.calls++
		p.last = responseText(r)
	}
}

func messageStartEvent() msg.StreamEvent {
	return msg.MessageStart{ID: "msg_stream", Model: "claude-haiku-4-5",
		Usage: msg.Usage{Input: msg.Ptr(10), Output: msg.Ptr(0)}}
}

func contentBlockStartEvent() msg.StreamEvent {
	return msg.BlockStart{Index: 0, Block: msg.Text{}}
}

func textDeltaEvent(s string) msg.StreamEvent {
	return msg.BlockDelta{Index: 0, Kind: msg.DeltaText, Text: s}
}

func contentBlockStopEvent() msg.StreamEvent {
	return msg.BlockStop{Index: 0}
}

func messageDeltaEvent() msg.StreamEvent {
	return msg.MessageDelta{StopReason: msg.StopEndTurn, Usage: msg.Usage{Output: msg.Ptr(5)}}
}

// textStreamEvents composes a complete, valid single-text-block streaming
// response out of the given deltas (message_start..message_delta) — the event
// sequence msg.Accumulator expects.
func textStreamEvents(parts ...string) []msg.StreamEvent {
	events := []msg.StreamEvent{messageStartEvent(), contentBlockStartEvent()}
	for _, p := range parts {
		events = append(events, textDeltaEvent(p))
	}
	return append(events, contentBlockStopEvent(), messageDeltaEvent())
}

// trailingDecoder replays a fixed event slice, then (once exhausted) surfaces
// trailErr — modeling a stream that fails after already delivering zero or more
// real events.
type trailingDecoder struct {
	events   []msg.StreamEvent
	idx      int
	trailErr error
}

func (d *trailingDecoder) Next() (msg.StreamEvent, error) {
	if d.idx >= len(d.events) {
		if d.trailErr != nil {
			return nil, d.trailErr
		}
		return nil, io.EOF
	}
	ev := d.events[d.idx]
	d.idx++
	return ev, nil
}

func (d *trailingDecoder) Close() error { return nil }

// streamScript is one scripted Stream call: openErr models a request-level
// rejection (e.g. a real prompt-too-large 400, which the API returns before any
// event is ever emitted); trailErr models a decoder-surfaced error once events
// are exhausted (used to prove the library's trim-retry guard does not rely on
// errors always arriving before content).
type streamScript struct {
	events   []msg.StreamEvent
	openErr  error
	trailErr error
	// newErr, when set, is returned by Send independent of openErr — models
	// the non-streaming retry (callModel, driven by sendAttempt on
	// attempted=false) hitting the same failing upstream after a streaming
	// attempt that got further than "rejected before any event".
	newErr error
}

// fakeStreamingSender scripts the streaming and non-streaming faces
// separately, replaying queued streamScripts in order (the last repeats).
type fakeStreamingSender struct {
	scripts     []streamScript
	calls       int // indexes scripts for Stream
	streamCalls int
	newCalls    int // indexes scripts for Send, independent of calls/streamCalls
	lastReq     []*msg.Request
}

func newFakeStreamingSender(events ...msg.StreamEvent) *fakeStreamingSender {
	return &fakeStreamingSender{scripts: []streamScript{{events: events}}}
}

// Send mirrors Stream's script selection (openErr fails the call exactly as it
// fails Stream, modeling a primary whose non-streaming retry hits the identical
// rejection) but indexes independently via newCalls, since a pre-delivery
// failover test drives Send and Stream as separate call sequences on the same
// sender.
func (f *fakeStreamingSender) Send(_ context.Context, req *msg.Request, _ msg.EncodeOptions) (*msg.Response, error) {
	f.lastReq = append(f.lastReq, req)
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
	acc := msg.NewAccumulator()
	for _, ev := range s.events {
		_ = acc.Add(ev)
	}
	return acc.Response(), nil
}

func (f *fakeStreamingSender) Stream(_ context.Context, req *msg.Request, _ msg.EncodeOptions) (msg.StreamDecoder, error) {
	f.streamCalls++
	f.lastReq = append(f.lastReq, req)
	i := f.calls
	if i >= len(f.scripts) {
		i = len(f.scripts) - 1
	}
	f.calls++
	s := f.scripts[i]
	if s.openErr != nil {
		return nil, s.openErr
	}
	return &trailingDecoder{events: s.events, trailErr: s.trailErr}, nil
}

// replySender answers every call with one text block, counting the calls that
// actually went out. It stands in for a fallback provider: reached through the
// non-streamed path, so calls counts those.
type replySender struct {
	reply string
	calls int
}

func (s *replySender) Send(_ context.Context, _ *msg.Request, _ msg.EncodeOptions) (*msg.Response, error) {
	s.calls++
	return cannedMessage(`{"id":"m","type":"message","role":"assistant","model":"m",
		"content":[{"type":"text","text":"` + s.reply + `"}],
		"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`), nil
}

func (s *replySender) Stream(ctx context.Context, req *msg.Request, opts msg.EncodeOptions) (msg.StreamDecoder, error) {
	resp, err := s.Send(ctx, req, opts)
	if err != nil {
		return nil, err
	}
	return &trailingDecoder{events: replayEvents(resp)}, nil
}

// replayEvents renders a whole response as the event sequence a stream would
// have produced for it.
func replayEvents(resp *msg.Response) []msg.StreamEvent {
	events := []msg.StreamEvent{msg.MessageStart{ID: resp.ID, Model: resp.Model, Provider: resp.Provider, Usage: resp.Usage}}
	for i, b := range resp.Content {
		events = append(events, msg.BlockStart{Index: i, Block: b}, msg.BlockStop{Index: i, Block: b})
	}
	return append(events, msg.MessageDelta{StopReason: resp.StopReason, Usage: resp.Usage})
}

// closeTrackingDecoder wraps trailingDecoder to record whether Close was called
// — the leak-detection probe for sendStreaming's defer-hoist fix below.
type closeTrackingDecoder struct {
	trailingDecoder
	closed bool
}

func (d *closeTrackingDecoder) Close() error {
	d.closed = true
	return nil
}

// leakySender's Stream returns a non-nil decoder ALONGSIDE a non-nil error —
// the shape no first-party implementation produces (every real sender returns a
// nil error; a failure is instead surfaced from the decoder's own Next), but
// which Sender's contract does not forbid. This is the one shape that exercises
// sendStreaming's Stream-error branch via the real return value rather than the
// decoder.
type leakySender struct {
	decoder *closeTrackingDecoder
}

func (s *leakySender) Send(_ context.Context, _ *msg.Request, _ msg.EncodeOptions) (*msg.Response, error) {
	return nil, errors.New("leakySender: Send called, want Stream")
}

func (s *leakySender) Stream(_ context.Context, _ *msg.Request, _ msg.EncodeOptions) (msg.StreamDecoder, error) {
	return s.decoder, errors.New("boom")
}

// TestSendStreaming_ClosesStreamEvenWhenStreamReturnsBothStreamAndError pins
// the leak fix: sendStreaming's defer that closes the stream must be registered
// BEFORE the Stream-error checks, not after, so a Sender that returns (non-nil
// decoder, non-nil err) still gets its decoder closed instead of leaked.
// leakySender has no fallback or breaker configured (newTestConversation), so
// sendStreaming takes the breaker-nil, attempted=true return — a single Stream
// call, no retry loop to confound the assertion.
func TestSendStreaming_ClosesStreamEvenWhenStreamReturnsBothStreamAndError(t *testing.T) {
	c := assert.NewCollecting(t)
	decoder := &closeTrackingDecoder{}
	sender := &leakySender{decoder: decoder}
	conv := newTestConversation(t, sender)

	_, err := conv.Send(context.Background(), UserText("hi"),
		WithStreamHandler(func(*msg.Response) {}))
	c.Require().Error(err, "Send must fail: leakySender's Stream always errors")
	c.True(decoder.closed, "stream must be closed even when Stream returns a non-nil error alongside a non-nil decoder")
}

func TestSend_StreamHandlerReceivesEventsAndAccumulates(t *testing.T) {
	c := assert.NewCollecting(t)
	sender := newFakeStreamingSender(textStreamEvents("Hel", "lo")...)
	conv := newTestConversation(t, sender)

	var p probe
	resp, err := conv.Send(context.Background(), UserText("hi"), WithStreamHandler(p.handler()))
	c.Require().NoError(err, "Send")
	c.Eq("Hello", p.last, "the handler's last snapshot")
	c.Eq("Hello", responseText(resp), "accumulated message")
	c.Eq(1, sender.streamCalls, "streamCalls")
}

// TestSend_StreamBackfillsInputTokensFromMessageDelta is the regression for a
// real bug: OpenRouter's Anthropic-compatible face reports input/cache token
// usage as zero in message_start for at least two backends (deepseek,
// moonshotai) and only fills in the correct cumulative counts in the final
// message_delta. msg.Accumulator's mergeUsage is where that correction lives.
// Without it, every turn through one of these backends persisted and reported 0
// input/cache tokens despite a real, non-trivial prompt.
func TestSend_StreamBackfillsInputTokensFromMessageDelta(t *testing.T) {
	c := assert.NewCollecting(t)
	events := []msg.StreamEvent{
		msg.MessageStart{ID: "msg_or", Model: "deepseek/deepseek-v4-pro",
			Usage: msg.Usage{Input: msg.Ptr(0), Output: msg.Ptr(0), CacheRead: msg.Ptr(0), CacheWrite: msg.Ptr(0)}},
		contentBlockStartEvent(),
		textDeltaEvent("hi"),
		contentBlockStopEvent(),
		msg.MessageDelta{StopReason: msg.StopEndTurn,
			Usage: msg.Usage{Input: msg.Ptr(101), Output: msg.Ptr(5), CacheRead: msg.Ptr(20), CacheWrite: msg.Ptr(3)}},
	}
	sender := newFakeStreamingSender(events...)
	conv := newTestConversation(t, sender)

	resp, err := conv.Send(context.Background(), UserText("hi"), WithStreamHandler(func(*msg.Response) {}))
	c.Require().NoError(err, "Send")
	c.Eq(int64(101), msg.Val(resp.Usage.Input), "Input")
	c.Eq(int64(20), msg.Val(resp.Usage.CacheRead), "CacheRead")
	c.Eq(int64(3), msg.Val(resp.Usage.CacheWrite), "CacheWrite")
	c.Eq(int64(5), msg.Val(resp.Usage.Output), "Output")
}

// TestSend_StreamMessageDeltaNeverClobbersRealAnthropicInputTokens is the
// safety net for the fix above: real Anthropic reports input tokens once,
// correctly, in message_start, and never repeats them in message_delta (whose
// own input/cache fields come back zero there). msg.Accumulator's "a reported
// zero is not evidence the count was zero" rule must leave message_start's
// value alone in that case — messageStartEvent sets input=10 and
// messageDeltaEvent carries no input field at all, exactly that shape.
func TestSend_StreamMessageDeltaNeverClobbersRealAnthropicInputTokens(t *testing.T) {
	c := assert.NewCollecting(t)
	sender := newFakeStreamingSender(textStreamEvents("hi")...)
	conv := newTestConversation(t, sender)

	resp, err := conv.Send(context.Background(), UserText("hi"), WithStreamHandler(func(*msg.Response) {}))
	c.Require().NoError(err, "Send")
	c.Eq(int64(10), msg.Val(resp.Usage.Input), "Input")
}

func TestSend_NoHandlerUsesNonStreamingPath(t *testing.T) {
	c := assert.NewCollecting(t)
	sender := newFakeStreamingSender(textStreamEvents("x")...)
	conv := newTestConversation(t, sender)

	_, err := conv.Send(context.Background(), UserText("hi"))
	c.Require().NoError(err, "Send")
	c.Eq(0, sender.streamCalls, "no handler means no streaming call")
}

// seedBigHistory writes n large messages so the default TrimPolicy has
// something to drop, mirroring TestConversationTrimRetryKeepsPrefixAndRows.
func seedBigHistory(t *testing.T, conv *Conversation, n int) {
	t.Helper()
	big := strings.Repeat("y", 80*1024)
	for i := range n {
		role := msg.User
		if i%2 == 1 {
			role = msg.Assistant
		}
		_, aerr := conv.appendMessage(context.Background(),
			msg.Message{Role: role, Content: []msg.Block{msg.Text{Text: big}}}, nil, "")
		assert.NewAborting(t).NoError(aerr)
	}
}

// TestSend_StreamTrimRetrySucceedsWhenNoEventsDelivered is the streaming analog
// of TestConversationTrimRetryKeepsPrefixAndRows: the first attempt is rejected
// as prompt-too-large before any event is delivered (the realistic API shape —
// see streamScript's openErr doc), so the retry must still happen and must
// still stream through the handler.
func TestSend_StreamTrimRetrySucceedsWhenNoEventsDelivered(t *testing.T) {
	c := assert.NewCollecting(t)
	sender := &fakeStreamingSender{scripts: []streamScript{
		{openErr: promptTooLargeErr()},
		{events: textStreamEvents("after trim")},
	}}
	conv := newTestConversation(t, sender)
	seedBigHistory(t, conv, 5)

	var p probe
	resp, err := conv.Send(context.Background(), UserText("question"), WithStreamHandler(p.handler()))
	c.Require().NoError(err, "Send with trim-retry")
	c.Eq("after trim", responseText(resp), "message")
	c.Eq("after trim", p.last, "handler saw")
	c.Eq(2, sender.streamCalls, "streamCalls")
	if len(sender.lastReq) != 2 || len(sender.lastReq[1].Messages) >= len(sender.lastReq[0].Messages) {
		t.Errorf("retry not trimmed: attempts=%d", len(sender.lastReq))
	}
}

// TestSend_StreamTrimRetryNeverRetriesAfterDelivery is the structural guarantee
// behind THE SECOND CONSTRAINT: once ANY event has reached the caller's handler
// for an attempt, sendWithTrim must never retry — even when the eventual error
// looks exactly like the ordinary isPromptTooLarge trim-retry trigger. This does
// not rely on the API's actual before-any-content error timing: the first
// scripted attempt deliberately delivers a real content event and THEN errors
// prompt-too-large, to prove the guard holds regardless.
func TestSend_StreamTrimRetryNeverRetriesAfterDelivery(t *testing.T) {
	c := assert.NewCollecting(t)
	sender := &fakeStreamingSender{scripts: []streamScript{
		{
			events:   []msg.StreamEvent{messageStartEvent(), contentBlockStartEvent(), textDeltaEvent("partial")},
			trailErr: promptTooLargeErr(),
		},
		{events: textStreamEvents("SHOULD NEVER BE DELIVERED")},
	}}
	conv := newTestConversation(t, sender)
	seedBigHistory(t, conv, 5)

	var p probe
	_, err := conv.Send(context.Background(), UserText("question"), WithStreamHandler(p.handler()))
	c.Require().Error(err, "Send must fail: a retry after partial delivery would double-deliver events")
	c.True(isPromptTooLarge(err), "expected the underlying prompt-too-large error to surface, got: %v", err)
	c.Eq(1, sender.streamCalls, "streamCalls")
	c.Eq(2, p.calls, "the block start and its delta are the content-bearing events")
	c.Eq("partial", p.last, "handler saw")
}

// TestSend_StreamEngagesWithFallbackAndBreakerConfigured guards the regression a
// narrower guard shipped: fundi enables Fallback(OpenRouter) AND WithBreaker
// together whenever an OpenRouter key is configured (the common case), and a
// guard that bailed out of streaming whenever BOTH were present made
// WithStreamHandler silently dead in exactly that deployment. Streaming must
// engage here even though a fallback chain and an active breaker are both
// configured, as long as nothing has actually failed.
func TestSend_StreamEngagesWithFallbackAndBreakerConfigured(t *testing.T) {
	ck := assert.NewCollecting(t)
	primary := newFakeStreamingSender(textStreamEvents("Hi", " there")...)
	fallback := &replySender{reply: "must not be used"}
	c, err := NewClient(
		WithProviderSender("anthropic", primary),
		WithProviderSender("openrouter", fallback),
		WithBreaker(15*time.Minute),
		WithCatalog(seededCatalog(t)),
		WithDefaultModel("claude-haiku-4-5"),
		WithLogger(testLogger(t)),
	)
	ck.Require().NoError(err)
	conv, err := c.Conversation(context.Background(),
		NewConversation("", "test"), Model("claude-haiku-4-5"), SystemText("sys"),
		Fallback("openrouter"))
	ck.Require().NoError(err)

	var p probe
	resp, err := conv.Send(context.Background(), UserText("hi"), WithStreamHandler(p.handler()))
	ck.Require().NoError(err, "Send")
	ck.Eq(1, primary.streamCalls, "streamCalls")
	ck.Eq("Hi there", p.last, "handler saw")
	ck.Eq("Hi there", responseText(resp), "message")
	ck.Eq(0, fallback.calls, "fallback called")
}

// TestSend_StreamFailsOverOnPreDeliveryPrimaryFailure proves the other half of
// the fix: when the primary's streaming request fails BEFORE delivering any
// event, sendStreaming must report attempted=false so the caller's non-streamed
// retry can reach the fallback chain — nothing was delivered, so nothing can be
// double-delivered by that retry.
func TestSend_StreamFailsOverOnPreDeliveryPrimaryFailure(t *testing.T) {
	ck := assert.NewCollecting(t)
	// One script entry: Stream AND Send (the callModel retry makes) both fail
	// the same retryable way, modeling a primary that's genuinely down rather
	// than one that behaves differently per call.
	primary := &fakeStreamingSender{scripts: []streamScript{{openErr: overloadedErr()}}}
	fallback := &replySender{reply: "fallback done"}
	c, err := NewClient(
		WithProviderSender("anthropic", primary),
		WithProviderSender("openrouter", fallback),
		WithBreaker(15*time.Minute),
		WithCatalog(seededCatalog(t)),
		WithDefaultModel("claude-haiku-4-5"),
		WithLogger(testLogger(t)),
	)
	ck.Require().NoError(err)
	conv, err := c.Conversation(context.Background(),
		NewConversation("", "test"), Model("claude-haiku-4-5"), SystemText("sys"),
		Fallback("openrouter"))
	ck.Require().NoError(err)

	var p probe
	resp, err := conv.Send(context.Background(), UserText("hi"), WithStreamHandler(p.handler()))
	ck.Require().NoError(err, "Send")
	ck.Eq("fallback done", responseText(resp), "message")
	ck.Eq(0, p.calls, "handler invoked")
	ck.Eq(1, primary.streamCalls, "primary.streamCalls")
	ck.Eq(1, primary.newCalls, "primary.newCalls")
	ck.Eq(1, fallback.calls, "fallback.calls")
	ck.True(c.Breaker("anthropic").Open(), "breaker must be open after the retryable primary failure")
}

// TestSend_FailsOverWhenStreamDiesAfterMessageStartButBeforeContent is the C4
// regression: a stream that is ACCEPTED (message_start arrives) and then dies
// before any content_block event has delivered nothing to the handler, so
// failing over is exactly as safe as the pre-delivery case above — and the
// non-streaming path would have recovered here. Gating `delivered` on
// message_start (the pre-fix behavior) makes this unreachable.
//
// primary's non-streaming retry (newErr) also fails retryably, modeling the
// same-upstream non-streaming attempt callModel makes after attempted=false.
func TestSend_FailsOverWhenStreamDiesAfterMessageStartButBeforeContent(t *testing.T) {
	ck := assert.NewCollecting(t)
	primary := &fakeStreamingSender{scripts: []streamScript{{
		events:   []msg.StreamEvent{messageStartEvent()},
		trailErr: overloadedErr(),
		newErr:   overloadedErr(),
	}}}
	fallback := &replySender{reply: "recovered"}
	c, err := NewClient(
		WithProviderSender("anthropic", primary),
		WithProviderSender("openrouter", fallback),
		WithBreaker(15*time.Minute),
		WithCatalog(seededCatalog(t)),
		WithDefaultModel("claude-haiku-4-5"),
		WithLogger(testLogger(t)),
	)
	ck.Require().NoError(err)
	conv, err := c.Conversation(context.Background(),
		NewConversation("", "test"), Model("claude-haiku-4-5"), SystemText("sys"),
		Fallback("openrouter"))
	ck.Require().NoError(err)

	var p probe
	resp, err := conv.Send(context.Background(), UserText("hi"), WithStreamHandler(p.handler()))
	ck.Require().NoError(err, "expected failover to the fallback, got error")
	ck.Eq("recovered", responseText(resp), "message")
	ck.Eq(0, p.calls, "handler saw content")
	ck.Eq(1, primary.streamCalls, "primary.streamCalls")
	ck.Eq(1, primary.newCalls, "primary.newCalls")
	ck.Eq(1, fallback.calls, "fallback.calls")
}

// TestSendStreaming_StepsAsideWhenBreakerIsOpen is the C3 regression: an open
// breaker means the primary is known-bad. sendStreaming must not issue to it at
// all — that is precisely the latency (a full timeout against a dead primary,
// every turn) the breaker exists to avoid. The scripted primary events are
// deliberately something that would be plainly visible if streamed, proving the
// sender is never touched rather than merely that its output goes unobserved.
func TestSendStreaming_StepsAsideWhenBreakerIsOpen(t *testing.T) {
	c := assert.NewCollecting(t)
	primary := newFakeStreamingSender(textDeltaEvent("should not be reached"), messageDeltaEvent())
	fallback := &replySender{reply: "via fallback"}
	conv, _ := newTestConversationWithOpenBreaker(t, primary, fallback)

	resp, err := conv.Send(context.Background(), UserText("hi"),
		WithStreamHandler(func(*msg.Response) {
			t.Error("handler fired despite an open breaker on the primary")
		}))
	c.Require().NoError(err, "Send")
	c.Eq("via fallback", responseText(resp), "message")
	c.Eq(0, primary.calls, "primary called")
	c.Eq(1, fallback.calls, "fallback called")
}

// TestSendStreaming_RecordsResultIntoBreaker is the other half of C3: a streamed
// failure must trip the breaker itself, or the breaker can only ever learn from
// the non-streaming handoff path and streaming quietly never contributes at all.
//
// The scripted failure delivers real content (a text delta) before dying
// retryably — i.e. the SAME "delivered, so no failover is possible" shape as
// TestSend_StreamTrimRetryNeverRetriesAfterDelivery — deliberately, so that
// sendStreaming's own result IS the final one for this send: no callModel retry
// ever follows, so there is no other code path that could have recorded this
// failure.
func TestSendStreaming_RecordsResultIntoBreaker(t *testing.T) {
	c := assert.NewCollecting(t)
	primary := &fakeStreamingSender{scripts: []streamScript{{
		events:   []msg.StreamEvent{messageStartEvent(), contentBlockStartEvent(), textDeltaEvent("partial")},
		trailErr: overloadedErr(),
	}}}
	fallback := &replySender{reply: "ok"}
	conv, client := newTestConversationWithBreaker(t, primary, fallback)

	_, err := conv.Send(context.Background(), UserText("hi"), WithStreamHandler(func(*msg.Response) {}))
	c.Require().Error(err, "Send must fail: content already delivered, so no failover is possible")
	c.Eq(0, fallback.calls, "fallback called")

	b := client.Breaker("anthropic")
	c.Require().NotNil(b, "no breaker configured")
	c.True(breakerSawFailure(b), "streamed failure was not recorded — the breaker cannot open from the streaming path")
}

// rateLimitStreamErr builds the error a codec produces for an in-band SSE
// rate-limit rejection — exactly the payload OpenRouter sent that killed a fundi
// turn at iteration 41 (conversation 01a03ed5).
func rateLimitStreamErr() error {
	return inBandErr(msg.ErrRateLimit, `{"type":"error","error":{"type":"rate_limit_error",`+
		`"message":"Provider returned error","error_type":"rate_limit_exceeded"}}`)
}

// TestSend_StreamRetriesOnPreDeliveryInBandRateLimit: the provider accepts the
// connection, then sends an SSE "error" event BEFORE any content. That is the
// in-band twin of the typed 429 below and must take the identical path:
// ModelGate backpressure, then one fresh attempt.
func TestSend_StreamRetriesOnPreDeliveryInBandRateLimit(t *testing.T) {
	ck := assert.NewCollecting(t)
	sender := &fakeStreamingSender{scripts: []streamScript{
		{trailErr: rateLimitStreamErr()},
		{events: textStreamEvents("after retry")},
	}}
	c, err := NewClient(
		WithProviderSender("anthropic", sender),
		WithDefaultModel("claude-haiku-4-5"),
		WithLogger(testLogger(t)),
	)
	ck.Require().NoError(err)
	c.modelGate = NewModelGate(10*time.Millisecond, 50*time.Millisecond)
	conv, err := c.Conversation(context.Background(),
		NewConversation("", "test"), Model("claude-haiku-4-5"), SystemText("sys"))
	ck.Require().NoError(err)

	var p probe
	resp, err := conv.Send(context.Background(), UserText("hi"), WithStreamHandler(p.handler()))
	ck.Require().NoError(err, "Send with in-band rate-limit retry")
	ck.Eq("after retry", responseText(resp), "message")
	ck.Eq("after retry", p.last, "handler saw")
	ck.Eq(2, sender.streamCalls, "streamCalls")
}

// TestSend_StreamDoesNotRetryAfterDeliveryEvenForInBandRateLimit pins the
// delivery guard against the in-band shape too: once content reached the
// handler, no classification of the trailing error can make the send safe to
// replay.
func TestSend_StreamDoesNotRetryAfterDeliveryEvenForInBandRateLimit(t *testing.T) {
	c := assert.NewCollecting(t)
	sender := &fakeStreamingSender{scripts: []streamScript{
		{
			events:   []msg.StreamEvent{messageStartEvent(), contentBlockStartEvent(), textDeltaEvent("partial")},
			trailErr: rateLimitStreamErr(),
		},
		{events: textStreamEvents("SHOULD NOT APPEAR")},
	}}
	conv := newTestConversation(t, sender)

	var p probe
	_, err := conv.Send(context.Background(), UserText("hi"), WithStreamHandler(p.handler()))
	c.Require().Error(err, "Send must fail: delivered content + in-band 429 cannot retry")
	c.True(IsRateLimitStreamError(err), "expected in-band rate limit error to surface, got: %v", err)
	c.Eq(1, sender.streamCalls, "streamCalls")
	c.Eq("partial", p.last, "handler saw")
}

func TestSend_StreamRetriesOnPreDeliveryRateLimit(t *testing.T) {
	ck := assert.NewCollecting(t)
	sender := &fakeStreamingSender{scripts: []streamScript{
		{openErr: rateLimitErr()},
		{events: textStreamEvents("after retry")},
	}}
	c, err := NewClient(
		WithProviderSender("anthropic", sender),
		WithDefaultModel("claude-haiku-4-5"),
		WithLogger(testLogger(t)),
	)
	ck.Require().NoError(err)
	c.modelGate = NewModelGate(10*time.Millisecond, 50*time.Millisecond)
	conv, err := c.Conversation(context.Background(),
		NewConversation("", "test"), Model("claude-haiku-4-5"), SystemText("sys"))
	ck.Require().NoError(err)

	var p probe
	resp, err := conv.Send(context.Background(), UserText("hi"), WithStreamHandler(p.handler()))
	ck.Require().NoError(err, "Send with rate-limit retry")
	ck.Eq("after retry", responseText(resp), "message")
	ck.Eq("after retry", p.last, "handler saw")
	ck.Eq(2, sender.streamCalls, "streamCalls")
}

func TestSend_StreamDoesNotRetryAfterDeliveryEvenForRateLimit(t *testing.T) {
	c := assert.NewCollecting(t)
	sender := &fakeStreamingSender{scripts: []streamScript{
		{
			events:   []msg.StreamEvent{messageStartEvent(), contentBlockStartEvent(), textDeltaEvent("partial")},
			trailErr: rateLimitErr(),
		},
		{events: textStreamEvents("SHOULD NOT APPEAR")},
	}}
	conv := newTestConversation(t, sender)

	var p probe
	_, err := conv.Send(context.Background(), UserText("hi"), WithStreamHandler(p.handler()))
	c.Require().Error(err, "Send must fail: delivered content + 429 cannot retry")
	isRL, _ := isRateLimit(err)
	c.True(isRL, "expected rate limit error to surface, got: %v", err)
	c.Eq(1, sender.streamCalls, "streamCalls")
	c.Eq("partial", p.last, "handler saw")
}

func TestSend_StreamDoesNotRetryNonRateLimitErrors(t *testing.T) {
	c := assert.NewCollecting(t)
	sender := &fakeStreamingSender{scripts: []streamScript{
		{openErr: authErr()},
		{events: textStreamEvents("SHOULD NOT APPEAR")},
	}}
	conv := newTestConversation(t, sender)

	_, err := conv.Send(context.Background(), UserText("hi"), WithStreamHandler(func(*msg.Response) {}))
	c.Require().Error(err, "Send must fail: 401 is not retryable")
	c.Eq(1, sender.streamCalls, "streamCalls")
}

func TestSend_StreamRespectsRateLimitPolicyMaxRetries(t *testing.T) {
	ck := assert.NewCollecting(t)
	gate := NewModelGate(10*time.Millisecond, 50*time.Millisecond)
	sender := &fakeStreamingSender{scripts: []streamScript{
		{openErr: rateLimitErr()},
	}}

	c, err := NewClient(
		WithProviderSender("anthropic", sender),
		WithDefaultModel("claude-haiku-4-5"),
		WithLogger(testLogger(t)),
	)
	ck.Require().NoError(err)
	c.modelGate = gate

	conv, err := c.Conversation(context.Background(),
		NewConversation("", "test"), Model("claude-haiku-4-5"), SystemText("sys"),
		WithRateLimitPolicy(RateLimitPolicy{MaxRetries: 2}))
	ck.Require().NoError(err)

	_, err = conv.Send(context.Background(), UserText("hi"), WithStreamHandler(func(*msg.Response) {}))
	ck.Require().Error(err, "Send must fail after 2 retries")
	// 1 initial + 2 retries = 3 total.
	ck.Eq(3, sender.streamCalls, "streamCalls")
}
