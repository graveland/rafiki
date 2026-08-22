package main

import (
	"testing"

	"go.graveland.dev/rafiki/pkg/childstore"
	"go.graveland.dev/rafiki/pkg/protocol"
)

func TestRecordRoundTrip_KindAndConfigDir(t *testing.T) {
	snap := childstore.Snapshot{
		ChildID:   "c1",
		Cwd:       "/tmp",
		Kind:      protocol.KindClaude,
		ConfigDir: "/home/u/.claude-personal",
		Status:    "exited",
	}
	rec := recordFromSnapshot(snap)
	if rec.Kind != protocol.KindClaude || rec.ConfigDir != "/home/u/.claude-personal" {
		t.Fatalf("record dropped fields: kind=%q configDir=%q", rec.Kind, rec.ConfigDir)
	}
	sess := sessionFromRecord(rec)
	got := sess.Snapshot()
	if got.Kind != protocol.KindClaude || got.ConfigDir != "/home/u/.claude-personal" {
		t.Fatalf("session-from-record dropped fields: kind=%q configDir=%q", got.Kind, got.ConfigDir)
	}
}

// TestRecordRoundTrip_RecordRequests guards against a child spawned with
// --record-requests losing that setting the moment it is persisted to disk
// and reloaded — e.g. across a daemon restart (loadOrphans/resumeWithAuto-
// Recovery) or a RespawnChild (new_session/switch_session interception),
// both of which rebuild the live Session from a persist.Record.
func TestRecordRoundTrip_RecordRequests(t *testing.T) {
	for _, want := range []bool{true, false} {
		snap := childstore.Snapshot{
			ChildID:        "c1",
			Cwd:            "/tmp",
			Status:         "exited",
			RecordRequests: want,
		}
		rec := recordFromSnapshot(snap)
		if rec.RecordRequests != want {
			t.Fatalf("record dropped RecordRequests: got %v, want %v", rec.RecordRequests, want)
		}
		got := sessionFromRecord(rec).Snapshot()
		if got.RecordRequests != want {
			t.Fatalf("session-from-record dropped RecordRequests: got %v, want %v", got.RecordRequests, want)
		}
	}
}

// TestRecordRoundTrip_MCPServersAndNoMCP is the on-disk half of the MCP
// allowlist's resume story: the record is what a daemon restart rebuilds a
// child from, so a field that survives snapshot-to-request but not
// snapshot-to-record is still lost — just later, and only to the failure mode
// nobody tests by hand.
func TestRecordRoundTrip_MCPServersAndNoMCP(t *testing.T) {
	snap := childstore.Snapshot{
		ChildID:    "c1",
		Cwd:        "/tmp",
		Status:     "exited",
		Kind:       protocol.KindFundi,
		MCPConfig:  "/work/.mcp.json",
		MCPServers: []string{"codescan"},
		NoMCP:      true,
	}
	rec := recordFromSnapshot(snap)
	if len(rec.MCPServers) != 1 || rec.MCPServers[0] != "codescan" || !rec.NoMCP {
		t.Fatalf("record dropped fields: mcpServers=%v noMcp=%v", rec.MCPServers, rec.NoMCP)
	}
	got := sessionFromRecord(rec).Snapshot()
	if len(got.MCPServers) != 1 || got.MCPServers[0] != "codescan" || !got.NoMCP {
		t.Fatalf("session-from-record dropped fields: mcpServers=%v noMcp=%v", got.MCPServers, got.NoMCP)
	}
}
