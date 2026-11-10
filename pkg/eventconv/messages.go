// SPDX-License-Identifier: Apache-2.0

package eventconv

import (
	"slices"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"go.graveland.dev/rafiki/pkg/conv"
	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/harness"
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
			// output_tokens on a summary row is the size of the working set the
			// compaction left behind — the same number the live CompactionEnd
			// event reports — and it is what a continuation from this boundary
			// will borrow. It is absent on a conversation compacted before it was
			// stored, and post_tokens is optional so absent stays distinguishable
			// from a zero.
			if n.OutputTokens != nil {
				post := int32(*n.OutputTokens)
				cb.PostTokens = &post
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
				UserMessage: UserMessageFrom(n.Origin, n.Msg),
			}
		}
		out = append(out, ev)
	}
	return out
}

// UserMessageFrom builds the wire message for one user-role row: the user's own
// blocks in content, the harness-injected ones in injected, wrapper removed and
// kind recorded.
//
// It is the ONE split. A live turn (the fundi emitter) and a history read
// (EventsFromNodes) both call it, because they used to disagree: the live path
// published a daemon-injected nudge as ordinary user text and the same row
// replayed from history as injected, so the cockpit's transcript changed
// underneath the reader.
//
// The harness that wrote the message is the only code that knows which of its
// blocks the user did not type. Marking returns a copy (a node's Msg is shared
// by the cache), and BlocksFromMsg keeps each block's index in the stored
// message, so the split is recoverable without renumbering or compacting
// anything.
//
// A message the user sent mid-turn, which Claude Code relayed inside a tool
// result, is split out as the user's own content text, carrying the tool
// result's index.
func UserMessageFrom(origin conv.Origin, m msg.Message) *rafikiv1.UserMessage {
	h := harness.For(origin)
	m, relays := h.SplitRelays(m)
	marked := h.Mark(m)
	var content, injected []*rafikiv1.ContentBlock
	for _, cb := range BlocksFromMsg(marked) {
		if t, ok := marked.Content[int(cb.Index)].(msg.Text); ok && t.Injected {
			cb.GetText().Text = h.Unwrap(t)
			cb.InjectedKind = t.InjectedKind
			injected = append(injected, cb)
			continue
		}
		content = append(content, cb)
	}
	for _, r := range relays {
		cb := &rafikiv1.ContentBlock{
			Index: int32(r.Index),
			Block: &rafikiv1.ContentBlock_Text{Text: &rafikiv1.TextBlock{Text: r.Text}},
		}
		at := len(content)
		for i, c := range content {
			if c.Index == cb.Index {
				at = i + 1
				break
			}
		}
		content = slices.Insert(content, at, cb)
	}
	return &rafikiv1.UserMessage{Content: content, Injected: injected}
}
