// SPDX-License-Identifier: Apache-2.0

// Package eventconv converts persisted conversation state into the
// rafiki-native event vocabulary. It is deliberately pure: no database, no
// network, no clock, so every conversion rule is testable without a DSN.
package eventconv

import (
	"encoding/json"

	"github.com/anthropics/anthropic-sdk-go"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
)

// BlocksFromParam converts one stored message's content blocks into proto
// blocks, assigning each a rafiki-local index. The index is positional within
// this message and is not the provider's own numbering.
func BlocksFromParam(p anthropic.MessageParam) []*rafikiv1.ContentBlock {
	out := make([]*rafikiv1.ContentBlock, 0, len(p.Content))
	for i, blk := range p.Content {
		cb := &rafikiv1.ContentBlock{Index: int32(i)}
		switch {
		case blk.OfText != nil:
			cb.Block = &rafikiv1.ContentBlock_Text{
				Text: &rafikiv1.TextBlock{Text: blk.OfText.Text},
			}
		case blk.OfThinking != nil:
			cb.Block = &rafikiv1.ContentBlock_Thinking{
				Thinking: &rafikiv1.ThinkingBlock{
					Thinking:  blk.OfThinking.Thinking,
					Signature: blk.OfThinking.Signature,
				},
			}
		case blk.OfToolUse != nil:
			cb.Block = &rafikiv1.ContentBlock_ToolUse{
				ToolUse: &rafikiv1.ToolUseBlock{
					Id:        blk.OfToolUse.ID,
					Name:      blk.OfToolUse.Name,
					InputJson: rawJSON(blk.OfToolUse.Input),
				},
			}
		case blk.OfToolResult != nil:
			cb.Block = &rafikiv1.ContentBlock_ToolResult{
				ToolResult: toolResult(blk.OfToolResult),
			}
		default:
			continue
		}
		out = append(out, cb)
	}
	return out
}

func toolResult(tr *anthropic.ToolResultBlockParam) *rafikiv1.ToolResultBlock {
	out := &rafikiv1.ToolResultBlock{
		ToolUseId: tr.ToolUseID,
		IsError:   tr.IsError.Or(false),
	}
	for i, c := range tr.Content {
		if c.OfText == nil {
			continue
		}
		out.Content = append(out.Content, &rafikiv1.ContentBlock{
			Index: int32(i),
			Block: &rafikiv1.ContentBlock_Text{
				Text: &rafikiv1.TextBlock{Text: c.OfText.Text},
			},
		})
	}
	return out
}

// rawJSON renders a tool's arguments back to their JSON text. Tool inputs stay
// opaque strings on the wire by design (spec §3.3): the shape is whatever the
// model produced for whatever tool.
func rawJSON(v any) string {
	if v == nil {
		return ""
	}
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(b)
}

// StopReasonFromString maps a provider's own stop-reason string onto rafiki's
// normalized enum. Anthropic and OpenAI spell the same outcomes differently;
// normalizing once here means no consumer has to know which provider answered.
// An unrecognized value maps to UNSPECIFIED and the caller keeps the raw text.
func StopReasonFromString(s string) rafikiv1.StopReason {
	switch s {
	case "end_turn", "stop":
		return rafikiv1.StopReason_STOP_REASON_END_TURN
	case "max_tokens", "length":
		return rafikiv1.StopReason_STOP_REASON_MAX_TOKENS
	case "tool_use", "tool_calls":
		return rafikiv1.StopReason_STOP_REASON_TOOL_USE
	case "stop_sequence":
		return rafikiv1.StopReason_STOP_REASON_STOP_SEQUENCE
	case "refusal", "content_filter":
		return rafikiv1.StopReason_STOP_REASON_REFUSAL
	default:
		return rafikiv1.StopReason_STOP_REASON_UNSPECIFIED
	}
}
