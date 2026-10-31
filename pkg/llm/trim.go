// SPDX-License-Identifier: Apache-2.0

package llm

import (
	"errors"

	"go.graveland.dev/rafiki/pkg/msg"
)

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
	Trim(msgs []msg.Message, attempt int) (trimmed []msg.Message, ok bool)
}

// Default trim budgets: ported from sc's diagnose loop (maxConversationChars
// with per-attempt halving — 300KB, then 150KB, then 75KB).
const defaultTrimBudget = 300 * 1024

// defaultTrimPolicy keeps the first message (the anchor: initial prompt /
// pre-collected context) and as many of the most recent messages as fit the
// halving byte budget, dropping from the middle.
type defaultTrimPolicy struct{}

func (defaultTrimPolicy) Trim(msgs []msg.Message, attempt int) ([]msg.Message, bool) {
	if len(msgs) <= 2 {
		return msgs, false
	}
	budget := defaultTrimBudget >> attempt
	if budget <= 0 {
		return msgs, false
	}

	total := messageSize(msgs[0])
	kept := []msg.Message{}
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
	out := make([]msg.Message, 0, 1+len(kept))
	out = append(out, msgs[0])
	for i := len(kept) - 1; i >= 0; i-- {
		out = append(out, kept[i])
	}
	if len(out) >= len(msgs) {
		return msgs, false // nothing was dropped; retrying identically is pointless
	}
	return out, true
}

// blockOverhead is the per-block byte allowance messageSize adds for the
// envelope around a block's payload (its type, braces and separators).
const blockOverhead = 32

// messageSize estimates a message's request footprint from its native content:
// the text, tool-input and raw lengths it carries, plus a fixed allowance per
// block and per image.
func messageSize(m msg.Message) int {
	size := len(m.Role.String()) + blockOverhead
	for _, b := range m.Content {
		size += blockSize(b)
	}
	return size
}

// blockSize is one content block's estimated footprint.
func blockSize(b msg.Block) int {
	switch t := b.(type) {
	case msg.Text:
		return len(t.Text) + len(t.Ext.Raw) + blockOverhead
	case msg.Thinking:
		return len(t.Text) + len(t.Sig.Value) + len(t.Ext.Raw) + blockOverhead
	case msg.ToolUse:
		return len(t.Name) + len(t.Input) + len(t.Ext.Raw) + blockOverhead
	case msg.ToolResult:
		size := len(t.ToolUseID) + len(t.Ext.Raw) + blockOverhead
		for _, c := range t.Content {
			size += blockSize(c)
		}
		return size
	case msg.Image:
		return 1000 + len(t.Ext.Raw)
	case msg.Opaque:
		return len(t.Raw) + blockOverhead
	}
	return blockOverhead
}

// IsPromptTooLarge reports whether err is the API's prompt-size rejection.
// Exported for hosts with domain-specific shrinking beyond the TrimPolicy
// (e.g. diagnose's priority-ordered dropping of pre-collected data from the
// first message, which the library structurally never trims).
func IsPromptTooLarge(err error) bool { return isPromptTooLarge(err) }

// isPromptTooLarge reports whether err is the API's prompt-size rejection — the
// trigger for TrimPolicy trim-and-retry. The codec that decoded the refusal
// decides the kind, so every dialect's spelling of "this prompt does not fit"
// reaches the same predicate.
func isPromptTooLarge(err error) bool {
	var apiErr *msg.APIError
	if !errors.As(err, &apiErr) {
		return false
	}
	return apiErr.Kind == msg.ErrPromptTooLarge
}
