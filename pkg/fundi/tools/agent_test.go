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

func (f *fakeSpawner) Models(context.Context) ([]ModelInfo, error) {
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

func TestAgentModelsRendersCatalog(t *testing.T) {
	sp := &fakeSpawner{models: []ModelInfo{
		{ID: "anthropic/claude-opus-4", Provider: "anthropic"},
		{ID: "openai/gpt-5", Provider: "openai"},
	}}
	reg, ctx := newAgentTools(t, sp)
	out, err := reg.Execute(ctx, "agent_models", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("agent_models: %v", err)
	}
	if !strings.Contains(out, "anthropic/claude-opus-4") || !strings.Contains(out, "openai/gpt-5") {
		t.Fatalf("got:\n%s", out)
	}
}
