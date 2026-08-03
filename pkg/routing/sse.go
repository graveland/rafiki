// SPDX-License-Identifier: Apache-2.0

package routing

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
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
}

type wireUsage struct {
	InputTokens              int64 `json:"input_tokens"`
	OutputTokens             int64 `json:"output_tokens"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
}

// ParseCapturedResponse extracts stop_reason + usage from an Anthropic response
// AND returns the canonical response body to persist: a streaming (SSE) response
// is reassembled into the final Message JSON so the stored `response` is always
// a valid JSON Message (the same shape the in-process core stores), never raw
// SSE. A non-SSE body is already a JSON Message and is returned unchanged.
//
// The error is non-nil when the body is not a recognizable Message at all —
// a gateway error page delivered with a success status, SDK/wire skew, or a
// reassembly gap — in which case canonical is nil and the caller must mark the
// turn errored: recording a zero-usage "completion" would both lie in the
// metrics and crash the JSONB append (raw SSE is not a valid canonical).
func ParseCapturedResponse(contentType string, body []byte) (string, CapturedUsage, []byte, error) {
	if strings.Contains(contentType, "text/event-stream") {
		return parseSSE(body)
	}
	stop, u, err := parseJSONMessage(body)
	if err != nil {
		return "", CapturedUsage{}, nil, err
	}
	return stop, u, body, nil
}

func parseJSONMessage(body []byte) (string, CapturedUsage, error) {
	var m struct {
		StopReason string    `json:"stop_reason"`
		Model      string    `json:"model"`
		Usage      wireUsage `json:"usage"`
	}
	if err := json.Unmarshal(body, &m); err != nil {
		return "", CapturedUsage{}, fmt.Errorf("response body is not a JSON Message: %w", err)
	}
	u := toCapturedUsage(m.Usage)
	u.Model = m.Model
	return m.StopReason, u, nil
}

// parseSSE walks the event stream once, extracting stop_reason + usage (output
// tokens come only from message_delta, so a truncated stream reports 0) and, in
// the same pass, accumulating the events into an anthropic.Message via the SDK
// so the caller can persist the finished message as canonical JSON instead of
// the raw stream.
func parseSSE(body []byte) (string, CapturedUsage, []byte, error) {
	var stop string
	var u CapturedUsage
	var msg anthropic.Message
	accumulated := false
	sc := bufio.NewScanner(bytes.NewReader(body))
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		data, ok := strings.CutPrefix(sc.Text(), "data: ")
		if !ok {
			continue
		}
		var ev struct {
			Type    string `json:"type"`
			Message struct {
				Model string    `json:"model"`
				Usage wireUsage `json:"usage"`
			} `json:"message"`
			Delta struct {
				StopReason string `json:"stop_reason"`
			} `json:"delta"`
			Usage wireUsage `json:"usage"`
		}
		if err := json.Unmarshal([]byte(data), &ev); err != nil {
			continue
		}
		switch ev.Type {
		case "message_start":
			cu := toCapturedUsage(ev.Message.Usage)
			u.InputTokens, u.CacheReadTokens, u.CacheCreationTokens = cu.InputTokens, cu.CacheReadTokens, cu.CacheCreationTokens
			u.Model = ev.Message.Model
		case "message_delta":
			if ev.Delta.StopReason != "" {
				stop = ev.Delta.StopReason
			}
			// Anthropic reports input/cache tokens in message_start and only
			// cumulative output_tokens here; OpenRouter's Anthropic face sends
			// zeros in message_start and the full usage in the final
			// message_delta. Take any field the delta actually populates.
			if ev.Usage.OutputTokens > 0 {
				u.OutputTokens = ev.Usage.OutputTokens // cumulative
			}
			if ev.Usage.InputTokens > 0 {
				u.InputTokens = ev.Usage.InputTokens
			}
			if ev.Usage.CacheReadInputTokens > 0 {
				u.CacheReadTokens = ev.Usage.CacheReadInputTokens
			}
			if ev.Usage.CacheCreationInputTokens > 0 {
				u.CacheCreationTokens = ev.Usage.CacheCreationInputTokens
			}
		}
		// Reassemble the finished message (best-effort: an event that doesn't
		// decode into an SDK stream event, or won't accumulate, is skipped).
		var sdkEv anthropic.MessageStreamEventUnion
		if err := json.Unmarshal([]byte(data), &sdkEv); err == nil {
			if err := msg.Accumulate(sdkEv); err == nil {
				accumulated = true
			}
		}
	}
	// The SDK accumulator only merges output_tokens from message_delta, so a
	// stream that reports input/cache usage there (OpenRouter) would persist
	// zeros for them in the canonical message; sync those with what we
	// extracted (output_tokens the accumulator already handles).
	msg.Usage.InputTokens = u.InputTokens
	msg.Usage.CacheReadInputTokens = u.CacheReadTokens
	msg.Usage.CacheCreationInputTokens = u.CacheCreationTokens
	// Persist whatever the SDK accumulated — including a legitimately
	// content-less Message (max_tokens can stop a turn before the first
	// content block) — but only when SOMETHING accumulated. Nothing
	// accumulating means the stream wasn't Anthropic wire format at all
	// (a gateway error page with a success status, SDK/wire skew): that's a
	// failed parse, not a zero-usage completion — recording it would lie in
	// the metrics and crash the JSONB append on the raw bytes.
	if scErr := sc.Err(); scErr != nil {
		return stop, u, nil, scErr
	}
	if !accumulated {
		return stop, u, nil, fmt.Errorf("response stream did not reassemble into a Message (%d bytes captured)", len(body))
	}
	canonical, err := json.Marshal(msg)
	if err != nil {
		return stop, u, nil, fmt.Errorf("marshal reassembled message: %w", err)
	}
	return stop, u, canonical, nil
}

func toCapturedUsage(w wireUsage) CapturedUsage {
	return CapturedUsage{
		InputTokens:         w.InputTokens,
		OutputTokens:        w.OutputTokens,
		CacheReadTokens:     w.CacheReadInputTokens,
		CacheCreationTokens: w.CacheCreationInputTokens,
	}
}
