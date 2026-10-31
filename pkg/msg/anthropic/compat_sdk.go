// SPDX-License-Identifier: Apache-2.0

package anthropic

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"strings"

	sdk "github.com/anthropics/anthropic-sdk-go"

	"go.graveland.dev/rafiki/pkg/msg"
)

// This file is the temporary bridge between the Anthropic SDK's types and the
// native request/response model, for the wave in which the client still builds
// SDK params and the senders already speak native types. Nothing here outlives
// that wave: once the client assembles msg.Request directly, the only surviving
// piece is ToParam/FromParam, which the stored-message codec owns.

// RequestFromSDK converts SDK params to a native request: messages through
// FromParam, and the system, tools, tool choice, sampling, thinking and max
// tokens the params carry. Cache-control markers are dropped, because a native
// request carries a cache POLICY rather than markers; the one exception is this
// wave's breakpoint derivation, which reads the markers back into a policy so
// the caller's placement survives the conversion (see cacheFromMarkers).
//
// The returned map holds the params' extra fields (SetExtraFields), which ride
// EncodeOptions.Extra: OpenRouter's "provider" object and a provider's own
// top-level knobs are exactly what the SDK had no field for.
func RequestFromSDK(p sdk.MessageNewParams) (*msg.Request, map[string]any, error) {
	req := &msg.Request{
		Model:     string(p.Model),
		MaxTokens: p.MaxTokens,
	}
	for _, s := range p.System {
		req.System = append(req.System, msg.Text{Text: s.Text})
	}
	for _, mp := range p.Messages {
		m, err := FromParam(mp)
		if err != nil {
			return nil, nil, err
		}
		req.Messages = append(req.Messages, m)
	}
	for _, t := range p.Tools {
		if t.OfTool == nil {
			// A server tool has no native declaration: the API defines its shape,
			// and nothing in a native request names it.
			continue
		}
		schema, err := json.Marshal(t.OfTool.InputSchema)
		if err != nil {
			return nil, nil, fmt.Errorf("anthropic: tool %q input schema: %w", t.OfTool.Name, err)
		}
		def := msg.ToolDef{Name: t.OfTool.Name, InputSchema: schema}
		if t.OfTool.Description.Valid() {
			def.Description = t.OfTool.Description.Value
		}
		req.Tools = append(req.Tools, def)
	}
	if p.ToolChoice.OfTool != nil {
		req.ForceTool = p.ToolChoice.OfTool.Name
	}
	if p.Temperature.Valid() {
		v := p.Temperature.Value
		req.Temperature = &v
	}
	if p.TopK.Valid() {
		v := p.TopK.Value
		req.TopK = &v
	}
	if p.Thinking.OfEnabled != nil {
		req.ThinkingBudget = p.Thinking.OfEnabled.BudgetTokens
	}
	req.Cache = cacheFromMarkers(p)
	return req, p.ExtraFields(), nil
}

// cacheFromMarkers derives a cache policy from the markers the params carry:
// the system TTL from the last system block's marker, and the message TTL and
// breakpoint count from the message markers (the count clamped to [1, 3], the
// TTL 1h when any marker asks for it, and off when there are none). The codec
// re-places them by the same rule that placed them here, so the wire keeps its
// breakpoints.
func cacheFromMarkers(p sdk.MessageNewParams) msg.CachePolicy {
	var policy msg.CachePolicy
	if n := len(p.System); n > 0 {
		policy.SystemTTL = cacheTTL(p.System[n-1].CacheControl)
	}
	policy.MessagesTTL = msg.CacheOff
	count := 0
	ttl := msg.CacheOff
	for _, mp := range p.Messages {
		for _, b := range mp.Content {
			cc := b.GetCacheControl()
			if cc == nil || cc.Type == "" {
				continue
			}
			count++
			if cacheTTL(*cc) == msg.Cache1h || ttl == msg.CacheOff {
				ttl = cacheTTL(*cc)
			}
		}
	}
	if count == 0 {
		return policy
	}
	if count > 3 {
		count = 3
	}
	policy.MessagesTTL = ttl
	policy.Breakpoints = count
	return policy
}

// cacheTTL maps a marker to its TTL. A zero marker is CacheOff: the SDK omits a
// cache_control field it was never given.
func cacheTTL(cc sdk.CacheControlEphemeralParam) msg.CacheTTL {
	if cc.Type == "" {
		return msg.CacheOff
	}
	if cc.TTL == sdk.CacheControlEphemeralTTLTTL1h {
		return msg.Cache1h
	}
	return msg.Cache5m
}

// responseWire is the Anthropic response body a native response is rendered as
// before the SDK decodes it.
type responseWire struct {
	ID         string           `json:"id"`
	Type       string           `json:"type"`
	Role       string           `json:"role"`
	Model      string           `json:"model"`
	Content    json.RawMessage  `json:"content"`
	StopReason string           `json:"stop_reason"`
	Usage      map[string]int64 `json:"usage"`
	Provider   string           `json:"provider,omitempty"`
}

// ToSDKMessage converts a native response to the SDK message: the response is
// encoded as Anthropic response JSON and decoded by the SDK, so every block
// keeps the raw JSON ContentBlockUnion.ToParam and the accumulators read.
// Provider rides the top-level "provider" field, which the SDK retains as an
// extra field — that is what ProviderOf reads.
func ToSDKMessage(r *msg.Response) (*sdk.Message, error) {
	if r == nil {
		return nil, errors.New("anthropic: nil response")
	}
	content, err := EncodeContent(msg.Message{Role: msg.Assistant, Content: r.Content})
	if err != nil {
		return nil, err
	}
	usage := map[string]int64{}
	if r.Usage.Input != nil {
		usage["input_tokens"] = *r.Usage.Input
	}
	if r.Usage.Output != nil {
		usage["output_tokens"] = *r.Usage.Output
	}
	if r.Usage.CacheRead != nil {
		usage["cache_read_input_tokens"] = *r.Usage.CacheRead
	}
	if r.Usage.CacheWrite != nil {
		usage["cache_creation_input_tokens"] = *r.Usage.CacheWrite
	}
	body, err := json.Marshal(responseWire{
		ID:         r.ID,
		Type:       "message",
		Role:       "assistant",
		Model:      r.Model,
		Content:    content,
		StopReason: r.StopReason.String(),
		Usage:      usage,
		Provider:   r.Provider,
	})
	if err != nil {
		return nil, fmt.Errorf("anthropic: encode response: %w", err)
	}
	var out sdk.Message
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("anthropic: decode response: %w", err)
	}
	return &out, nil
}

// ResponseJSON renders a native response as the body this package records: id,
// model, stop reason, the served-by provider when present, the usage counts the
// provider actually reported, and the content blocks. It is the canonical
// recorded form (capture's turn response and the raw trace), so it carries
// exactly the fields the native model has — never the SDK's own wire shape.
func ResponseJSON(r *msg.Response) ([]byte, error) {
	if r == nil {
		return nil, errors.New("anthropic: nil response")
	}
	content, err := EncodeContent(msg.Message{Role: msg.Assistant, Content: r.Content})
	if err != nil {
		return nil, err
	}
	body := map[string]any{
		"id":          r.ID,
		"model":       r.Model,
		"stop_reason": r.StopReason.String(),
		"usage":       usageBody(r.Usage),
		"content":     json.RawMessage(content),
	}
	if r.Provider != "" {
		body["provider"] = r.Provider
	}
	out, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("anthropic: encode response: %w", err)
	}
	return out, nil
}

// SDKEventAdapter converts native stream events back into the SDK's events, for
// the one wave in which the client's stream handler and accumulator are still
// SDK-typed.
//
// It keeps per-block state because a native stream reports a block's finalized
// form at BlockStop while the SDK path accumulates fragments: anything the
// final block carries that was never delivered as a delta — a citation list, a
// server-tool block's completed input — has to be replayed as the delta
// sequence the accumulator understands, and nothing already delivered may be
// replayed twice.
type SDKEventAdapter struct {
	blocks map[int]*adapterBlock
	// warned keeps the divergence warning to one per adapter (one per stream):
	// a provider that rewrites blocks it already streamed would otherwise
	// repeat the same line for every block. Like blocks, it is not synchronised:
	// an adapter belongs to one stream, driven by one goroutine.
	warned bool
}

// adapterBlock is one open block: its start JSON, the fields it started with,
// and everything the adapter has delivered for it since.
type adapterBlock struct {
	start          json.RawMessage
	startText      string
	startThinking  string
	startSignature string
	startInput     json.RawMessage
	startCitations []json.RawMessage

	text      strings.Builder
	thinking  strings.Builder
	signature strings.Builder
	input     strings.Builder
	inputSeen bool
}

// Add converts one native event into the SDK events it corresponds to.
func (a *SDKEventAdapter) Add(ev msg.StreamEvent) ([]sdk.MessageStreamEventUnion, error) {
	if a.blocks == nil {
		a.blocks = make(map[int]*adapterBlock)
	}
	switch e := ev.(type) {
	case msg.MessageStart:
		return wrapEvent(map[string]any{
			"type":    "message_start",
			"message": messageStartBody(e),
		})
	case msg.BlockStart:
		return a.blockStart(e)
	case msg.BlockDelta:
		return a.blockDelta(e)
	case msg.BlockStop:
		return a.blockStop(e)
	case msg.MessageDelta:
		out, err := wrapEvent(map[string]any{
			"type":  "message_delta",
			"delta": messageDeltaBody(e),
			"usage": usageBody(e.Usage),
		})
		if err != nil {
			return nil, err
		}
		stop, err := wrapEvent(map[string]any{"type": "message_stop"})
		if err != nil {
			return nil, err
		}
		return append(out, stop...), nil
	}
	return nil, fmt.Errorf("anthropic: unsupported stream event %T", ev)
}

// messageStartBody renders a native message start as the SDK's message.
func messageStartBody(e msg.MessageStart) map[string]any {
	body := map[string]any{
		"id":      e.ID,
		"type":    "message",
		"role":    "assistant",
		"model":   e.Model,
		"content": []any{},
		"usage":   usageBody(e.Usage),
	}
	if e.Provider != "" {
		body["provider"] = e.Provider
	}
	return body
}

// messageDeltaBody renders a native message delta's stop reason. An unknown
// stop reason is null, which is what the wire sends when a provider omits it.
func messageDeltaBody(e msg.MessageDelta) map[string]any {
	body := map[string]any{}
	if r := e.StopReason.String(); r != "" {
		body["stop_reason"] = r
	} else {
		body["stop_reason"] = nil
	}
	return body
}

// usageBody renders the counts a native usage reports. A count the provider did
// not report is omitted: an absent count and a reported zero are different
// facts, and only the provider knows which it sent.
func usageBody(u msg.Usage) map[string]any {
	body := map[string]any{}
	if u.Input != nil {
		body["input_tokens"] = *u.Input
	}
	if u.Output != nil {
		body["output_tokens"] = *u.Output
	}
	if u.CacheRead != nil {
		body["cache_read_input_tokens"] = *u.CacheRead
	}
	if u.CacheWrite != nil {
		body["cache_creation_input_tokens"] = *u.CacheWrite
	}
	return body
}

func (a *SDKEventAdapter) blockStart(e msg.BlockStart) ([]sdk.MessageStreamEventUnion, error) {
	if _, ok := a.blocks[e.Index]; ok {
		return nil, fmt.Errorf("anthropic: content block %d started twice", e.Index)
	}
	raw, err := encodeBlock(e.Block)
	if err != nil {
		return nil, err
	}
	st := &adapterBlock{start: raw}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, fmt.Errorf("anthropic: read content block start: %w", err)
	}
	st.startText = rawString(fields["text"])
	st.startThinking = rawString(fields["thinking"])
	st.startSignature = rawString(fields["signature"])
	if in, ok := fields["input"]; ok {
		st.startInput = in
	}
	st.startCitations = rawCitations(fields["citations"])
	a.blocks[e.Index] = st
	return wrapEvent(map[string]any{
		"type":          "content_block_start",
		"index":         e.Index,
		"content_block": raw,
	})
}

func (a *SDKEventAdapter) blockDelta(e msg.BlockDelta) ([]sdk.MessageStreamEventUnion, error) {
	st := a.blocks[e.Index]
	if st == nil {
		return nil, fmt.Errorf("anthropic: content block %d has no start", e.Index)
	}
	var delta map[string]any
	switch e.Kind {
	case msg.DeltaText:
		st.text.WriteString(e.Text)
		delta = map[string]any{"type": "text_delta", "text": e.Text}
	case msg.DeltaThinking:
		st.thinking.WriteString(e.Text)
		delta = map[string]any{"type": "thinking_delta", "thinking": e.Text}
	case msg.DeltaSignature:
		st.signature.WriteString(e.Text)
		delta = map[string]any{"type": "signature_delta", "signature": e.Text}
	case msg.DeltaInputJSON:
		st.input.WriteString(e.Text)
		st.inputSeen = true
		delta = map[string]any{"type": "input_json_delta", "partial_json": e.Text}
	default:
		return nil, fmt.Errorf("anthropic: unknown delta kind %d", e.Kind)
	}
	return wrapEvent(map[string]any{
		"type":  "content_block_delta",
		"index": e.Index,
		"delta": delta,
	})
}

// blockStop reconciles a finalized replacement block into the deltas that carry
// whatever the stream has not delivered yet, then closes the block.
func (a *SDKEventAdapter) blockStop(e msg.BlockStop) ([]sdk.MessageStreamEventUnion, error) {
	st := a.blocks[e.Index]
	delete(a.blocks, e.Index)
	var out []sdk.MessageStreamEventUnion
	if st != nil && e.Block != nil {
		reconciled, err := a.reconcile(e.Index, st, e.Block)
		if err != nil {
			return nil, err
		}
		out = append(out, reconciled...)
	}
	stop, err := wrapEvent(map[string]any{"type": "content_block_stop", "index": e.Index})
	if err != nil {
		return nil, err
	}
	return append(out, stop...), nil
}

// reconcile turns the difference between a block's finalized form and what has
// already been delivered into the SDK's delta sequence.
func (a *SDKEventAdapter) reconcile(index int, st *adapterBlock, final msg.Block) ([]sdk.MessageStreamEventUnion, error) {
	raw, err := encodeBlock(final)
	if err != nil {
		return nil, err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, fmt.Errorf("anthropic: read finalized content block: %w", err)
	}
	var out []sdk.MessageStreamEventUnion
	appendDelta := func(delta map[string]any) error {
		evs, err := wrapEvent(map[string]any{"type": "content_block_delta", "index": index, "delta": delta})
		if err != nil {
			return err
		}
		out = append(out, evs...)
		return nil
	}

	for _, f := range []struct {
		key     string
		emitted *strings.Builder
		start   string
		delta   func(string) map[string]any
	}{
		{"text", &st.text, st.startText, func(s string) map[string]any {
			return map[string]any{"type": "text_delta", "text": s}
		}},
		{"thinking", &st.thinking, st.startThinking, func(s string) map[string]any {
			return map[string]any{"type": "thinking_delta", "thinking": s}
		}},
		{"signature", &st.signature, st.startSignature, func(s string) map[string]any {
			return map[string]any{"type": "signature_delta", "signature": s}
		}},
	} {
		delivered := f.start + f.emitted.String()
		got := rawString(fields[f.key])
		if got == delivered {
			continue
		}
		if !strings.HasPrefix(got, delivered) {
			// The finalized value does not extend what was already delivered
			// (a provider rewrote it rather than growing it). The SDK's
			// accumulator can only append, so the delivered bytes stand and the
			// difference cannot be replayed; it must not vanish silently either.
			a.warnDivergence(index, f.key)
			continue
		}
		suffix := got[len(delivered):]
		f.emitted.WriteString(suffix)
		if err := appendDelta(f.delta(suffix)); err != nil {
			return nil, err
		}
	}

	// A block whose input arrived as fragments has already delivered it; one
	// whose input was only finalized (a server-tool block, whose arguments the
	// codec folds into the block itself) has not. The SDK's accumulator replaces
	// an initial "{}" with the first fragment and appends every later one, so a
	// single fragment is only faithful when nothing was delivered yet.
	if in, ok := fields["input"]; ok {
		delivered := st.startInput
		if st.inputSeen {
			delivered = json.RawMessage(st.input.String())
		}
		switch {
		case jsonEqual(delivered, in):
			// Nothing the stream has not already delivered.
		case !st.inputSeen && isJSONObject(st.startInput) && isEmptyObject(st.startInput):
			compacted, err := compactJSON(in)
			if err != nil {
				return nil, err
			}
			if err := appendDelta(map[string]any{"type": "input_json_delta", "partial_json": string(compacted)}); err != nil {
				return nil, err
			}
		default:
			// Input was already delivered (as fragments, or as a non-empty
			// start) and the finalized form differs from it. Appending the
			// final value would corrupt the accumulated JSON, and nothing can
			// retract what was sent, so the delivered form stands — visibly.
			a.warnDivergence(index, "input")
		}
	}

	// Citations arrive as their own deltas on the wire, so the codec's finalized
	// block is the only place a native stream reports them.
	finalCitations := rawCitations(fields["citations"])
	if n := len(st.startCitations); len(finalCitations) > n {
		for _, c := range finalCitations[n:] {
			if err := appendDelta(map[string]any{"type": "citations_delta", "citation": c}); err != nil {
				return nil, err
			}
		}
	}
	return out, nil
}

// warnDivergence reports a finalized block that the deltas already delivered
// cannot be reconciled with: the provider rewrote text, a signature or a tool
// input instead of extending it. The SDK's accumulator only appends, so the
// delivered form stands and the difference cannot be replayed — but it is not
// dropped in silence, and one warning per stream is enough to make it visible.
func (a *SDKEventAdapter) warnDivergence(index int, field string) {
	if a.warned {
		return
	}
	a.warned = true
	slog.Warn("anthropic: finalized block diverges from the deltas already delivered; keeping the delivered form",
		"index", index, "field", field)
}

// wrapEvent renders one SDK stream event from its wire JSON, so the typed union
// (and the raw bytes it keeps) come from the same decoder the SDK's own stream
// uses.
func wrapEvent(v any) ([]sdk.MessageStreamEventUnion, error) {
	body, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("anthropic: encode stream event: %w", err)
	}
	var ev sdk.MessageStreamEventUnion
	if err := json.Unmarshal(body, &ev); err != nil {
		return nil, fmt.Errorf("anthropic: decode stream event: %w", err)
	}
	return []sdk.MessageStreamEventUnion{ev}, nil
}

// rawString reads a JSON string value, or "" for anything else.
func rawString(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return ""
	}
	return s
}

// rawCitations reads a block's citations array, or nil for anything else.
func rawCitations(raw json.RawMessage) []json.RawMessage {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	var out []json.RawMessage
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil
	}
	return out
}

// jsonEqual reports whether two JSON values are the same value, key order
// included in neither side of the comparison.
func jsonEqual(a, b []byte) bool {
	if len(bytes.TrimSpace(a)) == 0 || len(bytes.TrimSpace(b)) == 0 {
		return len(bytes.TrimSpace(a)) == 0 && len(bytes.TrimSpace(b)) == 0
	}
	var x, y any
	if err := json.Unmarshal(a, &x); err != nil {
		return false
	}
	if err := json.Unmarshal(b, &y); err != nil {
		return false
	}
	return reflect.DeepEqual(x, y)
}

// isJSONObject reports whether raw is a JSON object.
func isJSONObject(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) > 0 && trimmed[0] == '{'
}

// isEmptyObject reports whether raw is the empty JSON object.
func isEmptyObject(raw json.RawMessage) bool {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return false
	}
	return len(fields) == 0
}

// compactJSON removes the whitespace from a JSON value.
func compactJSON(raw []byte) ([]byte, error) {
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return nil, fmt.Errorf("anthropic: compact JSON: %w", err)
	}
	return buf.Bytes(), nil
}
