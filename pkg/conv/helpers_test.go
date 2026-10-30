// SPDX-License-Identifier: Apache-2.0

package conv_test

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"go.graveland.dev/rafiki/pkg/conv"
	"go.graveland.dev/rafiki/pkg/convtree"
	"go.graveland.dev/rafiki/pkg/msg"
	msganthropic "go.graveland.dev/rafiki/pkg/msg/anthropic"
)

// textMsg is a one-block message.
func textMsg(role msg.Role, text string) msg.Message {
	return msg.Message{Role: role, Content: []msg.Block{msg.Text{Text: text}}}
}

// newConv returns a memory backend and a freshly created conversation id.
func newConv(t *testing.T) (*conv.MemBackend, string) {
	t.Helper()
	b := conv.NewMemBackend()
	id := uuid.Must(uuid.NewV7()).String()
	b.Ensure(id)
	return b, id
}

// nodeIDs returns nodes' ids in the order given.
func nodeIDs(nodes []*conv.Node) []string {
	out := make([]string, len(nodes))
	for i, n := range nodes {
		out[i] = n.ID
	}
	return out
}

// ordinals returns nodes' ordinals in the order given.
func ordinals(nodes []*conv.Node) []int {
	out := make([]int, len(nodes))
	for i, n := range nodes {
		out[i] = n.Ordinal
	}
	return out
}

// intPtr returns a pointer to v, for a PageQuery or trim bound.
func intPtr(v int) *int { return &v }

// historyIDs returns a snapshot's node ids, chronological.
func historyIDs(sn *conv.Snapshot) []string {
	out := []string{}
	for n := range sn.History() {
		out = append(out, n.ID)
	}
	return out
}

// transcriptIDs returns a snapshot's transcript ids, chronological.
func transcriptIDs(sn *conv.Snapshot) []string {
	out := []string{}
	for n := range sn.Transcript() {
		out = append(out, n.ID)
	}
	return out
}

// recorder wraps a Backend and records the ids a Fetch was asked for.
type recorder struct {
	conv.Backend
	mu    sync.Mutex
	fetch [][]string
	state int
	walk  int
}

func (r *recorder) State(ctx context.Context, convID string) (conv.State, error) {
	r.mu.Lock()
	r.state++
	r.mu.Unlock()
	return r.Backend.State(ctx, convID)
}

func (r *recorder) Walk(ctx context.Context, convID, fromID string, crossRoots bool) ([]conv.Ref, error) {
	r.mu.Lock()
	r.walk++
	r.mu.Unlock()
	return r.Backend.Walk(ctx, convID, fromID, crossRoots)
}

func (r *recorder) Fetch(ctx context.Context, convID string, ids []string) ([]*conv.Node, error) {
	r.mu.Lock()
	r.fetch = append(r.fetch, append([]string(nil), ids...))
	r.mu.Unlock()
	return r.Backend.Fetch(ctx, convID, ids)
}

func (r *recorder) fetched() [][]string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([][]string(nil), r.fetch...)
}

func (r *recorder) reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.fetch = nil
	r.state = 0
	r.walk = 0
}

// clock is a time source a test drives by hand: the cache's only notion of
// time, so an entry expires when the test says so and a sweep needs no sleep.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

// newClock returns a clock stopped at a fixed instant.
func newClock() *clock {
	return &clock{t: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)}
}

// Now returns the clock's current instant.
func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

// Advance moves the clock forward by d.
func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// commitGate is the synchronisation a test uses to hold one Commit in flight.
// entered is closed when the commit is held, release unblocks it.
type commitGate struct {
	mu      sync.Mutex
	armed   bool
	entered chan struct{}
	release chan struct{}
}

// newCommitGate returns a gate that is not yet armed.
func newCommitGate() *commitGate {
	return &commitGate{entered: make(chan struct{}), release: make(chan struct{})}
}

// arm makes the next Commit hold.
func (g *commitGate) arm() {
	g.mu.Lock()
	g.armed = true
	g.mu.Unlock()
}

// take reports whether this Commit should hold, and spends the arm.
func (g *commitGate) take() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	hold := g.armed
	g.armed = false
	return hold
}

// hold signals that the commit is being held and waits for the test to release
// it.
func (g *commitGate) hold() {
	close(g.entered)
	<-g.release
}

// gatedCommit applies a commit and only then holds it: the window a writer spends
// between its rows landing and its position being published.
type gatedCommit struct {
	conv.Backend
	gate *commitGate
}

// Commit applies the commit, then holds it when armed.
func (g *gatedCommit) Commit(ctx context.Context, commit conv.Commit) (time.Time, error) {
	created, err := g.Backend.Commit(ctx, commit)
	if err != nil {
		return created, err
	}
	if g.gate.take() {
		g.gate.hold()
	}
	return created, nil
}

// blockedCommit holds a commit BEFORE it reaches the backend: the window a
// writer spends between resolving its position and its rows landing.
type blockedCommit struct {
	conv.Backend
	gate *commitGate
}

// Commit holds when armed, then applies the commit.
func (b *blockedCommit) Commit(ctx context.Context, commit conv.Commit) (time.Time, error) {
	if b.gate.take() {
		b.gate.hold()
	}
	return b.Backend.Commit(ctx, commit)
}

// pausedWrite runs write in the background with the gate armed, so the write has
// reached the gate by the time this returns. The returned function releases it,
// waits for it to finish and reports its error.
func pausedWrite(gate *commitGate, write func() error) func() error {
	gate.arm()
	stalled := make(chan struct{})
	var err error
	go func() {
		defer close(stalled)
		err = write()
	}()
	select {
	case <-gate.entered:
	case <-stalled:
		// The write finished without reaching the gate, so there is nothing to
		// wait for; its error says why.
	}
	var once sync.Once
	return func() error {
		once.Do(func() {
			close(gate.release)
			<-stalled
		})
		return err
	}
}

// pausedAppend is pausedWrite for an append, reporting what it inserted.
func pausedAppend(gate *commitGate, h *conv.Handle, a conv.Append) func() (conv.Result, error) {
	var res conv.Result
	release := pausedWrite(gate, func() error {
		var err error
		res, err = h.Append(context.Background(), a)
		return err
	})
	return func() (conv.Result, error) {
		err := release()
		return res, err
	}
}

// conflictOnce moves the head underneath the first Commit it receives and
// reports a conflict, then behaves normally.
type conflictOnce struct {
	conv.Backend
	mu    sync.Mutex
	hooks []func() error
	fired int
}

func (b *conflictOnce) Commit(ctx context.Context, commit conv.Commit) (time.Time, error) {
	b.mu.Lock()
	if b.fired < len(b.hooks) {
		hook := b.hooks[b.fired]
		b.fired++
		b.mu.Unlock()
		if err := hook(); err != nil {
			return time.Time{}, err
		}
		return time.Time{}, conv.ErrConflict
	}
	b.mu.Unlock()
	return b.Backend.Commit(ctx, commit)
}

// stalledState holds the first State call it is armed for until the test
// releases it, and returns the answer it read BEFORE waiting: that is how a
// reader's observation is made to land after a writer's move.
type stalledState struct {
	conv.Backend
	mu      sync.Mutex
	armed   bool
	entered chan struct{}
	release chan struct{}
}

func (b *stalledState) arm() {
	b.mu.Lock()
	b.armed = true
	b.mu.Unlock()
}

func (b *stalledState) State(ctx context.Context, convID string) (conv.State, error) {
	st, err := b.Backend.State(ctx, convID)
	if err != nil {
		return conv.State{}, err
	}
	b.mu.Lock()
	stall := b.armed
	b.armed = false
	b.mu.Unlock()
	if !stall {
		return st, nil
	}
	close(b.entered)
	<-b.release
	return st, nil
}

// failingCommit is a backend whose Commit always answers err.
type failingCommit struct {
	conv.Backend
	mu      sync.Mutex
	calls   int
	failure error
}

func (b *failingCommit) Commit(context.Context, conv.Commit) (time.Time, error) {
	b.mu.Lock()
	b.calls++
	b.mu.Unlock()
	return time.Time{}, b.failure
}

func (b *failingCommit) count() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.calls
}

// storedNode builds a node the way a backend holds one, for tests that write
// through Commit directly.
func storedNode(t *testing.T, convID, parentID string, slot convtree.Hash, role msg.Role, text string, ordinal, depth int) *conv.Node {
	t.Helper()
	message := textMsg(role, text)
	content, err := msganthropic.EncodeContent(message)
	if err != nil {
		t.Fatalf("encode content: %v", err)
	}
	hashes, err := convtree.Chain(slot, []convtree.Msg{{Role: role.String(), Content: content}})
	if err != nil {
		t.Fatalf("hash chain: %v", err)
	}
	return &conv.Node{
		Ref: conv.Ref{
			ID:             uuid.Must(uuid.NewV7()).String(),
			ConversationID: convID,
			ParentID:       parentID,
			Ordinal:        ordinal,
			Depth:          depth,
			PathHash:       hashes[0],
		},
		Msg:     message,
		Content: content,
	}
}
