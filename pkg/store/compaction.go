// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/anthropics/anthropic-sdk-go"

	"go.graveland.dev/rafiki/pkg/convtree"
	"go.graveland.dev/rafiki/pkg/slashcmd"
)

// Kinds written by the compaction boundary path. A compaction appends a
// summary as a new ROOT of the tree, followed by copies of the tail messages
// the summary did not cover, hung under it.
const (
	KindCompactionSummary = "compaction_summary"
	KindCompactionTail    = "compaction_tail"
	// KindClear marks the row a /clear opens a fresh working set with: the root
	// of the new path. For a fundi conversation it is a synthetic user row
	// written by AppendClear; for a claude conversation capture tags the first
	// divergent head with it.
	KindClear = "clear"
)

// ClearBoundaryText is the content of the synthetic user row a fundi /clear
// opens the fresh working set with. The working set must be non-empty (Continue
// refuses an empty one), and a user turn is what the next prompt merges into at
// request assembly. It lives in slashcmd, a stdlib-only package, so pgx-free
// readers (pkg/recall) can match it.
const ClearBoundaryText = slashcmd.ClearBoundaryText

// IsClearBoundary reports whether m is the synthetic row AppendClear writes, as
// opposed to a claude clear row, which is the real first message of the new head
// and shares the kind. Full-history readers render the former as a divider.
func IsClearBoundary(m Message) bool {
	if m.Kind == nil || *m.Kind != KindClear || m.Param.Role != "user" || len(m.Param.Content) != 1 {
		return false
	}
	t := m.Param.Content[0].OfText
	return t != nil && t.Text == ClearBoundaryText
}

// LoadWorking returns the conversation's working path: the chain of nodes from
// the head to its root, in depth order. It is the working set every send,
// orphan repair and prefill classifier reads; Load stays full history.
func (m *Messages) LoadWorking(ctx context.Context, conversationID string) ([]Message, error) {
	return m.LoadPath(ctx, conversationID)
}

// AppendCompaction writes a compaction boundary: the summary becomes a new root
// of the conversation's tree, with copies of tail hung under it, and the head
// moves onto the last copy. Nothing is deleted or renumbered; the pre-compaction
// path stays where it was, unreachable from the new head.
//
// It returns the summary's ordinal.
func (m *Messages) AppendCompaction(ctx context.Context, conversationID string, summary anthropic.MessageParam, replacedTokens int, tail []Message) (int, error) {
	content, err := json.Marshal(summary.Content)
	if err != nil {
		return 0, fmt.Errorf("append compaction: marshal summary: %w", err)
	}
	nodes := make([]ChainNode, 0, len(tail)+1)
	nodes = append(nodes, ChainNode{
		Role:        string(summary.Role),
		Content:     jsonbSafe(content),
		ToolUseIDs:  toolUseIDs(summary),
		InputTokens: int64Ptr(int64(replacedTokens)),
	})
	for i, t := range tail {
		content, err := json.Marshal(t.Param.Content)
		if err != nil {
			return 0, fmt.Errorf("append compaction: marshal tail %d: %w", i, err)
		}
		nodes = append(nodes, ChainNode{
			Role:       string(t.Param.Role),
			Content:    jsonbSafe(content),
			ToolUseIDs: toolUseIDs(t.Param),
			StopReason: t.StopReason,
			Kind:       ptrTo(KindCompactionTail),
		})
	}
	result, err := m.AppendChain(ctx, ChainAppend{
		ConversationID:  conversationID,
		Root:            convtree.RootKindCompaction,
		DerivedFromHead: true,
		Nodes:           nodes,
	})
	if err != nil {
		return 0, fmt.Errorf("append compaction: %w", err)
	}
	return result.Nodes[0].Ordinal, nil
}

// AppendClear writes a clear boundary: boundary becomes a new root of the
// conversation's tree and the head moves onto it, so the working path restarts
// there. Nothing is deleted or renumbered.
//
// Two clears in a row are two distinct roots, because a clear root's seed mixes
// in the node it is derived from (see convtree.RootSeed).
//
// boundary must carry real content: an empty working set cannot be continued.
func (m *Messages) AppendClear(ctx context.Context, conversationID string, boundary anthropic.MessageParam) (int, error) {
	content, err := json.Marshal(boundary.Content)
	if err != nil {
		return 0, fmt.Errorf("append clear: marshal boundary: %w", err)
	}
	result, err := m.AppendChain(ctx, ChainAppend{
		ConversationID:  conversationID,
		Root:            convtree.RootKindClear,
		DerivedFromHead: true,
		Nodes: []ChainNode{{
			Role:       string(boundary.Role),
			Content:    jsonbSafe(content),
			ToolUseIDs: toolUseIDs(boundary),
		}},
	})
	if err != nil {
		return 0, fmt.Errorf("append clear: %w", err)
	}
	return result.Nodes[0].Ordinal, nil
}

// SeedChain idempotently persists params as a chain from a fresh root — the
// bridge for hosts carrying client-side history (e.g. a fundi prefill or a
// Slack thread continuation). An existing prefix is matched by node hash and
// only the missing suffix is inserted, so re-seeding the same history writes
// nothing.
func (m *Messages) SeedChain(ctx context.Context, conversationID string, params []anthropic.MessageParam) error {
	if len(params) == 0 {
		return nil
	}
	nodes := make([]ChainNode, len(params))
	for i, p := range params {
		content, err := json.Marshal(p.Content)
		if err != nil {
			return fmt.Errorf("seed chain: marshal node %d: %w", i, err)
		}
		nodes[i] = ChainNode{Role: string(p.Role), Content: jsonbSafe(content), ToolUseIDs: toolUseIDs(p)}
	}
	if _, err := m.AppendChain(ctx, ChainAppend{
		ConversationID: conversationID,
		Root:           convtree.RootKindRoot,
		Nodes:          nodes,
	}); err != nil {
		return fmt.Errorf("seed chain: %w", err)
	}
	return nil
}

// int64Ptr returns a pointer to a copy of v, for the optional ChainNode fields.
func int64Ptr(v int64) *int64 { return &v }

// ptrTo returns a pointer to a copy of v, for the optional ChainNode fields.
func ptrTo[T any](v T) *T { return &v }
