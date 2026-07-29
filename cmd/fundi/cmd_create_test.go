package main

import (
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// newTestCreateCmd returns a cobra.Command with spawn flags registered, suitable
// for use in buildSpawnRequest unit tests.
func newTestCreateCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "create"}
	addSpawnFlags(cmd)
	return cmd
}

func TestBuildSpawnRequest_ExplicitCwd(t *testing.T) {
	cmd := newTestCreateCmd()
	if err := cmd.Flags().Set("cwd", "/explicit/path"); err != nil {
		t.Fatal(err)
	}

	req, err := buildSpawnRequest(cmd, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if req.Cwd != "/explicit/path" {
		t.Errorf("Cwd = %q, want /explicit/path", req.Cwd)
	}
}

func TestBuildSpawnRequest_DefaultCwd(t *testing.T) {
	// When --cwd is omitted, buildSpawnRequest should use os.Getwd().
	wantCwd, err := os.Getwd()
	if err != nil {
		t.Skip("os.Getwd() failed — skipping:", err)
	}

	cmd := newTestCreateCmd()
	// cwd left at its zero value ("") intentionally.

	req, err := buildSpawnRequest(cmd, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if req.Cwd != wantCwd {
		t.Errorf("Cwd = %q, want %q", req.Cwd, wantCwd)
	}
}

func TestBuildSpawnRequest_RelativeCwdRejected(t *testing.T) {
	cmd := newTestCreateCmd()
	if err := cmd.Flags().Set("cwd", "relative/path"); err != nil {
		t.Fatal(err)
	}

	_, err := buildSpawnRequest(cmd, nil)
	if err == nil {
		t.Fatal("expected error for relative --cwd, got nil")
	}
}

// TestBuildSpawnRequest_SkillsDirAndMCPConfig covers task A6: --skills-dir
// (repeatable) and --mcp-config, previously reachable only via --extra-arg,
// now flow straight into their own SpawnRequest fields.
func TestBuildSpawnRequest_SkillsDirAndMCPConfig(t *testing.T) {
	cmd := newTestCreateCmd()
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

// TestBuildSpawnRequest_SkillsDirAndMCPConfigOmittedByDefault confirms the
// new fields stay unset (so buildAgentArgv emits neither flag) when the
// caller never touches them — matching every other optional spawn flag.
func TestBuildSpawnRequest_SkillsDirAndMCPConfigOmittedByDefault(t *testing.T) {
	cmd := newTestCreateCmd()
	if err := cmd.Flags().Set("cwd", "/tmp"); err != nil {
		t.Fatal(err)
	}

	req, err := buildSpawnRequest(cmd, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(req.SkillsDirs) != 0 {
		t.Errorf("SkillsDirs = %v, want empty", req.SkillsDirs)
	}
	if req.MCPConfig != "" {
		t.Errorf("MCPConfig = %q, want empty", req.MCPConfig)
	}
}

func TestBuildSpawnRequest_NameFromArgs(t *testing.T) {
	cmd := newTestCreateCmd()
	if err := cmd.Flags().Set("cwd", "/tmp"); err != nil {
		t.Fatal(err)
	}

	req, err := buildSpawnRequest(cmd, []string{"my-session"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if req.Name != "my-session" {
		t.Errorf("Name = %q, want my-session", req.Name)
	}
}

// ─── Mutual-exclusivity tests ─────────────────────────────────────────────────

// executeWithFlags runs cmd with the given flag args and returns any error.
// The RunE function is replaced with a no-op so the test only exercises Cobra's
// flag validation (mutual-exclusivity checks) without executing real business
// logic.
func executeWithFlags(cmd *cobra.Command, flagArgs ...string) error {
	// Replace RunE so we don't need a real daemon.
	cmd.RunE = func(_ *cobra.Command, _ []string) error { return nil }
	cmd.SetArgs(flagArgs)
	return cmd.Execute()
}

func TestCreateCmd_KillAndKeepAreMutuallyExclusive(t *testing.T) {
	cmd := newCreateCmd()
	err := executeWithFlags(cmd, "--kill-on-exit", "--keep-on-exit")
	if err == nil {
		t.Fatal("expected error when both --kill-on-exit and --keep-on-exit are set, got nil")
	}
	// Cobra's message contains "if any flags in the group" when mutual exclusion fires.
	if !strings.Contains(err.Error(), "kill-on-exit") || !strings.Contains(err.Error(), "keep-on-exit") {
		t.Errorf("expected flag names in error, got: %v", err)
	}
}

func TestCreateCmd_KillOnExitAlone_OK(t *testing.T) {
	cmd := newCreateCmd()
	if err := executeWithFlags(cmd, "--kill-on-exit"); err != nil {
		t.Errorf("unexpected error with only --kill-on-exit: %v", err)
	}
}

func TestCreateCmd_KeepOnExitAlone_OK(t *testing.T) {
	cmd := newCreateCmd()
	if err := executeWithFlags(cmd, "--keep-on-exit"); err != nil {
		t.Errorf("unexpected error with only --keep-on-exit: %v", err)
	}
}

func TestAttachCmd_KillAndKeepAreMutuallyExclusive(t *testing.T) {
	cmd := newAttachCmd()
	err := executeWithFlags(cmd, "--kill-on-exit", "--keep-on-exit")
	if err == nil {
		t.Fatal("expected error when both --kill-on-exit and --keep-on-exit are set, got nil")
	}
	// Cobra's message contains "if any flags in the group" when mutual exclusion fires.
	if !strings.Contains(err.Error(), "kill-on-exit") || !strings.Contains(err.Error(), "keep-on-exit") {
		t.Errorf("expected flag names in error, got: %v", err)
	}
}

func TestAttachCmd_KillOnExitAlone_OK(t *testing.T) {
	cmd := newAttachCmd()
	if err := executeWithFlags(cmd, "--kill-on-exit"); err != nil {
		t.Errorf("unexpected error with only --kill-on-exit: %v", err)
	}
}

func TestAttachCmd_KeepOnExitAlone_OK(t *testing.T) {
	cmd := newAttachCmd()
	if err := executeWithFlags(cmd, "--keep-on-exit"); err != nil {
		t.Errorf("unexpected error with only --keep-on-exit: %v", err)
	}
}

// ─── Label flag tests ─────────────────────────────────────────────────────────

func TestBuildSpawnRequest_LabelFlag(t *testing.T) {
	cmd := newTestCreateCmd()
	if err := cmd.Flags().Set("cwd", "/tmp"); err != nil {
		t.Fatal(err)
	}
	// Register the --label flag (added by addSpawnFlags).
	if err := cmd.Flags().Set("label", "env=prod"); err != nil {
		t.Fatal(err)
	}

	req, err := buildSpawnRequest(cmd, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if req.Labels["env"] != "prod" {
		t.Errorf("Labels[env] = %q, want prod", req.Labels["env"])
	}
}

func TestBuildSpawnRequest_PICDefaultModel(t *testing.T) {
	t.Setenv("FUNDI_DEFAULT_MODEL", "anthropic/claude-haiku-4-5")
	cmd := newTestCreateCmd()
	if err := cmd.Flags().Set("cwd", "/tmp"); err != nil {
		t.Fatal(err)
	}
	// --model not set; should fall back to env var.
	req, err := buildSpawnRequest(cmd, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if req.Model != "anthropic/claude-haiku-4-5" {
		t.Errorf("Model = %q, want anthropic/claude-haiku-4-5", req.Model)
	}
}

func TestBuildSpawnRequest_PICDefaultModelOverriddenByFlag(t *testing.T) {
	t.Setenv("FUNDI_DEFAULT_MODEL", "env-model")
	cmd := newTestCreateCmd()
	if err := cmd.Flags().Set("cwd", "/tmp"); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Flags().Set("model", "flag-model"); err != nil {
		t.Fatal(err)
	}
	req, err := buildSpawnRequest(cmd, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if req.Model != "flag-model" {
		t.Errorf("Model = %q, want flag-model (flag wins over env)", req.Model)
	}
}

func TestBuildSpawnRequest_PICDefaultLabels(t *testing.T) {
	t.Setenv("FUNDI_DEFAULT_LABELS", "context=work,env=prod")
	cmd := newTestCreateCmd()
	if err := cmd.Flags().Set("cwd", "/tmp"); err != nil {
		t.Fatal(err)
	}

	req, err := buildSpawnRequest(cmd, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if req.Labels["context"] != "work" || req.Labels["env"] != "prod" {
		t.Errorf("Labels from FUNDI_DEFAULT_LABELS: %v", req.Labels)
	}
}

func TestBuildSpawnRequest_FlagLabelWinsOverEnv(t *testing.T) {
	// Explicit --label should override FUNDI_DEFAULT_LABELS on the same key.
	t.Setenv("FUNDI_DEFAULT_LABELS", "env=staging")
	cmd := newTestCreateCmd()
	if err := cmd.Flags().Set("cwd", "/tmp"); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Flags().Set("label", "env=prod"); err != nil {
		t.Fatal(err)
	}

	req, err := buildSpawnRequest(cmd, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if req.Labels["env"] != "prod" {
		t.Errorf("Labels[env] = %q, want prod (flag wins over env)", req.Labels["env"])
	}
}

func TestBuildSpawnRequest_InvalidLabelKey(t *testing.T) {
	cmd := newTestCreateCmd()
	if err := cmd.Flags().Set("cwd", "/tmp"); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Flags().Set("label", "bad key=val"); err != nil {
		t.Fatal(err)
	}
	_, err := buildSpawnRequest(cmd, nil)
	if err == nil {
		t.Fatal("expected error for invalid label key")
	}
}

func TestBuildSpawnRequest_KindClaude(t *testing.T) {
	cmd := newTestCreateCmd()
	if err := cmd.Flags().Set("cwd", "/tmp"); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Flags().Set("kind", "claude"); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Flags().Set("config-dir", "/x"); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Flags().Set("append-system-prompt", "be terse"); err != nil {
		t.Fatal(err)
	}

	req, err := buildSpawnRequest(cmd, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if req.Kind != "claude" {
		t.Errorf("Kind = %q, want claude", req.Kind)
	}
	if req.ConfigDir != "/x" {
		t.Errorf("ConfigDir = %q, want /x", req.ConfigDir)
	}
	if req.AppendSystemPrompt != "be terse" {
		t.Errorf("AppendSystemPrompt = %q, want 'be terse'", req.AppendSystemPrompt)
	}
}

func TestBuildSpawnRequest_KindDefaultsPi(t *testing.T) {
	cmd := newTestCreateCmd()
	if err := cmd.Flags().Set("cwd", "/tmp"); err != nil {
		t.Fatal(err)
	}

	req, err := buildSpawnRequest(cmd, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if req.Kind != "pi" {
		t.Errorf("Kind = %q, want pi (default)", req.Kind)
	}
	if req.ConfigDir != "" {
		t.Errorf("ConfigDir = %q, want empty", req.ConfigDir)
	}
}

func TestBuildSpawnRequest_ReservedLabelKey(t *testing.T) {
	cmd := newTestCreateCmd()
	if err := cmd.Flags().Set("cwd", "/tmp"); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Flags().Set("label", "fundi/model=evil"); err != nil {
		t.Fatal(err)
	}
	_, err := buildSpawnRequest(cmd, nil)
	if err == nil {
		t.Fatal("expected error for fundi/ prefix")
	}
}
