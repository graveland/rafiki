// SPDX-License-Identifier: Apache-2.0

package insightstypes

import (
	"time"
)

// SearchFilter narrows a conversation search. Zero-value fields are ignored, so
// an empty filter returns the most recent conversations up to Limit.
//
// Since/Until, Model, and Source are TURN-level filters: a conversation
// matches when a single turn satisfies all of them (turn activity in the
// window, not conversation creation time) — the same population
// GlobalStats selects with the same filter.
type SearchFilter struct {
	Since, Until      *time.Time
	Owner             string // owner USERNAME; resolved to a users id server-side
	Persona           string
	Source            string
	Model             string
	Status            string
	Path              Path
	MinTokens         int64  // minimum total (input+output) tokens across the conversation's turns
	Text              string // ILIKE substring match against the first user message snippet
	Entrypoint        string // match conversations by origin_entrypoint
	ExcludeEntrypoint string // drop conversations with this origin_entrypoint
	Limit             int    // defaults to 50 when <= 0
}

// ConversationSummary is one row of Search: the conversation plus a cheap
// per-conversation turn aggregate and the first user message snippet.
type ConversationSummary struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Owner    string `json:"owner"` // username, resolved through the users FK
	Persona  string `json:"persona"`
	Source   string `json:"source"`
	Model    string `json:"model"`
	Status   string `json:"status"`
	DrivenBy string `json:"driven_by"`

	CreatedAt time.Time `json:"created_at"`
	Turns     int       `json:"turns"`

	InputTokens     int64 `json:"input_tokens"`
	OutputTokens    int64 `json:"output_tokens"`
	CacheReadTokens int64 `json:"cache_read_tokens"`

	CacheHitRatio float64 `json:"cache_hit_ratio"`
	TotalCostUSD  float64 `json:"total_cost_usd"`

	FirstMessage string `json:"first_message"`
}

// Search returns conversations matching f, most recent first. Token counts and
// turn counts come from a per-conversation aggregate over conversation_turn;
// FirstMessage is the leading 200 chars of the earliest user message.
