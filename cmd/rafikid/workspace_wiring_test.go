package main

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"go.graveland.dev/rafiki/pkg/childstore"
	"go.graveland.dev/rafiki/pkg/clock"
	"go.graveland.dev/rafiki/pkg/eventbuf"
	"go.graveland.dev/rafiki/pkg/execpool"
	"go.graveland.dev/rafiki/pkg/executorpb"
	"go.graveland.dev/rafiki/pkg/executorpb/executorpbconnect"
	"go.graveland.dev/rafiki/pkg/executors"
	"go.graveland.dev/rafiki/pkg/inject"
	"go.graveland.dev/rafiki/pkg/protocol"

	"github.com/multigres/testkit/assert"
)

const (
	lostExecID  = "exec-lost-000001"
	otherExecID = "exec-other-0002"
)

// The steer text, verbatim. Spelled out rather than built with executorLostSteer
// so a reworded or softened notice fails these tests instead of moving with
// them: the agent has to be told that calls to the machine fail, that the
// workspace is rebuilt when it returns, and that uncommitted work there is at
// risk — and that everything else still works.
const lostSteerBuild1 = "EXECUTOR LOST: executor build-1 disconnected. Calls to it fail until it reconnects; when it does, your workspace there is rebuilt and anything not committed on that machine may be gone. Your other tools, and any other executors, still work."

const lostSteerCiRunner7 = "EXECUTOR LOST: executor ci-runner-7 disconnected. Calls to it fail until it reconnects; when it does, your workspace there is rebuilt and anything not committed on that machine may be gone. Your other tools, and any other executors, still work."

// The fallback name: shortID truncates to 12 characters, and a steer that names
// nothing at all ("executor  disconnected") would be worse than one naming a
// fragment of an id.
const lostSteerShortID = "EXECUTOR LOST: executor exec-lost-00 disconnected. Calls to it fail until it reconnects; when it does, your workspace there is rebuilt and anything not committed on that machine may be gone. Your other tools, and any other executors, still work."

// lostFixture: a controller with the daemon's event-buffer shape — a manual
// clock and a capturing flush, so a steer is an assertion rather than a wait —
// the given executor pool, and the DURABLE executor rows an operator would have
// (which is where the lost machine's name comes from: by the time a park
// expires, the executor is already out of the live pool).
func lostFixture(t *testing.T, pool executorPool, rows ...executors.Executor) (*Controller, *clock.FakeClock, *capturedFlush) {
	t.Helper()
	clk := clock.NewFakeClock(time.Unix(0, 0))
	buf := eventbuf.New(eventbuf.Config{Debounce: 5 * time.Second}, clk)
	cp := &capturedFlush{}
	buf.SetFlush(cp.fn)
	buf.SetBusy(func(string) bool { return false })
	store := newFakeExecStore()
	for _, e := range rows {
		store.execs[e.ID] = e
	}
	return &Controller{st: childstore.New(), cm: newChildManager(), execPool: pool, execStore: store, evbuf: buf}, clk, cp
}

// row is a durable executor row carrying the machine label an operator gave it.
func row(id, machine string) executors.Executor {
	return executors.Executor{ID: id, Enabled: true, Labels: map[string]string{"machine": machine}}
}

// bindingOn gives the child a live boundExecutor holding ONE provisioned
// binding on execID, which is the state a running child is in when its machine
// disappears. home records execID as the child's home, as spawn or a migration
// would.
func bindingOn(t *testing.T, c *Controller, childID, execID string, home bool) *boundExecutor {
	t.Helper()
	be := newBoundExecutor(childID, newFakeBinder(&fakeExec{id: execID}))
	if home {
		be.setHome(execID)
	}
	if _, err := be.ensureBinding(context.Background(), execID, home); err != nil {
		t.Fatalf("binding %s to %s: %v", childID, execID, err)
	}
	c.noteBoundExecutor(childID, be)
	return be
}

// seedChildOn inserts a running child whose HOME binding is on execID.
func seedChildOn(c *Controller, childID, execID, wsID, mode string) {
	c.st.Insert(&childstore.Session{
		ChildID: childID, Status: protocol.StatusStreaming, StartedAt: time.Now(),
		Kind: protocol.KindFundi,
		Labels: map[string]string{
			"rafiki/executor":       execID,
			"rafiki/workspace":      wsID,
			"rafiki/workspace-mode": mode,
			"rafiki/home-executor":  execID,
		},
	})
}

// Losing an executor must not fail the child. The old handler steered a pinned
// child "executor lost — pinned workspace cannot be rescheduled" and killed it
// ten seconds later, so a pinned child lost its work whenever a laptop closed;
// the design now says losing a machine never fails a child, single-executor
// included.
//
// The steer assertion is what pins that. Its OLD text is this test's failure
// mode: the old code pushed a failChild steer through this same push, so the
// recorded fragment would be the "cannot be rescheduled" notice. A "no Kill"
// assertion would NOT catch it — the Kill was deferred ten seconds and no test
// waits that long, so the old code looks innocent on that reading.
func TestHandleExecutorLostKeepsPinnedChildRunning(t *testing.T) {
	ck := assert.NewAborting(t)
	c, clk, cp := lostFixture(t, &fakePool{}, row(lostExecID, "build-1"))
	seedChildOn(c, "c_pinned", lostExecID, "ws-home", "pinned")
	be := bindingOn(t, c, "c_pinned", lostExecID, true)

	c.HandleExecutorLost(lostExecID)
	clk.Advance(6 * time.Second)

	snap, ok := c.st.Get("c_pinned")
	ck.Require().True(ok, "the child's row must survive losing its executor")
	ck.Eq(protocol.StatusStreaming, snap.Status, "losing an executor must never fail the child; status =")
	ck.Eq("unbound", snap.Labels["rafiki/executor-state"], "the child is unbound until something binds it; executor-state =")
	_, claims := snap.Labels["rafiki/workspace"]
	ck.False(claims, "rafiki/workspace must be dropped: that workspace died with the machine")
	ck.Eq(lostExecID, snap.Labels["rafiki/executor"], "rafiki/executor names the machine the child's cwd lives on; =")
	ck.Eq("pinned", snap.Labels["rafiki/workspace-mode"], "rafiki/workspace-mode decides where a rebuild may happen; =")
	ck.Eq(lostExecID, snap.Labels["rafiki/home-executor"], "the home executor is where a pinned child is re-provisioned; =")

	ck.Empty(be.Bindings(), "a binding on the lost machine must be forgotten, or the next call uses a client whose transport is dead")
	if _, _, bound := be.Current(); bound {
		t.Fatal("the child still reports a home binding on the lost machine")
	}

	batches := cp.batches()
	ck.Len(batches, 1, "want exactly one steer, got %d: %+v", len(batches), batches)
	ck.Eq("c_pinned", batches[0].childID, "steer went to")
	ck.Len(batches[0].fragments, 1, "one fragment")
	ck.Eq(lostSteerBuild1, batches[0].fragments[0], "the steer must be the new notice, not a 'cannot be rescheduled' failure; got")
}

// The mode makes no difference: an ephemeral child is kept running too, and the
// handler does not re-select a machine for it here. Migration happens on the
// child's next implicit call, in boundExecutor, where the reachable set is read
// live — so nothing at loss time provisions anywhere.
func TestHandleExecutorLostKeepsEphemeralChildRunning(t *testing.T) {
	ck := assert.NewAborting(t)
	c, clk, cp := lostFixture(t, &fakePool{}, row(lostExecID, "build-1"))
	seedChildOn(c, "c_eph", lostExecID, "ws-home", "ephemeral")
	be := bindingOn(t, c, "c_eph", lostExecID, true)

	c.HandleExecutorLost(lostExecID)
	clk.Advance(6 * time.Second)

	snap, ok := c.st.Get("c_eph")
	ck.Require().True(ok, "the child's row must survive losing its executor")
	ck.Eq(protocol.StatusStreaming, snap.Status, "an ephemeral child is not failed either; status =")
	ck.Eq("ephemeral", snap.Labels["rafiki/workspace-mode"], "the mode is kept; =")
	_, claims := snap.Labels["rafiki/workspace"]
	ck.False(claims, "the dead workspace must not still be claimed")
	ck.Empty(be.Bindings(), "nothing is re-provisioned at loss time — the next call re-binds")

	batches := cp.batches()
	ck.Len(batches, 1, "want exactly one steer, got %d: %+v", len(batches), batches)
	ck.Eq(lostSteerBuild1, batches[0].fragments[0], "steer")
}

// A child whose HOME is elsewhere can still hold a workspace on the lost
// machine — one it named for a call, which the labels never mention. That
// binding is released and forgotten, and the home binding is left strictly
// alone: its workspace is live, its labels still name it, and no steer pretends
// the child's tree moved.
func TestHandleExecutorLostReleasesNonHomeBinding(t *testing.T) {
	ck := assert.NewAborting(t)
	pool, rec := joinReleaseRecorder(t, ex(lostExecID, map[string]string{"machine": "build-1"}, "").Executor)
	c, clk, cp := lostFixture(t, pool, row(lostExecID, "build-1"))

	fb := newFakeBinder(&fakeExec{id: lostExecID}, &fakeExec{id: otherExecID})
	be := newBoundExecutor("c_multi", fb)
	be.setHome(otherExecID)
	if _, err := be.ensureBinding(context.Background(), otherExecID, true); err != nil {
		t.Fatalf("home binding: %v", err)
	}
	if _, err := be.ensureBinding(context.Background(), lostExecID, false); err != nil {
		t.Fatalf("named-call binding: %v", err)
	}
	homeExec, homeWS, ok := be.Current()
	ck.Require().True(ok, "test setup: the home binding must exist")
	lostWS := be.Bindings()[lostExecID]
	ck.NotEq("", lostWS, "test setup: the lost machine's workspace id")

	c.st.Insert(&childstore.Session{
		ChildID: "c_multi", Status: protocol.StatusStreaming, StartedAt: time.Now(),
		Kind: protocol.KindFundi,
		Labels: map[string]string{
			"rafiki/executor":       homeExec,
			"rafiki/workspace":      homeWS,
			"rafiki/workspace-mode": "pinned",
			"rafiki/home-executor":  homeExec,
		},
	})
	c.noteBoundExecutor("c_multi", be)

	c.HandleExecutorLost(lostExecID)
	clk.Advance(6 * time.Second)

	ck.EqDeep([]string{lostWS}, rec.releasedIDs(), "only the lost machine's workspace may be released; the home one is live")
	ck.EqDeep(map[string]string{otherExecID: homeWS}, be.Bindings(), "bindings after the loss")
	if execID, wsID, bound := be.Current(); !bound || execID != otherExecID || wsID != homeWS {
		t.Fatalf("the home binding was disturbed: exec=%q ws=%q bound=%v", execID, wsID, bound)
	}

	snap, ok := c.st.Get("c_multi")
	ck.Require().True(ok, "the child's row must survive")
	ck.Eq(homeWS, snap.Labels["rafiki/workspace"], "the labels name the HOME workspace and must be untouched; =")
	ck.Eq(homeExec, snap.Labels["rafiki/executor"], "rafiki/executor (the home machine); =")
	_, unbound := snap.Labels["rafiki/executor-state"]
	ck.False(unbound, "a child with a live home binding is not unbound")

	batches := cp.batches()
	ck.Len(batches, 1, "the child must still be told; want one steer, got %d", len(batches))
	ck.Eq(lostSteerBuild1, batches[0].fragments[0], "steer")
}

// The steer names the machine the operator knows, and rides the executor source
// as a STEER (it interrupts the turn: a child mid-call to a dead machine must
// not spend the rest of that turn believing it still has one).
func TestHandleExecutorLostSteersChild(t *testing.T) {
	ck := assert.NewAborting(t)
	c, clk, cp := lostFixture(t, &fakePool{}, row("exec-ci-runner-0003", "ci-runner-7"))
	seedChildOn(c, "c_worker", "exec-ci-runner-0003", "ws-home", "pinned")

	c.HandleExecutorLost("exec-ci-runner-0003")
	clk.Advance(6 * time.Second)

	batches := cp.batches()
	ck.Require().Len(batches, 1, "want one steer, got %d: %+v", len(batches), batches)
	ck.Eq("c_worker", batches[0].childID, "steer went to")
	ck.Eq(string(inject.KindExecutor), batches[0].source, "source")
	ck.Require().Len(batches[0].fragments, 1, "want one fragment, got %d", len(batches[0].fragments))
	ck.Eq(lostSteerCiRunner7, batches[0].fragments[0], "steer text")
}

// The name must survive the executor leaving the live pool, because that is
// exactly when it is needed: pkg/execpool removes an executor from Live() BEFORE
// it parks it, so a name read from the live set would always be a short id by
// the time a park expires. This executor is NOT in the pool — only its durable
// row is — and the steer still names the machine.
func TestHandleExecutorLostNamesAParkedExecutorsMachine(t *testing.T) {
	ck := assert.NewAborting(t)
	c, clk, cp := lostFixture(t, &fakePool{}, row(lostExecID, "build-1"))
	ck.Empty(c.execPool.Live(), "test setup: a parked executor must not be in the live set")
	seedChildOn(c, "c_parked", lostExecID, "ws-home", "pinned")

	c.HandleExecutorLost(lostExecID)
	clk.Advance(6 * time.Second)

	batches := cp.batches()
	ck.Require().Len(batches, 1, "want one steer, got %d: %+v", len(batches), batches)
	ck.Require().Len(batches[0].fragments, 1, "want one fragment, got %d", len(batches[0].fragments))
	ck.Eq(lostSteerBuild1, batches[0].fragments[0], "the steer must name the machine from the row, not the id; got")
}

// With no machine label to name, the steer falls back to a short id — never to
// an empty name, which would leave a child told "executor  disconnected".
func TestHandleExecutorLostFallsBackToAShortIDWithoutAMachineLabel(t *testing.T) {
	cases := []struct {
		name string
		rows []executors.Executor
	}{
		{"row carries no machine label", []executors.Executor{{
			ID: lostExecID, Enabled: true, Labels: map[string]string{"env": "home"},
		}}},
		{"row is gone", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, clk, cp := lostFixture(t, &fakePool{}, tc.rows...)
			seedChildOn(c, "c_unnamed", lostExecID, "ws-home", "pinned")

			c.HandleExecutorLost(lostExecID)
			clk.Advance(6 * time.Second)

			batches := cp.batches()
			ck := assert.NewAborting(t)
			ck.Require().Len(batches, 1, "want one steer, got %d: %+v", len(batches), batches)
			ck.Require().Len(batches[0].fragments, 1, "want one fragment, got %d", len(batches[0].fragments))
			ck.Eq(lostSteerShortID, batches[0].fragments[0], "steer text")
		})
	}
}

// --- executor-side plumbing for the release assertion ---

// releaseRecorder is a fake executor that records every Release it is asked to
// perform. It is the only way a unit test can see a workspace actually torn
// down: Controller.releaseWorkspace reaches the executor through a real
// *execpool.Pool and does nothing with any other implementation.
type releaseRecorder struct {
	relayDescribeHandler
	mu       sync.Mutex
	released []string
}

func (h *releaseRecorder) Release(_ context.Context, req *connect.Request[executorpb.ReleaseRequest]) (*connect.Response[executorpb.ReleaseResponse], error) {
	h.mu.Lock()
	h.released = append(h.released, req.Msg.WorkspaceId)
	h.mu.Unlock()
	return connect.NewResponse(&executorpb.ReleaseResponse{}), nil
}

func (h *releaseRecorder) releasedIDs() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.released...)
}

// namedExecStore is enrollingStore with a Get that answers the SAME row the
// executor enrolled with, so the pool's periodic refreshRow (30s, and it keeps
// whatever Get returns) cannot replace the machine label with an empty row.
type namedExecStore struct {
	enrollingStore
}

func (s namedExecStore) Get(context.Context, string) (executors.Executor, error) {
	return s.exec, nil
}

// joinReleaseRecorder admits one fake executor into a real Pool over a unix
// socket — execpool.Connect on one end, Pool.Serve on the other — exactly the
// production path, so a release the controller makes really crosses the wire.
func joinReleaseRecorder(t *testing.T, exec executors.Executor) (*execpool.Pool, *releaseRecorder) {
	t.Helper()
	c := assert.NewAborting(t)

	// macOS TempDir paths often exceed the unix-socket path length limit;
	// /tmp directly with a short name avoids it.
	dir, err := os.MkdirTemp("/tmp", "lost-")
	c.NoError(err)
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "s")

	ln, err := net.Listen("unix", sock)
	c.NoError(err)
	t.Cleanup(func() { _ = ln.Close() })

	pool := execpool.New(namedExecStore{enrollingStore{exec: exec}})
	go func() { _ = pool.Serve(ln) }()

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	rec := &releaseRecorder{relayDescribeHandler: relayDescribeHandler{executorID: exec.ID}}
	_, h := executorpbconnect.NewExecutorServiceHandler(rec)
	go func() {
		_ = execpool.Connect(ctx, execpool.ConnectOptions{
			SocketPath:  sock,
			EnrollToken: "t_test",
			Handler:     h,
		})
	}()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if len(pool.Live()) == 1 {
			return pool, rec
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("fake executor never joined the pool")
	return nil, nil
}
