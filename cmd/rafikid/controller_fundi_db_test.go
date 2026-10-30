// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"

	"go.graveland.dev/rafiki/pkg/conv"
	msganthropic "go.graveland.dev/rafiki/pkg/msg/anthropic"

	"github.com/multigres/testkit/assert"
)

// TestDBRecentSkipsCompactionTailAndRendersSummaryBoundary pins the agent_view
// (GetRecent → dbRecent) contract for a compacted conversation: the
// compaction_tail copy is dropped (the tail shows once, through its original
// row) and the compaction_summary node renders as a compaction_boundary divider
// rather than a plain user prompt. Before the fix dbRecent passed the bare
// Params, so every tail row was shown twice and the summary looked like a user
// message.
func TestDBRecentSkipsCompactionTailAndRendersSummaryBoundary(t *testing.T) {
	ck := assert.NewCollecting(t)
	pool := scratchPool(t)
	ctx := context.Background()

	var convID string
	ck.Require().NoError(pool.QueryRow(ctx,
		`INSERT INTO conversations.conversation (origin_entrypoint, driven_by)
		 VALUES ('test', 'server') RETURNING id::text`).Scan(&convID), "insert conversation")

	rows := []anthropic.MessageParam{
		anthropic.NewUserMessage(anthropic.NewTextBlock("prompt-1")),
		anthropic.NewAssistantMessage(anthropic.NewTextBlock("reply-1")),
		anthropic.NewUserMessage(anthropic.NewTextBlock("prompt-2")),
		anthropic.NewAssistantMessage(anthropic.NewTextBlock("reply-2")),
	}
	for _, p := range rows {
		appendNode(t, pool, convID, p, nil)
	}
	tail := []anthropic.MessageParam{rows[2], rows[3]}
	summary := anthropic.MessageParam{
		Role:    anthropic.MessageParamRoleUser,
		Content: []anthropic.ContentBlockParamUnion{anthropic.NewTextBlock("SUMMARY-TEXT")},
	}
	summaryMsg, serr := msganthropic.FromParam(summary)
	ck.Require().NoError(serr, "convert summary")
	nodes := []conv.NewNode{{Msg: summaryMsg, InputTokens: ptr(int64(100))}}
	for i, p := range tail {
		m, merr := msganthropic.FromParam(p)
		ck.Require().NoError(merr, "convert tail %d", i)
		nodes = append(nodes, conv.NewNode{Msg: m, Kind: conv.KindCompactionTail})
	}
	_, err := convStoreFor(t, pool).Open(convID, conv.AsOwner()).Append(ctx, conv.Append{
		Root: conv.KindCompactionSummary, DerivedFromHead: true, Nodes: nodes,
	})
	ck.Require().NoError(err, "append compaction")

	ctrl := newTestController(t)
	ctrl.pool = pool
	ctrl.convs = convStoreFor(t, pool)

	var boundary, prompt2, summaryCount int
	for _, ev := range ctrl.dbRecent(convID, recentQuery{}) {
		var env struct {
			Type    string `json:"type"`
			Message struct {
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		}
		ck.Require().NoError(json.Unmarshal(ev.Bytes, &env), "frame %s", ev.Bytes)
		switch env.Type {
		case "compaction_boundary":
			boundary++
		case "message_end":
			var text string
			if json.Unmarshal(env.Message.Content, &text) != nil {
				continue
			}
			switch text {
			case "prompt-2":
				prompt2++
			case "SUMMARY-TEXT":
				summaryCount++
			}
		}
	}
	ck.Eq(1, boundary, "compaction_boundary frames")
	ck.Eq(1, prompt2, "the tail copy must not duplicate prompt-2 in agent_view")
	ck.Eq(0, summaryCount, "the summary must not render as a plain user message")
}
