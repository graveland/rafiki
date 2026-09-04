package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"go.graveland.dev/rafiki/pkg/eventbuf"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/users"
)

func TestSelfKillStoreSetAndTake(t *testing.T) {
	var s selfKillStore
	if s.take("c1") {
		t.Fatal("unmarked child must not be taken as marked")
	}
	s.set("c1")
	if !s.take("c1") {
		t.Fatal("marked child must be taken as marked")
	}
	if s.take("c1") {
		t.Fatal("take must clear the mark — a second take must find nothing")
	}
}

func TestSuppressExitNoticeRequiresCleanExit(t *testing.T) {
	cases := []struct {
		name     string
		marked   bool
		exitCode int
		signal   string
		want     bool
	}{
		{"not self-killed, clean exit", false, 0, "", false},
		{"self-killed, clean exit", true, 0, "", true},
		{"self-killed, panic sentinel", true, 2, "", false},
		{"self-killed, nonzero exit", true, 1, "", false},
		{"self-killed, escalated/forced kill", true, 0, "killed", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := &Controller{}
			if tc.marked {
				c.selfKilled.set("c1")
			}
			if got := c.suppressExitNotice("c1", tc.exitCode, tc.signal); got != tc.want {
				t.Fatalf("suppressExitNotice() = %v, want %v", got, tc.want)
			}
			// Consumed exactly once, regardless of outcome: a second call
			// must never find the marker still set.
			if c.suppressExitNotice("c1", tc.exitCode, tc.signal) {
				t.Fatal("marker was not consumed by the first call")
			}
		})
	}
}

// killNoticeFixture wires a real Controller — real spawn/kill machinery via
// newTestController — with a deterministic FakeClock eventbuf substituted
// in, the same technique settleFixture uses for the in-memory Controller, so
// a real handleChildExit's notifySubagentSettled call can be observed
// synchronously instead of racing a live debounce timer.
func killNoticeFixture(t *testing.T) (*Controller, *eventbuf.FakeClock, *capturedFlush) {
	t.Helper()
	ctrl := newTestController(t)
	clk := eventbuf.NewFakeClock(time.Unix(0, 0))
	buf := eventbuf.New(eventbuf.Config{Debounce: 5 * time.Second}, clk)
	cap := &capturedFlush{}
	buf.SetFlush(cap.fn)
	buf.SetBusy(func(string) bool { return false })
	ctrl.evbuf = buf
	return ctrl, clk, cap
}

func spawnTestChildWithParent(t *testing.T, ctrl *Controller, parentID string) string {
	t.Helper()
	req := protocol.SpawnRequest{
		Kind:          protocol.KindClaude,
		Cwd:           t.TempDir(),
		PiBinary:      fakePiBin(t),
		NoSession:     true,
		ParentChildID: parentID,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res, err := ctrl.Spawn(ctx, req, users.Identity{})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	return res.ChildID
}

// TestAgentKillSuppressesExitNotice pins the actual behavior this whole
// mechanism exists for: a coordinator killing its own subagent via the
// agent_kill tool path (controllerSpawner.Kill) must not get told the
// subagent "exited" — its tool call already confirmed that synchronously.
func TestAgentKillSuppressesExitNotice(t *testing.T) {
	t.Parallel()
	ctrl, clk, cap := killNoticeFixture(t)

	coordID := spawnTestChild(t, ctrl, nil)
	workerID := spawnTestChildWithParent(t, ctrl, coordID)

	spawner := newControllerSpawner(ctrl, coordID)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := spawner.Kill(ctx, workerID); err != nil {
		t.Fatalf("Kill: %v", err)
	}

	clk.Advance(6 * time.Second)
	if got := cap.batches(); len(got) != 0 {
		t.Fatalf("a self-initiated kill must not notify the coordinator: %+v", got)
	}
}

// TestCLIKillStillNotifies pins the other half: a human killing a
// coordinator's subagent directly (the CLI/Connect path, straight into
// Controller.Kill — never through controllerSpawner.Kill) must still notify
// the coordinator, since it did not already know.
func TestCLIKillStillNotifies(t *testing.T) {
	t.Parallel()
	ctrl, clk, cap := killNoticeFixture(t)

	coordID := spawnTestChild(t, ctrl, nil)
	workerID := spawnTestChildWithParent(t, ctrl, coordID)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := ctrl.Kill(ctx, workerID, 0, 0); err != nil {
		t.Fatalf("Kill: %v", err)
	}

	clk.Advance(6 * time.Second)
	batches := cap.batches()
	if len(batches) != 1 || !strings.Contains(batches[0].fragments[0], "exited") {
		t.Fatalf("a CLI kill must still notify the coordinator: %+v", batches)
	}
}
