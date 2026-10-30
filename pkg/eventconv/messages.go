// SPDX-License-Identifier: Apache-2.0

package eventconv

import (
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"go.graveland.dev/rafiki/pkg/conv"
	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/msg"
)

// EventsFromNodes converts a conversation's transcript nodes into durable-tier
// events, one per node, carrying the stored ordinal. The ordinal is the
// resumption cursor: it already exists as the (conversation_id, ordinal)
// append-idempotency key, so nothing new is invented here. Each event also
// carries the node's created_at as its ts, so a client can page by time; a node
// with no recorded created_at leaves ts unset rather than stamping the zero
// time.
//
// A node is already decoded, so content comes from BlocksFromMsg, which renders
// an opaque-by-type block a raw-row reader could not name.
func EventsFromNodes(childID string, nodes []*conv.Node) []*rafikiv1.Event {
	out := make([]*rafikiv1.Event, 0, len(nodes))
	for _, n := range nodes {
		if n.Kind == conv.KindCompactionTail {
			continue
		}
		ev := &rafikiv1.Event{
			ChildId: childID,
			Ordinal: proto.Int32(int32(n.Ordinal)),
		}
		if !n.CreatedAt.IsZero() {
			ev.Ts = timestamppb.New(n.CreatedAt)
		}
		if n.Kind == conv.KindCompactionSummary {
			cb := &rafikiv1.CompactionBoundary{}
			if n.InputTokens != nil {
				pre := int32(*n.InputTokens)
				cb.PreTokens = &pre
			}
			ev.Payload = &rafikiv1.Event_CompactionBoundary{CompactionBoundary: cb}
			out = append(out, ev)
			continue
		}
		if conv.IsClearBoundary(n) {
			// A fundi /clear's synthetic boundary row renders as the same
			// divider the live stream published; a claude clear row is a real
			// message and falls through.
			ev.Payload = &rafikiv1.Event_CompactionBoundary{CompactionBoundary: &rafikiv1.CompactionBoundary{Trigger: "clear"}}
			out = append(out, ev)
			continue
		}
		blocks := BlocksFromMsg(n.Msg)
		switch n.Msg.Role {
		case msg.Assistant:
			ev.Payload = &rafikiv1.Event_AssistantMessage{
				AssistantMessage: &rafikiv1.AssistantMessage{
					Content:       blocks,
					StopReason:    StopReasonFromString(n.StopReason),
					RawStopReason: n.StopReason,
				},
			}
		case msg.System:
			ev.Payload = &rafikiv1.Event_SystemMessage{
				SystemMessage: &rafikiv1.SystemMessage{Content: blocks},
			}
		default:
			ev.Payload = &rafikiv1.Event_UserMessage{
				UserMessage: &rafikiv1.UserMessage{Content: blocks},
			}
		}
		out = append(out, ev)
	}
	return out
}
