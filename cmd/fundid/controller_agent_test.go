package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"git.graveland.dev/brent/fundi/internal/child"
	"git.graveland.dev/brent/fundi/internal/server"
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
// exec'ing a child that immediately dies on `fundid agent`'s own flag-parse
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

// TestResolveSpawnPlanAgentKindBareModelFlagRequiresValue covers the fix to
// agentSpawnHasModel: a bare "--model" token in ExtraArgs with no following
// value (either because it's the last element, or because the next element
// is itself another flag) must NOT satisfy the required-model guard -
// parseAgentFlags would fail on it exactly as if --model were absent.
func TestResolveSpawnPlanAgentKindBareModelFlagRequiresValue(t *testing.T) {
	cases := []struct {
		name      string
		extraArgs []string
	}{
		{"trailing bare --model", []string{"--fake-turns", "/tmp/x.ndjson", "--model"}},
		{"--model immediately followed by another flag", []string{"--model", "--thinking"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := protocol.SpawnRequest{Kind: "agent", ExtraArgs: tc.extraArgs}
			if _, _, _, err := resolveSpawnPlan(req, "c_testbare", "/var/fundi-state"); err == nil {
				t.Fatalf("resolveSpawnPlan(agent kind, %s): want error, got nil", tc.name)
			}
		})
	}
}

// TestResolveSpawnPlanAgentKindRejectsProvider covers the invariant that the
// agent kind carries its provider inside the model id (e.g.
// "anthropic/sonnet-latest"), not as a separate field - a non-empty
// req.Provider must be rejected at spawn time rather than silently dropped
// or double-prefixed onto the reported model.
func TestResolveSpawnPlanAgentKindRejectsProvider(t *testing.T) {
	req := protocol.SpawnRequest{Kind: "agent", Model: "anthropic/sonnet-latest", Provider: "anthropic"}
	if _, _, _, err := resolveSpawnPlan(req, "c_testprov", "/var/fundi-state"); err == nil {
		t.Fatal("resolveSpawnPlan(agent kind, Provider set): want error, got nil")
	}
}

// TestResumeRequestFromSnapshotAgentRejoinsModel is the other half of
// TestResolveSpawnPlanAgentKindRejectsProvider, and the pairing is the whole
// point: the rejection above was tested, but nothing tested the resume path
// that FEEDS resolveSpawnPlan, so `fundi resume` was broken for every
// agent-kind child while both halves looked covered.
//
// At spawn time the controller splits the child-reported model with splitModel
// and stores the halves separately (snap.Provider="anthropic",
// snap.Model="sonnet-latest"). The agent kind requires the opposite shape: the
// provider folded into Model, and Provider empty. So the snapshot must be
// rejoined on the way back out, or resume dies with "agent kind does not
// accept a separate Provider".
//
// The load-bearing assertion is the last one: the rebuilt request must be
// something resolveSpawnPlan actually ACCEPTS. That holds regardless of how
// the rejoin is implemented, so it still catches a future re-break.
func TestResumeRequestFromSnapshotAgentRejoinsModel(t *testing.T) {
	snap := store.Snapshot{
		Kind:     "agent",
		Cwd:      "/tmp/fundi-smoke",
		Name:     "smoke6",
		Provider: "anthropic",
		Model:    "sonnet-latest",
	}

	req := resumeRequestFromSnapshot(snap, "")

	if req.Provider != "" {
		t.Errorf("Provider = %q, want empty: the agent kind takes no separate provider", req.Provider)
	}
	if req.Model != "anthropic/sonnet-latest" {
		t.Errorf("Model = %q, want %q (provider rejoined onto the model id)", req.Model, "anthropic/sonnet-latest")
	}
	if _, _, _, err := resolveSpawnPlan(req, "c_resume", "/var/fundi-state"); err != nil {
		t.Fatalf("resolveSpawnPlan(resumed agent request): %v\nresume must produce a request the spawn planner accepts", err)
	}
}

// TestResumeRequestFromSnapshotAgentBareModel covers the degenerate snapshot
// where no provider was ever recorded: joinModel must leave the model alone
// rather than emitting a leading "/", which would resolve to a bogus provider.
func TestResumeRequestFromSnapshotAgentBareModel(t *testing.T) {
	snap := store.Snapshot{Kind: "agent", Model: "sonnet-latest"}
	req := resumeRequestFromSnapshot(snap, "")
	if req.Model != "sonnet-latest" {
		t.Errorf("Model = %q, want %q unchanged", req.Model, "sonnet-latest")
	}
	if req.Provider != "" {
		t.Errorf("Provider = %q, want empty", req.Provider)
	}
}

// TestResumeRequestFromSnapshotCarriesSkillsDirsAndMCPConfig covers I4: a
// resumed agent-kind child must rejoin with the same skill inventory and MCP
// tool set it was spawned with, not a silently shrunk one.
func TestResumeRequestFromSnapshotCarriesSkillsDirsAndMCPConfig(t *testing.T) {
	snap := store.Snapshot{
		Kind:       "agent",
		Model:      "anthropic/claude-sonnet-5",
		SkillsDirs: []string{"/work/skills", "/other/skills"},
		MCPConfig:  "/work/.mcp.json",
	}
	req := resumeRequestFromSnapshot(snap, "")

	if !reflect.DeepEqual(req.SkillsDirs, snap.SkillsDirs) {
		t.Errorf("SkillsDirs = %v, want %v — a resumed child silently loses its skill dirs", req.SkillsDirs, snap.SkillsDirs)
	}
	if req.MCPConfig != snap.MCPConfig {
		t.Errorf("MCPConfig = %q, want %q — a resumed child silently loses its MCP servers", req.MCPConfig, snap.MCPConfig)
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

// TestBuildAgentArgv_RendersSkillsDirsAndMCPConfig covers task A6: --skills-dir
// and --mcp-config, previously reachable only via --extra-arg, now render
// straight from their own SpawnRequest fields.
func TestBuildAgentArgv_RendersSkillsDirsAndMCPConfig(t *testing.T) {
	req := protocol.SpawnRequest{
		Kind:       "agent",
		Model:      "anthropic/claude-sonnet-5",
		SkillsDirs: []string{"/a/skills", "/b/skills"},
		MCPConfig:  "/cfg/.mcp.json",
	}
	argv := buildAgentArgv(req, "child-1", "/state")
	joined := strings.Join(argv, " ")

	if strings.Count(joined, "--skills-dir") != 2 {
		t.Errorf("want one --skills-dir per entry, got: %v", argv)
	}
	if !strings.Contains(joined, "--skills-dir /a/skills") ||
		!strings.Contains(joined, "--skills-dir /b/skills") {
		t.Errorf("skills dirs missing from argv: %v", argv)
	}
	if !strings.Contains(joined, "--mcp-config /cfg/.mcp.json") {
		t.Errorf("mcp config missing from argv: %v", argv)
	}
}

// TestBuildAgentArgv_OmitsUnsetKnobs confirms --skills-dir/--mcp-config are
// omitted entirely when the request leaves them unset, matching the rest of
// buildAgentArgv's "only emit what's set" convention.
func TestBuildAgentArgv_OmitsUnsetKnobs(t *testing.T) {
	req := protocol.SpawnRequest{Kind: "agent", Model: "anthropic/claude-sonnet-5"}
	joined := strings.Join(buildAgentArgv(req, "child-1", "/state"), " ")
	if strings.Contains(joined, "--skills-dir") || strings.Contains(joined, "--mcp-config") {
		t.Errorf("unset knobs must not appear: %s", joined)
	}
}

// TestSpawnKindLabel_Agent covers the fundi/kind auto-label for the new kind.
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
// bulk sweep path (ForgetAllExited), used by the sweeper and 'fundi forget
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

// TestSend_RejectsSessionSwitchForAgentChild covers the silent-wrong-answer
// case RespawnChild's routing through resolveSpawnPlan created.
//
// new_session/switch_session are intercepted and implemented as a respawn with
// a session override. That override is meaningless for an agent child:
// buildAgentArgv ignores ResumeSession, and appendDaemonRef pins --ref to the
// unchanged childID, so the respawned child reattaches the ENTIRE prior
// conversation while the RPC reports success. A user asking for a fresh
// session would silently get their old one. Before the routing fix the same
// request produced a dead child, which at least surfaced the problem.
//
// Both commands must be refused, for both a live and an exited agent child.
func TestSend_RejectsSessionSwitchForAgentChild(t *testing.T) {
	for _, frame := range []string{
		`{"type":"new_session","id":"req-1"}`,
		`{"type":"switch_session","id":"req-2","sessionPath":"/tmp/other.jsonl"}`,
	} {
		t.Run(frame, func(t *testing.T) {
			ctrl := newTestController(t)
			const childID = "c_agent_session_switch"
			now := time.Now()
			ctrl.st.Insert(&store.Session{
				ChildID:      childID,
				Status:       protocol.StatusIdle,
				Kind:         "agent",
				Cwd:          t.TempDir(),
				StartedAt:    now,
				LastActivity: now,
			})

			err := ctrl.Send(childID, json.RawMessage(frame))
			if err == nil {
				t.Fatal("Send accepted a session switch for an agent child; it would silently reattach the same conversation")
			}
			var ce *server.ControllerError
			if !errors.As(err, &ce) {
				t.Fatalf("error is %T, want *server.ControllerError so the client sees a coded failure: %v", err, err)
			}
			if ce.Code != protocol.ErrInvalidArgs {
				t.Errorf("error code = %q, want %q", ce.Code, protocol.ErrInvalidArgs)
			}
			if !strings.Contains(ce.Message, "agent child") {
				t.Errorf("message %q does not explain that the agent kind is the problem", ce.Message)
			}

			// The child must be left completely alone: the rejection happens
			// before the kill+respawn ceremony, so it is still exactly as it was.
			snap, ok := ctrl.st.Get(childID)
			if !ok {
				t.Fatal("the rejected request removed the child from the store")
			}
			if snap.Status != protocol.StatusIdle {
				t.Errorf("child status = %q after a rejected session switch, want %q untouched",
					snap.Status, protocol.StatusIdle)
			}
		})
	}
}

// TestSend_AllowsSessionSwitchForNonAgentKinds pins the negative half: the
// rejection above must be scoped to the agent kind and not quietly break
// new_session for pi or claude children. A pi child with no live process
// fails later in the respawn ceremony, which is fine — what matters is that it
// is NOT refused up front with the agent-kind error.
func TestSend_AllowsSessionSwitchForNonAgentKinds(t *testing.T) {
	for _, kind := range []string{"", "pi", "claude"} {
		t.Run("kind="+kind, func(t *testing.T) {
			ctrl := newTestController(t)
			const childID = "c_nonagent_session_switch"
			now := time.Now()
			ctrl.st.Insert(&store.Session{
				ChildID:      childID,
				Status:       protocol.StatusIdle,
				Kind:         kind,
				Cwd:          t.TempDir(),
				StartedAt:    now,
				LastActivity: now,
			})

			err := ctrl.Send(childID, json.RawMessage(`{"type":"new_session","id":"req-1"}`))
			var ce *server.ControllerError
			if errors.As(err, &ce) && ce.Code == protocol.ErrInvalidArgs &&
				strings.Contains(ce.Message, "agent child") {
				t.Fatalf("kind %q was refused with the agent-kind rejection: %v", kind, err)
			}
		})
	}
}
