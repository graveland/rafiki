package main

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// TestResumeFromPiSessionRequest_ForwardsSkillsDirAndMCPConfig proves the
// --pi-session resume path actually carries --skills-dir/--mcp-config through
// to the SpawnRequest it sends to the daemon. It drives
// buildResumeFromPiSessionRequest — the exact function runResumeFromPiSession
// calls before dialing the daemon — rather than buildSpawnRequest directly:
// runResumeFromPiSession overwrites several request fields AFTER
// buildSpawnRequest returns (Cwd, Model, Provider, Thinking, Type,
// ResumeSession, ResumedFromSession), so a test that stops at buildSpawnRequest
// would pass even if one of those overwrites clobbered SkillsDirs/MCPConfig on
// its way out.
func TestResumeFromPiSessionRequest_ForwardsSkillsDirAndMCPConfig(t *testing.T) {
	// Isolate from a real RAFIKI_URL in the ambient environment (e.g. a dev
	// shell pointed at a remote daemon) — this test exercises the jsonl-cwd
	// fallback, not the remote-requires-explicit-cwd guard.
	t.Setenv("RAFIKI_URL", "")

	dir := t.TempDir()
	sessionPath := filepath.Join(dir, "session.jsonl")
	content := `{"type":"session","id":"test-uuid-skills","cwd":"/some/project"}
{"type":"model_change","provider":"anthropic","modelId":"claude-haiku-4-5"}
`
	if err := os.WriteFile(sessionPath, []byte(content), 0600); err != nil {
		t.Fatalf("write session file: %v", err)
	}

	cmd := newResumeCmd()
	if err := cmd.Flags().Set("skills-dir", "/a/skills"); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Flags().Set("skills-dir", "/b/skills"); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Flags().Set("mcp-config", "/cfg/.mcp.json"); err != nil {
		t.Fatal(err)
	}

	req, err := buildResumeFromPiSessionRequest(cmd, sessionPath)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if want := []string{"/a/skills", "/b/skills"}; !slices.Equal(req.SkillsDirs, want) {
		t.Errorf("SkillsDirs = %v, want %v — --pi-session resume silently loses its skill dirs", req.SkillsDirs, want)
	}
	if req.MCPConfig != "/cfg/.mcp.json" {
		t.Errorf("MCPConfig = %q, want /cfg/.mcp.json — --pi-session resume silently loses its MCP servers", req.MCPConfig)
	}
}
