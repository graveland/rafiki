// SPDX-License-Identifier: Apache-2.0

package llm

import (
	"reflect"
	"slices"

	convpkg "go.graveland.dev/rafiki/pkg/conv"
	"go.graveland.dev/rafiki/pkg/msg"
)

// mergeForRequestRows is mergeForRequest plus rowStart[i], the index in
// history of the first stored row merged into msgs[i].
//
// rowStart is measured in PATH POSITIONS, which is what a turn's trim range is
// measured in: the end of the anchor message and the start of a kept suffix are
// both directly rowStart lookups, even when a merged run shifted the later
// messages off their row indices.
//
// The merge itself also replaces tool_use blocks with empty names with
// explanatory text blocks — a model hallucination that, if forwarded as-is,
// causes a 400 rejection ("tool_calls[0].function.name must be a non-empty
// string"). The matching tool_result blocks (is_error feedback from the agent
// loop) are kept so the model sees the error and can self-correct.
//
// A System node is dropped: it is harness-injected history (a Claude Code
// notice, an environment update) that no provider request can carry as a turn,
// and dropping it keeps the turn alive instead of failing on a row the model
// never wrote.
func mergeForRequestRows(history []*convpkg.Node) (msgs []msg.Message, rowStart []int, err error) {
	out := make([]msg.Message, 0, len(history))
	start := make([]int, 0, len(history))
	for i, n := range history {
		if n.Msg.Role == msg.System {
			continue
		}
		m := msg.Message{Role: n.Msg.Role, Content: replaceEmptyToolUses(n.Msg)}
		if m.Role == msg.User && len(out) > 0 && out[len(out)-1].Role == msg.User {
			last := &out[len(out)-1]
			// Clone before appending: appending into a stored message's backing
			// array would mutate history the caller may still hold.
			last.Content = append(slices.Clone(last.Content), m.Content...)
			continue
		}
		start = append(start, i)
		out = append(out, m)
	}
	return out, start, nil
}

// trimDropped reports where a trim cut. It recognises only the anchor-plus-
// suffix shape defaultTrimPolicy produces: trimmed[0] is orig[0] and
// trimmed[1:] is the last len(trimmed)-1 messages of orig. For any other
// shape ok is false and the caller records no range.
//
// to is the history position of the first kept suffix message, and keptRowStart
// is rowStart for trimmed (the anchor plus the kept suffix). k == 1 is the
// no-op trim — the suffix would be all of orig[1:] — so it is refused with the
// same shape rules: a policy that dropped nothing has no range to record.
func trimDropped(orig, trimmed []msg.Message, rowStart []int) (to int, keptRowStart []int, ok bool) {
	n := len(trimmed) - 1
	k := len(orig) - n
	if n < 1 || k < 2 || len(rowStart) != len(orig) {
		return 0, nil, false
	}
	if !reflect.DeepEqual(trimmed[0], orig[0]) || !reflect.DeepEqual(trimmed[1:], orig[k:]) {
		return 0, nil, false
	}
	kept := make([]int, 0, 1+len(orig)-k)
	kept = append(kept, rowStart[0])
	kept = append(kept, rowStart[k:]...)
	return rowStart[k], kept, true
}

// requestHeadID names the stored row a request's history ends at — the turn's
// head. A store-less conversation's node ids are local-only (its backend is
// in-process), so they must not reach the uuid column.
func (conv *Conversation) requestHeadID(history []*convpkg.Node) string {
	if conv.client.capture == nil || len(history) == 0 {
		return ""
	}
	return history[len(history)-1].ID
}
