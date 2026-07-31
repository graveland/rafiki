package main

import (
	"testing"

	"go.graveland.dev/rafiki/pkg/childstore"
)

func TestRecordRoundTrip_KindAndConfigDir(t *testing.T) {
	snap := childstore.Snapshot{
		ChildID:   "c1",
		Cwd:       "/tmp",
		Kind:      "claude",
		ConfigDir: "/home/u/.claude-personal",
		Status:    "exited",
	}
	rec := recordFromSnapshot(snap)
	if rec.Kind != "claude" || rec.ConfigDir != "/home/u/.claude-personal" {
		t.Fatalf("record dropped fields: kind=%q configDir=%q", rec.Kind, rec.ConfigDir)
	}
	sess := sessionFromRecord(rec)
	got := sess.Snapshot()
	if got.Kind != "claude" || got.ConfigDir != "/home/u/.claude-personal" {
		t.Fatalf("session-from-record dropped fields: kind=%q configDir=%q", got.Kind, got.ConfigDir)
	}
}
