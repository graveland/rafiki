// SPDX-License-Identifier: Apache-2.0

// Package eventconv converts persisted conversation state into the
// rafiki-native event vocabulary. It is deliberately pure: no database, no
// network, no clock, so every conversion rule is testable without a DSN.
package eventconv

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"

	"github.com/anthropics/anthropic-sdk-go"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/msg"
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
		case blk.OfImage != nil:
			cb.Block = &rafikiv1.ContentBlock_Image{Image: imageBlock(blk.OfImage)}
		default:
			continue
		}
		out = append(out, cb)
	}
	return out
}

// BlocksFromMsg converts a native message's content blocks into proto blocks,
// assigning each a rafiki-local index. The index is positional over the blocks
// actually emitted and is not the provider's own numbering.
func BlocksFromMsg(m msg.Message) []*rafikiv1.ContentBlock {
	out := make([]*rafikiv1.ContentBlock, 0, len(m.Content))
	for _, b := range m.Content {
		cb, ok := protoBlock(b)
		if !ok {
			continue
		}
		cb.Index = int32(len(out))
		out = append(out, cb)
	}
	return out
}

// protoBlock converts one native block into its proto form, reporting false for
// a kind the proto vocabulary cannot name. An opaque block is one: it is kept
// for the dialect that minted it, and its proto form would be a guess.
func protoBlock(b msg.Block) (*rafikiv1.ContentBlock, bool) {
	switch t := b.(type) {
	case msg.Text:
		return &rafikiv1.ContentBlock{Block: &rafikiv1.ContentBlock_Text{
			Text: &rafikiv1.TextBlock{Text: t.Text},
		}}, true
	case msg.Thinking:
		return &rafikiv1.ContentBlock{Block: &rafikiv1.ContentBlock_Thinking{
			Thinking: &rafikiv1.ThinkingBlock{Thinking: t.Text, Signature: t.Sig.Value},
		}}, true
	case msg.ToolUse:
		return &rafikiv1.ContentBlock{Block: &rafikiv1.ContentBlock_ToolUse{
			ToolUse: &rafikiv1.ToolUseBlock{
				Id:        t.ID,
				Name:      t.Name,
				InputJson: inputJSON(t.Input),
			},
		}}, true
	case msg.ToolResult:
		return &rafikiv1.ContentBlock{Block: &rafikiv1.ContentBlock_ToolResult{
			ToolResult: &rafikiv1.ToolResultBlock{
				ToolUseId: t.ToolUseID,
				IsError:   t.IsError,
				Content:   toolResultContent(t.Content),
			},
		}}, true
	case msg.Image:
		return &rafikiv1.ContentBlock{Block: &rafikiv1.ContentBlock_Image{
			Image: &rafikiv1.ImageBlock{MediaType: t.MediaType, Data: t.Data},
		}}, true
	case msg.Opaque:
		return nil, false
	default:
		panic(fmt.Sprintf("eventconv: unmapped block %T", b))
	}
}

// imageBlock converts a stored image into its proto form. Only a base64
// source carries bytes; a URL source, or base64 that does not decode, still
// yields a block, so the image is named downstream rather than dropped.
func imageBlock(img *anthropic.ImageBlockParam) *rafikiv1.ImageBlock {
	out := &rafikiv1.ImageBlock{}
	if src := img.Source.OfBase64; src != nil {
		out.MediaType = string(src.MediaType)
		if data, err := base64.StdEncoding.DecodeString(src.Data); err == nil {
			out.Data = data
		}
	}
	return out
}

func toolResult(tr *anthropic.ToolResultBlockParam) *rafikiv1.ToolResultBlock {
	out := &rafikiv1.ToolResultBlock{
		ToolUseId: tr.ToolUseID,
		IsError:   tr.IsError.Or(false),
	}
	for i, c := range tr.Content {
		cb := &rafikiv1.ContentBlock{Index: int32(i)}
		switch {
		case c.OfText != nil:
			cb.Block = &rafikiv1.ContentBlock_Text{
				Text: &rafikiv1.TextBlock{Text: c.OfText.Text},
			}
		case c.OfImage != nil:
			cb.Block = &rafikiv1.ContentBlock_Image{Image: imageBlock(c.OfImage)}
		default:
			continue
		}
		out.Content = append(out.Content, cb)
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

// toolResultContent maps a tool result's nested blocks. Only text and image have
// a proto form inside a tool result, matching the SDK route: any other kind is
// skipped rather than failing the whole result.
func toolResultContent(blocks []msg.Block) []*rafikiv1.ContentBlock {
	out := make([]*rafikiv1.ContentBlock, 0, len(blocks))
	for _, b := range blocks {
		switch t := b.(type) {
		case msg.Text:
			out = append(out, &rafikiv1.ContentBlock{
				Index: int32(len(out)),
				Block: &rafikiv1.ContentBlock_Text{Text: &rafikiv1.TextBlock{Text: t.Text}},
			})
		case msg.Image:
			out = append(out, &rafikiv1.ContentBlock{
				Index: int32(len(out)),
				Block: &rafikiv1.ContentBlock_Image{
					Image: &rafikiv1.ImageBlock{MediaType: t.MediaType, Data: t.Data},
				},
			})
		}
	}
	return out
}

// inputJSON renders a tool use's arguments as compact JSON text. An absent input
// yields "", and an input that is not valid JSON is passed through verbatim so a
// malformed call still names its arguments.
func inputJSON(raw json.RawMessage) string {
	if len(bytes.TrimSpace(raw)) == 0 {
		return ""
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return string(raw)
	}
	return buf.String()
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
