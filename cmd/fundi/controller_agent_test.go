package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"git.graveland.dev/brent/fundi/internal/child"
	"git.graveland.dev/brent/fundi/internal/store"
	"git.graveland.dev/brent/fundi/protocol"
)

// TestResolveSpawnPlanAgentKind covers R1: the "agent" case resolves to the
// daemon's own binary (self re-exec) with the native pi protocol (no
// translator - the agent runtime speaks pi's rpc protocol directly).
func TestResolveSpawnPlanAgentKind(t *testing.T) {
	req := protocol.SpawnRequest{
		Kind:               "agent",
		Model:              "deepseek/deepseek-chat",
		Thinking:           "low",
		SystemPrompt:       "sp",
		AppendSystemPrompt: "asp",
		Skills:             []string{"a", "b"},
		NoContextFiles:     true,
		Name:               "my-session",
		ExtraArgs:          []string{"--fake-turns", "/tmp/turns.ndjson"},
	}

	bin, argv, prov, err := resolveSpawnPlan(req, "c_test123", "/var/fundi-state")
	if err != nil {
		t.Fatalf("resolveSpawnPlan: %v", err)
	}

	self, selfErr := os.Executable()
	if selfErr != nil {
		t.Fatalf("os.Executable: %v", selfErr)
	}
	if bin != self {
		t.Fatalf("bin = %q, want self-exec %q", bin, self)
	}

	if _, ok := prov.(child.PiProvider); !ok {
		t.Fatalf("provider = %T, want child.PiProvider (agent speaks pi protocol natively)", prov)
	}

	if len(argv) == 0 || argv[0] != "agent" {
		t.Fatalf("argv[0] = %v, want \"agent\" subcommand token: %v", argv, argv)
	}

	joined := strings.Join(argv, " ")
	for _, want := range []string{
		"--model deepseek/deepseek-chat",
		"--thinking low",
		"--system-prompt sp",
		"--append-system-prompt asp",
		"--skills a,b",
		"--no-context-files",
		"--name my-session",
		"--spill-dir " + filepath.Join("/var/fundi-state", "spill", "c_test123"),
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("argv missing %q: %v", want, argv)
		}
	}

	// --provider no longer exists as a flag - the model id alone determines
	// routing (see internal/agent/config.go's senderOptions).
	if strings.Contains(joined, "--provider") {
		t.Fatalf("argv unexpectedly contains --provider (removed in the provider/model redesign): %v", argv)
	}

	// ExtraArgs must remain the trailing tokens (last-flag-wins escape hatch).
	if argv[len(argv)-2] != "--fake-turns" || argv[len(argv)-1] != "/tmp/turns.ndjson" {
		t.Fatalf("ExtraArgs not appended last: %v", argv)
	}
}

// TestResolveSpawnPlanAgentKindRequiresModel covers the daemon-side half of
// the redesign's required-model invariant: an "agent" kind spawn with no
// resolvable model (neither SpawnRequest.Model nor a --model in ExtraArgs) is
// rejected at spawn time with a clean control-plane error, rather than
// exec'ing a child that immediately dies on `fundi agent`'s own flag-parse
// error.
func TestResolveSpawnPlanAgentKindRequiresModel(t *testing.T) {
	req := protocol.SpawnRequest{Kind: "agent"}
	if _, _, _, err := resolveSpawnPlan(req, "c_test456", "/var/fundi-state"); err == nil {
		t.Fatal("resolveSpawnPlan(agent kind, no model): want error, got nil")
	}
}

// TestResolveSpawnPlanAgentKindModelViaExtraArgs confirms the ExtraArgs
// escape hatch still satisfies the required-model check: a caller can supply
// --model through ExtraArgs instead of SpawnRequest.Model.
func TestResolveSpawnPlanAgentKindModelViaExtraArgs(t *testing.T) {
	req := protocol.SpawnRequest{Kind: "agent", ExtraArgs: []string{"--model", "anthropic/sonnet-latest"}}
	if _, _, _, err := resolveSpawnPlan(req, "c_test789", "/var/fundi-state"); err != nil {
		t.Fatalf("resolveSpawnPlan(agent kind, model via ExtraArgs): unexpected error: %v", err)
	}
}

// TestBuildAgentArgv_NoSkillsAndDefaults confirms the no-skills / minimal
// request path emits only --spill-dir plus whatever ExtraArgs were given, with
// none of the optional flags present when the request leaves them empty.
func TestBuildAgentArgv_NoSkillsAndDefaults(t *testing.T) {
	req := protocol.SpawnRequest{Kind: "agent", NoSkills: true}
	argv := buildAgentArgv(req, "c1", "/state")

	joined := strings.Join(argv, " ")
	if !strings.Contains(joined, "--no-skills") {
		t.Fatalf("argv missing --no-skills: %v", argv)
	}
	for _, unwanted := range []string{"--model", "--thinking", "--system-prompt", "--skills ", "--name"} {
		if strings.Contains(joined, unwanted) {
			t.Fatalf("argv unexpectedly contains %q: %v", unwanted, argv)
		}
	}
}

// TestBuildAgentArgv_SpillDirPinnedBeforeExtraArgs confirms --spill-dir is
// always emitted (so Forget can find and remove it deterministically) and
// that ExtraArgs come strictly after it, preserving last-flag-wins semantics
// (an ExtraArgs override of --spill-dir would win, matching pi/claude kinds).
func TestBuildAgentArgv_SpillDirPinnedBeforeExtraArgs(t *testing.T) {
	req := protocol.SpawnRequest{ExtraArgs: []string{"--fake-turns", "/tmp/t.ndjson"}}
	argv := buildAgentArgv(req, "c9", "/state-dir")

	spillIdx := -1
	extraIdx := -1
	for i, a := range argv {
		if a == "--spill-dir" {
			spillIdx = i
		}
		if a == "--fake-turns" {
			extraIdx = i
		}
	}
	if spillIdx == -1 {
		t.Fatalf("argv missing --spill-dir: %v", argv)
	}
	if extraIdx == -1 || extraIdx < spillIdx {
		t.Fatalf("ExtraArgs must come after --spill-dir: %v", argv)
	}
	wantSpill := filepath.Join("/state-dir", "spill", "c9")
	if argv[spillIdx+1] != wantSpill {
		t.Fatalf("--spill-dir value = %q, want %q", argv[spillIdx+1], wantSpill)
	}
}

// TestSpawnKindLabel_Agent covers the pic/kind auto-label for the new kind.
func TestSpawnKindLabel_Agent(t *testing.T) {
	if got := spawnKindLabel("agent"); got != "agent" {
		t.Fatalf("spawnKindLabel(\"agent\") = %q, want \"agent\"", got)
	}
}

// TestForget_RemovesAgentSpillDir covers R4: Forget removes the agent kind's
// spill directory, mirroring the existing deleteLogDump cleanup pattern. Uses
// a sentinel file rather than actually spawning an agent child, since the
// spill dir's existence (not its contents) is what Forget must guarantee is
// cleaned up.
func TestForget_RemovesAgentSpillDir(t *testing.T) {
	ctrl := newTestController(t)

	const childID = "c_spill_forget_test"
	spillDir := agentSpillDir(ctrl.stateDir, childID)
	if err := os.MkdirAll(spillDir, 0o700); err != nil {
		t.Fatalf("mkdirall spill dir: %v", err)
	}
	sentinel := filepath.Join(spillDir, "clipped-output.txt")
	if err := os.WriteFile(sentinel, []byte("clipped tool output"), 0o600); err != nil {
		t.Fatalf("write sentinel file: %v", err)
	}

	now := time.Now()
	ctrl.st.Insert(&store.Session{
		ChildID:      childID,
		Status:       protocol.StatusExited,
		Kind:         "agent",
		Cwd:          t.TempDir(),
		StartedAt:    now,
		LastActivity: now,
		ExitedAt:     now,
	})

	if err := ctrl.Forget(childID); err != nil {
		t.Fatalf("Forget: %v", err)
	}

	if _, err := os.Stat(spillDir); !os.IsNotExist(err) {
		t.Fatalf("spill dir %s still exists after Forget (err=%v)", spillDir, err)
	}
}

// TestForgetAllExited_RemovesAgentSpillDir covers the same cleanup via the
// bulk sweep path (ForgetAllExited), used by the sweeper and 'pic forget
// --all'.
func TestForgetAllExited_RemovesAgentSpillDir(t *testing.T) {
	ctrl := newTestController(t)

	const childID = "c_spill_forgetall_test"
	spillDir := agentSpillDir(ctrl.stateDir, childID)
	if err := os.MkdirAll(spillDir, 0o700); err != nil {
		t.Fatalf("mkdirall spill dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(spillDir, "clipped-output.txt"), []byte("x"), 0o600); err != nil {
		t.Fatalf("write sentinel file: %v", err)
	}

	now := time.Now()
	ctrl.st.Insert(&store.Session{
		ChildID:      childID,
		Status:       protocol.StatusExited,
		Kind:         "agent",
		Cwd:          t.TempDir(),
		StartedAt:    now,
		LastActivity: now,
		ExitedAt:     now,
	})

	n, err := ctrl.ForgetAllExited(0)
	if err != nil {
		t.Fatalf("ForgetAllExited: %v", err)
	}
	if n != 1 {
		t.Fatalf("ForgetAllExited count = %d, want 1", n)
	}

	if _, err := os.Stat(spillDir); !os.IsNotExist(err) {
		t.Fatalf("spill dir %s still exists after ForgetAllExited (err=%v)", spillDir, err)
	}
}
