// SPDX-License-Identifier: Apache-2.0

package convtest

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/multigres/testkit/assert"

	"go.graveland.dev/rafiki/pkg/conv"
)

// errFinalizeBoom is what the rollback subtest's hook returns.
var errFinalizeBoom = errors.New("finalize: boom")

// runFinalize exercises Commit's Finalize hook at the interface, where a Store
// cannot be in the way: the hook runs inside a successful commit's transaction
// and only there.
//
// The hooks here never use the transaction, so every backend can run them,
// including the ones with no database behind them.
func runFinalize(t *testing.T, newBackend func(t *testing.T) (conv.Backend, func(convID string))) {
	t.Helper()

	// setup returns a backend and a fresh, empty conversation.
	setup := func(t *testing.T) (conv.Backend, string) {
		t.Helper()
		b, create := newBackend(t)
		convID := uuid.Must(uuid.NewV7()).String()
		create(convID)
		return b, convID
	}

	// counting is a hook that counts its runs and touches nothing else.
	counting := func(runs *int) func(context.Context, conv.TxExec) error {
		return func(context.Context, conv.TxExec) error {
			*runs++
			return nil
		}
	}

	t.Run("finalize runs once after a successful commit", func(t *testing.T) {
		c := assert.NewAborting(t)
		b, convID := setup(t)
		ctx := context.Background()
		before, err := b.State(ctx, convID)
		c.NoError(err, "State")

		row := foreignRow(t, convID, "finalized", "", "")
		runs := 0
		_, err = b.Commit(ctx, conv.Commit{
			ConversationID:    convID,
			ExpectHead:        before.HeadID,
			ExpectNextOrdinal: before.NextOrdinal,
			Rows:              []*conv.Node{row},
			NewHead:           row.ID,
			Finalize:          counting(&runs),
		})
		c.NoError(err, "Commit")
		c.Eq(1, runs, "the hook ran once")

		after, err := b.State(ctx, convID)
		c.NoError(err, "State after")
		c.Eq(row.ID, after.HeadID, "the head moved to the row")
		c.Eq(before.NextOrdinal+1, after.NextOrdinal, "the commit is visible")
	})

	t.Run("finalize error rolls the commit back", func(t *testing.T) {
		c := assert.NewAborting(t)
		b, convID := setup(t)
		ctx := context.Background()
		before, err := b.State(ctx, convID)
		c.NoError(err, "State")

		row := foreignRow(t, convID, "doomed", "", "")
		_, err = b.Commit(ctx, conv.Commit{
			ConversationID:    convID,
			ExpectHead:        before.HeadID,
			ExpectNextOrdinal: before.NextOrdinal,
			Rows:              []*conv.Node{row},
			NewHead:           row.ID,
			Finalize: func(context.Context, conv.TxExec) error {
				return errFinalizeBoom
			},
		})
		c.ErrorIs(err, errFinalizeBoom, "the hook's error")

		after, err := b.State(ctx, convID)
		c.NoError(err, "State after")
		c.Eq(before.HeadID, after.HeadID, "the head did not move")
		c.Eq(before.NextOrdinal, after.NextOrdinal, "no ordinal was allocated")

		_, err = b.Walk(ctx, convID, row.ID, false)
		c.ErrorIs(err, conv.ErrNotFound, "the refused row is not in the conversation")
	})

	t.Run("finalize does not run on a head conflict", func(t *testing.T) {
		c := assert.NewAborting(t)
		b, convID := setup(t)
		ctx := context.Background()
		root := foreignRow(t, convID, "root", "", "")
		c.NoError(commitDirectly(t, b, convID, []*conv.Node{root}, root.ID), "seed the conversation")

		row := foreignRow(t, convID, "stale head", root.ID, "")
		runs := 0
		_, err := b.Commit(ctx, conv.Commit{
			ConversationID:    convID,
			ExpectHead:        "",
			ExpectNextOrdinal: 0,
			Rows:              []*conv.Node{row},
			NewHead:           row.ID,
			Finalize:          counting(&runs),
		})
		c.ErrorIs(err, conv.ErrConflict, "a stale head is a conflict")
		c.Eq(0, runs, "a refused commit runs no hook")
	})

	t.Run("finalize does not run when a parent is not held", func(t *testing.T) {
		c := assert.NewAborting(t)
		b, create := newBackend(t)
		mine := uuid.Must(uuid.NewV7()).String()
		other := uuid.Must(uuid.NewV7()).String()
		create(mine)
		create(other)
		ctx := context.Background()

		foreign := foreignRow(t, other, "elsewhere", "", "")
		c.NoError(commitDirectly(t, b, other, []*conv.Node{foreign}, foreign.ID), "seed the other conversation")

		before, err := b.State(ctx, mine)
		c.NoError(err, "State")
		row := foreignRow(t, mine, "hangs off another conversation", foreign.ID, "")
		runs := 0
		_, err = b.Commit(ctx, conv.Commit{
			ConversationID:    mine,
			ExpectHead:        before.HeadID,
			ExpectNextOrdinal: before.NextOrdinal,
			Rows:              []*conv.Node{row},
			NewHead:           row.ID,
			Finalize:          counting(&runs),
		})
		c.ErrorIs(err, conv.ErrNotFound, "a parent outside the conversation is not found")
		c.Eq(0, runs, "a refused commit runs no hook")
	})

	t.Run("finalize runs for a commit with no rows", func(t *testing.T) {
		c := assert.NewAborting(t)
		b, convID := setup(t)
		ctx := context.Background()
		root := foreignRow(t, convID, "head only", "", "")
		c.NoError(commitDirectly(t, b, convID, []*conv.Node{root}, root.ID), "seed the conversation")

		before, err := b.State(ctx, convID)
		c.NoError(err, "State")
		runs := 0
		_, err = b.Commit(ctx, conv.Commit{
			ConversationID:    convID,
			ExpectHead:        before.HeadID,
			ExpectNextOrdinal: before.NextOrdinal,
			NewHead:           before.HeadID,
			Finalize:          counting(&runs),
		})
		c.NoError(err, "Commit")
		c.Eq(1, runs, "a commit that writes no row still runs its hook")

		after, err := b.State(ctx, convID)
		c.NoError(err, "State after")
		c.Eq(before.HeadID, after.HeadID, "the head stands")
		c.Eq(before.NextOrdinal, after.NextOrdinal, "no ordinal was allocated")
	})
}
