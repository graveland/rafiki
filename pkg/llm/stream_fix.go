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
