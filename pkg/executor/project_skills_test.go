package executor

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"connectrpc.com/connect"

	executorpb "go.graveland.dev/rafiki/pkg/executorpb"

	"github.com/multigres/testkit/assert"
)

func writeSkill(t *testing.T, root, name, body string) {
	t.Helper()
	c := assert.NewAborting(t)
	dir := filepath.Join(root, ".claude", "skills", name)
	c.NoError(os.MkdirAll(dir, 0o755))
	content := "---\nname: " + name + "\ndescription: does " + name + "\n---\n\n" + body
	c.NoError(os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(content), 0o600))
}

func provision(t *testing.T, s *Server) string {
	t.Helper()
	resp, err := s.Provision(context.Background(),
		connect.NewRequest(&executorpb.ProvisionRequest{ChildId: "c_test"}))
	assert.NewAborting(t).NoError(err, "Provision")
	return resp.Msg.WorkspaceId
}

func TestProjectSkillsListsWorkspaceSkills(t *testing.T) {
	c := assert.NewCollecting(t)
	dir := t.TempDir()
	writeSkill(t, dir, "deploy", "deploy instructions")

	s := NewServer(Options{Root: dir, NoLSP: true})
	defer func() { _ = s.Close() }()
	ws := provision(t, s)

	resp, err := s.ProjectSkills(context.Background(),
		connect.NewRequest(&executorpb.ProjectSkillsRequest{WorkspaceId: ws}))
	c.Require().NoError(err, "ProjectSkills")
	if len(resp.Msg.Skills) != 1 || resp.Msg.Skills[0].Name != "deploy" {
		t.Fatalf("Skills = %+v, want one named deploy", resp.Msg.Skills)
	}
	c.NotEq("", resp.Msg.Skills[0].Description, "description is empty; the inventory would be useless to the model")
}

// A workspace with no skills is ordinary and must not be an error.
func TestProjectSkillsEmptyIsNotAnError(t *testing.T) {
	c := assert.NewCollecting(t)
	s := NewServer(Options{Root: t.TempDir(), NoLSP: true})
	defer func() { _ = s.Close() }()
	ws := provision(t, s)

	resp, err := s.ProjectSkills(context.Background(),
		connect.NewRequest(&executorpb.ProjectSkillsRequest{WorkspaceId: ws}))
	c.Require().NoError(err, "ProjectSkills")
	c.Empty(resp.Msg.Skills, "Skills")
}

func TestSkillBodyReturnsTheBody(t *testing.T) {
	c := assert.NewCollecting(t)
	dir := t.TempDir()
	writeSkill(t, dir, "deploy", "run the deploy script")

	s := NewServer(Options{Root: dir, NoLSP: true})
	defer func() { _ = s.Close() }()
	ws := provision(t, s)

	resp, err := s.SkillBody(context.Background(),
		connect.NewRequest(&executorpb.SkillBodyRequest{WorkspaceId: ws, Name: "deploy"}))
	c.Require().NoError(err, "SkillBody")
	c.StrContains(resp.Msg.Body, "run the deploy script", "Body")
	c.NotEq("", resp.Msg.Dir, "Dir is empty; the model is told this is the skill's base directory")
}

// An unknown name must be refused rather than answered with an empty body,
// which the model would read as a skill that exists and says nothing.
func TestSkillBodyRejectsAnUnknownName(t *testing.T) {
	s := NewServer(Options{Root: t.TempDir(), NoLSP: true})
	defer func() { _ = s.Close() }()
	ws := provision(t, s)

	_, err := s.SkillBody(context.Background(),
		connect.NewRequest(&executorpb.SkillBodyRequest{WorkspaceId: ws, Name: "nope"}))
	assert.NewCollecting(t).Error(err, "an unknown skill name was answered")
}

// Both calls must refuse an unknown workspace, or the handle is a formality.
func TestProjectSkillsRejectsAnUnknownWorkspace(t *testing.T) {
	s := NewServer(Options{Root: t.TempDir(), NoLSP: true})
	defer func() { _ = s.Close() }()

	_, err := s.ProjectSkills(context.Background(),
		connect.NewRequest(&executorpb.ProjectSkillsRequest{WorkspaceId: "ws_nope"}))
	assert.NewCollecting(t).Error(err, "an unknown workspace_id was answered")
}
