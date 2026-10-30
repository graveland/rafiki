// SPDX-License-Identifier: Apache-2.0

package conv_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/multigres/testkit/assert"

	"go.graveland.dev/rafiki/pkg/conv"
	"go.graveland.dev/rafiki/pkg/conv/convtest"
	"go.graveland.dev/rafiki/pkg/msg"
)

// postCommitGate holds the first Commit it is armed for AFTER the backend has
// made it durable, until the test releases it: the window between a commit and
// the cache learning of it.
type postCommitGate struct {
	conv.Backend
	mu        sync.Mutex
	armed     bool
	committed chan struct{}
	release   chan struct{}
}

func newPostCommitGate(b conv.Backend) *postCommitGate {
	return &postCommitGate{Backend: b, committed: make(chan struct{}), release: make(chan struct{})}
}

func (b *postCommitGate) arm() {
	b.mu.Lock()
	b.armed = true
	b.mu.Unlock()
}

func (b *postCommitGate) Commit(ctx context.Context, commit conv.Commit) (time.Time, error) {
	created, err := b.Backend.Commit(ctx, commit)
	if err != nil {
		return time.Time{}, err
	}
	b.mu.Lock()
	hold := b.armed
	b.armed = false
	b.mu.Unlock()
	if hold {
		close(b.committed)
		<-b.release
	}
	return created, nil
}

// failingStateAfter answers the State call numbered n (counting from arm) with
// an error and every other call normally.
type failingStateAfter struct {
	conv.Backend
	mu    sync.Mutex
	until int
	calls int
}

func (b *failingStateAfter) arm(n int) {
	b.mu.Lock()
	b.until = n
	b.calls = 0
	b.mu.Unlock()
}

func (b *failingStateAfter) State(ctx context.Context, convID string) (conv.State, error) {
	b.mu.Lock()
	b.calls++
	fail := b.until > 0 && b.calls == b.until
	b.mu.Unlock()
	if fail {
		return conv.State{}, errors.New("state unavailable")
	}
	return b.Backend.State(ctx, convID)
}

// seedOne appends one user node through a store of its own, so the store under
// test starts cold over a conversation that has a head.
func seedOne(t *testing.T, b conv.Backend, convID string) *conv.Node {
	t.Helper()
	res, err := conv.NewStore(b, conv.Options{}).Open(convID, conv.AsOwner()).
		Append(context.Background(), conv.Append{FromHead: true, Nodes: []conv.NewNode{{Msg: textMsg(msg.User, "seed")}}})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	return res.Nodes[0]
}

func appendUser(ctx context.Context, h *conv.Handle, text string) (*conv.Node, error) {
	res, err := h.Append(ctx, conv.Append{FromHead: true, Nodes: []conv.NewNode{{Msg: textMsg(msg.User, text)}}})
	if err != nil {
		return nil, err
	}
	return res.Nodes[0], nil
}

func TestWarmOwnerReadIssuesNoBackendCalls(t *testing.T) {
	c := assert.NewAborting(t)
	b, convID := newConv(t)
	ctx := context.Background()
	seedOne(t, b, convID)
	counting := convtest.NewCounting(b)
	h := conv.NewStore(counting, conv.Options{}).Open(convID, conv.AsOwner())

	_, err := h.Working(ctx)
	c.NoError(err, "cold Working")
	c.Eq(1, counting.Counts()["State"], "the cold read asks the backend once")

	counting.Reset()
	path, err := h.Working(ctx)
	c.NoError(err, "warm Working")
	c.Len(path, 1, "the working path")
	sn, err := h.Snapshot(ctx)
	c.NoError(err, "warm Snapshot")
	c.Len(historyIDs(sn), 1, "the snapshot")
	counts := counting.Counts()
	c.Eq(0, counts["State"], "a warm read makes no State call")
	c.Eq(0, counts["Walk"], "no Walk")
	c.Eq(0, counts["Fetch"], "and no Fetch")
}

func TestCommitRefreshesTheCachedHead(t *testing.T) {
	c := assert.NewAborting(t)
	b, convID := newConv(t)
	ctx := context.Background()
	counting := convtest.NewCounting(b)
	h := conv.NewStore(counting, conv.Options{}).Open(convID, conv.AsOwner())

	first, err := appendUser(ctx, h, "a")
	c.NoError(err, "a")
	c.Eq(2, counting.Counts()["State"], "an append reads State to commit and once more to refresh")

	counting.Reset()
	path, err := h.Working(ctx)
	c.NoError(err, "Working")
	c.EqDeep([]string{first.ID}, nodeIDs(path), "the read sees the commit")
	c.Eq(0, counting.Counts()["State"], "with no State call of its own")

	second, err := appendUser(ctx, h, "b")
	c.NoError(err, "b")
	counting.Reset()
	path, err = h.Working(ctx)
	c.NoError(err, "Working")
	c.EqDeep([]string{first.ID, second.ID}, nodeIDs(path), "and the next commit")
	c.Eq(0, counting.Counts()["State"], "again with no State call")

	c.NoError(h.RetractHead(ctx, second.ID), "retract")
	counting.Reset()
	path, err = h.Working(ctx)
	c.NoError(err, "Working")
	c.EqDeep([]string{first.ID}, nodeIDs(path), "a retract refreshes too")
	c.Eq(0, counting.Counts()["State"], "with no State call")
}

// A swept entry can never win the install: the loop restarts on the
// conversation's current entry and installs what the backend says then.
func TestUpdateRestartsWhenTheEntryDies(t *testing.T) {
	c := assert.NewAborting(t)
	b, convID := newConv(t)
	ctx := context.Background()
	seed := seedOne(t, b, convID)
	counting := convtest.NewCounting(b)
	stalled := &stalledState{Backend: counting, entered: make(chan struct{}), release: make(chan struct{})}
	clk := newClock()
	s := conv.NewStore(stalled, conv.Options{Now: clk.Now})

	stalled.arm()
	var path []*conv.Node
	var readErr error
	done := make(chan struct{})
	go func() {
		defer close(done)
		path, readErr = s.Open(convID).Working(ctx)
	}()
	<-stalled.entered

	clk.Advance(10 * time.Minute)
	s.Open(convID)
	added, err := appendUser(ctx, s.Open(convID, conv.AsOwner()), "after the sweep")
	c.NoError(err, "append")
	close(stalled.release)
	<-done

	c.NoError(readErr, "Working")
	c.EqDeep([]string{seed.ID, added.ID}, nodeIDs(path), "the restarted read sees the commit, not the dead entry's observation")
	c.Eq(4, counting.Counts()["State"], "the read asked twice, the append twice")

	counting.Reset()
	path, err = s.Open(convID, conv.AsOwner()).Working(ctx)
	c.NoError(err, "Working")
	c.EqDeep([]string{seed.ID, added.ID}, nodeIDs(path), "the live entry holds the same head")
	c.Eq(0, counting.Counts()["State"], "and serves it warm")
}

// A read that began before a commit's refresh loses its install: the cache ends
// at the commit, never at the pre-commit head the read observed.
func TestStaleObservationCannotInstall(t *testing.T) {
	c := assert.NewAborting(t)
	b, convID := newConv(t)
	ctx := context.Background()
	seed := seedOne(t, b, convID)
	counting := convtest.NewCounting(b)
	stalled := &stalledState{Backend: counting, entered: make(chan struct{}), release: make(chan struct{})}
	s := conv.NewStore(stalled, conv.Options{})

	stalled.arm()
	var path []*conv.Node
	var readErr error
	done := make(chan struct{})
	go func() {
		defer close(done)
		path, readErr = s.Open(convID).Working(ctx)
	}()
	<-stalled.entered

	added, err := appendUser(ctx, s.Open(convID, conv.AsOwner()), "during the read")
	c.NoError(err, "append")
	close(stalled.release)
	<-done

	c.NoError(readErr, "Working")
	c.EqDeep([]string{seed.ID, added.ID}, nodeIDs(path), "the losing read retried and saw the commit")
	c.Eq(4, counting.Counts()["State"], "the read asked twice, the append twice")

	counting.Reset()
	path, err = s.Open(convID, conv.AsOwner()).Working(ctx)
	c.NoError(err, "next Working")
	c.EqDeep([]string{seed.ID, added.ID}, nodeIDs(path), "the next read sees the commit")
	c.Eq(0, counting.Counts()["State"], "from the cache")
}

// A commit that is durable but not yet refreshed cannot be published later over
// a newer one: the late refresh reads the backend, which holds the newer head.
func TestDelayedCommitCannotOutrankANewerOne(t *testing.T) {
	c := assert.NewAborting(t)
	b, convID := newConv(t)
	ctx := context.Background()
	seed := seedOne(t, b, convID)
	counting := convtest.NewCounting(b)
	gate := newPostCommitGate(counting)
	clk := newClock()
	s := conv.NewStore(gate, conv.Options{Now: clk.Now})

	gate.arm()
	var first *conv.Node
	var firstErr error
	done := make(chan struct{})
	go func() {
		defer close(done)
		first, firstErr = appendUser(ctx, s.Open(convID, conv.AsOwner()), "first")
	}()
	<-gate.committed

	clk.Advance(10 * time.Minute)
	s.Open(convID)
	second, err := appendUser(ctx, s.Open(convID, conv.AsOwner()), "second")
	c.NoError(err, "second append")
	close(gate.release)
	<-done
	c.NoError(firstErr, "first append")

	counting.Reset()
	path, err := s.Open(convID, conv.AsOwner()).Working(ctx)
	c.NoError(err, "Working")
	c.EqDeep([]string{seed.ID, first.ID, second.ID}, nodeIDs(path), "the delayed commit did not displace the newer head")
	c.Eq(0, counting.Counts()["State"], "and the cache holds it")
	st, err := b.State(ctx, convID)
	c.NoError(err, "backend State")
	c.Eq(second.ID, st.HeadID, "the backend agrees")
}

// A write that cannot refresh still happened; the cache drops its position
// rather than keep the pre-commit head.
func TestFailedRefreshInvalidatesTheCachedHead(t *testing.T) {
	c := assert.NewAborting(t)
	b, convID := newConv(t)
	ctx := context.Background()
	failing := &failingStateAfter{Backend: b}
	h := conv.NewStore(failing, conv.Options{}).Open(convID, conv.AsOwner())

	first, err := appendUser(ctx, h, "a")
	c.NoError(err, "a")

	failing.arm(2)
	second, err := appendUser(ctx, h, "b")
	c.NoError(err, "the append succeeded although its refresh could not")

	path, err := h.Working(ctx)
	c.NoError(err, "Working")
	c.EqDeep([]string{first.ID, second.ID}, nodeIDs(path), "the next read asks the backend instead of serving the pre-commit head")
}

// The version orders this process's observations only, so a commit from another
// process is picked up when the cached head ages out, however busy the entry
// keeps itself in the meantime.
func TestCachedHeadAgesOutDespiteActiveReads(t *testing.T) {
	c := assert.NewAborting(t)
	b, convID := newConv(t)
	ctx := context.Background()
	clk := newClock()
	h := conv.NewStore(b, conv.Options{Now: clk.Now}).Open(convID, conv.AsOwner())
	first, err := appendUser(ctx, h, "a")
	c.NoError(err, "a")

	other, err := appendUser(ctx, conv.NewStore(b, conv.Options{}).Open(convID, conv.AsOwner()), "from another process")
	c.NoError(err, "other process append")

	for i := 0; i < 2; i++ {
		clk.Advance(2 * time.Minute)
		path, err := h.Working(ctx)
		c.NoError(err, "Working")
		c.EqDeep([]string{first.ID}, nodeIDs(path), "within the TTL the cached head is served")
	}
	clk.Advance(2 * time.Minute)
	path, err := h.Working(ctx)
	c.NoError(err, "Working")
	c.EqDeep([]string{first.ID, other.ID}, nodeIDs(path), "past the TTL it is refreshed although every read touched the entry")
}

// A handle that is not the owner never serves a cached head: the conversation's
// writer may be another process, so every read asks the backend once.
func TestReaderAsksTheBackendForTheHeadOnEveryRead(t *testing.T) {
	c := assert.NewAborting(t)
	b, convID := newConv(t)
	ctx := context.Background()
	counting := convtest.NewCounting(b)
	s := conv.NewStore(counting, conv.Options{})
	owner := s.Open(convID, conv.AsOwner())
	first, err := appendUser(ctx, owner, "a")
	c.NoError(err, "a")

	reader := s.Open(convID)
	for i := 0; i < 2; i++ {
		counting.Reset()
		path, err := reader.Working(ctx)
		c.NoError(err, "Working")
		c.EqDeep([]string{first.ID}, nodeIDs(path), "the working path")
		c.Eq(1, counting.Counts()["State"], "a reader asks the backend for the head exactly once")
		c.Eq(0, counting.Counts()["Fetch"], "and fetches nothing when the nodes are cached")
	}

	other, err := appendUser(ctx, conv.NewStore(b, conv.Options{}).Open(convID, conv.AsOwner()), "from another process")
	c.NoError(err, "other process append")
	path, err := reader.Working(ctx)
	c.NoError(err, "Working")
	c.EqDeep([]string{first.ID, other.ID}, nodeIDs(path), "a reader sees another process's commit at once")
}
