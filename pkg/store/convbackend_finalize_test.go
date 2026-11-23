// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/multigres/testkit/assert"

	"go.graveland.dev/rafiki/pkg/conv"
	"go.graveland.dev/rafiki/pkg/msg"
)

// errBoom is what a Finalize hook fails with when the test wants the commit
// rolled back.
var errBoom = errors.New("finalize: boom")

// conversationName is the conversation row's name, read through the pool so it
// cannot come from the committing transaction.
func conversationName(t *testing.T, pool *pgxpool.Pool, convID string) string {
	t.Helper()
	var name string
	err := pool.QueryRow(context.Background(),
		`SELECT coalesce(name, '') FROM conversations.conversation WHERE id = $1::uuid`, convID).Scan(&name)
	assert.NewAborting(t).NoError(err, "read the conversation name")
	return name
}

// setConversationName writes a name outside any commit, so a rolled-back hook
// write is visible as "unchanged".
func setConversationName(t *testing.T, pool *pgxpool.Pool, convID, name string) {
	t.Helper()
	_, err := pool.Exec(context.Background(),
		`UPDATE conversations.conversation SET name = $2 WHERE id = $1::uuid`, convID, name)
	assert.NewAborting(t).NoError(err, "set the conversation name")
}

// TestConvBackendFinalizeRunsInTheCommitTransaction pins the hook's place: it
// runs after the inserts and the counter update, inside the same transaction, so
// its write is durable with the commit that carried it. The hook's own predicate
// reads the counters this commit just moved, so a hook placed ahead of them
// updates nothing.
func TestConvBackendFinalizeRunsInTheCommitTransaction(t *testing.T) {
	c := assert.NewAborting(t)
	pool := leasePool(t)
	ctx := context.Background()
	convID := cleanConversation(t, pool)
	b := NewConvBackend(pool)

	row := commitRow(t, convID, "user", "a node with a finalize hook", 0, 0)
	var rowsAffected int64
	_, err := b.Commit(ctx, conv.Commit{
		ConversationID:    convID,
		ExpectHead:        "",
		ExpectNextOrdinal: 0,
		Rows:              []*conv.Node{row},
		NewHead:           row.ID,
		Finalize: func(ctx context.Context, x conv.TxExec) error {
			n, err := x.Exec(ctx,
				`UPDATE conversations.conversation SET name = $2
				  WHERE id = $1::uuid AND next_ordinal = 1 AND node_count = 1`, convID, "finalized")
			rowsAffected = n
			return err
		},
	})
	c.NoError(err, "the commit with a Finalize hook")
	c.Eq(int64(1), rowsAffected, "the hook ran after the inserts and the counters they moved")
	c.Eq("finalized", conversationName(t, pool, convID), "the hook's write committed with the tree")
	c.Eq(1, countNodes(t, pool, convID), "and the commit's node committed too")
}

// TestConvBackendFinalizeErrorRollsBackTheInsertsAndTheHooksWrites pins the
// hook's error as the whole commit's: the nodes stay out of the tree and the
// hook's own statement is undone with them.
func TestConvBackendFinalizeErrorRollsBackTheInsertsAndTheHooksWrites(t *testing.T) {
	c := assert.NewAborting(t)
	pool := leasePool(t)
	ctx := context.Background()
	convID := cleanConversation(t, pool)
	b := NewConvBackend(pool)

	setConversationName(t, pool, convID, "before")
	row := commitRow(t, convID, "user", "a node a failed hook rolls back", 0, 0)
	_, err := b.Commit(ctx, conv.Commit{
		ConversationID:    convID,
		ExpectHead:        "",
		ExpectNextOrdinal: 0,
		Rows:              []*conv.Node{row},
		NewHead:           row.ID,
		Finalize: func(ctx context.Context, x conv.TxExec) error {
			if _, err := x.Exec(ctx,
				`UPDATE conversations.conversation SET name = $2 WHERE id = $1::uuid`, convID, "finalized"); err != nil {
				return err
			}
			return errBoom
		},
	})
	c.ErrorIs(err, errBoom, "the hook's error is the commit's error")
	c.Eq("before", conversationName(t, pool, convID), "the hook's write was rolled back")
	c.Eq(0, countNodes(t, pool, convID), "the commit's nodes were rolled back")
}

// TestConvBackendFinalizeConflictIsNotRetriedThroughAppend pins the retry rule
// on the production backend: a hook error that wraps ErrConflict reaches the
// caller instead of the append's retry loop, so the hook runs once and no row is
// stored.
func TestConvBackendFinalizeConflictIsNotRetriedThroughAppend(t *testing.T) {
	c := assert.NewAborting(t)
	pool := leasePool(t)
	ctx := context.Background()
	convID := cleanConversation(t, pool)
	h := conv.NewStore(NewConvBackend(pool), conv.Options{}).Open(convID, conv.AsOwner())

	runs := 0
	_, err := h.Append(ctx, conv.Append{
		FromHead: true,
		Nodes: []conv.NewNode{{
			Msg: msg.Message{Role: msg.User, Content: []msg.Block{msg.Text{Text: "a hook that conflicts"}}},
		}},
		Finalize: func(context.Context, conv.TxExec, conv.Result) error {
			runs++
			return fmt.Errorf("hook: %w", conv.ErrConflict)
		},
	})
	c.ErrorIs(err, conv.ErrConflict, "the hook's wrapped conflict reaches the caller")
	c.Eq(1, runs, "a hook error is not retried")
	c.Eq(0, countNodes(t, pool, convID), "and nothing was stored")
}

// TestConvBackendFinalizeNotRunOnAHeadConflict pins the hook behind the head
// comparison: a commit that loses the conflict has run no inserts to finalize,
// so its hook must not run either.
func TestConvBackendFinalizeNotRunOnAHeadConflict(t *testing.T) {
	c := assert.NewAborting(t)
	pool := leasePool(t)
	ctx := context.Background()
	convID := cleanConversation(t, pool)
	b := NewConvBackend(pool)

	row := commitRow(t, convID, "user", "a node behind a stale head", 0, 0)
	calls := 0
	_, err := b.Commit(ctx, conv.Commit{
		ConversationID:    convID,
		ExpectHead:        row.ID, // the conversation is at no head at all
		ExpectNextOrdinal: 0,
		Rows:              []*conv.Node{row},
		NewHead:           row.ID,
		Finalize: func(ctx context.Context, x conv.TxExec) error {
			calls++
			return nil
		},
	})
	c.ErrorIs(err, conv.ErrConflict, "a stale head is a conflict")
	c.Eq(0, calls, "the refused commit ran no Finalize hook")
	c.Eq(0, countNodes(t, pool, convID), "and inserted nothing")
}
