// SPDX-License-Identifier: Apache-2.0

package fundi

import (
	"bytes"
	"io"
	"testing"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/msg"

	"github.com/multigres/testkit/assert"
)

// deltasOf collects the ContentBlockDelta events a sink saw, in order.
func deltasOf(events []*rafikiv1.Event) []*rafikiv1.ContentBlockDelta {
	out := make([]*rafikiv1.ContentBlockDelta, 0, len(events))
	for _, ev := range events {
		if d := ev.GetContentBlockDelta(); d != nil {
			out = append(out, d)
		}
	}
	return out
}

// A streamed turn must reach the native plane AS IT STREAMS, appended a
// fragment at a time: the pi frames StreamDelta writes go to an attached child
// stream, which a cockpit subscriber is not, so without these a viewer sees
// nothing until publishAssistant fires on the committed row and then the whole
// reply at once.
//
// Each delta carries only the SUFFIX that is new, because the consumer appends.
// Concatenating them here is therefore the exact text a viewer draws.
func TestEmitterPublishesContentDeltasAsSuffixes(t *testing.T) {
	c := assert.NewCollecting(t)
	em := NewEmitter(NewFrontend(bytes.NewReader(nil), io.Discard, nil), "anthropic", nil)
	sink := &nativeCapture{}
	em.SetNativeSink(sink)

	partial := func(blocks ...msg.Block) *msg.Response { return &msg.Response{Content: blocks} }

	first := partial(msg.Text{Text: "Hel"})
	em.StreamStart(em.mapMessage(first))
	em.publishStreamDelta(first)
	// A second block appears mid-stream (thinking before text is the usual
	// order, but either can start first) and both keep growing.
	em.publishStreamDelta(partial(msg.Text{Text: "Hello"}, msg.Thinking{Text: "why"}))
	em.publishStreamDelta(partial(msg.Text{Text: "Hello, world"}, msg.Thinking{Text: "why not"}))
	em.StreamEnd(em.mapMessage(partial(msg.Text{Text: "Hello, world"}, msg.Thinking{Text: "why not"})))
	// After StreamEnd the committed assistant message carries the content;
	// anything published here would duplicate it.
	em.publishStreamDelta(partial(msg.Text{Text: "Hello, world, and more"}))

	deltas := deltasOf(sink.events)
	var text, thinking string
	var textIdx, thinkIdx []int32
	for _, d := range deltas {
		switch v := d.Delta.(type) {
		case *rafikiv1.ContentBlockDelta_Text:
			text += v.Text
			textIdx = append(textIdx, d.GetIndex())
		case *rafikiv1.ContentBlockDelta_Thinking:
			thinking += v.Thinking
			thinkIdx = append(thinkIdx, d.GetIndex())
		}
	}
	c.Eq("Hello, world", text, "concatenated text deltas (deltas = %v)", deltas)
	c.Eq("why not", thinking, "concatenated thinking deltas")
	// One fragment per publish, each the suffix that was new: "Hel", then the
	// thinking block appearing, then both growing.
	c.Len(textIdx, 3, "text fragments")
	c.Len(thinkIdx, 2, "thinking fragments")
	for _, idx := range textIdx {
		c.Eq(int32(0), idx, "text block index")
	}
	for _, idx := range thinkIdx {
		c.Eq(int32(1), idx, "thinking block index")
	}
}

// A block whose content was REWRITTEN rather than extended (a retry
// re-streaming the same index) must emit nothing: the consumer has already
// appended the old text and has no way to unsay it, so re-sending would render
// the fragment twice. Streaming re-baselines from the new content instead.
func TestEmitterRebaselinesARewrittenBlockInsteadOfDuplicating(t *testing.T) {
	c := assert.NewCollecting(t)
	em := NewEmitter(NewFrontend(bytes.NewReader(nil), io.Discard, nil), "anthropic", nil)
	sink := &nativeCapture{}
	em.SetNativeSink(sink)

	partial := func(text string) *msg.Response {
		return &msg.Response{Content: []msg.Block{msg.Text{Text: text}}}
	}

	em.StreamStart(em.mapMessage(partial("abc")))
	em.publishStreamDelta(partial("abc"))
	em.publishStreamDelta(partial("xyz"))  // not an extension: re-baseline, emit nothing
	em.publishStreamDelta(partial("xyz!")) // extends the new baseline

	var got string
	for _, d := range deltasOf(sink.events) {
		got += d.GetText()
	}
	c.Eq("abc!", got, "streamed text")
}

// No sink is a complete no-op, like every other native publication.
func TestPublishStreamDeltaWithoutASinkDoesNothing(t *testing.T) {
	em := NewEmitter(NewFrontend(bytes.NewReader(nil), io.Discard, nil), "anthropic", nil)
	resp := &msg.Response{Content: []msg.Block{msg.Text{Text: "x"}}}
	em.StreamStart(em.mapMessage(resp))
	em.publishStreamDelta(resp) // must not panic
}

// The engine's streaming handler must call publishStreamDelta on its coalesced
// flush. It is one line away from being reachable-but-unwired, which is exactly
// how the cockpit came to have an applyDelta with no producer.
func TestEnginePublishesContentDeltasWhileStreaming(t *testing.T) {
	c := assert.NewCollecting(t)
	sender := newScriptedStreamingSender(streamScript{events: textTurnEvents("claude-x", "Hel", "lo")})
	sink := &nativeCapture{}
	eng, _ := newTestEngineWithConfig(t, fakeToolSet{}, sender, func(cfg *EngineConfig) {
		cfg.NativeSink = sink
	})

	eng.HandlePrompt("hi")
	eng.Wait()

	// The fragment COUNT is a wall-clock cadence (streamFlushInterval), not a
	// property of the turn: a slow sender flushes more often. What must hold is
	// that the suffixes concatenate to the reply.
	deltas := deltasOf(sink.events)
	c.Require().GreaterOrEqual(1, len(deltas), "no content deltas published for a streamed turn")
	var streamed string
	for _, d := range deltas {
		streamed += d.GetText()
	}
	c.Eq("Hello", streamed, "concatenated deltas")

	// Ordering is what makes the cockpit's replace-on-commit rule sound: the
	// streamed fragments arrive BEFORE the durable message they belong to.
	deltaAt, msgAt := -1, -1
	for i, ev := range sink.events {
		switch {
		case ev.GetContentBlockDelta() != nil && deltaAt == -1:
			deltaAt = i
		case ev.GetAssistantMessage() != nil && msgAt == -1:
			msgAt = i
		}
	}
	c.False(deltaAt < 0 || msgAt < 0 || deltaAt > msgAt, "delta at %d, assistant message at %d", deltaAt, msgAt)
}
