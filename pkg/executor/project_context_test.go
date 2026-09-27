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

func TestProjectContextReturnsWorkspaceInstructionFiles(t *testing.T) {
	c := assert.NewCollecting(t)
	dir := t.TempDir()
	c.Require().NoError(os.WriteFile(filepath.Join(dir, "CLAUDE.md"), []byte("executor-side rules"), 0o600))

	s := NewServer(Options{Root: dir, NoLSP: true})
	defer func() { _ = s.Close() }()

	prov, err := s.Provision(context.Background(),
		connect.NewRequest(&executorpb.ProvisionRequest{ChildId: "c_test"}))
	c.Require().NoError(err, "Provision")

	resp, err := s.ProjectContext(context.Background(),
		connect.NewRequest(&executorpb.ProjectContextRequest{WorkspaceId: prov.Msg.WorkspaceId}))
	c.Require().NoError(err, "ProjectContext")
	c.StrContains(resp.Msg.ContextFiles, "executor-side rules", "ContextFiles")
}

// An unknown workspace must be refused, not answered from the executor's root.
// Answering would turn the handle from a grant into a formality.
func TestProjectContextRejectsAnUnknownWorkspace(t *testing.T) {
	s := NewServer(Options{Root: t.TempDir(), NoLSP: true})
	defer func() { _ = s.Close() }()

	_, err := s.ProjectContext(context.Background(),
		connect.NewRequest(&executorpb.ProjectContextRequest{WorkspaceId: "ws_nope"}))
	assert.NewCollecting(t).Error(err, "an unknown workspace_id was answered")
}

// A workspace with no instruction files is the ordinary case and must be an
// empty answer rather than an error.
func TestProjectContextEmptyWorkspaceIsNotAnError(t *testing.T) {
	c := assert.NewCollecting(t)
	s := NewServer(Options{Root: t.TempDir(), NoLSP: true})
	defer func() { _ = s.Close() }()

	prov, err := s.Provision(context.Background(),
		connect.NewRequest(&executorpb.ProvisionRequest{ChildId: "c_test"}))
	c.Require().NoError(err, "Provision")
	resp, err := s.ProjectContext(context.Background(),
		connect.NewRequest(&executorpb.ProjectContextRequest{WorkspaceId: prov.Msg.WorkspaceId}))
	c.Require().NoError(err, "ProjectContext")
	c.Eq("", resp.Msg.ContextFiles, "ContextFiles")
}
