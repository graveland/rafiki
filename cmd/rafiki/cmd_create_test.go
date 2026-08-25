package main

import (
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"go.graveland.dev/rafiki/pkg/paths"
	"go.graveland.dev/rafiki/pkg/protocol"
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
	// When --cwd is omitted, buildSpawnRequest should use os.Getwd() — but
	// only against the local daemon; isolate from a real RAFIKI_URL set in
	// the ambient environment (e.g. a dev shell pointed at a remote daemon),
	// or this test's outcome depends on who is running it.
	t.Setenv("RAFIKI_URL", "")

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

// A pi/claude child is a literal subprocess of rafikid (cmd.Dir = req.Cwd,
// pkg/child/runner.go) — its cwd genuinely must exist on the daemon's own
// machine, so defaulting it from the CLIENT's cwd against a remote daemon
// would silently ship a path valid only here.
func TestBuildSpawnRequest_RemoteRequiresExplicitCwdForPi(t *testing.T) {
	t.Setenv("RAFIKI_URL", "https://rafiki.example.dev")

	cmd := newTestCreateCmd()
	if err := cmd.Flags().Set("kind", protocol.KindPi); err != nil {
		t.Fatal(err)
	}
	// cwd left at its zero value ("") intentionally: there is no local
	// directory to default to on a remote daemon's filesystem.

	_, err := buildSpawnRequest(cmd, nil)
	if err == nil {
		t.Fatal("expected error defaulting --cwd against a remote RAFIKI_URL, got nil")
	}
	if !strings.Contains(err.Error(), "--cwd") {
		t.Errorf("error = %q, want it to mention --cwd", err.Error())
	}

	// An explicit --cwd is still honored against a remote daemon.
	if err := cmd.Flags().Set("cwd", "/remote/project"); err != nil {
		t.Fatal(err)
	}
	req, err := buildSpawnRequest(cmd, nil)
	if err != nil {
		t.Fatalf("unexpected error with explicit --cwd: %v", err)
	}
	if req.Cwd != "/remote/project" {
		t.Errorf("Cwd = %q, want /remote/project", req.Cwd)
	}
}

// A fundi child never forks a daemon-local process: its filesystem access, if
// any, goes through whichever executor gets bound — by default the session
// executor `rafiki create` starts on the CLIENT's own machine, rooted at
// exactly this cwd. So the client's own os.Getwd() is always a valid default,
// remote daemon or not, and this must NOT error the way the pi/claude case does.
func TestBuildSpawnRequest_RemoteDefaultsCwdForFundi(t *testing.T) {
	t.Setenv("RAFIKI_URL", "https://rafiki.example.dev")

	wantCwd, err := os.Getwd()
	if err != nil {
		t.Skip("os.Getwd() failed — skipping:", err)
	}

	cmd := newTestCreateCmd()
	// kind left at its default (fundi); cwd left at its zero value ("").

	req, err := buildSpawnRequest(cmd, nil)
	if err != nil {
		t.Fatalf("unexpected error defaulting --cwd for a fundi child against a remote daemon: %v", err)
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
	t.Setenv("RAFIKI_DEFAULT_MODEL", "anthropic/claude-haiku-4-5")
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
	t.Setenv("RAFIKI_DEFAULT_MODEL", "env-model")
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
	t.Setenv("RAFIKI_DEFAULT_LABELS", "context=work,env=prod")
	cmd := newTestCreateCmd()
	if err := cmd.Flags().Set("cwd", "/tmp"); err != nil {
		t.Fatal(err)
	}

	req, err := buildSpawnRequest(cmd, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if req.Labels["context"] != "work" || req.Labels["env"] != "prod" {
		t.Errorf("Labels from RAFIKI_DEFAULT_LABELS: %v", req.Labels)
	}
}

func TestBuildSpawnRequest_FlagLabelWinsOverEnv(t *testing.T) {
	// Explicit --label should override RAFIKI_DEFAULT_LABELS on the same key.
	t.Setenv("RAFIKI_DEFAULT_LABELS", "env=staging")
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
	if err := cmd.Flags().Set("kind", protocol.KindClaude); err != nil {
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
	if req.Kind != protocol.KindClaude {
		t.Errorf("Kind = %q, want claude", req.Kind)
	}
	if req.ConfigDir != "/x" {
		t.Errorf("ConfigDir = %q, want /x", req.ConfigDir)
	}
	if req.AppendSystemPrompt != "be terse" {
		t.Errorf("AppendSystemPrompt = %q, want 'be terse'", req.AppendSystemPrompt)
	}
}

func TestBuildSpawnRequest_KindDefaultsAgent(t *testing.T) {
	cmd := newTestCreateCmd()
	if err := cmd.Flags().Set("cwd", "/tmp"); err != nil {
		t.Fatal(err)
	}

	req, err := buildSpawnRequest(cmd, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// The default is the native runtime, not a pi subprocess: it is the kind
	// with in-band abort and per-turn cost accounting, and the only one whose
	// model ids this repo can resolve. --model completion keys off the same
	// default (see modelSourcesForKind).
	if req.Kind != protocol.KindFundi {
		t.Errorf("Kind = %q, want fundi (default)", req.Kind)
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
	if err := cmd.Flags().Set("label", "rafiki/model=evil"); err != nil {
		t.Fatal(err)
	}
	_, err := buildSpawnRequest(cmd, nil)
	if err == nil {
		t.Fatal("expected error for rafiki/ prefix")
	}
}

func TestBuildSpawnRequest_ParentFlag(t *testing.T) {
	cmd := newTestCreateCmd()
	if err := cmd.Flags().Set("cwd", "/tmp"); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Flags().Set("parent", "c_abc123"); err != nil {
		t.Fatal(err)
	}
	req, err := buildSpawnRequest(cmd, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if req.ParentChildID != "c_abc123" {
		t.Fatalf("ParentChildID = %q; want c_abc123", req.ParentChildID)
	}
}

func TestBuildSpawnRequest_ParentFlagOmitted(t *testing.T) {
	cmd := newTestCreateCmd()
	if err := cmd.Flags().Set("cwd", "/tmp"); err != nil {
		t.Fatal(err)
	}
	req, err := buildSpawnRequest(cmd, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if req.ParentChildID != "" {
		t.Errorf("ParentChildID = %q, want empty", req.ParentChildID)
	}
}

// The flag is the only way a human can target an executor: the sole other
// writer of SpawnRequest.ExecutorSelector in this repo is the in-process
// agent_spawn tool.
func TestBuildSpawnRequest_ExecutorSelectorFlag(t *testing.T) {
	t.Setenv(paths.ExecutorSelector, "")

	cmd := newTestCreateCmd()
	if err := cmd.Flags().Set("executor-selector", "owner=brent,env=home"); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Flags().Set("cwd", "/w"); err != nil {
		t.Fatal(err)
	}

	req, err := buildSpawnRequest(cmd, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if req.ExecutorSelector != "owner=brent,env=home" {
		t.Errorf("ExecutorSelector = %q, want %q", req.ExecutorSelector, "owner=brent,env=home")
	}
}

// The environment default must apply when the flag is NOT passed. This is the
// half that --executor-socket got wrong: gating the read on Flags().Changed()
// makes a computed flag default unreachable, because Changed() reports whether
// the user typed the flag, not whether the value is non-zero.
//
// t.Setenv MUST precede newTestCreateCmd: addSpawnFlags evaluates
// paths.Get(...) when it REGISTERS the flag, not when the flag is read.
func TestBuildSpawnRequest_ExecutorSelectorFromEnv(t *testing.T) {
	t.Setenv(paths.ExecutorSelector, "owner=brent")

	cmd := newTestCreateCmd()
	if err := cmd.Flags().Set("cwd", "/w"); err != nil {
		t.Fatal(err)
	}

	req, err := buildSpawnRequest(cmd, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if req.ExecutorSelector != "owner=brent" {
		t.Errorf("ExecutorSelector = %q, want the env default %q", req.ExecutorSelector, "owner=brent")
	}
}

// An explicit flag beats the environment.
func TestBuildSpawnRequest_ExecutorSelectorFlagBeatsEnv(t *testing.T) {
	t.Setenv(paths.ExecutorSelector, "owner=brent")

	cmd := newTestCreateCmd()
	if err := cmd.Flags().Set("executor-selector", "env=ci"); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Flags().Set("cwd", "/w"); err != nil {
		t.Fatal(err)
	}

	req, err := buildSpawnRequest(cmd, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if req.ExecutorSelector != "env=ci" {
		t.Errorf("ExecutorSelector = %q, want the flag value %q", req.ExecutorSelector, "env=ci")
	}
}

// --no-local-executor is a session posture, not a spawn field. It must not
// appear on the wire: the daemon has no opinion about whether the client
// offered its own machine.
func TestNoLocalExecutorIsNotASpawnField(t *testing.T) {
	t.Setenv(paths.ExecutorSelector, "")

	cmd := newTestCreateCmd()
	if err := cmd.Flags().Set("no-local-executor", "true"); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Flags().Set("cwd", "/w"); err != nil {
		t.Fatal(err)
	}

	req, err := buildSpawnRequest(cmd, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if req.ExecutorSelector != "" {
		t.Errorf("ExecutorSelector = %q, want empty", req.ExecutorSelector)
	}
}
