// SPDX-License-Identifier: Apache-2.0

package fundi

import (
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"google.golang.org/protobuf/proto"

	"go.graveland.dev/rafiki/pkg/eventconv"
	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
)

// NativeSink receives fundi's rafiki-native events. It is optional: an Emitter
// with no sink behaves exactly as before, which is what lets this land without
// touching any existing caller.
//
// Publish must not block — it is called on the turn's goroutine.
type NativeSink interface {
	Publish(ev *rafikiv1.Event)
}

// SetNativeSink attaches a sink. Safe to leave unset.
func (e *Emitter) SetNativeSink(s NativeSink) { e.native = s }

// publishAssistant publishes the turn's assistant message as a durable native
// event, and is called from BOTH the streamed and non-streamed paths.
//
// It matters most on the streamed one: content_block_delta is ephemeral and
// never written to the log, so a streamed turn that published nothing durable
// would replay as a user prompt with no answer — which is exactly how the
// event log came to hold child_spawned, user_message, the tool events and
// nothing the model ever said.
//
// The content blocks are read from resp's FLATTENED FIELDS rather than through
// resp.ToParam(), for the reason MapAssistantMessage documents at length:
// ToParam goes through AsAny(), which unmarshals ContentBlockUnion.JSON.raw,
// and Message.Accumulate only rewrites that raw JSON on content_block_stop.
// A mid-stream accumulated message therefore yields EMPTY blocks through
// ToParam while the struct fields are already correct. Both current callers
// pass a complete response, so ToParam would work today — and would silently
// start dropping content the first time anyone published from a partial one.
func (e *Emitter) publishAssistant(resp *anthropic.Message) {
	if e.native == nil {
		return
	}
	blocks := make([]*rafikiv1.ContentBlock, 0, len(resp.Content))
	for _, b := range resp.Content {
		cb := &rafikiv1.ContentBlock{Index: int32(len(blocks))}
		switch b.Type {
		case "text":
			if b.Text == "" {
				continue
			}
			cb.Block = &rafikiv1.ContentBlock_Text{Text: &rafikiv1.TextBlock{Text: b.Text}}
		case "thinking":
			if b.Thinking == "" {
				continue
			}
			cb.Block = &rafikiv1.ContentBlock_Thinking{
				Thinking: &rafikiv1.ThinkingBlock{Thinking: b.Thinking, Signature: b.Signature},
			}
		case "tool_use":
			cb.Block = &rafikiv1.ContentBlock_ToolUse{ToolUse: &rafikiv1.ToolUseBlock{
				Id: b.ID, Name: b.Name, InputJson: string(b.Input),
			}}
		default:
			continue
		}
		blocks = append(blocks, cb)
	}
	e.lastStop = string(resp.StopReason)
	e.publishNative(&rafikiv1.AssistantMessage{
		Content:       blocks,
		StopReason:    eventconv.StopReasonFromString(e.lastStop),
		RawStopReason: e.lastStop,
	})
}

// publishTurnEnd closes the turn for durable consumers. The TUI does not need
// it -- an assistant message already finalizes its block -- but a turn
// boundary is one of the things the event plane exists to give a non-TUI
// consumer, and the usage rides along so a cost consumer needs no second
// source. Every field of Usage is optional in the proto because a reported
// zero and an unreported count are different facts; these are all reported.
func (e *Emitter) publishTurnEnd() {
	u := e.usage
	e.publishNative(&rafikiv1.TurnEnd{
		StopReason:    eventconv.StopReasonFromString(e.lastStop),
		RawStopReason: e.lastStop,
		Usage: &rafikiv1.Usage{
			InputTokens:      proto.Int64(int64(u.Input)),
			OutputTokens:     proto.Int64(int64(u.Output)),
			CacheReadTokens:  proto.Int64(int64(u.CacheRead)),
			CacheWriteTokens: proto.Int64(int64(u.CacheWrite)),
		},
		CostUsd: proto.Float64(u.Cost.Total),
	})
}

func (e *Emitter) publishNative(payload any) {
	if e.native == nil {
		return
	}
	ev := &rafikiv1.Event{TsUnixMs: time.Now().UnixMilli()}
	switch p := payload.(type) {
	case *rafikiv1.UserMessage:
		ev.Payload = &rafikiv1.Event_UserMessage{UserMessage: p}
	case *rafikiv1.AssistantMessage:
		ev.Payload = &rafikiv1.Event_AssistantMessage{AssistantMessage: p}
	case *rafikiv1.TurnEnd:
		ev.Payload = &rafikiv1.Event_TurnEnd{TurnEnd: p}
	case *rafikiv1.ContentBlockDelta:
		ev.Payload = &rafikiv1.Event_ContentBlockDelta{ContentBlockDelta: p}
	case *rafikiv1.ToolExecutionStart:
		ev.Payload = &rafikiv1.Event_ToolExecutionStart{ToolExecutionStart: p}
	case *rafikiv1.ToolExecutionEnd:
		ev.Payload = &rafikiv1.Event_ToolExecutionEnd{ToolExecutionEnd: p}
	default:
		return
	}
	e.native.Publish(ev)
}

func nativeText(s string) []*rafikiv1.ContentBlock {
	return []*rafikiv1.ContentBlock{{
		Index: 0,
		Block: &rafikiv1.ContentBlock_Text{Text: &rafikiv1.TextBlock{Text: s}},
	}}
}
