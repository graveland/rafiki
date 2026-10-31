// SPDX-License-Identifier: Apache-2.0

package llm

import (
	convpkg "go.graveland.dev/rafiki/pkg/conv"
	"go.graveland.dev/rafiki/pkg/msg"
)

// CompactionPolicy governs proactive context compaction: when to summarise
// older history and how much verbatim tail to keep beside the summary. A zero
// value means the documented defaults, never "disabled"; summary-only is the
// explicit NoTail field.
type CompactionPolicy struct {
	ContextWindowFn  func() int // tokens; nil or returning <= 0 means unknown → no proactive trigger
	HeadroomBuffer   int        // tokens; 0 → 40_000
	TailPercent      int        // 0 → 10
	TailCap          int        // tokens; 0 → 50_000
	NoTail           bool       // true → summary only, no verbatim tail
	SummaryMaxTokens int64      // 0 → 16_000
}

// CompactionPhase marks the start or end of a compaction, for observers.
type CompactionPhase string

const (
	CompactionStart CompactionPhase = "start"
	CompactionEnd   CompactionPhase = "end"
)

// CompactionEvent reports a compaction boundary to an observer.
type CompactionEvent struct {
	Phase      CompactionPhase
	Trigger    string // "threshold" or "overflow"
	PreTokens  int    // set on CompactionEnd: tokens the summary replaced
	PostTokens int    // set on CompactionEnd: estimated tokens of the new working set
	// Succeeded reports whether this event marks a compaction that actually
	// wrote a boundary. It is false on Start and on every End from a failed or
	// no-op attempt (a failed summary call, a truncated or absent summary).
	// Gate anything that records a boundary on this, never on a token count:
	// a sender may legitimately report a zero usage figure, so PreTokens > 0 is
	// not a success signal.
	Succeeded bool
}

// minCompactableRows is the fewest rows worth compacting: below it a summary
// would replace almost nothing and cost a model call.
const minCompactableRows = 4

// withDefaults fills every zero (or negative) field with its documented
// default, then raises the headroom buffer so it covers the summary call's own
// output plus the prompt that asks for it.
func (p CompactionPolicy) withDefaults() CompactionPolicy {
	if p.HeadroomBuffer <= 0 {
		p.HeadroomBuffer = 40_000
	}
	if p.TailPercent <= 0 {
		p.TailPercent = 10
	}
	if p.TailCap <= 0 {
		p.TailCap = 50_000
	}
	if p.SummaryMaxTokens <= 0 {
		p.SummaryMaxTokens = 16_000
	}
	if p.HeadroomBuffer < int(p.SummaryMaxTokens)+4_000 {
		p.HeadroomBuffer = int(p.SummaryMaxTokens) + 4_000
	}
	return p
}

// window is the model's context size in tokens, or 0 when unknown. Call it on
// a defaulted policy.
func (p CompactionPolicy) window() int {
	if p.ContextWindowFn == nil {
		return 0
	}
	w := p.ContextWindowFn()
	if w < 0 {
		return 0
	}
	return w
}

// shouldCompact reports whether the remaining headroom has fallen to the
// buffer. An unknown window never triggers proactively — overflow handles it.
func (p CompactionPolicy) shouldCompact(usedTokens int) bool {
	w := p.window()
	return w > 0 && w-usedTokens <= p.HeadroomBuffer
}

// tailBudget is the token budget for the verbatim tail kept beside the
// summary: a percentage of the window, capped, or the cap alone when the
// window is unknown. NoTail means no verbatim tail at all.
func (p CompactionPolicy) tailBudget() int {
	if p.NoTail {
		return 0
	}
	w := p.window()
	if w == 0 {
		return p.TailCap
	}
	budget := w * p.TailPercent / 100
	if budget > p.TailCap {
		return p.TailCap
	}
	return budget
}

// envelopeBytes is the byte length of the request envelope a message rides in:
// {"role":"","content":}. It is the fixed overhead estimateTokens adds to the
// stored content.
const envelopeBytes = 24

// estimateTokens approximates a node's token count from its stored content
// plus the request envelope around it (roughly four bytes per token). It reads
// the stored bytes rather than re-marshalling the message, so it costs nothing
// on a node the cache already holds.
func estimateTokens(n *convpkg.Node) int {
	return (len(n.Content) + len(n.Msg.Role.String()) + envelopeBytes + 3) / 4
}

// estimateHistoryTokens sums estimateTokens across a history.
func estimateHistoryTokens(nodes []*convpkg.Node) int {
	total := 0
	for _, n := range nodes {
		total += estimateTokens(n)
	}
	return total
}

// isLegalCut reports whether a tail may start at n: a tool_result block must
// never be separated from the assistant tool_use that produced it, so any user
// row carrying one is off limits.
//
// A block that degraded to msg.Opaque is classified by the type it declares:
// storage keeps a tool_result typed, with every key the core does not model in
// its Ext, and keeps it Opaque only when it has no native core (a block type
// this codec does not model) or cannot be re-encoded faithfully. A boundary
// that could not see one would split the pair and have the next request
// rejected.
func isLegalCut(n *convpkg.Node) bool {
	if n.Msg.Role == msg.Assistant {
		return true
	}
	for _, b := range n.Msg.Content {
		switch t := b.(type) {
		case msg.ToolResult:
			return false
		case msg.Opaque:
			if msg.OpaqueType(t) == "tool_result" {
				return false
			}
		}
	}
	return true
}

// cutTail returns the index where the tail starts, in [1, len(history)];
// len(history) means an empty tail. Index 0 is never a tail start: the oldest
// row is always part of what the summary replaces. It picks the earliest legal
// start whose suffix fits the budget, or the empty tail when nothing fits.
//
// O(n): each row's estimateTokens is computed once and the suffix size is
// accumulated walking backward from the end. Suffix sizes only GROW as the
// start index decreases, so once the accumulated suffix exceeds the budget no
// smaller index can fit and the walk stops; the last legal index seen within
// budget is therefore the earliest such start.
func cutTail(history []*convpkg.Node, budgetTokens int) int {
	if budgetTokens <= 0 {
		return len(history)
	}
	best := len(history) // empty tail until a legal, fitting start is seen
	suffix := 0
	for i := len(history) - 1; i >= 1; i-- {
		suffix += estimateTokens(history[i])
		if suffix > budgetTokens {
			break
		}
		if isLegalCut(history[i]) {
			best = i
		}
	}
	return best
}
