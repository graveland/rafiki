// SPDX-License-Identifier: Apache-2.0

package routing

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"go.graveland.dev/rafiki/pkg/msg"
	msganthropic "go.graveland.dev/rafiki/pkg/msg/anthropic"
)

type CapturedUsage struct {
	InputTokens         int64
	OutputTokens        int64
	CacheReadTokens     int64
	CacheCreationTokens int64

	// Model is the served model reported by the response (message.model), the
	// ground truth when the request carried an alias (~vendor/x-latest). Empty
	// when the response omits it.
	Model string

	// Provider is the endpoint that actually served the request, reported by
	// OpenRouter's Anthropic face as a non-standard top-level "provider" field
	// (e.g. "Novita"). Empty for native Anthropic responses, which have no such
	// field. This is the only per-response signal of WHICH provider OpenRouter
	// routed to, and ProviderGuard needs it to attribute a cache miss.
	Provider string
}

type wireUsage struct {
	InputTokens              int64 `json:"input_tokens"`
	OutputTokens             int64 `json:"output_tokens"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
}

// CaptureDiagnostics is what reassembly noticed but did not fail the turn over.
// The proxy's capture is BEST-EFFORT: the client already has the bytes, so
// discarding an accumulated turn over a shape the codec did not model would
// only lose the record and the billing row. The caller logs every entry loudly
// and persists the turn as the normal completion it was.
type CaptureDiagnostics struct {
	// Repaired describes each content block whose input JSON did not reassemble
	// and was replaced with the malformed-input placeholder (repairToolInput).
	Repaired []string
	// Anomalies describes each stream event reassembly could not fold in and
	// therefore skipped, in arrival order — an unsupported content delta, a delta
	// or stop for a block index that was never started, an in-band error event, a
	// malformed event. Bounded by maxCaptureAnomalies, with a trailing count.
	Anomalies []string
}

// ParseCapturedResponse extracts stop_reason + usage from an Anthropic response
// AND returns the canonical response body to persist: a streaming (SSE) response
// is reassembled into the final Message JSON so the stored `response` is always
// a valid JSON Message (the same shape the in-process core stores), never raw
// SSE. A non-SSE body is already a JSON Message and is returned unchanged.
//
// diag is what reassembly skipped or repaired along the way: non-empty for a
// turn that still persists, and the caller should log it. A tool_use block whose
// input JSON failed to reassemble is repaired, and an event the codec or the
// accumulator refuses is skipped, so neither costs the turn its usage or its
// content.
//
// The error is non-nil only when the body is not a recognizable Message at all —
// a gateway error page delivered with a success status, wire skew, a body with
// no reassemblable event — in which case canonical is nil and the caller must
// mark the turn errored: recording a zero-usage "completion" would both lie in
// the metrics and crash the JSONB append (raw SSE is not a valid canonical).
func ParseCapturedResponse(contentType string, body []byte) (string, CapturedUsage, []byte, CaptureDiagnostics, error) {
	if strings.Contains(contentType, "text/event-stream") {
		return parseSSE(body)
	}
	stop, u, err := parseJSONMessage(body)
	if err != nil {
		return "", CapturedUsage{}, nil, CaptureDiagnostics{}, err
	}
	return stop, u, body, CaptureDiagnostics{}, nil
}

func parseJSONMessage(body []byte) (string, CapturedUsage, error) {
	var m struct {
		StopReason string    `json:"stop_reason"`
		Model      string    `json:"model"`
		Provider   string    `json:"provider"`
		Usage      wireUsage `json:"usage"`
	}
	if err := json.Unmarshal(body, &m); err != nil {
		return "", CapturedUsage{}, fmt.Errorf("response body is not a JSON Message: %w", err)
	}
	u := toCapturedUsage(m.Usage)
	u.Model = m.Model
	u.Provider = m.Provider
	return m.StopReason, u, nil
}

// invalidInputs is the codec decoder's repair hook: the raw, unparseable
// input fragments it buffered for a block. The accumulator replaces such a
// block's input with an empty object, so this is the only place the original
// bytes survive.
type invalidInputs interface {
	InvalidInput(index int) (json.RawMessage, bool)
}

// maxCaptureAnomalies bounds how many skipped events reassembly reports. A
// stream carrying one unsupported delta per token can refuse thousands; the
// first few identify the shape and a trailing count covers the rest.
const maxCaptureAnomalies = 10

// parseSSE walks the event stream once through the shared Anthropic codec,
// accumulating the events into a native response so the caller can persist the
// finished message as canonical JSON instead of the raw stream.
//
// Every event is translated by the codec and folded in by msg.Accumulator, so a
// captured turn and the same turn decoded anywhere else in the daemon carry
// identical blocks: start-block residue (a tool's caller, a server tool's own
// type and metadata), citations added by a delta, a signature seeded at the
// block start, and usage backfill (Anthropic reports input/cache counts in
// message_start, OpenRouter sends zeros there and the full usage in
// message_delta) all come from the one implementation.
//
// Reassembly is BEST-EFFORT, deliberately unlike msg.StreamDecoder's callers: an
// event the codec refuses (an unsupported content block delta, an in-band error
// event, a malformed event) or the accumulator refuses (a delta or a stop for a
// block index that was never started) is SKIPPED and recorded in the returned
// diagnostics, and the events that did accumulate are still persisted. On this
// path the client has already received its bytes, so failing the capture over
// one event only loses the record and the billing row — the very outcome
// best-effort capture exists to prevent. A body that produced no event at all is
// still a parse error: there is nothing to persist.
func parseSSE(body []byte) (string, CapturedUsage, []byte, CaptureDiagnostics, error) {
	dec := msganthropic.NewStreamDecoder(io.NopCloser(bytes.NewReader(body)))
	acc := msg.NewAccumulator()
	var diag CaptureDiagnostics
	skipped := 0
	skip := func(reason string) {
		skipped++
		if len(diag.Anomalies) < maxCaptureAnomalies {
			diag.Anomalies = append(diag.Anomalies, reason)
		}
	}
	seen := false
	for {
		ev, err := dec.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if errors.Is(err, io.ErrUnexpectedEOF) {
			// The body ended without message_stop. The proxy's copy loop is where
			// a mid-stream cut is detected and failed (a truncated turn never
			// reaches capture), so what arrives here is whatever the upstream
			// wrote; the events accumulated so far are the record of it.
			break
		}
		if err != nil {
			skip(err.Error())
			continue
		}
		if err := acc.Add(ev); err != nil {
			skip(err.Error())
			continue
		}
		seen = true
	}
	if cerr := dec.Close(); cerr != nil {
		return "", CapturedUsage{}, nil, diag, fmt.Errorf("reassemble response stream: %w", cerr)
	}
	if skipped > len(diag.Anomalies) {
		diag.Anomalies = append(diag.Anomalies, fmt.Sprintf("%d further event(s) skipped", skipped-len(diag.Anomalies)))
	}
	// Nothing reassembling means the stream wasn't Anthropic wire format at all
	// (a gateway error page with a success status, wire skew): a failed parse,
	// not a zero-usage completion. A stream whose only event is a message_start
	// or a message_delta still is one, and persists — a legitimately
	// content-less Message (max_tokens can stop a turn before the first content
	// block) must not be lost.
	if !seen {
		if len(diag.Anomalies) > 0 {
			return "", CapturedUsage{}, nil, diag, fmt.Errorf("response stream did not reassemble into a Message (%d bytes captured): %s", len(body), diag.Anomalies[0])
		}
		return "", CapturedUsage{}, nil, diag, fmt.Errorf("response stream did not reassemble into a Message (%d bytes captured)", len(body))
	}
	resp := acc.Response()
	diag.Repaired = repairToolInput(resp, dec)
	canonical, err := capturedResponse(resp)
	if err != nil {
		return "", CapturedUsage{}, nil, diag, err
	}
	u := CapturedUsage{
		InputTokens:         msg.Val(resp.Usage.Input),
		OutputTokens:        msg.Val(resp.Usage.Output),
		CacheReadTokens:     msg.Val(resp.Usage.CacheRead),
		CacheCreationTokens: msg.Val(resp.Usage.CacheWrite),
		Model:               resp.Model,
		Provider:            resp.Provider,
	}
	return resp.StopReasonWire(), u, canonical, diag, nil
}

// capturedResponse renders a reassembled response as the recorded body: the
// Anthropic message envelope this path has always stored (id, type, role,
// model, and the served-by provider when OpenRouter named one) around the
// codec's content blocks, with usage by presence — a count the provider did not
// report is omitted rather than recorded as zero. The stop reason keeps the
// wire's own spelling when the codec does not model it.
func capturedResponse(r *msg.Response) ([]byte, error) {
	content, err := msganthropic.EncodeContent(msg.Message{Role: msg.Assistant, Content: r.Content})
	if err != nil {
		return nil, fmt.Errorf("encode reassembled content: %w", err)
	}
	body := map[string]any{
		"id":          r.ID,
		"type":        "message",
		"role":        "assistant",
		"model":       r.Model,
		"content":     json.RawMessage(content),
		"stop_reason": r.StopReasonWire(),
		"usage":       capturedUsageBody(r.Usage),
	}
	if r.Provider != "" {
		body["provider"] = r.Provider
	}
	out, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal reassembled message: %w", err)
	}
	return out, nil
}

// capturedUsageBody renders the counts the provider actually reported.
func capturedUsageBody(u msg.Usage) map[string]int64 {
	body := map[string]int64{}
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

// repairToolInput repairs any block whose reassembled input is not valid JSON,
// replacing the input with a placeholder that preserves the raw fragments for
// forensics. Both block kinds are repaired: a client tool call keeps its typed
// block, and an opaque server-tool block keeps its own type and every other key
// it carried. Returns a description per repaired block, or nil if nothing
// needed fixing.
//
// Anthropic has been observed sending a spurious duplicate input_json_delta
// chunk for a tool_use block (an extra fragment repeating part of the next one),
// and the accumulator concatenates partial_json chunks with no validation, so
// one bad chunk corrupts that block's input into invalid JSON. usage/stop are
// accumulated independently and are already correct — repair the block instead
// of discarding the whole turn over it, or a rare upstream glitch silently drops
// billing data for that turn. A server-tool block is opaque, so there is no
// client tool executor to misroute it to, but its input is still part of the
// record: storing an empty object would claim the tool was called with no
// arguments.
func repairToolInput(r *msg.Response, dec msg.StreamDecoder) []string {
	invalid, ok := dec.(invalidInputs)
	if !ok {
		return nil
	}
	var repaired []string
	for i, block := range r.Content {
		raw, bad := invalid.InvalidInput(i)
		if !bad {
			continue
		}
		switch b := block.(type) {
		case msg.ToolUse:
			b.Input = malformedInput(raw)
			r.Content[i] = b
			repaired = append(repaired, fmt.Sprintf("content[%d] tool_use %q: invalid input JSON replaced with placeholder (raw=%q)", i, b.Name, string(raw)))
		case msg.Opaque:
			patched, err := opaqueMalformedInput(b.Raw, raw)
			if err != nil {
				repaired = append(repaired, fmt.Sprintf("content[%d] %s: invalid input JSON could not be recorded (raw=%q): %v", i, msg.OpaqueType(b), string(raw), err))
				continue
			}
			b.Raw = patched
			r.Content[i] = b
			repaired = append(repaired, fmt.Sprintf("content[%d] %s: invalid input JSON replaced with placeholder (raw=%q)", i, msg.OpaqueType(b), string(raw)))
		}
	}
	return repaired
}

// opaqueMalformedInput replaces an opaque block's input with the malformed-input
// placeholder, leaving its own type and every other key as the provider sent
// them.
func opaqueMalformedInput(block, raw json.RawMessage) (json.RawMessage, error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(block, &obj); err != nil {
		return nil, fmt.Errorf("read opaque block: %w", err)
	}
	obj["input"] = malformedInput(raw)
	out, err := json.Marshal(obj)
	if err != nil {
		return nil, fmt.Errorf("encode opaque block: %w", err)
	}
	return out, nil
}

// malformedInput is the placeholder a repaired input is replaced with: an
// object carrying the original fragments as a string.
func malformedInput(raw json.RawMessage) json.RawMessage {
	quoted, err := json.Marshal(string(raw))
	if err != nil {
		return json.RawMessage(`{"_rafiki_malformed_input":""}`)
	}
	return json.RawMessage(fmt.Sprintf(`{"_rafiki_malformed_input":%s}`, quoted))
}

func toCapturedUsage(w wireUsage) CapturedUsage {
	return CapturedUsage{
		InputTokens:         w.InputTokens,
		OutputTokens:        w.OutputTokens,
		CacheReadTokens:     w.CacheReadInputTokens,
		CacheCreationTokens: w.CacheCreationInputTokens,
	}
}
