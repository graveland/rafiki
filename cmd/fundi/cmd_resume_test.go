package main

import (
	"slices"
	"testing"
)

// TestResumeCmd_ForwardsSkillsDirAndMCPConfig proves fundi resume's --pi-session
// path actually READS the --skills-dir/--mcp-config flags it registers (via
// the shared addSpawnFlags) and forwards them into the SpawnRequest, rather
// than merely accepting them on the command line and dropping them on the
// floor. runResumeFromPiSession builds its request with exactly this call —
// buildSpawnRequest(cmd, nil) — before overlaying jsonl-derived cwd/model/
// thinking, so exercising that call against a real newResumeCmd() is the
// same path production takes.
func TestResumeCmd_ForwardsSkillsDirAndMCPConfig(t *testing.T) {
	cmd := newResumeCmd()
	if err := cmd.Flags().Set("cwd", "/tmp"); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Flags().Set("skills-dir", "/a/skills"); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Flags().Set("skills-dir", "/b/skills"); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Flags().Set("mcp-config", "/cfg/.mcp.json"); err != nil {
		t.Fatal(err)
	}

	req, err := buildSpawnRequest(cmd, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if want := []string{"/a/skills", "/b/skills"}; !slices.Equal(req.SkillsDirs, want) {
		t.Errorf("SkillsDirs = %v, want %v", req.SkillsDirs, want)
	}
	if req.MCPConfig != "/cfg/.mcp.json" {
		t.Errorf("MCPConfig = %q, want /cfg/.mcp.json", req.MCPConfig)
	}
}
