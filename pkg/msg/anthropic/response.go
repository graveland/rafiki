// SPDX-License-Identifier: Apache-2.0

package anthropic

import (
	"encoding/json"
	"errors"
	"fmt"

	"go.graveland.dev/rafiki/pkg/msg"
)

// ResponseJSON renders a native response as the body this package records: id,
// model, stop reason, the served-by provider when present, the usage counts the
// provider actually reported, and the content blocks. It is the canonical
// recorded form (capture's turn response and the raw trace), so it carries
// exactly the fields the native model has — never the SDK's own wire shape.
// The stop reason is the wire's own spelling when this package does not model
// it, so an unrecognised value is recorded rather than erased.
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
		"stop_reason": r.StopReasonWire(),
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

// usageBody renders the counts a native usage reports. A count the provider did
// not report is omitted: an absent count and a reported zero are different
// facts, and only the provider knows which it sent. Reasoning is the endpoint's
// own reasoning-token count, reported by the Responses dialect; it is stored
// under the same presence rule as the cache counts.
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
	if u.Reasoning != nil {
		body["reasoning_tokens"] = *u.Reasoning
	}
	return body
}
