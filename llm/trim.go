package llm

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
)

// Message is a conversation message in request form. Alias of the SDK param
// type: stored rows round-trip through it byte-identically.
type Message = anthropic.MessageParam

// TrimPolicy shrinks the message history when the API rejects a request as
// too large. It receives ONLY the message history — never system, never
// tools, which are immutable for the life of a conversation (the library
// enforces this structurally so no policy can invalidate the cached
// tools+system prefix). Trim is request-assembly-time only: stored
// conversation_message rows are never mutated.
//
// Trim returns the reduced history, or ok=false when it cannot shrink
// further (Send then surfaces the too-large error to the caller). attempt is
// 0-based and increments per retry.
type TrimPolicy interface {
	Trim(msgs []Message, attempt int) (trimmed []Message, ok bool)
}

// Default trim budgets: ported from sc's diagnose loop (maxConversationChars
// with per-attempt halving — 300KB, then 150KB, then 75KB).
const defaultTrimBudget = 300 * 1024

// defaultTrimPolicy keeps the first message (the anchor: initial prompt /
// pre-collected context) and as many of the most recent messages as fit the
// halving byte budget, dropping from the middle.
type defaultTrimPolicy struct{}

func (defaultTrimPolicy) Trim(msgs []Message, attempt int) ([]Message, bool) {
	if len(msgs) <= 2 {
		return msgs, false
	}
	budget := defaultTrimBudget >> attempt
	if budget <= 0 {
		return msgs, false
	}

	total := messageSize(msgs[0])
	kept := []Message{}
	// Walk the tail backwards, keeping the most recent messages that fit.
	for i := len(msgs) - 1; i >= 1; i-- {
		size := messageSize(msgs[i])
		if total+size > budget && len(kept) > 0 {
			break
		}
		kept = append(kept, msgs[i])
		total += size
	}
	// Reverse kept into chronological order after msgs[0].
	out := make([]Message, 0, 1+len(kept))
	out = append(out, msgs[0])
	for i := len(kept) - 1; i >= 0; i-- {
		out = append(out, kept[i])
	}
	if len(out) >= len(msgs) {
		return msgs, false // nothing was dropped; retrying identically is pointless
	}
	return out, true
}

// messageSize estimates a message's request footprint by marshaled length.
func messageSize(m Message) int {
	b, err := json.Marshal(m)
	if err != nil {
		return 0
	}
	return len(b)
}

// IsPromptTooLarge reports whether err is the API's prompt-size rejection.
// Exported for hosts with domain-specific shrinking beyond the TrimPolicy
// (e.g. diagnose's priority-ordered dropping of pre-collected data from the
// first message, which the library structurally never trims).
func IsPromptTooLarge(err error) bool { return isPromptTooLarge(err) }

// isPromptTooLarge reports whether err is the API's prompt-size rejection
// (HTTP 400 whose body says the prompt is too long) — the trigger for
// TrimPolicy trim-and-retry. Ported from sc's diagnose loop.
func isPromptTooLarge(err error) bool {
	var apiErr *anthropic.Error
	if !errors.As(err, &apiErr) {
		return false
	}
	if apiErr.StatusCode != http.StatusBadRequest {
		return false
	}
	return strings.Contains(strings.ToLower(apiErr.RawJSON()), "prompt is too long")
}
