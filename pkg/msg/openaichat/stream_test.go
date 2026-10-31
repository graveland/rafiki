package openaichat_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/multigres/testkit/assert"

	"go.graveland.dev/rafiki/pkg/msg"
	"go.graveland.dev/rafiki/pkg/msg/msgtest"
	"go.graveland.dev/rafiki/pkg/msg/openaichat"
)

// TestStreamGolden runs the recorded SSE fixture through the decoder and the
// accumulator: the accumulated response equals the recorded streamed decode,
// apart from the same usage differences TestDecodeGolden pins.
func TestStreamGolden(t *testing.T) {
	c := assert.NewAborting(t)

	raw, err := os.ReadFile(msgtest.GoldenPath("responses/openaichat-tooluse.sse"))
	c.NoError(err, "read the SSE fixture")
	got := accumulate(c, bytes.NewReader(raw))

	golden := decodeGolden(c, "golden/openaichat-tooluse.sse.decoded.json")
	c.Eq(golden.ID, got.ID)
	c.Eq(golden.Model, got.Model)
	c.Eq(golden.StopReason, got.StopReason.String())
	c.Eq(golden.Provider, got.Provider)
	c.EqDeep(golden.Content, renderedContent(c, got.Content), "content")

	// The same presence rules as DecodeResponse: the usage-only trailer carries
	// prompt_tokens including the cached prefix, plus a reasoning count.
	c.EqDeep(msg.Ptr(60), got.Usage.Input, "input is prompt_tokens net of the cached prefix")
	c.EqDeep(msg.Ptr(40), got.Usage.CacheRead)
	c.EqDeep(msg.Ptr(25), got.Usage.Output)
	c.EqDeep(msg.Ptr(3), got.Usage.Reasoning)
	c.True(got.Usage.CacheWrite == nil, "Chat Completions reports no cache write")
}

// TestStreamOneBlockOpen pins the close-then-open rule: a text → tool → text
// switch produces start/stop pairs with no block ever overlapping another, so
// every delta belongs to the block its start opened.
func TestStreamOneBlockOpen(t *testing.T) {
	c := assert.NewAborting(t)

	body := strings.Join([]string{
		chunk(`{"id":"c1","model":"m","choices":[{"index":0,"delta":{"role":"assistant","content":"a"},"finish_reason":null}]}`),
		chunk(`{"id":"c1","model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"read_file","arguments":""}}]},"finish_reason":null}]}`),
		chunk(`{"id":"c1","model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"path\":"}}]},"finish_reason":null}]}`),
		chunk(`{"id":"c1","model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"a\"}"}}]},"finish_reason":null}]}`),
		chunk(`{"id":"c1","model":"m","choices":[{"index":0,"delta":{"content":"b"},"finish_reason":null}]}`),
		chunk(`{"id":"c1","model":"m","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`),
		chunk("[DONE]"),
	}, "")

	dec := openaichat.NewStreamDecoder(io.NopCloser(strings.NewReader(body)))
	acc := msg.NewAccumulator()

	open := -1
	var started []msg.Block
	for {
		ev, err := dec.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		c.NoError(err, "next event")

		switch e := ev.(type) {
		case msg.BlockStart:
			c.Eq(-1, open, "block %d started while block %d was still open", e.Index, open)
			open = e.Index
			started = append(started, e.Block)
		case msg.BlockDelta:
			c.Eq(open, e.Index, "a delta arrived for a block that is not the open one")
		case msg.BlockStop:
			c.Eq(open, e.Index, "a stop arrived for a block that is not the open one")
			open = -1
		}
		c.NoError(acc.Add(ev), "accumulate %T", ev)
	}
	c.Eq(-1, open, "every block was stopped")
	c.NoError(dec.Close(), "close the decoder")

	c.Eq(3, len(started), "a text block, a tool block, a text block")
	c.EqDeep([]msg.Block{
		msg.Text{},
		msg.ToolUse{ID: "call_1", Name: "read_file", Input: json.RawMessage(`{}`)},
		msg.Text{},
	}, started)

	resp := acc.Response()
	c.EqDeep([]msg.Block{
		msg.Text{Text: "a"},
		msg.ToolUse{ID: "call_1", Name: "read_file", Input: json.RawMessage(`{"path":"a"}`)},
		msg.Text{Text: "b"},
	}, resp.Content, "each block keeps its own deltas")
}

// TestStreamKeepsUnmodelledFinishReason pins the streaming half of the rule: a
// finish_reason this codec does not map reaches the response verbatim through
// the MessageDelta, rather than being erased to "".
func TestStreamKeepsUnmodelledFinishReason(t *testing.T) {
	c := assert.NewAborting(t)
	const wire = "model_context_window_exceeded"
	body := chunk(`{"id":"c1","model":"m","choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":null}]}`) +
		chunk(`{"id":"c1","model":"m","choices":[{"index":0,"delta":{},"finish_reason":"`+wire+`"}],`+
			`"usage":{"prompt_tokens":3,"completion_tokens":1}}`) +
		chunk("[DONE]")
	resp := accumulate(c, strings.NewReader(body))
	c.Eq(msg.StopUnknown, resp.StopReason, "a value this codec does not map stays unknown")
	c.Eq(wire, resp.StopReasonRaw, "the stream's finish_reason is carried verbatim")
	c.Eq(wire, resp.StopReasonWire(), "the record falls back to the wire's spelling")
}

// TestStreamInBandTransientIsRetryable pins that an in-band chunk error becomes
// a status-0 APIError classified from its own payload.
func TestStreamInBandTransientIsRetryable(t *testing.T) {
	c := assert.NewAborting(t)

	body := chunk(`{"id":"c1","model":"m","error":{"code":500,"type":"server_error","message":"upstream exploded"}}`) +
		chunk("[DONE]")
	dec := openaichat.NewStreamDecoder(io.NopCloser(strings.NewReader(body)))

	_, err := dec.Next()
	apiErr := c.ErrorAs[*msg.APIError](err, "an in-band chunk error is an APIError")
	c.Eq(0, apiErr.Status, "an in-band error carries no HTTP status")
	c.Eq(msg.ErrTransient, apiErr.Kind)
	c.Eq(msg.DialectOpenAIChat, apiErr.Dialect)
	c.Eq("upstream exploded", apiErr.Message)
	c.NotEmpty(apiErr.Body, "the event's own JSON is the body")

	_, err = dec.Next()
	c.True(errors.Is(err, io.EOF), "the stream is over after the error, got %v", err)
}

// TestStreamInBandErrorFollowsContent pins that content already streamed is
// delivered before the failure, rather than swallowed by it.
func TestStreamInBandErrorFollowsContent(t *testing.T) {
	c := assert.NewAborting(t)

	body := chunk(`{"id":"c1","model":"m","choices":[{"index":0,"delta":{"content":"partial"},"finish_reason":null}]}`) +
		chunk(`{"id":"c1","model":"m","error":{"type":"overloaded_error","message":"Overloaded"}}`) +
		chunk("[DONE]")
	dec := openaichat.NewStreamDecoder(io.NopCloser(strings.NewReader(body)))

	start, err := dec.Next()
	c.NoError(err, "the message start is delivered")
	c.EqDeep[msg.StreamEvent](msg.MessageStart{ID: "c1", Model: "m"}, start)

	_, err = dec.Next()
	c.NoError(err, "the block start is delivered")

	delta, err := dec.Next()
	c.NoError(err, "the text delta is delivered")
	c.EqDeep[msg.StreamEvent](msg.BlockDelta{Index: 0, Kind: msg.DeltaText, Text: "partial"}, delta)

	_, err = dec.Next()
	apiErr := c.ErrorAs[*msg.APIError](err, "the failure follows the content")
	c.Eq(msg.ErrOverloaded, apiErr.Kind)
}

// TestStreamTruncatedIsTransient pins that a body which ends without a
// finish_reason fails rather than reading as a completed turn.
func TestStreamTruncatedIsTransient(t *testing.T) {
	c := assert.NewAborting(t)

	body := chunk(`{"id":"c1","model":"m","choices":[{"index":0,"delta":{"content":"partial"},"finish_reason":null}]}`)
	dec := openaichat.NewStreamDecoder(io.NopCloser(strings.NewReader(body)))

	var last error
	for {
		_, err := dec.Next()
		if errors.Is(err, io.EOF) {
			c.Fail("a truncated stream ended cleanly")
			break
		}
		if err != nil {
			last = err
			break
		}
	}
	apiErr := c.ErrorAs[*msg.APIError](last, "a truncated stream is an APIError")
	c.Eq(0, apiErr.Status)
	c.Eq(msg.ErrTransient, apiErr.Kind)
	c.StrContains(apiErr.Message, "without a finish_reason")
}

// accumulate drains a stream through an accumulator, which is how a caller
// consumes one: it also proves the decoder's event order is legal.
func accumulate(c *assert.C, r io.Reader) *msg.Response {
	dec := openaichat.NewStreamDecoder(io.NopCloser(r))
	acc := msg.NewAccumulator()
	for {
		ev, err := dec.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		c.NoError(err, "next event")
		c.NoError(acc.Add(ev), "accumulate %T", ev)
	}
	c.NoError(dec.Close(), "close the decoder")
	return acc.Response()
}

// chunk renders one SSE event block.
func chunk(data string) string { return "data: " + data + "\n\n" }
