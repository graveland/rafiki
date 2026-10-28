// SPDX-License-Identifier: Apache-2.0

package llm

import (
	"reflect"
	"slices"

	"github.com/anthropics/anthropic-sdk-go"

	"go.graveland.dev/rafiki/pkg/store"
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
func mergeForRequestRows(history []store.Message) (msgs []Message, rowStart []int) {
	out := make([]Message, 0, len(history))
	start := make([]int, 0, len(history))
	for i, m := range history {
		m.Param.Content = replaceEmptyToolUses(m.Param)
		if m.Param.Role == anthropic.MessageParamRoleUser && len(out) > 0 &&
			out[len(out)-1].Role == anthropic.MessageParamRoleUser {
			last := &out[len(out)-1]
			// Clone before appending: appending into a stored param's backing
			// array would mutate history the caller may still hold.
			last.Content = append(slices.Clone(last.Content), m.Param.Content...)
			continue
		}
		start = append(start, i)
		out = append(out, m.Param)
	}
	return out, start
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
func trimDropped(orig, trimmed []Message, rowStart []int) (to int, keptRowStart []int, ok bool) {
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
// head. An in-memory conversation's row ids are synthetic (random hex, never
// conversation_message nodes), so they must not reach the uuid column.
func (conv *Conversation) requestHeadID(history []store.Message) string {
	if conv.client.messages == nil || len(history) == 0 {
		return ""
	}
	return history[len(history)-1].ID
}
