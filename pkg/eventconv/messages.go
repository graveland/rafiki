// SPDX-License-Identifier: Apache-2.0

package eventconv

import (
	"google.golang.org/protobuf/proto"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/store"
)

// EventsFromMessages converts a conversation's persisted messages into
// durable-tier events, one per message, carrying the stored ordinal. The
// ordinal is the resumption cursor: it already exists as the
// (conversation_id, ordinal) append-idempotency key, so nothing new is
// invented here.
func EventsFromMessages(childID string, msgs []store.Message) []*rafikiv1.Event {
	out := make([]*rafikiv1.Event, 0, len(msgs))
	for _, m := range msgs {
		ev := &rafikiv1.Event{
			ChildId: childID,
			Ordinal: proto.Int32(int32(m.Ordinal)),
		}
		blocks := BlocksFromParam(m.Param)
		if m.Param.Role == "assistant" {
			ev.Payload = &rafikiv1.Event_AssistantMessage{
				AssistantMessage: &rafikiv1.AssistantMessage{
					Content:       blocks,
					StopReason:    StopReasonFromString(m.StopReason),
					RawStopReason: m.StopReason,
				},
			}
		} else {
			ev.Payload = &rafikiv1.Event_UserMessage{
				UserMessage: &rafikiv1.UserMessage{Content: blocks},
			}
		}
		out = append(out, ev)
	}
	return out
}
