// SPDX-License-Identifier: Apache-2.0

package llm

import (
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
