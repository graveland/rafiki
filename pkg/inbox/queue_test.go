// SPDX-License-Identifier: Apache-2.0

package inbox_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"go.graveland.dev/rafiki/pkg/inbox"
)

type recorder struct {
	mu       sync.Mutex
	batches  []inbox.Batch
	awaitAck bool
	err      error
}

func (r *recorder) deliver(_ context.Context, b inbox.Batch) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return false, r.err
	}
	r.batches = append(r.batches, b)
	return r.awaitAck, nil
}

func (r *recorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.batches)
}

func newQueue(t *testing.T, rec *recorder, validate inbox.ValidateFn) (*inbox.Queue, inbox.Store) {
	t.Helper()
	st := inbox.NewMemory()
	return inbox.NewQueue(inbox.QueueConfig{
		Store:    st,
		Validate: validate,
		Deliver:  rec.deliver,
	}), st
}

func TestQueueValidateRunsBeforePersist(t *testing.T) {
	rec := &recorder{}
	boom := errors.New("child has exited")
	q, st := newQueue(t, rec, func(string) error { return boom })

	if _, err := q.Accept(context.Background(), inbox.Inbound{ChildID: "c_1", Mode: inbox.ModePrompt, Text: "hi"}); !errors.Is(err, boom) {
		t.Fatalf("Accept err = %v, want %v", err, boom)
	}
	rows, _ := st.Pending(context.Background(), "c_1")
	if len(rows) != 0 {
		t.Errorf("a refused message must not be persisted; got %d rows", len(rows))
	}
}

func TestQueueDeliverMarksSentWhenAnAckIsComing(t *testing.T) {
	rec := &recorder{awaitAck: true}
	q, st := newQueue(t, rec, nil)
	ctx := context.Background()

	id, err := q.Accept(ctx, inbox.Inbound{ChildID: "c_1", Mode: inbox.ModePrompt, Text: "hi"})
	if err != nil {
		t.Fatalf("Accept: %v", err)
	}
	if err := q.DeliverAll(ctx, "c_1"); err != nil {
		t.Fatalf("DeliverAll: %v", err)
	}
	if rec.count() != 1 {
		t.Fatalf("want 1 delivered batch, got %d", rec.count())
	}
	// The row is sent, not pending: a second drain must not deliver it again.
	if err := q.DeliverAll(ctx, "c_1"); err != nil {
		t.Fatalf("second DeliverAll: %v", err)
	}
	if rec.count() != 1 {
		t.Fatalf("a sent row was delivered twice")
	}
	if err := q.Consume(ctx, "c_1", []string{id}); err != nil {
		t.Fatalf("Consume: %v", err)
	}
	if rows, _ := st.Pending(ctx, "c_1"); len(rows) != 0 {
		t.Errorf("consumed row is still pending")
	}
}

func TestQueueDeliverConsumesImmediatelyWhenNoAckIsComing(t *testing.T) {
	rec := &recorder{awaitAck: false}
	q, st := newQueue(t, rec, nil)
	ctx := context.Background()

	if _, err := q.Accept(ctx, inbox.Inbound{ChildID: "c_1", Mode: inbox.ModePrompt, Text: "hi"}); err != nil {
		t.Fatalf("Accept: %v", err)
	}
	if err := q.DeliverAll(ctx, "c_1"); err != nil {
		t.Fatalf("DeliverAll: %v", err)
	}
	// The row must actually be consumed, not merely absent from Pending for
	// some other reason (e.g. never delivered at all) -- Reset alone cannot
	// distinguish those, since ResetSent only moves StateSent rows and a row
	// stuck in StatePending also yields (0, nil).
	if rows, _ := st.Pending(ctx, "c_1"); len(rows) != 0 {
		t.Errorf("a no-ack delivery must consume the row immediately; got %d rows still pending", len(rows))
	}
	if n, err := q.Reset(ctx, "c_1"); err != nil || n != 0 {
		t.Errorf("Reset after a no-ack delivery = (%d, %v), want (0, nil): the row is terminal", n, err)
	}
}

func TestQueueDeliverLeavesRowsPendingOnFailure(t *testing.T) {
	rec := &recorder{err: errors.New("child not found")}
	q, st := newQueue(t, rec, nil)
	ctx := context.Background()

	if _, err := q.Accept(ctx, inbox.Inbound{ChildID: "c_1", Mode: inbox.ModePrompt, Text: "hi"}); err != nil {
		t.Fatalf("Accept: %v", err)
	}
	if err := q.DeliverAll(ctx, "c_1"); err == nil {
		t.Fatal("DeliverAll must surface the delivery error")
	}
	if rows, _ := st.Pending(ctx, "c_1"); len(rows) != 1 {
		t.Errorf("a failed delivery must leave the row pending; got %d", len(rows))
	}
}

func TestQueueDeliverFiltersBySource(t *testing.T) {
	rec := &recorder{awaitAck: true}
	q, _ := newQueue(t, rec, nil)
	ctx := context.Background()

	for _, in := range []inbox.Inbound{
		{ChildID: "c_1", Mode: inbox.ModePrompt, Text: "direct"},
		{ChildID: "c_1", Mode: inbox.ModePrompt, Source: "subagents", Text: "frag"},
	} {
		if _, err := q.Accept(ctx, in); err != nil {
			t.Fatalf("Accept: %v", err)
		}
	}
	if err := q.Deliver(ctx, "c_1", "subagents"); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if rec.count() != 1 || rec.batches[0].Source != "subagents" {
		t.Fatalf("Deliver(source) delivered %+v, want only the subagents batch", rec.batches)
	}
}

// TestQueueSerializesDeliveryPerChild races 8 concurrent drains -- the
// immediate delivery racing the idle drain -- across 20 independent trials,
// each with a fresh queue, fresh row, and fresh assertion. A single trial's
// race is not reliably won by an unguarded bug (observed ~60% catch rate per
// trial, see the task report), so one trial is not a trustworthy gate; 20
// pushes the miss probability for a genuinely missing lock down to roughly
// 0.4^20 ~= 1e-8, and it still runs in milliseconds because nothing here
// sleeps.
func TestQueueSerializesDeliveryPerChild(t *testing.T) {
	for trial := range 20 {
		rec := &recorder{awaitAck: true}
		q, _ := newQueue(t, rec, nil)
		ctx := context.Background()
		if _, err := q.Accept(ctx, inbox.Inbound{ChildID: "c_1", Mode: inbox.ModePrompt, Text: "hi"}); err != nil {
			t.Fatalf("trial %d: Accept: %v", trial, err)
		}

		var wg sync.WaitGroup
		for range 8 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				_ = q.DeliverAll(ctx, "c_1")
			}()
		}
		wg.Wait()
		if rec.count() != 1 {
			t.Fatalf("trial %d: concurrent drains delivered %d copies, want 1", trial, rec.count())
		}
	}
}
