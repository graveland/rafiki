// SPDX-License-Identifier: Apache-2.0

package fundi

import (
	"go.graveland.dev/rafiki/pkg/conv"
	"go.graveland.dev/rafiki/pkg/msg"
)

// nodeText returns the text of n's first text block, or "" when it has none.
func nodeText(n *conv.Node) string { return firstText(n.Msg.Content) }

// firstText returns the text of the first text block, or "" when there is none.
func firstText(blocks []msg.Block) string {
	for _, b := range blocks {
		if text, ok := b.(msg.Text); ok {
			return text.Text
		}
	}
	return ""
}

// nodeToolUse returns n's first tool_use block.
func nodeToolUse(n *conv.Node) (msg.ToolUse, bool) {
	for _, b := range n.Msg.Content {
		if tu, ok := b.(msg.ToolUse); ok {
			return tu, true
		}
	}
	return msg.ToolUse{}, false
}

// nodeToolResult returns n's first tool_result block.
func nodeToolResult(n *conv.Node) (msg.ToolResult, bool) {
	for _, b := range n.Msg.Content {
		if tr, ok := b.(msg.ToolResult); ok {
			return tr, true
		}
	}
	return msg.ToolResult{}, false
}
