package executor

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"connectrpc.com/connect"

	executorpb "go.graveland.dev/rafiki/pkg/executorpb"
)

func writeSkill(t *testing.T, root, name, body string) {
	t.Helper()
	dir := filepath.Join(root, ".claude", "skills", name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	content := "---\nname: " + name + "\ndescription: does " + name + "\n---\n\n" + body
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func provision(t *testing.T, s *Server) string {
	t.Helper()
	resp, err := s.Provision(context.Background(),
		connect.NewRequest(&executorpb.ProvisionRequest{ChildId: "c_test"}))
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	return resp.Msg.WorkspaceId
}

func TestProjectSkillsListsWorkspaceSkills(t *testing.T) {
	dir := t.TempDir()
	writeSkill(t, dir, "deploy", "deploy instructions")

	s := NewServer(Options{Root: dir, NoLSP: true})
	defer func() { _ = s.Close() }()
	ws := provision(t, s)

	resp, err := s.ProjectSkills(context.Background(),
		connect.NewRequest(&executorpb.ProjectSkillsRequest{WorkspaceId: ws}))
	if err != nil {
		t.Fatalf("ProjectSkills: %v", err)
	}
	if len(resp.Msg.Skills) != 1 || resp.Msg.Skills[0].Name != "deploy" {
		t.Fatalf("Skills = %+v, want one named deploy", resp.Msg.Skills)
	}
	if resp.Msg.Skills[0].Description == "" {
		t.Error("description is empty; the inventory would be useless to the model")
	}
}

// A workspace with no skills is ordinary and must not be an error.
func TestProjectSkillsEmptyIsNotAnError(t *testing.T) {
	s := NewServer(Options{Root: t.TempDir(), NoLSP: true})
	defer func() { _ = s.Close() }()
	ws := provision(t, s)

	resp, err := s.ProjectSkills(context.Background(),
		connect.NewRequest(&executorpb.ProjectSkillsRequest{WorkspaceId: ws}))
	if err != nil {
		t.Fatalf("ProjectSkills: %v", err)
	}
	if len(resp.Msg.Skills) != 0 {
		t.Errorf("Skills = %+v, want none", resp.Msg.Skills)
	}
}

func TestSkillBodyReturnsTheBody(t *testing.T) {
	dir := t.TempDir()
	writeSkill(t, dir, "deploy", "run the deploy script")

	s := NewServer(Options{Root: dir, NoLSP: true})
	defer func() { _ = s.Close() }()
	ws := provision(t, s)

	resp, err := s.SkillBody(context.Background(),
		connect.NewRequest(&executorpb.SkillBodyRequest{WorkspaceId: ws, Name: "deploy"}))
	if err != nil {
		t.Fatalf("SkillBody: %v", err)
	}
	if !strings.Contains(resp.Msg.Body, "run the deploy script") {
		t.Errorf("Body = %q, want the skill's text", resp.Msg.Body)
	}
	if resp.Msg.Dir == "" {
		t.Error("Dir is empty; the model is told this is the skill's base directory")
	}
}

// An unknown name must be refused rather than answered with an empty body,
// which the model would read as a skill that exists and says nothing.
func TestSkillBodyRejectsAnUnknownName(t *testing.T) {
	s := NewServer(Options{Root: t.TempDir(), NoLSP: true})
	defer func() { _ = s.Close() }()
	ws := provision(t, s)

	if _, err := s.SkillBody(context.Background(),
		connect.NewRequest(&executorpb.SkillBodyRequest{WorkspaceId: ws, Name: "nope"})); err == nil {
		t.Error("an unknown skill name was answered")
	}
}

// Both calls must refuse an unknown workspace, or the handle is a formality.
func TestProjectSkillsRejectsAnUnknownWorkspace(t *testing.T) {
	s := NewServer(Options{Root: t.TempDir(), NoLSP: true})
	defer func() { _ = s.Close() }()

	if _, err := s.ProjectSkills(context.Background(),
		connect.NewRequest(&executorpb.ProjectSkillsRequest{WorkspaceId: "ws_nope"})); err == nil {
		t.Error("an unknown workspace_id was answered")
	}
}
