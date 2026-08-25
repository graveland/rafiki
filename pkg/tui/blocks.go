// SPDX-License-Identifier: Apache-2.0

// Package tui is the rafiki daily-driver TUI. It renders a fundi conversation
// streamed over the Connect control plane.
//
// Architecture: a bubbletea Model holds an append-only list of Blocks.
// Finalized blocks are rendered through glamour and cached as strings;
// the live tail (current streaming assistant turn) re-renders on a 250ms
// coalescence tick. See docs/plans/2026-08-12-rafiki-tui-design.md §4.2.
package tui

import (
	"strings"
	"time"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
)

// blockKind identifies what kind of conversational unit a Block is.
type blockKind int

const (
	kindSystem blockKind = iota
	kindUser
	kindAssistant
	kindPendingUser
)

// Block is one renderable unit in the transcript.
type Block struct {
	Kind       blockKind
	At         time.Time
	Text       string // user/pending text or assistant plain-text fallback
	Content    []*rafikiv1.ContentBlock
	ThinkText  string // accumulated thinking
	ToolCalls  []toolCallState
	StopReason string
	Final      bool // true when the turn has ended
}

type toolCallState struct {
	ID         string
	Name       string
	Input      string
	Result     string
	IsError    bool
	Running    bool
	DurationMs int64
}

// fingerprint returns a cheap content hash for cache-invalidation.
func (b Block) fingerprint() string {
	var sb strings.Builder
	sb.WriteString(b.Text)
	sb.WriteString(b.ThinkText)
	for _, tc := range b.ToolCalls {
		sb.WriteString(tc.ID)
		sb.WriteString(tc.Name)
		sb.WriteString(tc.Result)
		if tc.Running {
			sb.WriteString("running")
		}
	}
	sb.WriteString(b.StopReason)
	if b.Final {
		sb.WriteString("final")
	}
	return sb.String()
}

// appendBlock returns a new transcript slice with b appended.
func appendBlock(blocks []Block, b Block) []Block {
	return append(blocks, b)
}

// lastAssistant returns a pointer to the most recent assistant block,
// or nil if there is none.
func lastAssistant(blocks []Block) *Block {
	for i := len(blocks) - 1; i >= 0; i-- {
		if blocks[i].Kind == kindAssistant {
			return &blocks[i]
		}
	}
	return nil
}
