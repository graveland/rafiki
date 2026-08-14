// SPDX-License-Identifier: Apache-2.0

package insightstypes

import (
	"encoding/json"
)

// TranscriptTurn is one message in an exported conversation, annotated with the
// skills it invoked and — for assistant messages — the producing turn's metrics
// (matched via conversation_turn.response_ordinal = message ordinal).
type TranscriptTurn struct {
	Ordinal int             `json:"ordinal"`
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"` // verbatim content-block array

	Skills []string `json:"skills"` // skills invoked in this message (Skill tool_use / user /slash markers)

	InputTokens     int64  `json:"input_tokens"`
	OutputTokens    int64  `json:"output_tokens"`
	CacheReadTokens int64  `json:"cache_read_tokens"`
	LatencyMS       int    `json:"latency_ms"`
	Model           string `json:"model"`
	PrefixHash      string `json:"prefix_hash"`
}

// Transcript is a decomposed conversation: header identity, the ordered message
// list, and the recovered skill catalog available to the agent.
type Transcript struct {
	ConversationID string `json:"conversation_id"`
	Owner          string `json:"owner"`
	Persona        string `json:"persona"`
	Source         string `json:"source"`
	DrivenBy       string `json:"driven_by"`

	Turns           []TranscriptTurn `json:"turns"`
	AvailableSkills []string         `json:"available_skills"` // catalog recovered from prefix_content, else union of invoked skills
}

// turnMetrics holds the per-turn figures attached to an assistant message.

// Export reconstructs a conversation as an ordered, decomposed transcript.
// Works for both paths: the proxy path decomposes requests into
// conversation_message rows just as the in-process path does.
