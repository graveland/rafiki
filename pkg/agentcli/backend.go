// SPDX-License-Identifier: Apache-2.0

// Package agentcli defines the transport-agnostic seam between the CLI and
// backend services: a DSN-backed local implementation today, leaving room for a
// gRPC one.
package agentcli

import (
	"context"

	"go.graveland.dev/rafiki/pkg/analyze"
	"go.graveland.dev/rafiki/pkg/insights"
	"go.graveland.dev/rafiki/pkg/store"
)

// Backend is the transport-agnostic seam: the DSN-backed local implementation
// today, leaving room for a gRPC one. It powers agent CLI commands with
// insights queries, analysis, and finding management.
type Backend interface {
	Stats(ctx context.Context, f insights.StatsFilter) (*insights.Stats, error)
	ConversationStats(ctx context.Context, id string) (*insights.Stats, error)
	Search(ctx context.Context, f insights.SearchFilter) ([]insights.ConversationSummary, error)
	Export(ctx context.Context, id string) (*insights.Transcript, error)
	Analyze(ctx context.Context, req AnalyzeRequest) (<-chan AnalyzeEvent, error)
	Findings(ctx context.Context, f store.FindingFilter) ([]store.FindingRow, error)
	SetFindingStatus(ctx context.Context, id, status string) (store.FindingRow, error)
}

// AnalyzeRequest selects a population and the stage to stop after.
// ConversationIDs and Filter are mutually exclusive; CorpusDir is
// local-only (the gRPC backend rejects it).
type AnalyzeRequest struct {
	ConversationIDs []string
	Filter          *insights.SearchFilter
	CorpusDir       string
	Profile         *analyze.Profile
	StopAfter       string
	Force           bool
	Limit           int
	SkillFiles      []analyze.SkillFile
	NoStore         bool
}

// EventKind values for AnalyzeEvent.Kind select which payload field is non-nil.
const (
	EventProgress = "progress"
	EventAnalysis = "analysis"
	EventSummary  = "summary"
	EventError    = "error"
)

// AnalyzeEvent is a typed union: exactly one payload pointer is non-nil,
// selected by Kind. Compact-stage runs carry Transcript instead of
// Analysis (there is no analysis yet at that stage).
//
// When Kind is EventError, Err is set and all payload pointers are nil.
// An EventError is the LAST event on the channel, and the channel closes
// immediately after. A consumer that sees EventError must stop and surface
// the error.
type AnalyzeEvent struct {
	Kind       string
	Progress   *Progress
	Analysis   *analyze.Analysis
	Transcript *insights.Transcript
	Summary    *Summary
	Err        error
}

// ProgressState values for Progress.State indicate the outcome of analyzing
// one conversation.
const (
	StateStarted = "started"
	StateSkipped = "skipped"
	StateDone    = "done"
	StateFailed  = "failed"
)

// Progress reports the outcome of analyzing a single conversation: the state
// (started, skipped, done, failed), any failure reason or skip reason, and the
// turn's token usage and cost.
//
// Tags use the same snake_case field names as the server's own wire shape
// (client/pkg/server/agent_analyze.go's progressPayload/summaryTotals) —
// conversation_id/state/detail/input_tokens/output_tokens/cost_usd — so a
// caller marshaling either backend's events (or a store.FindingRow, tagged
// to match) gets an identical JSON contract regardless of which one is live.
type Progress struct {
	ConversationID string  `json:"conversation_id"`
	State          string  `json:"state"`
	Detail         string  `json:"detail,omitempty"`
	InputTokens    int64   `json:"input_tokens"`
	OutputTokens   int64   `json:"output_tokens"`
	CostUSD        float64 `json:"cost_usd"`
}

// RankedFindingWithDraft nests an optional Draft result under its ranked
// finding. A side-map keyed by Title cannot represent this: analyze.Rank
// groups by (Axis, SkillName‖TopicKey) and only tie-breaks equal titles by
// axis, so two distinct ranked findings can share a Title — a Title-keyed
// map would silently attach one draft to both. Mirrors the server's
// rankedFindingWithDraft exactly, including the "draft,omitempty" tag.
type RankedFindingWithDraft struct {
	analyze.RankedFinding
	Draft *analyze.SkillEdit `json:"draft,omitempty"`
}

// Summary is the final event of an Analyze run: the ranked findings (each
// carrying its own draft skill edit, if one was drafted), population
// counts, and aggregate token usage + cost. Tags mirror the server's
// summaryPayload (Ranked/Totals/Remaining are wire-visible there too;
// Analyzed/Skipped/Failed/Population live on the server's nested
// summaryTotals instead of top-level Summary, but this type keeps them
// top-level to match its own pre-existing Go shape — only the JSON key
// spelling is made to match).
type Summary struct {
	Ranked     []RankedFindingWithDraft `json:"ranked"`
	Analyzed   int                      `json:"analyzed"`
	Skipped    int                      `json:"skipped"`
	Failed     int                      `json:"failed"`
	Remaining  int                      `json:"remaining"`
	Population int                      `json:"population"`
	Totals     Totals                   `json:"totals"`
}

// Totals aggregates token usage and cost across an entire Analyze run.
// Tags match the server's summaryTotals exactly for these three fields.
type Totals struct {
	InputTokens  int64   `json:"input_tokens"`
	OutputTokens int64   `json:"output_tokens"`
	CostUSD      float64 `json:"cost_usd"`
}
