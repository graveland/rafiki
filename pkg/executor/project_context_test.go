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

func TestProjectContextReturnsWorkspaceInstructionFiles(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "CLAUDE.md"), []byte("executor-side rules"), 0o600); err != nil {
		t.Fatal(err)
	}

	s := NewServer(Options{Root: dir, NoLSP: true})
	defer func() { _ = s.Close() }()

	prov, err := s.Provision(context.Background(),
		connect.NewRequest(&executorpb.ProvisionRequest{ChildId: "c_test"}))
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}

	resp, err := s.ProjectContext(context.Background(),
		connect.NewRequest(&executorpb.ProjectContextRequest{WorkspaceId: prov.Msg.WorkspaceId}))
	if err != nil {
		t.Fatalf("ProjectContext: %v", err)
	}
	if !strings.Contains(resp.Msg.ContextFiles, "executor-side rules") {
		t.Errorf("ContextFiles = %q, want the workspace's CLAUDE.md", resp.Msg.ContextFiles)
	}
}

// An unknown workspace must be refused, not answered from the executor's root.
// Answering would turn the handle from a grant into a formality.
func TestProjectContextRejectsAnUnknownWorkspace(t *testing.T) {
	s := NewServer(Options{Root: t.TempDir(), NoLSP: true})
	defer func() { _ = s.Close() }()

	_, err := s.ProjectContext(context.Background(),
		connect.NewRequest(&executorpb.ProjectContextRequest{WorkspaceId: "ws_nope"}))
	if err == nil {
		t.Error("an unknown workspace_id was answered")
	}
}

// A workspace with no instruction files is the ordinary case and must be an
// empty answer rather than an error.
func TestProjectContextEmptyWorkspaceIsNotAnError(t *testing.T) {
	s := NewServer(Options{Root: t.TempDir(), NoLSP: true})
	defer func() { _ = s.Close() }()

	prov, err := s.Provision(context.Background(),
		connect.NewRequest(&executorpb.ProvisionRequest{ChildId: "c_test"}))
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	resp, err := s.ProjectContext(context.Background(),
		connect.NewRequest(&executorpb.ProjectContextRequest{WorkspaceId: prov.Msg.WorkspaceId}))
	if err != nil {
		t.Fatalf("ProjectContext: %v", err)
	}
	if resp.Msg.ContextFiles != "" {
		t.Errorf("ContextFiles = %q, want empty", resp.Msg.ContextFiles)
	}
}
