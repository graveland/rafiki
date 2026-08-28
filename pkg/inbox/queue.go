// SPDX-License-Identifier: Apache-2.0

package inbox

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// DeliverFn hands one assembled batch to a child.
//
// awaitAck says whether the child will confirm that the batch entered a turn.
// false means no confirmation is ever coming -- a claude child, which has no
// such signal, or an abort, which has no turn to enter -- and the queue marks
// the batch consumed on the spot. An error leaves every row pending.
type DeliverFn func(ctx context.Context, b Batch) (awaitAck bool, err error)

// ValidateFn is the command-plane check that runs BEFORE a message is
// persisted: does this child exist, and can it still be sent to.
//
// It runs first on purpose. "Accepted and durably queued" is a promise, and
// making it to a caller whose child has already exited turns a clean error
// into a row nobody will ever consume.
type ValidateFn func(childID string) error

// QueueConfig configures a Queue. Store is required; the rest are optional.
type QueueConfig struct {
	Store    Store
	Validate ValidateFn
	Deliver  DeliverFn
	Batch    BatchConfig
}

// Queue is the seam between "a client submitted something for an agent" and
// "the agent received it", with the durability the seam always promised.
type Queue struct {
	cfg   QueueConfig
	locks *keyedMutex
}

// NewQueue returns a Queue over cfg.Store.
func NewQueue(cfg QueueConfig) *Queue {
	return &Queue{cfg: cfg, locks: newKeyedMutex()}
}

// Accept validates, persists, and returns the row id. It does NOT deliver:
// the caller decides whether to attempt immediate delivery, because a
// submitter wants the round trip and a recovery replay does not.
func (q *Queue) Accept(ctx context.Context, in Inbound) (string, error) {
	if in.ChildID == "" {
		return "", errors.New("inbox: ChildID is required")
	}
	if q.cfg.Store == nil {
		return "", errors.New("inbox: no store configured")
	}
	if q.cfg.Validate != nil {
		if err := q.cfg.Validate(in.ChildID); err != nil {
			return "", err
		}
	}
	rec, err := q.cfg.Store.Accept(ctx, in)
	if err != nil {
		return "", err
	}
	return rec.ID, nil
}

// Deliver writes childID's pending rows for exactly one source. source == ""
// selects direct messages.
func (q *Queue) Deliver(ctx context.Context, childID, source string) error {
	return q.deliver(ctx, childID, &source)
}

// DeliverAll writes every pending row for childID. This is the drain: used on
// the child's idle transition (retrying anything a failed immediate delivery
// left behind) and by recovery replay after a restart.
func (q *Queue) DeliverAll(ctx context.Context, childID string) error {
	return q.deliver(ctx, childID, nil)
}

func (q *Queue) deliver(ctx context.Context, childID string, source *string) error {
	if q.cfg.Store == nil || q.cfg.Deliver == nil {
		return nil
	}
	// Per child, read-coalesce-write is one critical section. Without it the
	// immediate delivery and the idle drain both read the same pending rows
	// and the child receives two copies.
	unlock := q.locks.lock(childID)
	defer unlock()

	rows, err := q.cfg.Store.Pending(ctx, childID)
	if err != nil {
		return err
	}
	if source != nil {
		filtered := rows[:0:0]
		for _, r := range rows {
			if r.Source == *source {
				filtered = append(filtered, r)
			}
		}
		rows = filtered
	}
	if len(rows) == 0 {
		return nil
	}

	for _, b := range Coalesce(rows, q.cfg.Batch) {
		awaitAck, err := q.cfg.Deliver(ctx, b)
		if err != nil {
			// Stop at the first failure rather than pressing on: a later
			// message must not overtake an earlier one that is still queued.
			return fmt.Errorf("inbox: deliver to %s: %w", childID, err)
		}
		if awaitAck {
			if err := q.cfg.Store.MarkSent(ctx, b.IDs); err != nil {
				return err
			}
			continue
		}
		if err := q.cfg.Store.MarkConsumed(ctx, b.IDs); err != nil {
			return err
		}
	}
	return nil
}

// Consume marks rows the child confirmed it took into a turn.
func (q *Queue) Consume(ctx context.Context, ids []string) error {
	if q.cfg.Store == nil || len(ids) == 0 {
		return nil
	}
	return q.cfg.Store.MarkConsumed(ctx, ids)
}

// Reset returns childID's unconfirmed rows to pending. Call it when the child
// holding them died -- on its exit, and for each child this daemon resumes at
// startup. Never call it for a child another daemon is running.
func (q *Queue) Reset(ctx context.Context, childID string) (int, error) {
	if q.cfg.Store == nil {
		return 0, nil
	}
	unlock := q.locks.lock(childID)
	defer unlock()
	return q.cfg.Store.ResetSent(ctx, childID)
}

// Drop terminates childID's undelivered rows. Call it when there will never
// be a turn to inject into.
func (q *Queue) Drop(ctx context.Context, childID, reason string) (int, error) {
	if q.cfg.Store == nil {
		return 0, nil
	}
	unlock := q.locks.lock(childID)
	defer unlock()
	return q.cfg.Store.Drop(ctx, childID, reason)
}

// Sweep deletes terminal rows older than before.
func (q *Queue) Sweep(ctx context.Context, before time.Time) (int, error) {
	if q.cfg.Store == nil {
		return 0, nil
	}
	return q.cfg.Store.Sweep(ctx, before)
}

var _ Accepter = (*Queue)(nil)
