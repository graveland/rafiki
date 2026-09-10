// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
)

// ── helpers ──────────────────────────────────────────────────────────────────

func spawnedFor(id, name, parent string) *rafikiv1.Event {
	return &rafikiv1.Event{ChildId: id,
		Payload: &rafikiv1.Event_ChildSpawned{ChildSpawned: &rafikiv1.ChildSpawned{
			ChildId: id, ParentId: parent, Name: name}}}
}

func watchStatusFor(id, state string) *rafikiv1.Event {
	return &rafikiv1.Event{ChildId: id,
		Payload: &rafikiv1.Event_AgentStatus{AgentStatus: &rafikiv1.AgentStatus{State: state}}}
}

func exitedFor(id string, code *int32, signal string) *rafikiv1.Event {
	return &rafikiv1.Event{ChildId: id,
		Payload: &rafikiv1.Event_ChildExited{ChildExited: &rafikiv1.ChildExited{
			ExitCode: code, Signal: signal}}}
}

func turnEndForCost(id string, cost float64) *rafikiv1.Event {
	return &rafikiv1.Event{ChildId: id,
		Payload: &rafikiv1.Event_TurnEnd{TurnEnd: &rafikiv1.TurnEnd{
			CostUsd:    &cost,
			StopReason: rafikiv1.StopReason_STOP_REASON_END_TURN,
		}}}
}

// ── the tracker ──────────────────────────────────────────────────────────────

// A full lifecycle in five lines, with the durations that make the status
// lines worth reading: how long each state lasted, and the child's lifetime.
func TestWatchTrackerRendersAFullLifecycle(t *testing.T) {
	tr := newWatchTracker()
	t0 := time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)

	got := tr.observe(spawnedFor("c_1", "impl-auth", "c_root"), t0)
	for _, want := range []string{"spawn", "impl-auth", "parent=c_root"} {
		if !strings.Contains(got, want) {
			t.Errorf("spawn line %q missing %q", got, want)
		}
	}

	got = tr.observe(watchStatusFor("c_1", "streaming"), t0.Add(2*time.Second))
	if !strings.Contains(got, "spawning → streaming") {
		t.Errorf("first status line should show spawning→streaming, got %q", got)
	}

	// Four seconds of streaming; the transition OUT of it carries the
	// duration of the work.
	got = tr.observe(watchStatusFor("c_1", "idle"), t0.Add(6*time.Second))
	if !strings.Contains(got, "streaming → idle (4.0s)") {
		t.Errorf("idle transition should carry the working duration, got %q", got)
	}

	got = tr.observe(turnEndForCost("c_1", 0.0142), t0.Add(7*time.Second))
	if !strings.Contains(got, "cost=$0.0142") || !strings.Contains(got, "stop=END_TURN") {
		t.Errorf("turn line = %q", got)
	}

	// Lifetime runs from spawn.
	got = tr.observe(exitedFor("c_1", int32Ptr(0), ""), t0.Add(3*time.Minute))
	if !strings.Contains(got, "code=0") || !strings.Contains(got, "(lifetime 3m00s)") {
		t.Errorf("exit line = %q", got)
	}
}

// A child whose exit carried no code was signalled; the line must say so
// rather than print code=0, which means success.
func TestWatchTrackerRenderASignalledExit(t *testing.T) {
	tr := newWatchTracker()
	t0 := time.Now()
	_ = tr.observe(spawnedFor("c_1", "impl-auth", ""), t0)

	got := tr.observe(exitedFor("c_1", nil, "SIGKILL"), t0.Add(time.Second))
	if !strings.Contains(got, "signal=SIGKILL") || strings.Contains(got, "code=") {
		t.Errorf("signalled exit line = %q", got)
	}
}

// An event for a child the tracker has never met reads [unnamed]. That is not
// cosmetic: it is exactly what the rail looks like when discovery misses, so
// the line must name the gap rather than print an empty cell.
func TestWatchTrackerNamesAnUnmetChild(t *testing.T) {
	tr := newWatchTracker()
	got := tr.observe(watchStatusFor("c_stranger", "idle"), time.Now())
	if !strings.Contains(got, "[unnamed]") || !strings.Contains(got, "c_stranger") {
		t.Errorf("unknown-child line = %q", got)
	}
}

// A duplicate status event (same state twice) must not render a phantom "x →
// x" transition, and a turn_end that arrives while the child is already idle
// carries no duration rather than a bogus one.
func TestWatchTrackerKeepsDuplicateStatusAndTurnSane(t *testing.T) {
	tr := newWatchTracker()
	t0 := time.Now()
	_ = tr.observe(watchStatusFor("c_1", "idle"), t0)

	got := tr.observe(watchStatusFor("c_1", "idle"), t0.Add(time.Second))
	if strings.Contains(got, "→") {
		t.Errorf("duplicate status rendered a transition: %q", got)
	}

	got = tr.observe(turnEndForCost("c_1", 0), t0.Add(2*time.Second))
	if strings.Contains(got, "(") {
		t.Errorf("turn_end after idle invented a duration: %q", got)
	}
	// cost 0 is a real reading on a free model and must still print.
	if !strings.Contains(got, "cost=$0.0000") {
		t.Errorf("zero cost dropped: %q", got)
	}
}

// seed() names children the stream never announced — the reconnect case,
// where child_spawned is in the past and will not replay.
func TestWatchTrackerSeedSuppliesNamesAfterReconnect(t *testing.T) {
	tr := newWatchTracker()
	var notes bytes.Buffer
	client := stubbedRosterSource{listChildren: func(ctx context.Context, req *connect.Request[rafikiv1.ListChildrenRequest]) (*connect.Response[rafikiv1.ListChildrenResponse], error) {
		return connect.NewResponse(&rafikiv1.ListChildrenResponse{Children: []*rafikiv1.ChildSummary{
			{ChildId: "c_1", Name: "coordinator", Status: "streaming"},
		}}), nil
	}}

	tr.seed(context.Background(), &notes, client, "test")
	if got := tr.observe(watchStatusFor("c_1", "idle"), time.Now()); !strings.Contains(got, "coordinator") {
		t.Errorf("seeded name missing from line: %q", got)
	}
	if notes.Len() != 0 {
		t.Errorf("successful seed wrote notes: %q", notes.String())
	}
}

// A failed roster fetch is a note, not an error: the stream still works.
func TestWatchTrackerSeedFailureIsANote(t *testing.T) {
	tr := newWatchTracker()
	var notes bytes.Buffer
	client := stubbedRosterSource{listChildren: func(ctx context.Context, req *connect.Request[rafikiv1.ListChildrenRequest]) (*connect.Response[rafikiv1.ListChildrenResponse], error) {
		return nil, connect.NewError(connect.CodeUnavailable, context.DeadlineExceeded)
	}}

	tr.seed(context.Background(), &notes, client, "describe-here")
	if !strings.Contains(notes.String(), "# roster unavailable at describe-here") {
		t.Errorf("seed failure not noted: %q", notes.String())
	}
}

func TestWatchTrackerIgnoresEmptyAndNilEvents(t *testing.T) {
	tr := newWatchTracker()
	if got := tr.observe(nil, time.Now()); got != "" {
		t.Errorf("nil event rendered %q", got)
	}
	if got := tr.observe(&rafikiv1.Event{}, time.Now()); got != "" {
		t.Errorf("child-less event rendered %q", got)
	}
}

func TestFmtDur(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{900 * time.Millisecond, "0.9s"},
		{3200 * time.Millisecond, "3.2s"},
		{72 * time.Second, "1m12s"},
		{62 * time.Minute, "1h02m"},
	}
	for _, tc := range cases {
		if got := fmtDur(tc.d); got != tc.want {
			t.Errorf("fmtDur(%v) = %q, want %q", tc.d, got, tc.want)
		}
	}
}

// ── the command ──────────────────────────────────────────────────────────────

// The flag exists and is not read by nobody: it widens the subscription.
func TestWatchCmdDeclaresAllTypes(t *testing.T) {
	cmd := newWatchCmd()
	b, err := cmd.Flags().GetBool("all-types")
	if err != nil || b {
		t.Fatalf("all-types default = %v (err %v), want false", b, err)
	}
}

func int32Ptr(v int32) *int32 { return &v }

// stubbedRosterSource serves the one RPC seed() makes.
type stubbedRosterSource struct {
	listChildren func(ctx context.Context, req *connect.Request[rafikiv1.ListChildrenRequest]) (*connect.Response[rafikiv1.ListChildrenResponse], error)
}

func (s stubbedRosterSource) ListChildren(ctx context.Context, req *connect.Request[rafikiv1.ListChildrenRequest]) (*connect.Response[rafikiv1.ListChildrenResponse], error) {
	return s.listChildren(ctx, req)
}
