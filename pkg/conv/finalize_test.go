// SPDX-License-Identifier: Apache-2.0

package conv_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/multigres/testkit/assert"

	"go.graveland.dev/rafiki/pkg/conv"
	"go.graveland.dev/rafiki/pkg/convtree"
	"go.graveland.dev/rafiki/pkg/msg"
)

// dropsFinalize is a backend that commits an Append's rows without running its
// Finalize hook: the contract Handle.Append has to report.
type dropsFinalize struct{ conv.Backend }

func (d dropsFinalize) Commit(ctx context.Context, c conv.Commit) (time.Time, error) {
	c.Finalize = nil
	return d.Backend.Commit(ctx, c)
}

// firstCommitConflicts refuses its first Commit with ErrConflict and delegates
// from the second on, so a retried append reaches the backend twice.
type firstCommitConflicts struct {
	conv.Backend
	mu    sync.Mutex
	fired bool
}

func (b *firstCommitConflicts) Commit(ctx context.Context, c conv.Commit) (time.Time, error) {
	b.mu.Lock()
	first := !b.fired
	b.fired = true
	b.mu.Unlock()
	if first {
		return time.Time{}, conv.ErrConflict
	}
	return b.Backend.Commit(ctx, c)
}

// hookRecord is a Finalize hook that remembers how often it ran, the Result it
// was handed, and whether each node it saw was still unstamped.
type hookRecord struct {
	runs   int
	res    conv.Result
	zeroAt []bool
}

func (r *hookRecord) hook(err error) func(context.Context, conv.TxExec, conv.Result) error {
	return func(_ context.Context, _ conv.TxExec, res conv.Result) error {
		r.runs++
		r.res = res
		r.zeroAt = nil
		for _, n := range res.Nodes {
			r.zeroAt = append(r.zeroAt, n.CreatedAt.IsZero())
		}
		return err
	}
}

// The hook sees the chain as the commit leaves it: every node of the append in
// order, its id and ordinal already assigned, and no created_at, which only the
// backend's commit can stamp.
func TestFinalizeRunsOnceWithTheCommittedIdentities(t *testing.T) {
	c := assert.NewAborting(t)
	b, convID := newConv(t)
	ctx := context.Background()
	h := conv.NewStore(b, conv.Options{}).Open(convID, conv.AsOwner())

	var rec hookRecord
	res, err := h.Append(ctx, conv.Append{
		FromHead: true,
		Nodes: []conv.NewNode{
			{Msg: textMsg(msg.User, "one")},
			{Msg: textMsg(msg.Assistant, "two")},
		},
		Finalize: rec.hook(nil),
	})
	c.NoError(err, "append")
	c.Eq(1, rec.runs, "the hook ran once")
	c.Eq(2, rec.res.Inserted, "both nodes were inserted")
	c.Len(rec.res.Nodes, 2, "the hook sees the whole chain")
	c.EqDeep(nodeIDs(res.Nodes), nodeIDs(rec.res.Nodes), "the same ids in the same order")
	c.EqDeep(ordinals(res.Nodes), ordinals(rec.res.Nodes), "the same ordinals in the same order")
	c.EqDeep([]bool{true, true}, rec.zeroAt, "no node is stamped inside the hook")
	for i, n := range res.Nodes {
		c.False(n.CreatedAt.IsZero(), "node %d is stamped once the append returned", i)
	}
}

// A hook error rolls the append back: the head and the ordinal allocation are
// the ones the attempt read, and no row exists.
func TestFinalizeErrorRollsTheAppendBack(t *testing.T) {
	c := assert.NewAborting(t)
	b, convID := newConv(t)
	ctx := context.Background()
	s := conv.NewStore(b, conv.Options{})
	h := s.Open(convID, conv.AsOwner())

	before, err := h.State(ctx)
	c.NoError(err, "State")
	errBoom := errors.New("boom")
	var rec hookRecord
	_, err = h.Append(ctx, conv.Append{
		FromHead: true,
		Nodes:    []conv.NewNode{{Msg: textMsg(msg.User, "doomed")}},
		Finalize: rec.hook(errBoom),
	})
	c.ErrorIs(err, errBoom, "the hook's error")
	c.Eq(1, rec.runs, "a hook that ran failed the attempt")

	after, err := h.State(ctx)
	c.NoError(err, "State after")
	c.Eq(before.HeadID, after.HeadID, "the head did not move")
	c.Eq(before.NextOrdinal, after.NextOrdinal, "no ordinal was allocated")

	nodes, err := s.Transcript(ctx, convID)
	c.NoError(err, "Transcript")
	c.Empty(nodes, "the append left no row")
}

// A hook error that wraps ErrConflict is still a hook error: the rows a
// conflicting commit would leave were never written, so retrying writes them
// again against a tree that agrees with the attempt.
func TestFinalizeErrorWrappingConflictIsNotRetried(t *testing.T) {
	c := assert.NewAborting(t)
	b, convID := newConv(t)
	ctx := context.Background()
	h := conv.NewStore(b, conv.Options{}).Open(convID, conv.AsOwner())

	var rec hookRecord
	_, err := h.Append(ctx, conv.Append{
		FromHead: true,
		Nodes:    []conv.NewNode{{Msg: textMsg(msg.User, "conflicting hook")}},
		Finalize: rec.hook(fmt.Errorf("x: %w", conv.ErrConflict)),
	})
	c.ErrorIs(err, conv.ErrConflict, "the hook's wrapped conflict is returned")
	c.ErrorContains(err, "conv: finalize: x:", "the hook's error is reported as a finalize error")
	c.Eq(1, rec.runs, "a hook error is never retried")
	c.NotErrorIs(err, conv.ErrFinalizeNotRun, "the hook did run")
}

// An attempt that loses the head race never reaches a hook: only the attempt
// whose commit lands runs it.
func TestFinalizeRunsOnlyOnTheAttemptThatCommits(t *testing.T) {
	c := assert.NewAborting(t)
	m, convID := newConv(t)
	ctx := context.Background()
	h := conv.NewStore(&firstCommitConflicts{Backend: m}, conv.Options{}).Open(convID, conv.AsOwner())

	var rec hookRecord
	res, err := h.Append(ctx, conv.Append{
		FromHead: true,
		Nodes:    []conv.NewNode{{Msg: textMsg(msg.User, "retried")}},
		Finalize: rec.hook(nil),
	})
	c.NoError(err, "append after the conflict")
	c.Eq(1, rec.runs, "the hook ran once")
	c.Eq(1, res.Inserted, "one row inserted")
	c.EqDeep(nodeIDs(res.Nodes), nodeIDs(rec.res.Nodes), "the hook saw the row that committed")
}

// A backend that commits without running the hook stored rows whose companion
// writes are missing, which the caller must not read as success.
func TestFinalizeBackendThatIgnoresTheHookIsReported(t *testing.T) {
	c := assert.NewAborting(t)
	m, convID := newConv(t)
	ctx := context.Background()
	h := conv.NewStore(dropsFinalize{Backend: m}, conv.Options{}).Open(convID, conv.AsOwner())

	var rec hookRecord
	_, err := h.Append(ctx, conv.Append{
		FromHead: true,
		Nodes:    []conv.NewNode{{Msg: textMsg(msg.User, "hook skipped")}},
		Finalize: rec.hook(nil),
	})
	c.ErrorIs(err, conv.ErrFinalizeNotRun, "a backend that skipped the hook is reported")
	c.Eq(0, rec.runs, "the hook never ran")
}

// A replay inserts nothing and still runs the hook: the chain is the same one,
// so the companion writes are for a chain the conversation already holds.
func TestFinalizeRunsOnAReplayThatInsertsNothing(t *testing.T) {
	c := assert.NewAborting(t)
	b, convID := newConv(t)
	ctx := context.Background()
	h := conv.NewStore(b, conv.Options{}).Open(convID, conv.AsOwner())

	chain := []conv.NewNode{
		{Msg: textMsg(msg.User, "replay one")},
		{Msg: textMsg(msg.Assistant, "replay two")},
	}
	first, err := h.Append(ctx, conv.Append{Root: convtree.RootKindRoot, Nodes: chain})
	c.NoError(err, "first append")
	c.Eq(2, first.Inserted, "the chain was new")

	var rec hookRecord
	replay, err := h.Append(ctx, conv.Append{
		Root:     convtree.RootKindRoot,
		Nodes:    chain,
		Finalize: rec.hook(nil),
	})
	c.NoError(err, "replay")
	c.Eq(1, rec.runs, "the hook ran on the replay")
	c.Eq(0, rec.res.Inserted, "the replay inserted nothing")
	c.Len(rec.res.Nodes, len(chain), "the hook sees the whole chain")
	c.EqDeep(nodeIDs(first.Nodes), nodeIDs(rec.res.Nodes), "the chain's committed ids")
	c.Eq(0, replay.Inserted, "the replay reports no insert")
}

// An append with no hook is untouched by the check that reports a skipped one.
func TestAppendWithoutFinalizeIsUnchanged(t *testing.T) {
	c := assert.NewAborting(t)
	b, convID := newConv(t)
	ctx := context.Background()
	h := conv.NewStore(b, conv.Options{}).Open(convID, conv.AsOwner())

	res, err := h.Append(ctx, conv.Append{FromHead: true, Nodes: []conv.NewNode{{Msg: textMsg(msg.User, "plain")}}})
	c.NoError(err, "append")
	c.Eq(1, res.Inserted, "one row inserted")
	c.NotErrorIs(err, conv.ErrFinalizeNotRun, "no hook, nothing to report")

	st, err := h.State(ctx)
	c.NoError(err, "State")
	c.Eq(res.Nodes[0].ID, st.HeadID, "the head moved to the row")
}
