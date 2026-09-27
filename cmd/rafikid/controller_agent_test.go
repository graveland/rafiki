package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.graveland.dev/rafiki/pkg/child"
	"go.graveland.dev/rafiki/pkg/childstore"
	"go.graveland.dev/rafiki/pkg/connectapi"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/proxyenv"

	"github.com/multigres/testkit/assert"
)

// TestResolveSpawnPlanAgentKind covers R1: the "fundi" case resolves to the
// daemon's own binary (self re-exec) with the native pi protocol (no
// translator - the agent runtime speaks pi's rpc protocol directly).
func TestResolveSpawnPlanAgentKind(t *testing.T) {
	c := assert.NewAborting(t)
	req := protocol.SpawnRequest{
		Kind:               protocol.KindFundi,
		Model:              "deepseek/deepseek-chat",
		Thinking:           "low",
		SystemPrompt:       "sp",
		AppendSystemPrompt: "asp",
		Skills:             []string{"a", "b"},
		NoContextFiles:     true,
		Name:               "my-session",
		ExtraArgs:          []string{"--fake-turns", "/tmp/turns.ndjson"},
	}

	bin, argv, prov, err := resolveSpawnPlan(req, "c_test123", "/var/rafiki-state", proxyenv.Values{})
	c.NoError(err, "resolveSpawnPlan")

	self, selfErr := os.Executable()
	c.NoError(selfErr, "os.Executable")
	c.Eq(self, bin, "bin")

	_, ok := prov.(child.IdentityProvider)
	c.True(ok, "provider = %T, want child.IdentityProvider (agent speaks pi protocol natively)", prov)

	c.False(len(argv) == 0 || argv[0] != protocol.KindFundi, "argv[0] = %v, want \"agent\" subcommand token: %v", argv, argv)

	joined := strings.Join(argv, " ")
	for _, want := range []string{
		"--model deepseek/deepseek-chat",
		"--thinking low",
		"--system-prompt sp",
		"--append-system-prompt asp",
		"--skills a,b",
		"--no-context-files",
		"--name my-session",
		"--spill-dir " + filepath.Join("/var/rafiki-state", "spill", "c_test123"),
	} {
		c.StrContains(joined, want, "argv missing %q: %v", want, argv)
	}

	// --provider no longer exists as a flag - the model id alone determines
	// routing (see pkg/fundi/config.go's senderOptions).
	c.NotStrContains(joined, "--provider", "argv unexpectedly contains --provider (removed in the provider/model redesign): %v", argv)

	// ExtraArgs must remain the trailing tokens (last-flag-wins escape hatch).
	c.False(argv[len(argv)-2] != "--fake-turns" || argv[len(argv)-1] != "/tmp/turns.ndjson", "ExtraArgs not appended last: %v", argv)
}

// TestResolveSpawnPlanAgentKindRequiresModel covers the daemon-side half of
// the redesign's required-model invariant: a "fundi" kind spawn with no
// resolvable model (neither SpawnRequest.Model nor a --model in ExtraArgs) is
// rejected at spawn time with a clean control-plane error, rather than
// exec'ing a child that immediately dies on `rafikid agent`'s own flag-parse
// error.
func TestResolveSpawnPlanAgentKindRequiresModel(t *testing.T) {
	req := protocol.SpawnRequest{Kind: protocol.KindFundi}
	_, _, _, err := resolveSpawnPlan(req, "c_test456", "/var/rafiki-state", proxyenv.Values{})
	assert.NewAborting(t).Error(err, "resolveSpawnPlan(agent kind, no model): want error, got nil")
}

// TestResolveSpawnPlanAgentKindModelViaExtraArgs confirms the ExtraArgs
// escape hatch still satisfies the required-model check: a caller can supply
// --model through ExtraArgs instead of SpawnRequest.Model.
func TestResolveSpawnPlanAgentKindModelViaExtraArgs(t *testing.T) {
	req := protocol.SpawnRequest{Kind: protocol.KindFundi, ExtraArgs: []string{"--model", "anthropic/sonnet-latest"}}
	_, _, _, err := resolveSpawnPlan(req, "c_test789", "/var/rafiki-state", proxyenv.Values{})
	assert.NewAborting(t).NoError(err, "resolveSpawnPlan(agent kind, model via ExtraArgs): unexpected error")
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
			req := protocol.SpawnRequest{Kind: protocol.KindFundi, ExtraArgs: tc.extraArgs}
			_, _, _, err := resolveSpawnPlan(req, "c_testbare", "/var/rafiki-state", proxyenv.Values{})
			assert.NewAborting(t).Error(err, "resolveSpawnPlan(agent kind, %s): want error, got nil", tc.name)
		})
	}
}

// TestResolveSpawnPlanAgentKindRejectsProvider covers the invariant that the
// agent kind carries its provider inside the model id (e.g.
// "anthropic/sonnet-latest"), not as a separate field - a non-empty
// req.Provider must be rejected at spawn time rather than silently dropped
// or double-prefixed onto the reported model.
func TestResolveSpawnPlanAgentKindRejectsProvider(t *testing.T) {
	req := protocol.SpawnRequest{Kind: protocol.KindFundi, Model: "anthropic/sonnet-latest", Provider: "anthropic"}
	_, _, _, err := resolveSpawnPlan(req, "c_testprov", "/var/rafiki-state", proxyenv.Values{})
	assert.NewAborting(t).Error(err, "resolveSpawnPlan(agent kind, Provider set): want error, got nil")
}

// TestResumeRequestFromSnapshotAgentRejoinsModel is the other half of
// TestResolveSpawnPlanAgentKindRejectsProvider, and the pairing is the whole
// point: the rejection above was tested, but nothing tested the resume path
// that FEEDS resolveSpawnPlan, so `rafiki resume` was broken for every
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
	c := assert.NewCollecting(t)
	snap := childstore.Snapshot{
		Kind:     protocol.KindFundi,
		Cwd:      "/tmp/rafiki-smoke",
		Name:     "smoke6",
		Provider: "anthropic",
		Model:    "sonnet-latest",
	}

	req := resumeRequestFromSnapshot(snap, "")

	c.Eq("", req.Provider, "Provider")
	c.Eq("anthropic/sonnet-latest", req.Model, "Model")
	_, _, _, err := resolveSpawnPlan(req, "c_resume", "/var/rafiki-state", proxyenv.Values{})
	c.Require().NoError(err, "resolveSpawnPlan(resumed agent request)")
}

// TestResumeRequestFromSnapshotAgentBareModel covers the degenerate snapshot
// where no provider was ever recorded: joinModel must leave the model alone
// rather than emitting a leading "/", which would resolve to a bogus provider.
func TestResumeRequestFromSnapshotAgentBareModel(t *testing.T) {
	c := assert.NewCollecting(t)
	snap := childstore.Snapshot{Kind: protocol.KindFundi, Model: "sonnet-latest"}
	req := resumeRequestFromSnapshot(snap, "")
	c.Eq("sonnet-latest", req.Model, "Model")
	c.Eq("", req.Provider, "Provider")
}

// TestResumeRequestFromSnapshotCarriesSkillsDirsAndMCPConfig covers I4: a
// resumed agent-kind child must rejoin with the same skill inventory and MCP
// tool set it was spawned with, not a silently shrunk one.
func TestResumeRequestFromSnapshotCarriesSkillsDirsAndMCPConfig(t *testing.T) {
	c := assert.NewCollecting(t)
	snap := childstore.Snapshot{
		Kind:       protocol.KindFundi,
		Model:      "anthropic/claude-sonnet-5",
		SkillsDirs: []string{"/work/skills", "/other/skills"},
		MCPConfig:  "/work/.mcp.json",
	}
	req := resumeRequestFromSnapshot(snap, "")

	c.EqDiff(snap.SkillsDirs, req.SkillsDirs, "SkillsDirs")
	c.Eq(snap.MCPConfig, req.MCPConfig, "MCPConfig")
}

// TestResumeRequestFromSnapshotCarriesMCPServersAndNoMCP is the same guard for
// the allowlist half of the MCP knobs. MCPConfig surviving resume is not
// enough: a child spawned with a narrowed --mcp-servers (or --no-mcp) that
// comes back with the full .mcp.json connected is strictly worse than one that
// lost the config file, because the narrowing is usually there to keep a
// small-context model's tool inventory inside its window.
func TestResumeRequestFromSnapshotCarriesMCPServersAndNoMCP(t *testing.T) {
	c := assert.NewCollecting(t)
	snap := childstore.Snapshot{
		Kind:       protocol.KindFundi,
		Model:      "anthropic/claude-sonnet-5",
		MCPConfig:  "/work/.mcp.json",
		MCPServers: []string{"codescan", "cachecache"},
	}
	req := resumeRequestFromSnapshot(snap, "")
	c.EqDiff(snap.MCPServers, req.MCPServers, "MCPServers")

	off := resumeRequestFromSnapshot(childstore.Snapshot{
		Kind:      protocol.KindFundi,
		MCPConfig: "/work/.mcp.json",
		NoMCP:     true,
	}, "")
	c.True(off.NoMCP, "NoMCP = false, want true — a resumed child silently regains MCP it was spawned without")
}

// TestBuildAgentArgv_NoSkillsAndDefaults confirms the no-skills / minimal
// request path emits only --spill-dir plus whatever ExtraArgs were given, with
// none of the optional flags present when the request leaves them empty.
func TestBuildAgentArgv_NoSkillsAndDefaults(t *testing.T) {
	c := assert.NewAborting(t)
	req := protocol.SpawnRequest{Kind: protocol.KindFundi, NoSkills: true}
	argv := buildAgentArgv(req, "c1", "/state")

	joined := strings.Join(argv, " ")
	c.StrContains(joined, "--no-skills", "argv missing --no-skills: %v", argv)
	for _, unwanted := range []string{"--model", "--thinking", "--system-prompt", "--skills ", "--name"} {
		c.NotStrContains(joined, unwanted, "argv unexpectedly contains %q: %v", unwanted, argv)
	}
}

// TestBuildAgentArgv_SpillDirPinnedBeforeExtraArgs confirms --spill-dir is
// always emitted (so Forget can find and remove it deterministically) and
// that ExtraArgs come strictly after it, preserving last-flag-wins semantics
// (an ExtraArgs override of --spill-dir would win, matching pi/claude kinds).
func TestBuildAgentArgv_SpillDirPinnedBeforeExtraArgs(t *testing.T) {
	c := assert.NewAborting(t)
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
	c.NotEq(-1, spillIdx, "argv missing --spill-dir: %v", argv)
	c.False(extraIdx == -1 || extraIdx < spillIdx, "ExtraArgs must come after --spill-dir: %v", argv)
	wantSpill := filepath.Join("/state-dir", "spill", "c9")
	c.Eq(wantSpill, argv[spillIdx+1], "--spill-dir value")
}

// TestBuildAgentArgv_RendersSkillsDirsAndMCPConfig covers task A6: --skills-dir
// and --mcp-config, previously reachable only via --extra-arg, now render
// straight from their own SpawnRequest fields.
func TestBuildAgentArgv_RendersSkillsDirsAndMCPConfig(t *testing.T) {
	c := assert.NewCollecting(t)
	req := protocol.SpawnRequest{
		Kind:       protocol.KindFundi,
		Model:      "anthropic/claude-sonnet-5",
		SkillsDirs: []string{"/a/skills", "/b/skills"},
		MCPConfig:  "/cfg/.mcp.json",
	}
	argv := buildAgentArgv(req, "child-1", "/state")
	joined := strings.Join(argv, " ")

	c.Eq(2, strings.Count(joined, "--skills-dir"), "want one --skills-dir per entry, got: %v", argv)
	c.False(!strings.Contains(joined, "--skills-dir /a/skills") ||
		!strings.Contains(joined, "--skills-dir /b/skills"), "skills dirs missing from argv: %v", argv)
	c.StrContains(joined, "--mcp-config /cfg/.mcp.json", "mcp config missing from argv: %v", argv)
}

// TestBuildAgentArgv_OmitsUnsetKnobs confirms --skills-dir/--mcp-config are
// omitted entirely when the request leaves them unset, matching the rest of
// buildAgentArgv's "only emit what's set" convention.
func TestBuildAgentArgv_OmitsUnsetKnobs(t *testing.T) {
	req := protocol.SpawnRequest{Kind: protocol.KindFundi, Model: "anthropic/claude-sonnet-5"}
	joined := strings.Join(buildAgentArgv(req, "child-1", "/state"), " ")
	assert.NewCollecting(t).False(strings.Contains(joined, "--skills-dir") || strings.Contains(joined, "--mcp-config") || strings.Contains(joined, "--mcp-servers"), "unset knobs must not appear: %s", joined)
}

// TestBuildAgentArgv_RendersMCPServersAndNoMCP confirms --mcp-servers and
// --no-mcp are emitted from their SpawnRequest fields.
func TestBuildAgentArgv_RendersMCPServersAndNoMCP(t *testing.T) {
	c := assert.NewCollecting(t)
	req := protocol.SpawnRequest{
		Kind:       protocol.KindFundi,
		Model:      "anthropic/claude-sonnet-5",
		MCPServers: []string{"codescan", "other"},
	}
	joined := strings.Join(buildAgentArgv(req, "child-1", "/state"), " ")
	c.StrContains(joined, "--mcp-servers codescan,other", "mcp-servers missing from argv")

	req2 := protocol.SpawnRequest{Kind: protocol.KindFundi, Model: "anthropic/claude-sonnet-5", NoMCP: true}
	joined2 := strings.Join(buildAgentArgv(req2, "child-1", "/state"), " ")
	c.StrContains(joined2, "--no-mcp", "--no-mcp missing from argv")
}

// TestSpawnKindLabel_Agent covers the rafiki/kind auto-label for the new kind.
func TestSpawnKindLabel_Agent(t *testing.T) {
	assert.NewAborting(t).Eq(protocol.KindFundi, spawnKindLabel(protocol.KindFundi), "spawnKindLabel(")
}

// TestForget_RemovesAgentSpillDir covers R4: Forget removes the agent kind's
// spill directory, mirroring the existing deleteLogDump cleanup pattern. Uses
// a sentinel file rather than actually spawning an agent child, since the
// spill dir's existence (not its contents) is what Forget must guarantee is
// cleaned up.
func TestForget_RemovesAgentSpillDir(t *testing.T) {
	c := assert.NewAborting(t)
	ctrl := newTestController(t)

	const childID = "c_spill_forget_test"
	spillDir := agentSpillDir(ctrl.stateDir, childID)
	c.NoError(os.MkdirAll(spillDir, 0o700), "mkdirall spill dir")
	sentinel := filepath.Join(spillDir, "clipped-output.txt")
	c.NoError(os.WriteFile(sentinel, []byte("clipped tool output"), 0o600), "write sentinel file")

	now := time.Now()
	ctrl.st.Insert(&childstore.Session{
		ChildID:      childID,
		Status:       protocol.StatusExited,
		Kind:         protocol.KindFundi,
		Cwd:          t.TempDir(),
		StartedAt:    now,
		LastActivity: now,
		ExitedAt:     now,
	})

	c.NoError(ctrl.Close(childID), "Forget")

	_, err := os.Stat(spillDir)
	c.True(os.IsNotExist(err), "spill dir %s still exists after Forget (err=%v)", spillDir, err)
}

// TestForgetAllExited_RemovesAgentSpillDir covers the same cleanup via the
// bulk sweep path (ForgetAllExited), used by the sweeper and 'rafiki forget
// --all'.
func TestForgetAllExited_RemovesAgentSpillDir(t *testing.T) {
	c := assert.NewAborting(t)
	ctrl := newTestController(t)

	const childID = "c_spill_forgetall_test"
	spillDir := agentSpillDir(ctrl.stateDir, childID)
	c.NoError(os.MkdirAll(spillDir, 0o700), "mkdirall spill dir")
	c.NoError(os.WriteFile(filepath.Join(spillDir, "clipped-output.txt"), []byte("x"), 0o600), "write sentinel file")

	now := time.Now()
	ctrl.st.Insert(&childstore.Session{
		ChildID:      childID,
		Status:       protocol.StatusExited,
		Kind:         protocol.KindFundi,
		Cwd:          t.TempDir(),
		StartedAt:    now,
		LastActivity: now,
		ExitedAt:     now,
	})

	closed, err := ctrl.CloseAllExited(0)
	c.NoError(err, "ForgetAllExited")
	c.Len(closed, 1, "ForgetAllExited count = %d, want 1", len(closed))

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
			c := assert.NewCollecting(t)
			ctrl := newTestController(t)
			const childID = "c_agent_session_switch"
			now := time.Now()
			ctrl.st.Insert(&childstore.Session{
				ChildID:      childID,
				Status:       protocol.StatusIdle,
				Kind:         protocol.KindFundi,
				Cwd:          t.TempDir(),
				StartedAt:    now,
				LastActivity: now,
			})

			err := ctrl.Send(childID, json.RawMessage(frame))
			c.Require().Error(err, "Send accepted a session switch for an agent child; it would silently reattach the same conversation")
			var ce *connectapi.ControllerError
			c.Require().True(errors.As(err, &ce), "error is %T, want *connectapi.ControllerError so the client sees a coded failure: %v", err, err)
			c.Eq(protocol.ErrInvalidArgs, ce.Code, "error code")
			c.StrContains(ce.Message, "agent child", "message")

			// The child must be left completely alone: the rejection happens
			// before the kill+respawn ceremony, so it is still exactly as it was.
			snap, ok := ctrl.st.Get(childID)
			c.Require().True(ok, "the rejected request removed the child from the store")
			c.Eq(protocol.StatusIdle, snap.Status, "child status")
		})
	}
}

// TestSend_AllowsSessionSwitchForNonAgentKinds pins the negative half: the
// rejection above must be scoped to the agent kind and not quietly break
// new_session for claude children.
func TestSend_AllowsSessionSwitchForNonAgentKinds(t *testing.T) {
	for _, kind := range []string{"", protocol.KindClaude} {
		t.Run("kind="+kind, func(t *testing.T) {
			ctrl := newTestController(t)
			const childID = "c_nonagent_session_switch"
			now := time.Now()
			ctrl.st.Insert(&childstore.Session{
				ChildID:      childID,
				Status:       protocol.StatusIdle,
				Kind:         kind,
				Cwd:          t.TempDir(),
				StartedAt:    now,
				LastActivity: now,
			})

			err := ctrl.Send(childID, json.RawMessage(`{"type":"new_session","id":"req-1"}`))
			var ce *connectapi.ControllerError
			assert.NewAborting(t).False(errors.As(err, &ce) && ce.Code == protocol.ErrInvalidArgs &&
				strings.Contains(ce.Message, "agent child"), "kind %q was refused with the agent-kind rejection: %v", kind, err)
		})
	}
}

// TestSend_RejectsSessionSwitchForScriptChild covers the wave-4 review's
// MINOR-3: a new_session/switch_session aimed at a script child must be
// refused UP FRONT, the way the agent kind already is — not via the
// kill-then-fail-respawn path (the script would die and the respawn would
// fail with "a script's exit is its result", leaving a dead child and an
// error, the worst of both). A script has no session, and respawning one
// would silently start the work over.
func TestSend_RejectsSessionSwitchForScriptChild(t *testing.T) {
	for _, frame := range []string{
		`{"type":"new_session","id":"req-1"}`,
		`{"type":"switch_session","id":"req-2","sessionPath":"/tmp/other.jsonl"}`,
	} {
		t.Run(frame, func(t *testing.T) {
			c := assert.NewCollecting(t)
			ctrl := newTestController(t)
			const childID = "c_script_session_switch"
			now := time.Now()
			ctrl.st.Insert(&childstore.Session{
				ChildID:      childID,
				Status:       protocol.StatusStreaming,
				Kind:         protocol.KindScript,
				Cwd:          t.TempDir(),
				StartedAt:    now,
				LastActivity: now,
			})

			err := ctrl.Send(childID, json.RawMessage(frame))
			c.Require().Error(err, "Send accepted a session switch for a script child; a respawn would silently start the work over")
			var ce *connectapi.ControllerError
			c.Require().True(errors.As(err, &ce), "error is %T, want *connectapi.ControllerError so the client sees a coded failure: %v", err, err)
			c.Eq(protocol.ErrInvalidArgs, ce.Code, "error code")
			c.StrContains(ce.Message, "script child", "message")
			c.StrContains(ce.Message, "exit is its result", "message")

			// The refusal happens before the kill ceremony: the row is
			// exactly as it was (a live child would still be live).
			snap, ok := ctrl.st.Get(childID)
			c.Require().True(ok, "the rejected request removed the child from the store")
			c.Eq(protocol.StatusStreaming, snap.Status, "child status")
		})
	}
}
