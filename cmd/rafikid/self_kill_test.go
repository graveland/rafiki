package main

import (
	"context"
	"strings"
	"syscall"
	"testing"
	"time"

	"go.graveland.dev/rafiki/pkg/child"
	"go.graveland.dev/rafiki/pkg/eventbuf"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/users"
)

func TestSelfKillStoreSetAndTake(t *testing.T) {
	var s selfKillStore
	if _, ok := s.take("c1"); ok {
		t.Fatal("unmarked child must not be taken as marked")
	}
	s.set("c1", killMark{parent: true})
	mark, ok := s.take("c1")
	if !ok || !mark.parent || mark.mcpUser != "" {
		t.Fatalf("take() = (%+v, %v), want the parent mark", mark, ok)
	}
	if _, ok := s.take("c1"); ok {
		t.Fatal("take must clear the mark — a second take must find nothing")
	}
}

// exitCausedByShutdown is the shape rule the suppression rests on. The cases
// name the real exits each row stands for: (143, "") with the causality bit
// is a daraja-hosted claude child killed by the SIGTERM its own coordinator's
// agent_kill drove (claude catches SIGTERM and exits 128+15 rather than dying
// by signal); (0, "killed") with the bit is an escalated/abandoned ladder;
// the same shapes WITHOUT the bit are foreign deaths that landed while a kill
// was in flight.
func TestExitCausedByShutdown(t *testing.T) {
	cases := []struct {
		name string
		res  child.ShutdownResult
		want bool
	}{
		{"clean stdin-close stop", child.ShutdownResult{ExitCode: 0}, true},
		{"claude's handled SIGTERM, ladder-driven", child.ShutdownResult{ExitCode: 143, ByShutdown: true}, true},
		{"escalated SIGKILL, ladder-driven", child.ShutdownResult{ExitCode: 0, Signal: "killed", ByShutdown: true}, true},
		{"abandoned wait, ladder-driven", child.ShutdownResult{ExitCode: 0, Signal: "killed", Abandoned: true, ByShutdown: true}, true},
		{"fundi panic sentinel, not the kill", child.ShutdownResult{ExitCode: 2}, false},
		{"subprocess crash, not the kill", child.ShutdownResult{ExitCode: 1}, false},
		{"foreign SIGKILL during the passive wait", child.ShutdownResult{ExitCode: 0, Signal: "killed"}, false},
		{"foreign SIGTERM during the passive wait", child.ShutdownResult{ExitCode: 0, Signal: "terminated"}, false},
		{"indeterminate", child.ShutdownResult{ExitCode: -1}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := exitCausedByShutdown(tc.res); got != tc.want {
				t.Fatalf("exitCausedByShutdown(%+v) = %v, want %v", tc.res, got, tc.want)
			}
		})
	}
}

// The disposition is audience-scoped: a coordinator's mark silences its own
// parent fragment, an MCP caller's mark excludes its own user's fan-out, and
// a death that was NOT the kill's doing silences nothing — a crash or a
// foreign signal racing the kill is news to every audience.
func TestSelfKillDispositionFor(t *testing.T) {
	cases := []struct {
		name   string
		mark   killMark
		caused bool
		want   selfKillDisposition
	}{
		{"coordinator kill, its death", killMark{parent: true}, true,
			selfKillDisposition{suppressParent: true}},
		{"mcp caller kill, its death", killMark{mcpUser: "u-op"}, true,
			selfKillDisposition{excludeMCPUser: "u-op"}},
		{"coordinator kill, crash raced it", killMark{parent: true}, false,
			selfKillDisposition{}},
		{"mcp caller kill, crash raced it", killMark{mcpUser: "u-op"}, false,
			selfKillDisposition{}},
		{"no mark", killMark{}, true, selfKillDisposition{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := selfKillDispositionFor(tc.mark, tc.caused)
			if got != tc.want {
				t.Fatalf("selfKillDispositionFor(%+v, %v) = %+v, want %+v", tc.mark, tc.caused, got, tc.want)
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

// TestAgentKillOfASignallingChildStillSuppresses pins the claude half of the
// guard: a child that does NOT stop on the passive stdin close — the fake's
// FAKE_PI_SHUTDOWN_DELAY stands in for a claude mid-turn — is ended by the
// ladder's SIGTERM rung, and the death must still read as the kill's own
// doing (ShutdownResult.ByShutdown) rather than as a foreign crash. A
// daraja-hosted claude child hits exactly this on every agent_kill: its
// stdin Close IS the stop request, and claude exits 143 from the SIGTERM it
// delivers.
//
// The two steps below are controllerSpawner.Kill's own (it cannot pass
// custom ladder timeouts, so the test drives them directly): mark, then Kill.
func TestAgentKillOfASignallingChildStillSuppresses(t *testing.T) {
	t.Parallel()
	ctrl, clk, cap := killNoticeFixture(t)

	coordID := spawnTestChild(t, ctrl, nil)
	// The shutdown delay keeps the fake alive past the first rung, forcing
	// the SIGTERM escalation a real mid-turn claude child takes.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res, err := ctrl.Spawn(ctx, protocol.SpawnRequest{
		Kind: protocol.KindClaude, Cwd: t.TempDir(), PiBinary: fakePiBin(t),
		NoSession: true, ParentChildID: coordID,
		Env: map[string]string{"FAKE_PI_SHUTDOWN_DELAY": "2"},
	}, users.Identity{})
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	workerID := res.ChildID

	ctrl.selfKilled.set(workerID, killMark{parent: true})
	ctx2, cancel2 := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel2()
	killRes, err := ctrl.Kill(ctx2, workerID, 250, 250)
	if err != nil {
		t.Fatalf("Kill: %v", err)
	}
	if !killRes.Escalated || killRes.Signal == "" {
		t.Fatalf("the ladder must have escalated to a signal for this fixture to exercise the rung: %+v", killRes)
	}

	clk.Advance(6 * time.Second)
	if got := cap.batches(); len(got) != 0 {
		t.Fatalf("a self-initiated kill must suppress the notice even when it had to signal: %+v", got)
	}
}

// TestForeignDeathDuringASelfKillStillNotifies pins the reason the causality
// bit exists: the (code, signal) pair alone cannot tell the daemon's own
// ladder from a foreign kill. A child marked for a self-kill that then dies
// of an EXTERNAL SIGKILL (the process group is killed out from under the
// daemon, OOM-killer style) still notifies — the mark alone must not swallow
// a death the kill did not cause.
func TestForeignDeathDuringASelfKillStillNotifies(t *testing.T) {
	t.Parallel()
	ctrl, clk, cap := killNoticeFixture(t)

	coordID := spawnTestChild(t, ctrl, nil)
	workerID := spawnTestChildWithParent(t, ctrl, coordID)

	ctrl.selfKilled.set(workerID, killMark{parent: true})
	ch, ok := ctrl.cm.Get(workerID)
	if !ok {
		t.Fatal("worker not live")
	}
	if err := syscall.Kill(-ch.PID(), syscall.SIGKILL); err != nil {
		t.Fatalf("external kill: %v", err)
	}
	if !waitForChildRemoval(ctrl.cm, workerID, killWaitTimeout) {
		t.Fatal("worker never removed")
	}

	clk.Advance(6 * time.Second)
	batches := cap.batches()
	if len(batches) != 1 || !strings.Contains(batches[0].fragments[0], "exited") {
		t.Fatalf("a foreign kill during a self-kill must still notify the coordinator: %+v", batches)
	}
}
