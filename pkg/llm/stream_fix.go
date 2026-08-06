// SPDX-License-Identifier: Apache-2.0

package llm

import (
	"encoding/json"
	"log/slog"

	"github.com/anthropics/anthropic-sdk-go"
)

// FixEmptyToolInput patches a content_block_start event whose content block is
// a tool_use or server_tool_use with an empty-string "input" field. Non-Anthropic
// providers (OpenRouter, etc.) sometimes emit "input":"" rather than "input":{},
// and json.RawMessage("") fails to marshal during content_block_stop accumulation
// with "unexpected end of JSON input".
//
// However, the SDK's Accumulate (messageutil.go line 34) reconstructs content
// blocks from ContentBlock.RawJSON() — the original stream bytes — completely
// ignoring the typed Input field this function mutates. That's why we also need
// FixAccumulatedEmptyToolInput, which patches the ContentBlockUnion.Input after
// Accumulate has already run.
func FixEmptyToolInput(ev *anthropic.MessageStreamEventUnion) {
	if ev.Type != "content_block_start" {
		return
	}
	if ev.ContentBlock.Type != "tool_use" && ev.ContentBlock.Type != "server_tool_use" {
		return
	}
	if s, ok := ev.ContentBlock.Input.(string); ok && s == "" {
		ev.ContentBlock.Input = map[string]any{}
	}
}

// FixAccumulatedEmptyToolInput patches the last content block in the accumulated
// Message after a content_block_start for a tool_use or server_tool_use. It
// undoes the damage from a provider emitting "input":"" (an empty JSON string)
// instead of "input":{} (an empty JSON object). Returns true if a fix was applied.
//
// Must be called after Message.Accumulate returns for every event, and only on
// success (a nil return from Accumulate).
func FixAccumulatedEmptyToolInput(acc *anthropic.Message, ev anthropic.MessageStreamEventUnion) bool {
	if ev.Type != "content_block_start" {
		return false
	}
	if ev.ContentBlock.Type != "tool_use" && ev.ContentBlock.Type != "server_tool_use" {
		return false
	}
	if len(acc.Content) == 0 {
		return false
	}
	cb := &acc.Content[len(acc.Content)-1]
	// json.RawMessage of an empty JSON string is the two-byte sequence `""`.
	// Replace it with `{}` so the SDK's InputJSONDelta handler (messageutil.go
	// line 48) finds `"{}"` and replaces rather than appends.
	if len(cb.Input) == 2 && cb.Input[0] == '"' && cb.Input[1] == '"' {
		cb.Input = []byte{'{', '}'}
		return true
	}
	return false
}

// SanitizeInvalidAccumulatedInput is a backstop for content_block.Input
// corruption that FixEmptyToolInput/FixAccumulatedEmptyToolInput don't catch.
// Both of those target one specific provider quirk (literal "input":"" from
// content_block_start); this instead enforces the invariant the SDK actually
// needs — Input must be nil or valid JSON — right before the two Accumulate
// event types that marshal it (messageutil.go: ContentBlockStopEvent marshals
// the last block, MessageStopEvent marshals the whole message). A non-nil,
// invalid Input at that point means some upstream step (a non-Anthropic
// provider's streaming quirk, an InputJSONDelta append racing a corrupt seed
// value, etc.) left the block's JSON incomplete or malformed; there's no safe
// way to salvage partial tool-call arguments, so this drops them to "{}"
// rather than let Message.Accumulate fail the whole turn on a
// json.Marshal/json.RawMessage error ("unexpected end of JSON input").
//
// Must be called BEFORE acc.Accumulate(ev) — by the time Accumulate returns
// an error from the marshal, the turn has already failed and there's nothing
// left to patch.
func SanitizeInvalidAccumulatedInput(acc *anthropic.Message, ev anthropic.MessageStreamEventUnion) {
	if ev.Type != "content_block_stop" && ev.Type != "message_stop" {
		return
	}
	for i := range acc.Content {
		cb := &acc.Content[i]
		if cb.Input == nil || json.Valid(cb.Input) {
			continue
		}
		slog.Warn("llm: dropping invalid accumulated tool input", "index", i, "input", string(cb.Input))
		cb.Input = []byte("{}")
	}
}
