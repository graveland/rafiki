// SPDX-License-Identifier: Apache-2.0

package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/packages/ssestream"

	"go.graveland.dev/rafiki/pkg/msg"
	msganthropic "go.graveland.dev/rafiki/pkg/msg/anthropic"
)

// This file is the temporary SDK-shaped seam: the interfaces the SDK-backed
// sender and the test fakes still implement, and the adapter that lets them
// stand in for a native Sender. It is deleted once every caller builds native
// requests.

// SDKSender issues one Messages-API call. The SDK client is wrapped behind this
// seam so tests inject fakes and the client stays provider-agnostic.
type SDKSender interface {
	New(ctx context.Context, params anthropic.MessageNewParams) (*anthropic.Message, error)
}

// SDKStreamingSender is an optional capability: an SDKSender that can also open
// a streaming Messages call. Callers type-assert for it and fall back to New,
// so an SDKSender that cannot stream (test fakes, custom transports) stays
// valid.
//
// The error return exists for implementations other than sdkSender: the
// Anthropic SDK's own Messages.NewStreaming never fails this way, so no
// first-party sender ever returns a non-nil error here. An SDKStreamingSender
// that does MAY also return a non-nil stream alongside it — the caller is
// responsible for closing that stream on every path, including this one, so
// implementations should not assume an error return means nothing needs
// cleanup.
type SDKStreamingSender interface {
	SDKSender
	NewStreaming(ctx context.Context, params anthropic.MessageNewParams) (*ssestream.Stream[anthropic.MessageStreamEventUnion], error)
}

type sdkSender struct{ client anthropic.Client }

// New sends one Messages call with foreign thinking removed: a thinking block
// whose signature was minted by another provider (the Responses senders write
// openai-responses:<mode>:<id>:<blob>) is unverifiable here, and Anthropic
// rejects a request carrying one. params is copied first: this is a filter on
// the way out, never a mutation of the caller's history.
func (s sdkSender) New(ctx context.Context, params anthropic.MessageNewParams) (*anthropic.Message, error) {
	params = withoutForeignThinking(params, false)
	return s.client.Messages.New(ctx, params)
}

// withoutForeignThinking applies StripForeignThinking to a shallow copy of
// params: the filter lives in the Messages slice, so nothing else needs copying.
func withoutForeignThinking(params anthropic.MessageNewParams, forResponses bool) anthropic.MessageNewParams {
	params.Messages = StripForeignThinking(params.Messages, forResponses)
	return params
}

// NewStreaming opens a streaming Messages call. The SDK's NewStreaming does not
// itself return an error (errors surface as an error event on the stream or from
// the stream's Err() after iteration), so the nil here is not a stub — it
// matches the real signature.
func (s sdkSender) NewStreaming(ctx context.Context, params anthropic.MessageNewParams) (*ssestream.Stream[anthropic.MessageStreamEventUnion], error) {
	params = withoutForeignThinking(params, false)
	return s.client.Messages.NewStreaming(ctx, params), nil
}

// FromSDK wraps a pre-built SDK client as a Sender — e.g. sc's forward-mode
// diagnose client pointed at the central /v1/messages proxy.
func FromSDK(client anthropic.Client) Sender {
	return FromSDKSender(sdkSender{client: client})
}

// FromSDKSender adapts an SDK-typed sender (the SDK client, or a test fake) to
// the native Sender. Send encodes the native request as an Anthropic body,
// decodes it into SDK params, calls the sender and decodes the reply back;
// Stream requires SDKStreamingSender, and otherwise falls back to Send and
// replays the response as events.
func FromSDKSender(s SDKSender) Sender {
	return sdkAdapter{s: s}
}

// sdkAdapter is FromSDKSender's implementation.
type sdkAdapter struct{ s SDKSender }

// Send issues one non-streamed call.
func (a sdkAdapter) Send(ctx context.Context, req *msg.Request, opts msg.EncodeOptions) (*msg.Response, error) {
	params, err := sdkParams(req, opts)
	if err != nil {
		return nil, err
	}
	m, err := a.s.New(ctx, params)
	if err != nil {
		return nil, apiErrorFromSDK(err)
	}
	return responseFromSDK(m)
}

// Stream opens a streamed call, re-rendering each SDK event as an SSE frame the
// Anthropic codec's decoder reads — which is what keeps an SDK-typed fake's
// events classified by the same codec the real senders use.
func (a sdkAdapter) Stream(ctx context.Context, req *msg.Request, opts msg.EncodeOptions) (msg.StreamDecoder, error) {
	params, err := sdkParams(req, opts)
	if err != nil {
		return nil, err
	}
	streamer, ok := a.s.(SDKStreamingSender)
	if !ok {
		resp, err := a.s.New(ctx, params)
		if err != nil {
			return nil, apiErrorFromSDK(err)
		}
		dec, err := responseFromSDK(resp)
		if err != nil {
			return nil, err
		}
		return &responseDecoder{resp: dec}, nil
	}
	stream, err := streamer.NewStreaming(ctx, params)
	if err != nil {
		// A third-party implementation may hand back a stream AND an error; the
		// stream still owns a decoder and a response body, so it is closed here
		// rather than leaked.
		if stream != nil {
			_ = stream.Close()
		}
		return nil, apiErrorFromSDK(err)
	}
	if stream == nil {
		return nil, errors.New("llm: sdk sender returned no stream")
	}
	pr, pw := io.Pipe()
	go renderSDKStream(stream, pw)
	return msganthropic.NewStreamDecoder(pr), nil
}

// renderSDKStream writes stream's events into pw as SSE frames. A stream error
// the SDK rendered as an in-band "error" event is written back as one, so the
// codec classifies it exactly as it classifies a real provider's error event.
func renderSDKStream(stream *ssestream.Stream[anthropic.MessageStreamEventUnion], pw *io.PipeWriter) {
	defer stream.Close()
	for stream.Next() {
		ev := stream.Current()
		// One data line per event: a raw payload with embedded newlines would be
		// read as several data lines and reassembled with them still in it.
		if _, err := fmt.Fprintf(pw, "event: %s\ndata: %s\n\n", ev.Type, oneLine(ev.RawJSON())); err != nil {
			_ = pw.CloseWithError(err)
			return
		}
	}
	if serr := stream.Err(); serr != nil {
		if payload, ok := inBandStreamPayload(serr); ok {
			if _, err := fmt.Fprintf(pw, "event: error\ndata: %s\n\n", oneLine(payload)); err != nil {
				_ = pw.CloseWithError(err)
				return
			}
			_ = pw.Close()
			return
		}
		// An SDK error carries its status and body; converting it keeps the
		// classifiers that read *msg.APIError working on this path too.
		_ = pw.CloseWithError(apiErrorFromSDK(serr))
		return
	}
	_ = pw.Close()
}

// oneLine renders a JSON payload as a single line.
func oneLine(raw string) string {
	var buf bytes.Buffer
	if err := json.Compact(&buf, []byte(raw)); err != nil {
		return raw
	}
	return buf.String()
}

// inBandStreamPayload recovers the raw JSON of an SSE "error" event from the
// opaque wrapper the SDK's stream produces for one.
func inBandStreamPayload(err error) (string, bool) {
	const prefix = "received error while streaming: "
	text := err.Error()
	i := strings.Index(text, prefix)
	if i < 0 {
		return "", false
	}
	payload := text[i+len(prefix):]
	if !json.Valid([]byte(payload)) {
		return "", false
	}
	return payload, true
}

// sdkParams converts a native request into the SDK's params, through the
// Anthropic codec so the conversion is the codec's, not a second copy of it.
func sdkParams(req *msg.Request, opts msg.EncodeOptions) (anthropic.MessageNewParams, error) {
	var params anthropic.MessageNewParams
	body, err := msganthropic.Encode(req, opts)
	if err != nil {
		return params, err
	}
	if err := json.Unmarshal(body, &params); err != nil {
		return params, fmt.Errorf("llm: decode native request as sdk params: %w", err)
	}
	// The SDK's params decoder drops a top-level field it does not model, so the
	// extras the codec just merged are re-attached here: OpenRouter's "provider"
	// object is exactly the field an SDK sender cannot represent any other way.
	if len(opts.Extra) > 0 {
		params.SetExtraFields(opts.Extra)
	}
	return params, nil
}

// responseFromSDK decodes an SDK message back into a native response.
//
// The body is rebuilt by hand rather than by marshalling the SDK message: a
// response type's MarshalJSON writes only the fields it models, which drops
// OpenRouter's top-level "provider" — the one field the guard needs to know
// which backend served the turn. Each content block's own raw JSON is carried
// through verbatim.
func responseFromSDK(m *anthropic.Message) (*msg.Response, error) {
	if m == nil {
		return nil, errors.New("llm: sdk sender returned no message")
	}
	content := make([]json.RawMessage, 0, len(m.Content))
	for _, b := range m.Content {
		raw := b.RawJSON()
		if raw == "" {
			// A block built in code (a canned test message) has no wire bytes of
			// its own; the union's own JSON is the closest faithful rendering.
			enc, err := json.Marshal(b)
			if err != nil {
				return nil, fmt.Errorf("llm: encode sdk content block: %w", err)
			}
			raw = string(enc)
		}
		content = append(content, json.RawMessage(raw))
	}
	usage := map[string]int64{
		"input_tokens":                m.Usage.InputTokens,
		"output_tokens":               m.Usage.OutputTokens,
		"cache_read_input_tokens":     m.Usage.CacheReadInputTokens,
		"cache_creation_input_tokens": m.Usage.CacheCreationInputTokens,
	}
	body := map[string]any{
		"id":          m.ID,
		"type":        "message",
		"role":        "assistant",
		"model":       string(m.Model),
		"content":     content,
		"stop_reason": string(m.StopReason),
		"usage":       usage,
	}
	if p := ProviderOf(m); p != "" {
		body["provider"] = p
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("llm: encode sdk message: %w", err)
	}
	return msganthropic.DecodeResponse(raw)
}

// apiErrorFromSDK turns an SDK error into the native API error every classifier
// reads. An error that is not the SDK's HTTP error is returned unchanged: it is
// a transport or configuration failure, not a provider refusal.
func apiErrorFromSDK(err error) error {
	var sdkErr *anthropic.Error
	if !errors.As(err, &sdkErr) {
		return err
	}
	status := sdkErr.StatusCode
	header := http.Header(nil)
	if sdkErr.Response != nil {
		if status == 0 {
			status = sdkErr.Response.StatusCode
		}
		header = sdkErr.Response.Header.Clone()
	}
	body := []byte(sdkErr.RawJSON())
	apiErr := &msg.APIError{
		Status:  status,
		Header:  header,
		Body:    body,
		Kind:    msganthropic.Classify(status, header, body),
		Dialect: msg.DialectAnthropic,
	}
	if header != nil {
		apiErr.RetryAfter = parseRetryAfter(header.Get("Retry-After"))
	}
	if se, ok := parseInBandError(body); ok {
		apiErr.Message = se.Message
	} else if status != 0 {
		apiErr.Message = errorMessageOf(body)
	}
	return apiErr
}

// errorMessageOf reads an Anthropic error body's error.message.
func errorMessageOf(body []byte) string {
	var wire struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &wire); err != nil {
		return ""
	}
	return wire.Error.Message
}

// responseDecoder replays a whole response as stream events, for an SDK sender
// that cannot stream.
type responseDecoder struct {
	resp   *msg.Response
	events []msg.StreamEvent
	next   int
}

func (d *responseDecoder) Next() (msg.StreamEvent, error) {
	if d.events == nil {
		d.events = eventsForResponse(d.resp)
	}
	if d.next >= len(d.events) {
		return nil, io.EOF
	}
	ev := d.events[d.next]
	d.next++
	return ev, nil
}

func (d *responseDecoder) Close() error { return nil }

// eventsForResponse renders a whole response as the event sequence a stream
// would have produced for it.
func eventsForResponse(resp *msg.Response) []msg.StreamEvent {
	events := []msg.StreamEvent{msg.MessageStart{
		ID:       resp.ID,
		Model:    resp.Model,
		Provider: resp.Provider,
		Usage:    resp.Usage,
	}}
	for i, b := range resp.Content {
		events = append(events, msg.BlockStart{Index: i, Block: b}, msg.BlockStop{Index: i, Block: b})
	}
	return append(events, msg.MessageDelta{StopReason: resp.StopReason, Usage: resp.Usage})
}
