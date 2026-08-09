package tools

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// fakeLSPMutClient provides canned mutation responses.
type fakeLSPMutClient struct {
	fakeLSPClient
	renameFiles []string
	renameErr   error
	restartErr  error
}

func (f *fakeLSPMutClient) Definition(context.Context, string, int, int) ([]LSPLocation, error) {
	return nil, nil
}
func (f *fakeLSPMutClient) References(context.Context, string, int, int) ([]LSPLocation, error) {
	return nil, nil
}
func (f *fakeLSPMutClient) DocumentSymbols(context.Context, string) ([]LSPLocation, error) {
	return nil, nil
}
func (f *fakeLSPMutClient) WorkspaceSymbols(context.Context, string) ([]LSPLocation, error) {
	return nil, nil
}
func (f *fakeLSPMutClient) PrepareCallHierarchy(context.Context, string, int, int) ([]LSPCallHierarchyItem, error) {
	return nil, nil
}
func (f *fakeLSPMutClient) IncomingCalls(context.Context, LSPCallHierarchyItem) ([]LSPCallHierarchyItem, error) {
	return nil, nil
}
func (f *fakeLSPMutClient) OutgoingCalls(context.Context, LSPCallHierarchyItem) ([]LSPCallHierarchyItem, error) {
	return nil, nil
}
func (f *fakeLSPMutClient) Rename(_ context.Context, _ string, _, _ int, _ string) ([]string, error) {
	return f.renameFiles, f.renameErr
}
func (f *fakeLSPMutClient) Restart(_ context.Context, _ string) error {
	return f.restartErr
}

func TestLSPRename_Execute(t *testing.T) {
	dir := t.TempDir()
	// Create a test file so Rename can stat it for FileTracker refresh.
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	fake := &fakeLSPMutClient{
		fakeLSPClient: fakeLSPClient{diags: map[string][]LSPDiagnostic{}},
		renameFiles:   []string{filepath.Join(dir, "main.go"), filepath.Join(dir, "other.go")},
	}

	tool, err := LSPRenameBlueprint{}.Materialize(ToolOpts{
		LSP:         fake,
		Cwd:         dir,
		FileTracker: NewFileTracker(),
	})
	if err != nil {
		t.Fatalf("Materialize: %v", err)
	}

	result, err := tool.Execute(context.Background(), ToolInput(mustJSON(lspRenameInput{
		Path: "main.go", Line: 1, Col: 7, NewName: "renamedFunc",
	})))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	t.Logf("result: %s", result.Text)
}

func TestLSPRename_Materialize_Declines(t *testing.T) {
	bp := LSPRenameBlueprint{}
	tool, err := bp.Materialize(ToolOpts{LSP: nil, Cwd: "/tmp"})
	if err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	if tool != nil {
		t.Error("expected nil tool when LSP is nil")
	}
}

func TestLSPRestart_Execute(t *testing.T) {
	fake := &fakeLSPMutClient{
		fakeLSPClient: fakeLSPClient{diags: map[string][]LSPDiagnostic{}},
	}

	tool, err := LSPRestartBlueprint{}.Materialize(ToolOpts{LSP: fake, Cwd: "/tmp"})
	if err != nil {
		t.Fatalf("Materialize: %v", err)
	}

	result, err := tool.Execute(context.Background(), ToolInput(mustJSON(lspRestartInput{Path: "main.go"})))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	t.Logf("result: %s", result.Text)
}
