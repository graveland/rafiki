// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"strings"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
)

// Append's conflict semantics: an identical replay is a no-op, diverging
// content at an existing ordinal is a loud error (never a silent fork).
func TestAppendReplayAndDivergence(t *testing.T) {
	pool := testPool(t)
	ctx := context.Background()
	if err := Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	var convID string
	if err := pool.QueryRow(ctx, `INSERT INTO conversations.conversation (origin_entrypoint, driven_by)
		VALUES ('test','server') RETURNING id::text`).Scan(&convID); err != nil {
		t.Fatal(err)
	}
	m := NewMessages(pool)
	msg := anthropic.NewUserMessage(anthropic.NewTextBlock("hello"))

	if err := m.Append(ctx, convID, 0, msg, nil); err != nil {
		t.Fatalf("first append: %v", err)
	}
	// Identical replay: no-op, no error, still one row.
	if err := m.Append(ctx, convID, 0, msg, nil); err != nil {
		t.Fatalf("replay append: %v", err)
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM conversations.conversation_message
		WHERE conversation_id=$1::uuid`, convID).Scan(&n); err != nil || n != 1 {
		t.Fatalf("rows = %d err=%v, want 1", n, err)
	}
	// Diverging content at the same ordinal: loud error.
	err := m.Append(ctx, convID, 0, anthropic.NewUserMessage(anthropic.NewTextBlock("DIFFERENT")), nil)
	if err == nil || !strings.Contains(err.Error(), "diverged") {
		t.Fatalf("diverged append err = %v, want history-diverged error", err)
	}
}
