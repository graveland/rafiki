package main

import (
	"testing"
	"time"

	"go.graveland.dev/rafiki/pkg/childstore"
	"go.graveland.dev/rafiki/pkg/clock"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/sched"

	"github.com/multigres/testkit/assert"
)

// graceFixture wires a fake-clock scheduler and a one-hour grace window onto a
// real test controller.
func graceFixture(t *testing.T) (*Controller, *clock.FakeClock) {
	t.Helper()
	c := newTestController(t)
	clk := clock.NewFakeClock(time.Unix(0, 0))
	c.clk = clk
	c.sched = sched.New(clk, time.Second)
	c.graceWindow = time.Hour
	return c, clk
}

// An exited child is closed exactly when its grace window expires — present
// just short of the deadline, gone at it.
func TestGraceClosesExitedChildAtDeadline(t *testing.T) {
	ck := assert.NewAborting(t)
	c, clk := graceFixture(t)
	exitedAt := clk.Now()
	c.st.Insert(&childstore.Session{ChildID: "c_g1", Status: protocol.StatusExited, ExitedAt: exitedAt})
	c.armGrace("c_g1", exitedAt)

	clk.Advance(59 * time.Minute)
	_, ok := c.st.Get("c_g1")
	ck.True(ok, "child must still be present just short of the grace window")

	clk.Advance(1 * time.Minute)
	_, ok = c.st.Get("c_g1")
	ck.False(ok, "child must be closed once the grace window expires")
}

// A child that resumes before its window expires is no longer exited, so the
// stale deadline's re-read skips it.
func TestGraceSkipsAResumedChild(t *testing.T) {
	ck := assert.NewAborting(t)
	c, clk := graceFixture(t)
	exitedAt := clk.Now()
	c.st.Insert(&childstore.Session{ChildID: "c_g2", Status: protocol.StatusExited, ExitedAt: exitedAt})
	c.armGrace("c_g2", exitedAt)

	// The child resumes: it is working again, not exited.
	c.st.SetStatus("c_g2", protocol.StatusIdle)

	clk.Advance(2 * time.Hour)
	_, ok := c.st.Get("c_g2")
	ck.True(ok, "a resumed child must not be closed by its stale grace deadline")
}

// A child that exits a SECOND time arms its own later deadline under the same
// key; the stale deadline must not close the child short of the later one.
func TestGraceIgnoresAStaleDeadlineAfterALaterExit(t *testing.T) {
	ck := assert.NewAborting(t)
	c, clk := graceFixture(t)
	t0 := clk.Now()
	c.st.Insert(&childstore.Session{ChildID: "c_g3", Status: protocol.StatusExited, ExitedAt: t0})
	c.armGrace("c_g3", t0)

	// A second exit 30m later, re-arming the same key at the later timestamp.
	later := t0.Add(30 * time.Minute)
	clk.Advance(30 * time.Minute)
	ck.True(c.st.MarkExited("c_g3", later, 0, "", nil, nil), "re-exit")
	c.armGrace("c_g3", later)

	// Past the stale deadline, short of the new one.
	clk.Advance(31 * time.Minute)
	_, ok := c.st.Get("c_g3")
	ck.True(ok, "the stale deadline must not close a child that exited again later")

	// Past the child's own later deadline.
	clk.Advance(30 * time.Minute)
	_, ok = c.st.Get("c_g3")
	ck.False(ok, "the child must close once its own later deadline expires")
}

// TestArmRecoveredGraceArmsEveryExitedRow pins the helper every loadChildren
// path calls — including the lease-failure early return, which loads rows as
// exited but would otherwise return before arming anything. A row with no
// ExitedAt is skipped (nothing to measure from); a non-exited row is skipped.
func TestArmRecoveredGraceArmsEveryExitedRow(t *testing.T) {
	ck := assert.NewAborting(t)
	c, clk := graceFixture(t)
	exitedAt := clk.Now()
	c.st.Insert(&childstore.Session{ChildID: "c_e1", Status: protocol.StatusExited, ExitedAt: exitedAt})
	c.st.Insert(&childstore.Session{ChildID: "c_e2", Status: protocol.StatusExited, ExitedAt: exitedAt.Add(time.Minute)})
	c.st.Insert(&childstore.Session{ChildID: "c_z", Status: protocol.StatusExited}) // no ExitedAt: nothing to arm
	c.st.Insert(&childstore.Session{ChildID: "c_live", Status: protocol.StatusStreaming})

	c.armRecoveredGrace()

	pending := map[string]bool{}
	for _, e := range c.sched.Pending() {
		pending[e.Key] = true
	}
	ck.True(pending[graceKey("c_e1")], "c_e1's grace deadline must be armed")
	ck.True(pending[graceKey("c_e2")], "c_e2's grace deadline must be armed")
	ck.False(pending[graceKey("c_z")], "a row with no ExitedAt must not be armed")
	ck.False(pending[graceKey("c_live")], "a non-exited row must not be armed")

	// And it actually closes: past the later deadline, both exited rows close.
	clk.Advance(61 * time.Minute)
	for _, id := range []string{"c_e1", "c_e2"} {
		_, ok := c.st.Get(id)
		ck.False(ok, "%s must be closed once its window expires", id)
	}
}
