package store_test

import (
	"testing"
	"time"

	"graveland.dev/pi-controller/internal/protocol"
	"graveland.dev/pi-controller/internal/store"
)

func TestSession_Snapshot_CopiesFields(t *testing.T) {
	s := &store.Session{
		ChildID: "c_1", Name: "x",
		Status: protocol.StatusIdle, StartedAt: time.Unix(100, 0),
	}
	snap := s.Snapshot()

	// Mutate the original; snapshot must not change.
	s.Name = "y"
	s.Status = protocol.StatusStreaming
	if snap.Name != "x" {
		t.Fatalf("snapshot Name aliased: got %q, want %q", snap.Name, "x")
	}
	if snap.Status != protocol.StatusIdle {
		t.Fatalf("snapshot Status aliased: got %v, want %v", snap.Status, protocol.StatusIdle)
	}
}

func TestSession_Snapshot_CopiesExitCode(t *testing.T) {
	code := 1
	s := &store.Session{ChildID: "c1", ExitCode: &code}
	snap := s.Snapshot()

	// Mutate original; snapshot's ExitCode must not change.
	*s.ExitCode = 2
	if snap.ExitCode == nil {
		t.Fatal("snapshot ExitCode is nil")
	}
	if *snap.ExitCode != 1 {
		t.Fatalf("ExitCode aliased: got %d, want 1", *snap.ExitCode)
	}

	// nil ExitCode → nil in snapshot.
	s2 := &store.Session{ChildID: "c2"}
	snap2 := s2.Snapshot()
	if snap2.ExitCode != nil {
		t.Fatalf("nil ExitCode should stay nil, got pointer to %d", *snap2.ExitCode)
	}
}

func TestSession_Snapshot_CopiesSlices(t *testing.T) {
	s := &store.Session{
		ChildID:    "c_1",
		Extensions: []string{"a", "b"},
	}
	snap := s.Snapshot()
	s.Extensions[0] = "MUTATED"
	if snap.Extensions[0] != "a" {
		t.Fatalf("slice aliased: %v", snap.Extensions)
	}
}
