// SPDX-License-Identifier: Apache-2.0

package fundi

import (
	"time"

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
