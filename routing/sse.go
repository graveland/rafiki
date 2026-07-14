package routing

import (
	"bufio"
	"bytes"
	"encoding/json"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
)

type CapturedUsage struct {
	InputTokens         int64
	OutputTokens        int64
	CacheReadTokens     int64
	CacheCreationTokens int64
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
// Best-effort on content: malformed input yields zero values and (for SSE)
// whatever Message could be accumulated. The returned error is non-nil only when
// the SSE scanner itself failed (e.g. a token exceeding the buffer on a
// truncated stream); callers should Warn on it but the values remain usable.
func ParseCapturedResponse(contentType string, body []byte) (string, CapturedUsage, []byte, error) {
	if strings.Contains(contentType, "text/event-stream") {
		return parseSSE(body)
	}
	stop, u := parseJSONMessage(body)
	return stop, u, body, nil
}

func parseJSONMessage(body []byte) (string, CapturedUsage) {
	var m struct {
		StopReason string    `json:"stop_reason"`
		Usage      wireUsage `json:"usage"`
	}
	if err := json.Unmarshal(body, &m); err != nil {
		return "", CapturedUsage{}
	}
	return m.StopReason, toCapturedUsage(m.Usage)
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
	// Persist the reassembled message only if accumulation actually produced
	// content; otherwise (SDK/wire skew, or deltas with no content_block_start)
	// a zero-value Message still marshals to a valid-but-empty {"content":null}
	// skeleton that would masquerade as a real empty completion — so fall back to
	// the raw stream, which at least preserves the response for forensics.
	canonical, err := json.Marshal(msg)
	if err != nil || !accumulated || len(msg.Content) == 0 {
		canonical = body
	}
	return stop, u, canonical, sc.Err()
}

func toCapturedUsage(w wireUsage) CapturedUsage {
	return CapturedUsage{
		InputTokens:         w.InputTokens,
		OutputTokens:        w.OutputTokens,
		CacheReadTokens:     w.CacheReadInputTokens,
		CacheCreationTokens: w.CacheCreationInputTokens,
	}
}
