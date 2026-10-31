// SPDX-License-Identifier: Apache-2.0

package llm

import (
	"reflect"
	"slices"

	convpkg "go.graveland.dev/rafiki/pkg/conv"
	"go.graveland.dev/rafiki/pkg/harness"
	"go.graveland.dev/rafiki/pkg/msg"
)

// borrowedThinkingPlaceholder stands in for a borrowed assistant message whose
// only content was reasoning: a message is never emptied.
const borrowedThinkingPlaceholder = "(borrowed reasoning omitted)"

// replayContext is one request's replay state: which conversation is asking,
// which tool names the request offers, and one replayer per foreign origin.
//
// It is built fresh per request and never shared: a replayer carries state
// between messages, so two requests sharing one would make the second's bytes
// depend on the first's history.
type replayContext struct {
	convID string
	has    func(name string) bool
	// replayers is created lazily, one per origin the history actually holds.
	replayers map[convpkg.Origin]harness.Replayer
}

// newReplayContext builds the replay state for a request from convID offering
// tools.
func newReplayContext(convID string, tools []msg.ToolDef) *replayContext {
	names := make(map[string]struct{}, len(tools))
	for _, t := range tools {
		names[t.Name] = struct{}{}
	}
	return &replayContext{
		convID:    convID,
		has:       func(name string) bool { _, ok := names[name]; return ok },
		replayers: make(map[convpkg.Origin]harness.Replayer, 1),
	}
}

// replayer returns the replayer for origin, creating it on first use. A nil
// context has none: nothing is replayed.
func (rc *replayContext) replayer(origin convpkg.Origin) harness.Replayer {
	if rc == nil {
		return nil
	}
	if r, ok := rc.replayers[origin]; ok {
		return r
	}
	r := harness.For(origin).Replayer(rc.has)
	rc.replayers[origin] = r
	return r
}

// forRequest is n's message as pkg/llm sends it. pkg/llm is the fundi
// harness: a node another harness wrote passes through that harness's replayer,
// which renames the tool calls this run can serve, renders the rest as text and
// drops what is false here. A borrowed assistant node's thinking is stripped
// too, because its signature is bound to the upstream that minted it. A nil rc
// replays nothing.
//
// It depends only on the node sequence and rc, so replayed history encodes to
// the same bytes every turn.
func forRequest(n *convpkg.Node, rc *replayContext) msg.Message {
	if n.Origin == convpkg.OriginUnclassified || n.Origin == convpkg.OriginFundi {
		return stripBorrowedThinking(n, n.Msg, rc)
	}
	if r := rc.replayer(n.Origin); r != nil {
		return stripBorrowedThinking(n, r.Replay(n.Msg), rc)
	}
	return stripBorrowedThinking(n, harness.For(n.Origin).ForeignReplay(n.Msg), rc)
}

// stripBorrowedThinking drops every thinking block from a borrowed assistant
// message — a node another conversation wrote, whose signature only its own
// upstream accepts — keeping a placeholder when nothing else is left. Own nodes
// are returned unchanged.
func stripBorrowedThinking(n *convpkg.Node, m msg.Message, rc *replayContext) msg.Message {
	if rc == nil || n.ConversationID == rc.convID || m.Role != msg.Assistant {
		return m
	}
	kept := make([]msg.Block, 0, len(m.Content))
	for _, b := range m.Content {
		if _, ok := b.(msg.Thinking); ok {
			continue
		}
		kept = append(kept, b)
	}
	if len(kept) == len(m.Content) {
		return m
	}
	if len(kept) == 0 {
		kept = append(kept, msg.Text{Text: borrowedThinkingPlaceholder})
	}
	m.Content = kept
	return m
}

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
func mergeForRequestRows(history []*convpkg.Node, rc *replayContext) (msgs []msg.Message, rowStart []int, err error) {
	out := make([]msg.Message, 0, len(history))
	start := make([]int, 0, len(history))
	for i, n := range history {
		nm := forRequest(n, rc)
		if nm.Role == msg.System {
			continue
		}
		m := msg.Message{Role: nm.Role, Content: replaceEmptyToolUses(nm)}
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
