// SPDX-License-Identifier: Apache-2.0

package clientstate

// ModelView is the stored form of the cockpit's model filter and ordering.
//
// Everything is named by its STRING label, never by an enum ordinal. Ordinals
// are a source-order detail — inserting a column between two existing ones
// renumbers every field after it — and a saved query that silently
// reinterpreted "sort by intelligence" as "sort by code" after an upgrade is
// worse than one that failed to load. The same argument applies to the
// threshold stops: adding a "≥256k" between two existing ones must not turn a
// remembered "≥1M" into something else.
//
// The reader drops anything it no longer recognises, which degrades a
// remembered query rather than refusing it.
type ModelView struct {
	Keys   []SortKey        `json:"keys,omitempty"`
	Bounds map[string]Bound `json:"bounds,omitempty"`

	ToolsOnly    bool `json:"toolsOnly"`
	VisionOnly   bool `json:"visionOnly"`
	ThinkingOnly bool `json:"thinkingOnly"`
}

// SortKey is one ordering term. Field is the column's label.
type SortKey struct {
	Field string `json:"field"`
	Desc  bool   `json:"desc"`
}

// Bound is a numeric constraint, holding the stop LABELS rather than indices.
// An empty side is unset.
type Bound struct {
	Min string `json:"min,omitempty"`
	Max string `json:"max,omitempty"`
}
