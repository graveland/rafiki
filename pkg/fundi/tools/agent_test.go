package tools

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// fakeSpawner is an in-memory AgentSpawner. It records calls so a test can
// assert on what the daemon adapter would have been asked to do, and it
// enforces nothing — every authority rule lives on the daemon side and is
// tested there (cmd/rafikid/agent_spawner_test.go), not here.
type fakeSpawner struct {
	children []AgentInfo
	models   []ModelInfo
	view     string

	spawned  []SpawnSpec
	sent     []struct{ ChildID, Message string }
	killed   []string
	nextID   string
	spawnErr error
	sendErr  error
	killErr  error
	viewErr  error
	listErr  error
}

func (f *fakeSpawner) List(context.Context) ([]AgentInfo, error) {
	return f.children, f.listErr
}

func (f *fakeSpawner) Models(context.Context, ModelQuery) ([]ModelInfo, error) {
	return f.models, nil
}

func (f *fakeSpawner) Spawn(_ context.Context, spec SpawnSpec) (AgentInfo, error) {
	if f.spawnErr != nil {
		return AgentInfo{}, f.spawnErr
	}
	f.spawned = append(f.spawned, spec)
	id := f.nextID
	if id == "" {
		id = "c_fake"
	}
	info := AgentInfo{ChildID: id, Name: spec.Name, Model: spec.Model, Status: "idle", Cwd: spec.Cwd}
	f.children = append(f.children, info)
	return info, nil
}

func (f *fakeSpawner) View(_ context.Context, childID string, limit int) (string, error) {
	if f.viewErr != nil {
		return "", f.viewErr
	}
	return f.view, nil
}

func (f *fakeSpawner) Send(_ context.Context, childID, message string) error {
	if f.sendErr != nil {
		return f.sendErr
	}
	f.sent = append(f.sent, struct{ ChildID, Message string }{childID, message})
	return nil
}

func (f *fakeSpawner) Kill(_ context.Context, childID string) error {
	if f.killErr != nil {
		return f.killErr
	}
	f.killed = append(f.killed, childID)
	return nil
}

func newAgentTools(t *testing.T, sp AgentSpawner) (*Registry, context.Context) {
	t.Helper()
	reg := DefaultBlueprint.MaterializeAll(ToolOpts{
		Cwd:     t.TempDir(),
		Agents:  sp,
		ChildID: "c_self",
	})
	ctx := context.WithValue(context.Background(), ConversationIDKey{}, "conv-1")
	return reg, ctx
}

// A nil spawner must remove all six tools, not register ones that can only
// answer "not configured". Same rule SkillBlueprint follows for zero skills.
func TestAgentToolsDeclineWithoutSpawner(t *testing.T) {
	reg := DefaultBlueprint.MaterializeAll(ToolOpts{Cwd: t.TempDir()})
	for _, name := range []string{
		"agent_spawn", "agent_list", "agent_view", "agent_send", "agent_kill", "agent_models",
	} {
		if _, err := reg.Execute(context.Background(), name, json.RawMessage(`{}`)); err == nil {
			t.Errorf("%s must not be registered without a spawner", name)
		} else if !strings.Contains(err.Error(), "unknown tool") {
			t.Errorf("%s: want unknown tool, got %v", name, err)
		}
	}
}

func TestAgentListRendersDescendants(t *testing.T) {
	sp := &fakeSpawner{children: []AgentInfo{
		{ChildID: "c_a", Name: "reviewer", Model: "anthropic/claude-opus-4", Status: "idle", Cwd: "/w", Depth: 1},
		{ChildID: "c_b", Name: "impl", Model: "anthropic/claude-sonnet-4", Status: "streaming", Cwd: "/w", Depth: 1},
	}}
	reg, ctx := newAgentTools(t, sp)
	out, err := reg.Execute(ctx, "agent_list", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("agent_list: %v", err)
	}
	for _, want := range []string{"c_a", "reviewer", "idle", "c_b", "impl", "streaming"} {
		if !strings.Contains(out, want) {
			t.Errorf("agent_list output missing %q; got:\n%s", want, out)
		}
	}
}

func TestAgentListEmptyIsNotAnError(t *testing.T) {
	reg, ctx := newAgentTools(t, &fakeSpawner{})
	out, err := reg.Execute(ctx, "agent_list", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("an empty subtree must not be an error: %v", err)
	}
	if !strings.Contains(out, "0 agent(s)") {
		t.Fatalf("got:\n%s", out)
	}
}

func TestAgentListSurfacesStoreErrors(t *testing.T) {
	reg, ctx := newAgentTools(t, &fakeSpawner{listErr: errors.New("boom")})
	if _, err := reg.Execute(ctx, "agent_list", json.RawMessage(`{}`)); err == nil {
		t.Fatal("a spawner error must reach the model, not be swallowed")
	}
}

// TestAgentModelsRendersCatalog: a NARROWED call renders rows. A bare call
// deliberately does not -- it answers with a summary, because the live catalog
// is several hundred models and dumping it fills the agent's context. See
// TestNoArgsReturnsASummaryNotAList.
func TestAgentModelsRendersCatalog(t *testing.T) {
	sp := &fakeSpawner{models: []ModelInfo{
		{ID: "anthropic/claude-opus-4", Provider: "anthropic"},
		{ID: "openai/gpt-5", Provider: "openai"},
	}}
	reg, ctx := newAgentTools(t, sp)
	out, err := reg.Execute(ctx, "agent_models", json.RawMessage(`{"limit":10}`))
	if err != nil {
		t.Fatalf("agent_models: %v", err)
	}
	if !strings.Contains(out, "anthropic/claude-opus-4") || !strings.Contains(out, "openai/gpt-5") {
		t.Fatalf("got:\n%s", out)
	}
}

func TestAgentSpawnPassesSpecThrough(t *testing.T) {
	sp := &fakeSpawner{nextID: "c_worker"}
	reg, ctx := newAgentTools(t, sp)
	out, err := reg.Execute(ctx, "agent_spawn", json.RawMessage(
		`{"name":"impl","model":"anthropic/claude-sonnet-4","prompt":"do the thing","cwd":"/w","task":"2.1"}`))
	if err != nil {
		t.Fatalf("agent_spawn: %v", err)
	}
	if len(sp.spawned) != 1 {
		t.Fatalf("want 1 spawn, got %d", len(sp.spawned))
	}
	got := sp.spawned[0]
	if got.Name != "impl" || got.Model != "anthropic/claude-sonnet-4" ||
		got.Prompt != "do the thing" || got.Cwd != "/w" || got.Task != "2.1" {
		t.Fatalf("spec not passed through: %+v", got)
	}
	if !strings.Contains(out, "c_worker") {
		t.Fatalf("result must return the handle; got:\n%s", out)
	}
}

// The tool must not accept a parent: a coordinator that can name its own
// parent can spawn into a sibling's subtree. The schema has no such property,
// and an unknown JSON key must not smuggle one in.
func TestAgentSpawnIgnoresForgedParent(t *testing.T) {
	sp := &fakeSpawner{}
	reg, ctx := newAgentTools(t, sp)
	if _, err := reg.Execute(ctx, "agent_spawn", json.RawMessage(
		`{"prompt":"x","parent":"c_stranger","parentChildId":"c_stranger"}`)); err != nil {
		t.Fatal(err)
	}
	// SpawnSpec has no parent field at all, so this is a compile-time
	// guarantee reinforced by an observable one: nothing the tool produces
	// can name a parent.
	if len(sp.spawned) != 1 {
		t.Fatalf("want 1 spawn, got %d", len(sp.spawned))
	}
}

func TestAgentSpawnRequiresPrompt(t *testing.T) {
	reg, ctx := newAgentTools(t, &fakeSpawner{})
	if _, err := reg.Execute(ctx, "agent_spawn", json.RawMessage(`{"name":"x"}`)); err == nil {
		t.Fatal("a spawn with nothing to do must be refused")
	}
}

func TestAgentSpawnSurfacesRefusal(t *testing.T) {
	sp := &fakeSpawner{spawnErr: errors.New("depth limit: absolute depth 3 exceeds RAFIKI_MAX_DEPTH")}
	reg, ctx := newAgentTools(t, sp)
	_, err := reg.Execute(ctx, "agent_spawn", json.RawMessage(`{"prompt":"x"}`))
	if err == nil || !strings.Contains(err.Error(), "depth limit") {
		t.Fatalf("a controller refusal must reach the model verbatim; got %v", err)
	}
}

func TestAgentViewReturnsTranscript(t *testing.T) {
	sp := &fakeSpawner{view: "user: do the thing\nassistant: done\n"}
	reg, ctx := newAgentTools(t, sp)
	out, err := reg.Execute(ctx, "agent_view", json.RawMessage(`{"agent":"c_a"}`))
	if err != nil {
		t.Fatalf("agent_view: %v", err)
	}
	if !strings.Contains(out, "do the thing") {
		t.Fatalf("got:\n%s", out)
	}
}

func TestAgentSendDeliversMessage(t *testing.T) {
	sp := &fakeSpawner{}
	reg, ctx := newAgentTools(t, sp)
	if _, err := reg.Execute(ctx, "agent_send", json.RawMessage(
		`{"agent":"c_a","message":"also update the docs"}`)); err != nil {
		t.Fatalf("agent_send: %v", err)
	}
	if len(sp.sent) != 1 || sp.sent[0].ChildID != "c_a" || sp.sent[0].Message != "also update the docs" {
		t.Fatalf("got %+v", sp.sent)
	}
}

func TestAgentKillStopsNamedAgent(t *testing.T) {
	sp := &fakeSpawner{}
	reg, ctx := newAgentTools(t, sp)
	if _, err := reg.Execute(ctx, "agent_kill", json.RawMessage(`{"agent":"c_a"}`)); err != nil {
		t.Fatalf("agent_kill: %v", err)
	}
	if len(sp.killed) != 1 || sp.killed[0] != "c_a" {
		t.Fatalf("got %v", sp.killed)
	}
}

// A refusal from the daemon must reach the model as an error it can act on,
// naming the offending id — not be flattened into a generic failure.
func TestSteeringRefusalsReachTheModel(t *testing.T) {
	sp := &fakeSpawner{
		viewErr: errors.New("agent c_stranger is not a descendant of yours"),
		sendErr: errors.New("agent c_stranger is not a descendant of yours"),
		killErr: errors.New("agent c_stranger is not a descendant of yours"),
	}
	reg, ctx := newAgentTools(t, sp)
	for name, args := range map[string]string{
		"agent_view": `{"agent":"c_stranger"}`,
		"agent_send": `{"agent":"c_stranger","message":"x"}`,
		"agent_kill": `{"agent":"c_stranger"}`,
	} {
		_, err := reg.Execute(ctx, name, json.RawMessage(args))
		if err == nil || !strings.Contains(err.Error(), "c_stranger") {
			t.Errorf("%s: want a refusal naming c_stranger, got %v", name, err)
		}
	}
}

func TestSteeringVerbsRequireAnAgentID(t *testing.T) {
	reg, ctx := newAgentTools(t, &fakeSpawner{})
	for name, args := range map[string]string{
		"agent_view": `{}`,
		"agent_send": `{"message":"x"}`,
		"agent_kill": `{}`,
	} {
		if _, err := reg.Execute(ctx, name, json.RawMessage(args)); err == nil {
			t.Errorf("%s with no agent id must fail", name)
		}
	}
}

func TestAgentSpawnPassesTheExecutorGrantThrough(t *testing.T) {
	sp := &fakeSpawner{nextID: "c_worker"}
	reg, ctx := newAgentTools(t, sp)
	if _, err := reg.Execute(ctx, "agent_spawn", json.RawMessage(
		`{"prompt":"build it","executor":"env=work,os=linux","workspace":"ephemeral"}`)); err != nil {
		t.Fatalf("agent_spawn: %v", err)
	}
	got := sp.spawned[0]
	if got.ExecutorSelector != "env=work,os=linux" {
		t.Errorf("selector = %q", got.ExecutorSelector)
	}
	if got.WorkspaceMode != "ephemeral" {
		t.Errorf("workspace = %q", got.WorkspaceMode)
	}
}

// The entire model-facing grant is a selector and a mode. Nothing path-shaped
// survives on the model-facing side — a coordinator choosing labels cannot
// make the subtle mistakes a coordinator composing path allowlists would, and
// that property is what makes grants safe to author without human review.
func TestAgentSpawnHasNoPathShapedParameter(t *testing.T) {
	schema := AgentSpawnBlueprint{}.InputSchema()
	for _, p := range schema.Properties {
		switch p.Name {
		case "mount", "mounts", "roots", "path", "paths", "volume", "allow", "deny":
			t.Errorf("agent_spawn grew a path-shaped parameter %q; the grant is a selector and a mode", p.Name)
		}
	}
}

// An unknown workspace mode is refused at the tool, not silently defaulted.
// "ephemeral" and "pinned" mean genuinely different things about whether the
// worker's uncommitted work can survive a machine going away.
func TestUnknownWorkspaceModeIsRefused(t *testing.T) {
	reg, ctx := newAgentTools(t, &fakeSpawner{})
	_, err := reg.Execute(ctx, "agent_spawn", json.RawMessage(
		`{"prompt":"x","workspace":"whatever"}`))
	if err == nil {
		t.Fatal("an unrecognised workspace mode must be refused")
	}
	if !strings.Contains(err.Error(), "ephemeral") || !strings.Contains(err.Error(), "pinned") {
		t.Errorf("the refusal must name the valid values: %v", err)
	}
}

// A malformed selector must fail at spawn with a message about the selector —
// not later, as a mysterious no-match.
func TestMalformedSelectorIsRefusedWithItsOwnMessage(t *testing.T) {
	sp := &fakeSpawner{spawnErr: errors.New(`invalid executor selector "os in": ...`)}
	reg, ctx := newAgentTools(t, sp)
	_, err := reg.Execute(ctx, "agent_spawn", json.RawMessage(`{"prompt":"x","executor":"os in"}`))
	if err == nil || !strings.Contains(err.Error(), "selector") {
		t.Fatalf("got %v", err)
	}
}
