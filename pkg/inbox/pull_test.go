// SPDX-License-Identifier: Apache-2.0

package inbox_test

import (
	"context"
	"testing"

	"go.graveland.dev/rafiki/pkg/inbox"
)

// TestPullRetiresAndReturnsPending pins Queue.Pull's contract for the Receive
// stream (cmd/rafikid connect_script.go): the pending rows come back in
// order, they are consumed BEFORE the consumer delivers them, and a second
// pull sees none of them again. A deliver fn that would consume the same rows
// is deliberately NOT configured, so the pull is the only consumer.
func TestPullRetiresAndReturnsPending(t *testing.T) {
	q := inbox.NewQueue(inbox.QueueConfig{Store: inbox.NewMemory()})
	ctx := context.Background()

	for _, text := range []string{"first", "second"} {
		if _, err := q.Accept(ctx, inbox.Inbound{ChildID: "c_w", Text: text}); err != nil {
			t.Fatalf("accept %q: %v", text, err)
		}
	}
	rows, err := q.Pull(ctx, "c_w")
	if err != nil {
		t.Fatalf("pull: %v", err)
	}
	if len(rows) != 2 || rows[0].Text != "first" || rows[1].Text != "second" {
		t.Fatalf("pull = %+v", rows)
	}
	again, err := q.Pull(ctx, "c_w")
	if err != nil {
		t.Fatalf("second pull: %v", err)
	}
	if len(again) != 0 {
		t.Fatalf("second pull = %+v, want none", again)
	}
}

// TestPullOnAnEmptyChildIsANoOp, and a queue with no store answers nothing
// rather than panicking.
func TestPullOnAnEmptyChildIsANoOp(t *testing.T) {
	q := inbox.NewQueue(inbox.QueueConfig{Store: inbox.NewMemory()})
	rows, err := q.Pull(context.Background(), "c_nobody")
	if err != nil || len(rows) != 0 {
		t.Fatalf("pull on unknown child = %+v / %v, want empty", rows, err)
	}

	var bare inbox.Queue
	if rows, err := bare.Pull(context.Background(), "c_w"); err != nil || len(rows) != 0 {
		t.Fatalf("pull with no store = %+v / %v, want empty", rows, err)
	}
}
