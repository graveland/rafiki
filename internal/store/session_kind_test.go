package store

import "testing"

func TestSnapshot_RoundTripsKindAndConfigDir(t *testing.T) {
	s := &Session{
		ChildID:   "c1",
		Cwd:       "/tmp",
		Kind:      "claude",
		ConfigDir: "/home/u/.claude-personal",
	}
	snap := s.Snapshot()
	if snap.Kind != "claude" {
		t.Fatalf("snapshot Kind = %q, want claude", snap.Kind)
	}
	if snap.ConfigDir != "/home/u/.claude-personal" {
		t.Fatalf("snapshot ConfigDir = %q", snap.ConfigDir)
	}
}
