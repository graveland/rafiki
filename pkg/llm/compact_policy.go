// SPDX-License-Identifier: Apache-2.0

package llm

import (
	"github.com/anthropics/anthropic-sdk-go"

	"go.graveland.dev/rafiki/pkg/store"
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

// estimateTokens approximates a message's token count from its marshaled
// request footprint (roughly four bytes per token).
func estimateTokens(m store.Message) int { return (messageSize(m.Param) + 3) / 4 }

// estimateHistoryTokens sums estimateTokens across a history.
func estimateHistoryTokens(msgs []store.Message) int {
	total := 0
	for _, m := range msgs {
		total += estimateTokens(m)
	}
	return total
}

// isLegalCut reports whether a tail may start at m: a tool_result block must
// never be separated from the assistant tool_use that produced it, so any
// user row carrying one is off limits.
func isLegalCut(m store.Message) bool {
	if m.Param.Role == anthropic.MessageParamRoleAssistant {
		return true
	}
	for _, b := range m.Param.Content {
		if b.OfToolResult != nil {
			return false
		}
	}
	return true
}

// cutTail returns the index where the tail starts, in [1, len(history)];
// len(history) means an empty tail. Index 0 is never a tail start: the oldest
// row is always part of what the summary replaces. It picks the earliest legal
// start whose suffix fits the budget, or the empty tail when nothing fits.
func cutTail(history []store.Message, budgetTokens int) int {
	if budgetTokens <= 0 {
		return len(history)
	}
	for i := 1; i < len(history); i++ {
		if !isLegalCut(history[i]) {
			continue
		}
		if estimateHistoryTokens(history[i:]) <= budgetTokens {
			return i
		}
	}
	return len(history)
}
