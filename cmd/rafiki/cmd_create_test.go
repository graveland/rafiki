package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	"go.graveland.dev/rafiki/pkg/clientstate"
	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/gen/rafiki/v1/rafikiv1connect"
	"go.graveland.dev/rafiki/pkg/paths"
	"go.graveland.dev/rafiki/pkg/presets"
	"go.graveland.dev/rafiki/pkg/profile"
	"go.graveland.dev/rafiki/pkg/protocol"

	"github.com/multigres/testkit/assert"
)

// remoteProfileForTest seeds an isolated profile manifest naming a remote
// daemon, and points the process at it — for tests exercising buildSpawnRequest's
// --cwd-against-a-remote-profile check (mustProfile(cmd).URL != "").
func remoteProfileForTest(t *testing.T, url string) {
	t.Helper()
	c := assert.NewAborting(t)
	isolateProfiles(t)
	resetProfileCache()
	c.NoError(profile.Save(profile.Set{Profiles: map[string]profile.Profile{
		"remote": {Name: "remote", URL: url},
	}}), "Save")
	c.NoError(profile.SavePointer("remote"), "SavePointer")
}

// newTestCreateCmd returns a cobra.Command with spawn flags registered, suitable
// for use in buildSpawnRequest unit tests.
func newTestCreateCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "create"}
	addSpawnFlags(cmd)
	return cmd
}

func TestBuildSpawnRequest_ExplicitCwd(t *testing.T) {
	c := assert.NewCollecting(t)
	cmd := newTestCreateCmd()
	c.Require().NoError(cmd.Flags().Set("cwd", "/explicit/path"))

	req, err := buildSpawnRequest(cmd, nil)
	c.Require().NoError(err, "unexpected error")
	c.Eq("/explicit/path", req.Cwd, "Cwd")
}

func TestBuildSpawnRequest_DefaultCwd(t *testing.T) {
	c := assert.NewCollecting(t)
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
	c.Require().NoError(err, "unexpected error")
	c.Eq(wantCwd, req.Cwd, "Cwd")
}

// A claude child used to be a literal subprocess of rafikid (cmd.Dir =
// req.Cwd, pkg/child/runner.go), so defaulting its cwd from the CLIENT
// against a remote daemon would silently ship a path valid only here — this
// was true before daraja existed. It no longer is: once the daemon has an
// executor pool, --kind claude routes through whichever executor gets
// bound — by default the session executor `rafiki create` starts on the
// CLIENT's own machine, rooted at exactly this cwd — exactly like fundi
// below. So the client's own os.Getwd() must default for claude too, remote
// daemon or not, and buildSpawnRequest must NOT require an explicit --cwd
// the way it used to.
func TestBuildSpawnRequest_RemoteDefaultsCwdForClaude(t *testing.T) {
	c := assert.NewCollecting(t)
	remoteProfileForTest(t, "https://rafiki.example.dev")

	wantCwd, err := os.Getwd()
	if err != nil {
		t.Skip("os.Getwd() failed — skipping:", err)
	}

	cmd := newTestCreateCmd()
	c.Require().NoError(cmd.Flags().Set("kind", protocol.KindClaude))
	// cwd left at its zero value ("") intentionally.

	req, err := buildSpawnRequest(cmd, nil)
	c.Require().NoError(err, "unexpected error defaulting --cwd for a claude child against a remote daemon")
	c.Eq(wantCwd, req.Cwd, "Cwd")

	// An explicit --cwd still wins.
	c.Require().NoError(cmd.Flags().Set("cwd", "/remote/project"))
	req, err = buildSpawnRequest(cmd, nil)
	c.Require().NoError(err, "unexpected error with explicit --cwd")
	c.Eq("/remote/project", req.Cwd, "Cwd")
}

// A fundi child never forks a daemon-local process: its filesystem access, if
// any, goes through whichever executor gets bound — by default the session
// executor `rafiki create` starts on the CLIENT's own machine, rooted at
// exactly this cwd. So the client's own os.Getwd() is always a valid default,
// remote daemon or not — the same reasoning the claude case above now shares.
func TestBuildSpawnRequest_RemoteDefaultsCwdForFundi(t *testing.T) {
	c := assert.NewCollecting(t)
	remoteProfileForTest(t, "https://rafiki.example.dev")

	wantCwd, err := os.Getwd()
	if err != nil {
		t.Skip("os.Getwd() failed — skipping:", err)
	}

	cmd := newTestCreateCmd()
	// kind left at its default (fundi); cwd left at its zero value ("").

	req, err := buildSpawnRequest(cmd, nil)
	c.Require().NoError(err, "unexpected error defaulting --cwd for a fundi child against a remote daemon")
	c.Eq(wantCwd, req.Cwd, "Cwd")
}

func TestBuildSpawnRequest_RelativeCwdRejected(t *testing.T) {
	c := assert.NewAborting(t)
	cmd := newTestCreateCmd()
	c.NoError(cmd.Flags().Set("cwd", "relative/path"))

	_, err := buildSpawnRequest(cmd, nil)
	c.Error(err, "expected error for relative --cwd, got nil")
}

// TestBuildSpawnRequest_SkillsDirAndMCPConfig covers task A6: --skills-dir
// (repeatable) and --mcp-config, previously reachable only via --extra-arg,
// now flow straight into their own SpawnRequest fields.
func TestBuildSpawnRequest_SkillsDirAndMCPConfig(t *testing.T) {
	c := assert.NewCollecting(t)
	cmd := newTestCreateCmd()
	c.Require().NoError(cmd.Flags().Set("cwd", "/tmp"))
	c.Require().NoError(cmd.Flags().Set("skills-dir", "/a/skills"))
	c.Require().NoError(cmd.Flags().Set("skills-dir", "/b/skills"))
	c.Require().NoError(cmd.Flags().Set("mcp-config", "/cfg/.mcp.json"))

	req, err := buildSpawnRequest(cmd, nil)
	c.Require().NoError(err, "unexpected error")
	c.EqDiff([]string{"/a/skills", "/b/skills"}, req.SkillsDirs, "SkillsDirs")
	c.Eq("/cfg/.mcp.json", req.MCPConfig, "MCPConfig")
}

// TestBuildSpawnRequest_SkillsDirAndMCPConfigOmittedByDefault confirms the
// new fields stay unset (so buildAgentArgv emits neither flag) when the
// caller never touches them — matching every other optional spawn flag.
func TestBuildSpawnRequest_SkillsDirAndMCPConfigOmittedByDefault(t *testing.T) {
	c := assert.NewCollecting(t)
	cmd := newTestCreateCmd()
	c.Require().NoError(cmd.Flags().Set("cwd", "/tmp"))

	req, err := buildSpawnRequest(cmd, nil)
	c.Require().NoError(err, "unexpected error")
	c.Empty(req.SkillsDirs, "SkillsDirs")
	c.Eq("", req.MCPConfig, "MCPConfig")
}

func TestBuildSpawnRequest_NameFromArgs(t *testing.T) {
	c := assert.NewCollecting(t)
	cmd := newTestCreateCmd()
	c.Require().NoError(cmd.Flags().Set("cwd", "/tmp"))

	req, err := buildSpawnRequest(cmd, []string{"my-session"})
	c.Require().NoError(err, "unexpected error")
	c.Eq("my-session", req.Name, "Name")
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
	c := assert.NewCollecting(t)
	cmd := newCreateCmd()
	err := executeWithFlags(cmd, "--kill-on-exit", "--keep-on-exit")
	c.Require().Error(err, "expected error when both --kill-on-exit and --keep-on-exit are set, got nil")
	// Cobra's message contains "if any flags in the group" when mutual exclusion fires.
	c.False(!strings.Contains(err.Error(), "kill-on-exit") || !strings.Contains(err.Error(), "keep-on-exit"), "expected flag names in error, got: %v", err)
}

func TestCreateCmd_KillOnExitAlone_OK(t *testing.T) {
	cmd := newCreateCmd()
	assert.NewCollecting(t).NoError(executeWithFlags(cmd, "--kill-on-exit"), "unexpected error with only --kill-on-exit")
}

func TestCreateCmd_KeepOnExitAlone_OK(t *testing.T) {
	cmd := newCreateCmd()
	assert.NewCollecting(t).NoError(executeWithFlags(cmd, "--keep-on-exit"), "unexpected error with only --keep-on-exit")
}

// ─── Label flag tests ─────────────────────────────────────────────────────────

func TestBuildSpawnRequest_LabelFlag(t *testing.T) {
	c := assert.NewCollecting(t)
	cmd := newTestCreateCmd()
	c.Require().NoError(cmd.Flags().Set("cwd", "/tmp"))
	// Register the --label flag (added by addSpawnFlags).
	c.Require().NoError(cmd.Flags().Set("label", "env=prod"))

	req, err := buildSpawnRequest(cmd, nil)
	c.Require().NoError(err, "unexpected error")
	c.Eq("prod", req.Labels["env"], "Labels[env]")
}

// ─── --model completion respects the resolved profile's kind ──────────────────
//
// Pins Fix 6: since Task 10 changed --kind's default from "fundi" to "",
// the --model completion function used to pass an unset --kind flag's raw
// value ("") straight to completeModel, which internally maps "" to "fundi"
// -- so a profile with kind = "claude" got fundi-only (OpenRouter) model
// completions when --kind was never typed on the line. The completion
// function must resolve the same way buildSpawnRequest does: via
// resolveKind(flagKind, profileKind).

// stubModelsControl records the Kind a ListModels call asked for.
type stubModelsControl struct {
	rafikiv1connect.UnimplementedControlHandler
	gotKind string
}

func (s *stubModelsControl) ListModels(
	_ context.Context,
	req *connect.Request[rafikiv1.ListModelsRequest],
) (*connect.Response[rafikiv1.ListModelsResponse], error) {
	s.gotKind = req.Msg.GetKind()
	return connect.NewResponse(&rafikiv1.ListModelsResponse{}), nil
}

func TestModelCompletionUsesTheResolvedProfilesKind(t *testing.T) {
	c := assert.NewAborting(t)
	isolateProfiles(t)
	resetProfileCache()
	t.Setenv("XDG_CACHE_HOME", filepath.Join(t.TempDir(), "cache"))

	dir, err := os.MkdirTemp("", "h")
	c.NoError(err, "MkdirTemp")
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "controller.sock")

	stub := &stubModelsControl{}
	routePath, handler := rafikiv1connect.NewControlHandler(stub)
	serveConnectOnUnixSocket(t, sock, routePath, handler)

	c.NoError(profile.Save(profile.Set{Profiles: map[string]profile.Profile{
		"claudework": {Name: "claudework", Socket: sock, Kind: protocol.KindClaude},
	}}), "Save")
	c.NoError(profile.SavePointer("claudework"), "SavePointer")

	cmd := newTestCreateCmd()
	// --kind deliberately left unset, matching the bug report exactly.
	fn, ok := cmd.GetFlagCompletionFunc("model")
	c.True(ok, "no completion function registered for --model")
	fn(cmd, nil, "")

	c.Eq(protocol.KindClaude, stub.gotKind, "ListModels asked for kind")
}

// localProfileForTest seeds an isolated profile manifest naming a local
// socket, with the given spawn defaults, and points the process at it.
// RAFIKI_DEFAULT_MODEL/PRESET/LABELS are retired client-side (profile.CheckRetiredEnv
// errors on them via mustProfile), so any test that used to exercise a
// default via those variables seeds a profile field instead.
func localProfileForTest(t *testing.T, p profile.Profile) {
	t.Helper()
	c := assert.NewAborting(t)
	isolateProfiles(t)
	resetProfileCache()
	p.Name = "test"
	p.Socket = "/tmp/rafiki-test.sock"
	c.NoError(profile.Save(profile.Set{Profiles: map[string]profile.Profile{"test": p}}), "Save")
	c.NoError(profile.SavePointer("test"), "SavePointer")
}

// TestBuildSpawnRequest_ModelNotDefaultedFromProfile confirms Step 4.3: the
// profile's model is no longer applied inside buildSpawnRequest at all —
// req.Model carries only the --model flag. The full chain (preset > profile >
// remembered) is finished off in runCreate via resolveModel, pinned by
// TestModelPrecedence.
func TestBuildSpawnRequest_ModelNotDefaultedFromProfile(t *testing.T) {
	c := assert.NewCollecting(t)
	localProfileForTest(t, profile.Profile{Model: "prof-model"})
	cmd := newTestCreateCmd()
	c.Require().NoError(cmd.Flags().Set("cwd", "/tmp"))
	req, err := buildSpawnRequest(cmd, nil)
	c.Require().NoError(err, "unexpected error")
	c.Eq("", req.Model, "Model")
}

func TestBuildSpawnRequest_ProfileDefaultLabels(t *testing.T) {
	c := assert.NewCollecting(t)
	localProfileForTest(t, profile.Profile{Labels: map[string]string{"context": "work", "env": "prod"}})
	cmd := newTestCreateCmd()
	c.Require().NoError(cmd.Flags().Set("cwd", "/tmp"))

	req, err := buildSpawnRequest(cmd, nil)
	c.Require().NoError(err, "unexpected error")
	c.False(req.Labels["context"] != "work" || req.Labels["env"] != "prod", "Labels from the profile's default labels: %v", req.Labels)
}

func TestBuildSpawnRequest_FlagLabelWinsOverProfile(t *testing.T) {
	c := assert.NewCollecting(t)
	// Explicit --label should override the profile's labels on the same key.
	localProfileForTest(t, profile.Profile{Labels: map[string]string{"env": "staging"}})
	cmd := newTestCreateCmd()
	c.Require().NoError(cmd.Flags().Set("cwd", "/tmp"))
	c.Require().NoError(cmd.Flags().Set("label", "env=prod"))

	req, err := buildSpawnRequest(cmd, nil)
	c.Require().NoError(err, "unexpected error")
	c.Eq("prod", req.Labels["env"], "Labels[env]")
}

func TestBuildSpawnRequest_InvalidLabelKey(t *testing.T) {
	c := assert.NewAborting(t)
	cmd := newTestCreateCmd()
	c.NoError(cmd.Flags().Set("cwd", "/tmp"))
	c.NoError(cmd.Flags().Set("label", "bad key=val"))
	_, err := buildSpawnRequest(cmd, nil)
	c.Error(err, "expected error for invalid label key")
}

func TestBuildSpawnRequest_KindClaude(t *testing.T) {
	c := assert.NewCollecting(t)
	cmd := newTestCreateCmd()
	c.Require().NoError(cmd.Flags().Set("cwd", "/tmp"))
	c.Require().NoError(cmd.Flags().Set("kind", protocol.KindClaude))
	c.Require().NoError(cmd.Flags().Set("config-dir", "/x"))
	c.Require().NoError(cmd.Flags().Set("append-system-prompt", "be terse"))

	req, err := buildSpawnRequest(cmd, nil)
	c.Require().NoError(err, "unexpected error")
	c.Eq(protocol.KindClaude, req.Kind, "Kind")
	c.Eq("/x", req.ConfigDir, "ConfigDir")
	c.Eq("be terse", req.AppendSystemPrompt, "AppendSystemPrompt")
}

func TestBuildSpawnRequest_KindDefaultsAgent(t *testing.T) {
	c := assert.NewCollecting(t)
	cmd := newTestCreateCmd()
	c.Require().NoError(cmd.Flags().Set("cwd", "/tmp"))

	req, err := buildSpawnRequest(cmd, nil)
	c.Require().NoError(err, "unexpected error")
	// The default is the native runtime, not a foreign subprocess: it is the kind
	// with in-band abort and per-turn cost accounting, and the only one whose
	// model ids this repo can resolve. --model completion keys off the same
	// default (kind scoping lives daemon-side; see cmd/rafikid's sourcesForKind).
	c.Eq(protocol.KindFundi, req.Kind, "Kind")
	c.Eq("", req.ConfigDir, "ConfigDir")
}

func TestBuildSpawnRequest_ReservedLabelKey(t *testing.T) {
	c := assert.NewAborting(t)
	cmd := newTestCreateCmd()
	c.NoError(cmd.Flags().Set("cwd", "/tmp"))
	c.NoError(cmd.Flags().Set("label", "rafiki/model=evil"))
	_, err := buildSpawnRequest(cmd, nil)
	c.Error(err, "expected error for rafiki/ prefix")
}

func TestBuildSpawnRequest_ParentFlag(t *testing.T) {
	c := assert.NewAborting(t)
	cmd := newTestCreateCmd()
	c.NoError(cmd.Flags().Set("cwd", "/tmp"))
	c.NoError(cmd.Flags().Set("parent", "c_abc123"))
	req, err := buildSpawnRequest(cmd, nil)
	c.NoError(err, "unexpected error")
	c.Eq("c_abc123", req.ParentChildID, "ParentChildID")
}

func TestBuildSpawnRequest_ParentFlagOmitted(t *testing.T) {
	c := assert.NewCollecting(t)
	cmd := newTestCreateCmd()
	c.Require().NoError(cmd.Flags().Set("cwd", "/tmp"))
	req, err := buildSpawnRequest(cmd, nil)
	c.Require().NoError(err, "unexpected error")
	c.Eq("", req.ParentChildID, "ParentChildID")
}

// The flag is the only way a human can target an executor: the sole other
// writer of SpawnRequest.ExecutorSelector in this repo is the in-process
// agent_spawn tool.
func TestBuildSpawnRequest_ExecutorSelectorFlag(t *testing.T) {
	t.Setenv(paths.ExecutorSelector, "")
	c := assert.NewCollecting(t)

	cmd := newTestCreateCmd()
	c.Require().NoError(cmd.Flags().Set("executor-selector", "owner=brent,env=home"))
	c.Require().NoError(cmd.Flags().Set("cwd", "/w"))

	req, err := buildSpawnRequest(cmd, nil)
	c.Require().NoError(err, "unexpected error")
	c.Eq("owner=brent,env=home", req.ExecutorSelector, "ExecutorSelector")
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
	c := assert.NewCollecting(t)

	cmd := newTestCreateCmd()
	c.Require().NoError(cmd.Flags().Set("cwd", "/w"))

	req, err := buildSpawnRequest(cmd, nil)
	c.Require().NoError(err, "unexpected error")
	c.Eq("owner=brent", req.ExecutorSelector, "ExecutorSelector")
}

// An explicit flag beats the environment.
func TestBuildSpawnRequest_ExecutorSelectorFlagBeatsEnv(t *testing.T) {
	t.Setenv(paths.ExecutorSelector, "owner=brent")
	c := assert.NewCollecting(t)

	cmd := newTestCreateCmd()
	c.Require().NoError(cmd.Flags().Set("executor-selector", "env=ci"))
	c.Require().NoError(cmd.Flags().Set("cwd", "/w"))

	req, err := buildSpawnRequest(cmd, nil)
	c.Require().NoError(err, "unexpected error")
	c.Eq("env=ci", req.ExecutorSelector, "ExecutorSelector")
}

// --no-local-executor is a session posture, not a spawn field. It must not
// appear on the wire: the daemon has no opinion about whether the client
// offered its own machine.
func TestNoLocalExecutorIsNotASpawnField(t *testing.T) {
	t.Setenv(paths.ExecutorSelector, "")
	c := assert.NewCollecting(t)

	cmd := newTestCreateCmd()
	c.Require().NoError(cmd.Flags().Set("no-local-executor", "true"))
	c.Require().NoError(cmd.Flags().Set("cwd", "/w"))

	req, err := buildSpawnRequest(cmd, nil)
	c.Require().NoError(err, "unexpected error")
	c.Eq("", req.ExecutorSelector, "ExecutorSelector")
}

func TestMaxCostConvertsThroughConfiguredCurrency(t *testing.T) {
	c := assert.NewAborting(t)
	dir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", dir)
	clientstate.UpdateScoped(clientstate.Scope{}, func(s *clientstate.State) {
		s.Currency = &clientstate.Currency{Code: "CAD", Rate: 1.38}
	})

	cmd := newTestCreateCmd()
	c.NoError(cmd.Flags().Set("cwd", t.TempDir()))
	c.NoError(cmd.Flags().Set("max-cost", "13.80"))

	req, err := buildSpawnRequest(cmd, nil)
	c.NoError(err)
	c.NotNil(req.MaxCost, "MaxCost is nil, want a converted USD value")
	if diff := *req.MaxCost - 10.0; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("MaxCost = %v, want ~10.0 (13.80 CAD at 1.38 CAD/USD)", *req.MaxCost)
	}
}

func TestMaxCostWithNoCurrencyIsUnconverted(t *testing.T) {
	c := assert.NewCollecting(t)
	dir := t.TempDir()
	t.Setenv("XDG_STATE_HOME", dir)

	cmd := newTestCreateCmd()
	c.Require().NoError(cmd.Flags().Set("cwd", t.TempDir()))
	c.Require().NoError(cmd.Flags().Set("max-cost", "10"))

	req, err := buildSpawnRequest(cmd, nil)
	c.Require().NoError(err)
	c.False(req.MaxCost == nil || *req.MaxCost != 10, "MaxCost = %v, want 10 (no currency configured)", req.MaxCost)
}

func TestResolveExecutor(t *testing.T) {
	cases := []struct {
		name                       string
		flagExecutor, flagSelector string
		remembered                 string
		rememberedEligible         bool
		wantRef, wantSelector      string
	}{
		{name: "flag wins", flagExecutor: "greyshift", flagSelector: "env=home", remembered: "silvershift", rememberedEligible: true, wantRef: "greyshift"},
		{name: "selector wins over remembered", flagSelector: "env=home", remembered: "silvershift", rememberedEligible: true, wantSelector: "env=home"},
		{name: "remembered wins when eligible", remembered: "greyshift", rememberedEligible: true, wantRef: "greyshift"},
		{name: "remembered ignored when not eligible", remembered: "greyshift", rememberedEligible: false, wantRef: "", wantSelector: ""},
		{name: "nothing given", wantRef: "", wantSelector: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotRef, gotSel := resolveExecutor(tc.flagExecutor, tc.flagSelector, tc.remembered, tc.rememberedEligible)
			assert.NewAborting(t).False(gotRef != tc.wantRef || gotSel != tc.wantSelector, "got ref=%q selector=%q, want ref=%q selector=%q", gotRef, gotSel, tc.wantRef, tc.wantSelector)
		})
	}
}

func TestCreateTextFields(t *testing.T) {
	c := assert.NewCollecting(t)
	// The Connect SpawnResponse carries the child id only (the framed payload's
	// session/model fields were dropped on the wire); the text view renders
	// exactly what the record carries.
	var buf bytes.Buffer
	c.Require().NoError(renderCreateSummary(&buf, &rafikiv1.SpawnResponse{ChildId: "c_123"}, outputTable), "renderCreateSummary")
	c.Eq("childId: c_123\n", buf.String(), "text output")

	// A field the record does not carry is skipped, not printed as a dash.
	var empty bytes.Buffer
	c.Require().NoError(renderCreateSummary(&empty, &rafikiv1.SpawnResponse{}, outputTable), "renderCreateSummary empty")
	c.Eq("", empty.String(), "empty-field output")
}

// The detached create JSON is the Spawn response's canonical protojson —
// pretty and JSONL are the same bytes modulo whitespace (protoout's contract).
func TestCreateJSONShape(t *testing.T) {
	c := assert.NewCollecting(t)
	resp := &rafikiv1.SpawnResponse{ChildId: "c_123"}

	var buf bytes.Buffer
	c.Require().NoError(renderCreateSummary(&buf, resp, outputJSON), "renderCreateSummary")
	c.Eq("{\n  \"childId\": \"c_123\"\n}\n", buf.String(), "json output")

	var line bytes.Buffer
	c.Require().NoError(renderCreateSummary(&line, resp, outputJSONL), "renderCreateSummary")
	c.Eq(`{"childId":"c_123"}`+"\n", line.String(), "jsonl output")
}

// TestConnectSpawnRequestCarriesEveryField pins the protocol→wire conversion:
// every field buildSpawnRequest assembles rides onto rafikiv1.SpawnRequest —
// including the operator-only 15–29 (a child credential is refused daemon-side,
// never client-side) — and the budgets stay pointers so unset and zero keep
// their opposite meanings.
func TestConnectSpawnRequestCarriesEveryField(t *testing.T) {
	depth, cost, kids := 0, 0.0, 0 // explicit zeros must stay SET on the wire
	in := protocol.SpawnRequest{
		Name:               "worker",
		Cwd:                "/tmp/w",
		Kind:               protocol.KindFundi,
		ConfigDir:          "/cfg",
		AppendSystemPrompt: "be brief",
		Model:              "anthropic/claude-sonnet-4-5",
		Thinking:           "low",
		NoSession:          true,
		ResumeSession:      "s.jsonl",
		ForkSession:        "f.jsonl",
		NoExtensions:       true,
		Extensions:         []string{"x"},
		Verbose:            true,
		ExtraArgs:          []string{"--fast"},
		SkillsDirs:         []string{"/skills"},
		MCPConfig:          "/mcp.json",
		Labels:             map[string]string{"env": "home"},
		ParentChildID:      "c_parent",
		Env:                map[string]string{"K": "V"},
		RecordRequests:     true,
		PassthroughAuth:    "on",
		ExecutorSelector:   "env=home",
		ExecutorRef:        "greyshift",
		Preset:             "impl",
		Script:             &protocol.ScriptSpec{Repo: "local", Script: "driver", Args: []string{"--fast"}},
		Prefill:            []protocol.PrefillRead{{Path: "/a.md", Start: 1, End: 9}},
		MaxDepth:           &depth,
		MaxCost:            &cost,
		MaxChildren:        &kids,
	}
	out := connectSpawnRequest(in)

	assert.NewAborting(t).False(out.GetCwd() != in.Cwd || out.GetName() != in.Name || out.GetKind() != in.Kind ||
		out.GetModel() != in.Model || out.GetPreset() != in.Preset ||
		out.GetConfigDir() != in.ConfigDir || out.GetAppendSystemPrompt() != in.AppendSystemPrompt ||
		out.GetThinking() != in.Thinking || out.GetNoSession() != in.NoSession ||
		out.GetResumeSession() != in.ResumeSession || out.GetForkSession() != in.ForkSession ||
		out.GetNoExtensions() != in.NoExtensions || !slices.Equal(out.GetExtensions(), in.Extensions) ||
		out.GetVerbose() != in.Verbose || !slices.Equal(out.GetExtraArgs(), in.ExtraArgs) ||
		!slices.Equal(out.GetSkillsDirs(), in.SkillsDirs) || out.GetMcpConfig() != in.MCPConfig ||
		out.GetParentChildId() != in.ParentChildID || out.GetRecordRequests() != in.RecordRequests ||
		out.GetPassthroughAuth() != in.PassthroughAuth || out.GetExecutorSelector() != in.ExecutorSelector ||
		out.GetExecutorRef() != in.ExecutorRef, "a scalar field was lost on the wire: %+v", out)
	if out.GetLabels()["env"] != "home" || out.GetEnv()["K"] != "V" {
		t.Fatalf("a map field was lost: labels=%v env=%v", out.GetLabels(), out.GetEnv())
	}
	if out.Script == nil || out.Script.GetRepo() != "local" || out.Script.GetScript() != "driver" ||
		!slices.Equal(out.Script.GetArgs(), []string{"--fast"}) {
		t.Fatalf("script spec lost: %v", out.Script)
	}
	if len(out.Prefill) != 1 || out.Prefill[0].GetPath() != "/a.md" ||
		out.Prefill[0].GetStart() != 1 || out.Prefill[0].GetEnd() != 9 {
		t.Fatalf("prefill lost: %v", out.Prefill)
	}

	// Explicit zeros stay SET — zero means "may not spawn" / "spend nothing",
	// and only the pointer preserves that on the wire.
	if out.MaxDepth == nil || *out.MaxDepth != 0 {
		t.Fatalf("zero max_depth must stay set, got %v", out.MaxDepth)
	}
	if out.MaxCost == nil || *out.MaxCost != 0 {
		t.Fatalf("zero max_cost must stay set, got %v", out.MaxCost)
	}
	if out.MaxChildren == nil || *out.MaxChildren != 0 {
		t.Fatalf("zero max_children must stay set, got %v", out.MaxChildren)
	}

	// Unset budgets stay unset: nil, not zero (unset means default/unlimited).
	empty := connectSpawnRequest(protocol.SpawnRequest{Cwd: "/tmp"})
	if empty.MaxDepth != nil || empty.MaxCost != nil || empty.MaxChildren != nil {
		t.Fatalf("unset budgets must stay nil, got %v/%v/%v", empty.MaxDepth, empty.MaxCost, empty.MaxChildren)
	}
}

// TestCreatePresetDropsAmbientModel pins the preset branch of create's request shaping:
// the request carries the preset name and the preset's kind, its model is
// ONLY the --model flag value (empty when the flag is unset, even though a
// profile model exists), and an explicit --kind that contradicts the preset
// fails.
func TestCreatePresetDropsAmbientModel(t *testing.T) {
	c := assert.NewCollecting(t)
	rec := presets.Record{Name: "reviewer", Kind: presets.KindClaude, Model: "claude-x"}

	// No --model flag: Model stays empty even with a profile model in play.
	req2 := &protocol.SpawnRequest{}
	c.Require().NoError(applyCreatePreset(req2, "reviewer", rec, "", false, ""), "applyCreatePreset")
	c.Eq("reviewer", req2.Preset, "Preset")
	c.Eq(presets.KindClaude, req2.Kind, "Kind")
	c.Eq("", req2.Model, "Model")

	// An explicit --model flag is kept.
	req3 := &protocol.SpawnRequest{}
	c.Require().NoError(applyCreatePreset(req3, "reviewer", rec, "", false, "flag-model"), "applyCreatePreset")
	c.Eq("flag-model", req3.Model, "Model")

	// A matching explicit --kind is fine.
	req4 := &protocol.SpawnRequest{}
	c.NoError(applyCreatePreset(req4, "reviewer", rec, presets.KindClaude, true, ""), "applyCreatePreset with a matching --kind")

	// A conflicting --kind fails.
	err := applyCreatePreset(&protocol.SpawnRequest{}, "reviewer", rec, "fundi", true, "")
	c.Require().Error(err, "applyCreatePreset with a conflicting --kind = nil error, want a failure")
	c.StrContains(err.Error(), `--kind "fundi" conflicts with preset "reviewer" (kind "claude")`, "error = %v, want the conflict named", err)
}

// ─── resolvePresetName ───────────────────────────────────────────────────────
//
// The preset NAME is resolved client-side (the --preset flag, else the
// resolved profile's `preset` field); the preset's CONTENT now resolves in
// the daemon. These tests pin only that name resolution — no presets.json
// fixtures, which stopped existing with the client-side loader.

// TestProfilePresetName_AppliedWhenFlagUnset checks that the resolved
// profile's `preset` field is read (via resolvePresetName) when --preset is
// not passed.
func TestProfilePresetName_AppliedWhenFlagUnset(t *testing.T) {
	c := assert.NewCollecting(t)
	localProfileForTest(t, profile.Profile{Preset: "mypreset"})

	cmd := newCreateCmd()
	c.Require().NoError(cmd.Flags().Set("cwd", "/tmp"))
	// The same resolution runCreate does, not a copy of it.
	c.Eq("mypreset", resolvePresetName(cmd), "presetName")
}

// TestProfilePresetName_FlagWinsOverProfile checks that an explicit --preset
// wins over the profile's `preset` field.
func TestProfilePresetName_FlagWinsOverProfile(t *testing.T) {
	c := assert.NewCollecting(t)
	localProfileForTest(t, profile.Profile{Preset: "profpreset"})

	cmd := newCreateCmd()
	c.Require().NoError(cmd.Flags().Set("preset", "flagpreset"))
	c.Eq("flagpreset", resolvePresetName(cmd), "presetName")
}

// TestResolveModelChainWithoutPreset pins the 3-argument model chain: the
// named preset no longer takes part client-side — it resolves in the daemon
// — so the chain is flag > profile > remembered.
func TestResolveModelChainWithoutPreset(t *testing.T) {
	cases := []struct {
		name       string
		flagModel  string
		profModel  string
		remembered string
		want       string
	}{
		{"flag wins over everything", "flag-m", "prof-m", "remembered-m", "flag-m"},
		{"profile default beats the remembered model", "", "prof-m", "remembered-m", "prof-m"},
		{"remembered is the last resort", "", "", "remembered-m", "remembered-m"},
		{"nothing set leaves it to the daemon", "", "", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := resolveModel(tc.flagModel, tc.profModel, tc.remembered)
			assert.NewAborting(t).Eq(tc.want, got, "resolveModel(%q,%q,%q) = %q, want", tc.flagModel, tc.profModel, tc.remembered, got)
		})
	}
}
