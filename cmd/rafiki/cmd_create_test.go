package main

import (
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"go.graveland.dev/rafiki/pkg/clientstate"
	"go.graveland.dev/rafiki/pkg/paths"
	"go.graveland.dev/rafiki/pkg/profile"
	"go.graveland.dev/rafiki/pkg/protocol"
)

// remoteProfileForTest seeds an isolated profile manifest naming a remote
// daemon, and points the process at it — for tests exercising buildSpawnRequest's
// --cwd-against-a-remote-profile check (mustProfile(cmd).URL != "").
func remoteProfileForTest(t *testing.T, url string) {
	t.Helper()
	isolateProfiles(t)
	resetProfileCache()
	if err := profile.Save(profile.Set{Profiles: map[string]profile.Profile{
		"remote": {Name: "remote", URL: url},
	}}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := profile.SavePointer("remote"); err != nil {
		t.Fatalf("SavePointer: %v", err)
	}
}

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
	// When --cwd is omitted, buildSpawnRequest should use os.Getwd(). The
	// default kind is fundi, which defaults cwd unconditionally regardless of
	// where the profile's daemon lives — its filesystem access goes through
	// whichever executor gets bound, not the daemon's own process — so there
	// is no remote-profile branch to isolate from here (unlike the claude
	// case below).
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

// A claude child is a literal subprocess of rafikid (cmd.Dir = req.Cwd,
// pkg/child/runner.go) — its cwd genuinely must exist on the daemon's own
// machine, so defaulting it from the CLIENT's cwd against a remote daemon
// would silently ship a path valid only here.
func TestBuildSpawnRequest_RemoteRequiresExplicitCwdForClaude(t *testing.T) {
	remoteProfileForTest(t, "https://rafiki.example.dev")

	cmd := newTestCreateCmd()
	if err := cmd.Flags().Set("kind", protocol.KindClaude); err != nil {
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
// remote daemon or not, and this must NOT error the way the claude case does.
func TestBuildSpawnRequest_RemoteDefaultsCwdForFundi(t *testing.T) {
	remoteProfileForTest(t, "https://rafiki.example.dev")

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

// localProfileForTest seeds an isolated profile manifest naming a local
// socket, with the given spawn defaults, and points the process at it.
// RAFIKI_DEFAULT_MODEL/PRESET/LABELS are retired client-side (profile.CheckRetiredEnv
// errors on them via mustProfile), so any test that used to exercise a
// default via those variables seeds a profile field instead.
func localProfileForTest(t *testing.T, p profile.Profile) {
	t.Helper()
	isolateProfiles(t)
	resetProfileCache()
	p.Name = "test"
	p.Socket = "/tmp/rafiki-test.sock"
	if err := profile.Save(profile.Set{Profiles: map[string]profile.Profile{"test": p}}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := profile.SavePointer("test"); err != nil {
		t.Fatalf("SavePointer: %v", err)
	}
}

// TestBuildSpawnRequest_ModelNotDefaultedFromProfile confirms Step 4.3: the
// profile's model is no longer applied inside buildSpawnRequest at all —
// req.Model carries only the --model flag. The full chain (preset > profile >
// remembered) is finished off in runCreate via resolveModel, pinned by
// TestModelPrecedence.
func TestBuildSpawnRequest_ModelNotDefaultedFromProfile(t *testing.T) {
	localProfileForTest(t, profile.Profile{Model: "prof-model"})
	cmd := newTestCreateCmd()
	if err := cmd.Flags().Set("cwd", "/tmp"); err != nil {
		t.Fatal(err)
	}
	req, err := buildSpawnRequest(cmd, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if req.Model != "" {
		t.Errorf("Model = %q, want empty (buildSpawnRequest must not apply the profile default)", req.Model)
	}
}

func TestBuildSpawnRequest_ProfileDefaultLabels(t *testing.T) {
	localProfileForTest(t, profile.Profile{Labels: map[string]string{"context": "work", "env": "prod"}})
	cmd := newTestCreateCmd()
	if err := cmd.Flags().Set("cwd", "/tmp"); err != nil {
		t.Fatal(err)
	}

	req, err := buildSpawnRequest(cmd, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if req.Labels["context"] != "work" || req.Labels["env"] != "prod" {
		t.Errorf("Labels from the profile's default labels: %v", req.Labels)
	}
}

func TestBuildSpawnRequest_FlagLabelWinsOverProfile(t *testing.T) {
	// Explicit --label should override the profile's labels on the same key.
	localProfileForTest(t, profile.Profile{Labels: map[string]string{"env": "staging"}})
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
		t.Errorf("Labels[env] = %q, want prod (flag wins over profile default)", req.Labels["env"])
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
	// The default is the native runtime, not a foreign subprocess: it is the kind
	// with in-band abort and per-turn cost accounting, and the only one whose
	// model ids this repo can resolve. --model completion keys off the same
	// default (kind scoping lives daemon-side; see cmd/rafikid's sourcesForKind).
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

func TestMaxCostConvertsThroughConfiguredCurrency(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", dir)
	clientstate.UpdateScoped(clientstate.Scope{}, func(s *clientstate.State) {
		s.Currency = &clientstate.Currency{Code: "CAD", Rate: 1.38}
	})

	cmd := newTestCreateCmd()
	if err := cmd.Flags().Set("cwd", t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Flags().Set("max-cost", "13.80"); err != nil {
		t.Fatal(err)
	}

	req, err := buildSpawnRequest(cmd, nil)
	if err != nil {
		t.Fatal(err)
	}
	if req.MaxCost == nil {
		t.Fatal("MaxCost is nil, want a converted USD value")
	}
	if diff := *req.MaxCost - 10.0; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("MaxCost = %v, want ~10.0 (13.80 CAD at 1.38 CAD/USD)", *req.MaxCost)
	}
}

func TestMaxCostWithNoCurrencyIsUnconverted(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", dir)

	cmd := newTestCreateCmd()
	if err := cmd.Flags().Set("cwd", t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Flags().Set("max-cost", "10"); err != nil {
		t.Fatal(err)
	}

	req, err := buildSpawnRequest(cmd, nil)
	if err != nil {
		t.Fatal(err)
	}
	if req.MaxCost == nil || *req.MaxCost != 10 {
		t.Errorf("MaxCost = %v, want 10 (no currency configured)", req.MaxCost)
	}
}
