package childstore

import (
	"testing"
	"time"

	"go.graveland.dev/rafiki/pkg/protocol"

	"github.com/multigres/testkit/assert"
)

// TestRecordRoundTrip is the test that actually protects the JSONB split. Every
// field on Session that is meant to survive a daemon restart must come back
// through Snapshot -> ChildRecord -> Session unchanged; a field forgotten in
// either conversion is silently dropped at runtime and only a round trip that
// populates EVERY field catches it.
func TestRecordRoundTrip(t *testing.T) {
	exitCode := 3
	started := time.Now().Add(-time.Hour).Truncate(time.Millisecond)
	active := time.Now().Add(-time.Minute).Truncate(time.Millisecond)
	exited := time.Now().Truncate(time.Millisecond)

	orig := &Session{
		ChildID:        "c_01ABCDEF",
		OwnerUserID:    "u_owner1",
		PID:            4321,
		Name:           "worker",
		Cwd:            "/tmp/work",
		Kind:           protocol.KindFundi,
		ConfigDir:      "/tmp/cfg",
		Provider:       "anthropic",
		Model:          "claude-opus-5",
		Thinking:       "medium",
		SessionID:      "018f3a2c-7b1e-7c4a-9f21-3d5e8a1b0c44",
		SessionFile:    "/tmp/session.jsonl",
		SessionDir:     "/tmp/sessions",
		NoSession:      true,
		ResumeSession:  "/tmp/resume.jsonl",
		ForkSession:    "/tmp/fork.jsonl",
		Tools:          []string{"read", "write"},
		NoTools:        true,
		NoBuiltinTools: true,
		// The resolved routing spec: rides the config JSONB like Tools, so the
		// round trip must carry it both directions.
		Routing:            "sort=price,nodata",
		Extensions:         []string{"ext1"},
		NoExtensions:       true,
		Skills:             []string{"skill1"},
		NoSkills:           true,
		SkillsDirs:         []string{"/tmp/skills"},
		MCPConfig:          "/tmp/.mcp.json",
		MCPServers:         []string{"srv1"},
		NoMCP:              true,
		PromptTemplates:    []string{"tpl"},
		NoPromptTemplates:  true,
		Themes:             []string{"dark"},
		NoThemes:           true,
		NoContextFiles:     true,
		SystemPrompt:       "sys",
		AppendSystemPrompt: "append",
		Prefill: []protocol.PrefillRead{
			{Path: "docs/plan.md", Start: 10, End: 40},
			{Path: "notes/*"},
		},
		Verbose:          true,
		PiBinary:         "/usr/bin/pi",
		ExtraArgs:        []string{"--flag"},
		RecordRequests:   true,
		ExecutorSelector: "env=prod",
		WorkspaceMode:    "ephemeral",
		MaxDepth:         5,
		MaxCost:          12.5,
		MaxChildren:      7,
		Status:           protocol.StatusIdle,
		StartedAt:        started,
		LastActivity:     active,
		ExitedAt:         exited,
		ExitCode:         &exitCode,
		ExitSignal:       "SIGTERM",
		Labels:           map[string]string{"owner": "brent", "rafiki/kind": "fundi"},
	}

	rec := RecordFromSnapshot(orig.Snapshot())
	got := SessionFromRecord(rec)

	// Compare via Snapshot so the unexported mutex does not defeat DeepEqual.
	want := orig.Snapshot()
	have := got.Snapshot()
	assert.NewCollecting(t).EqDiff(have, want, "round trip lost data:\n want")
}

// TestRecordFromSnapshotSetsLastStatusEmpty proves RecordFromSnapshot does not
// invent a last_status. Only the exit path writes that column (design §1.5),
// and a record that guesses one would make every child look resume-worthy.
func TestRecordFromSnapshotSetsLastStatusEmpty(t *testing.T) {
	c := assert.NewCollecting(t)
	s := &Session{ChildID: "c_1", Kind: protocol.KindFundi, Status: protocol.StatusIdle}
	rec := RecordFromSnapshot(s.Snapshot())
	c.Eq("", rec.LastStatus, "LastStatus")
	c.Eq(string(protocol.StatusIdle), rec.Status, "Status = %q, want %q", rec.Status, protocol.StatusIdle)
}

// TestSessionFromRecordDoesNotRestoreRings pins design §1.3: the exit-time ring
// snapshots are not persisted, so a recovered session must come back with nil
// rings rather than an empty non-nil slice a caller might mistake for data.
func TestSessionFromRecordDoesNotRestoreRings(t *testing.T) {
	c := assert.NewCollecting(t)
	sess := SessionFromRecord(ChildRecord{ChildID: "c_1", Kind: protocol.KindFundi, Status: "exited"})
	c.Nil(sess.ExitedRing, "ExitedRing")
	c.Nil(sess.ExitedRenderRing, "ExitedRenderRing")
}
