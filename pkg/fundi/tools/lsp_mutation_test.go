package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/multigres/testkit/assert"
)

// fakeLSPMutClient provides canned mutation responses.
type fakeLSPMutClient struct {
	fakeLSPClient
	renameFiles []string
	renameErr   error
	restartErr  error
	restarted   bool
	symbols     []LSPLocation
	symbolsErr  error
}

func (f *fakeLSPMutClient) Definition(context.Context, string, int, int) ([]LSPLocation, error) {
	return nil, nil
}
func (f *fakeLSPMutClient) References(context.Context, string, int, int) ([]LSPLocation, error) {
	return nil, nil
}
func (f *fakeLSPMutClient) DocumentSymbols(context.Context, string) ([]LSPLocation, error) {
	return f.symbols, f.symbolsErr
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
	f.restarted = true
	return f.restartErr
}

func TestLSPRename_Execute(t *testing.T) {
	c := assert.NewCollecting(t)
	dir := t.TempDir()
	// Create a test file so Rename can stat it for FileTracker refresh.
	c.Require().NoError(os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n"), 0o644))

	fake := &fakeLSPMutClient{
		fakeLSPClient: fakeLSPClient{diags: map[string][]LSPDiagnostic{}},
		renameFiles:   []string{filepath.Join(dir, "main.go"), filepath.Join(dir, "other.go")},
	}

	tool, err := LSPRenameBlueprint{}.Materialize(ToolOpts{
		LSP:         fake,
		Cwd:         dir,
		FileTracker: NewFileTracker(),
	})
	c.Require().NoError(err, "Materialize")

	result, err := tool.Execute(context.Background(), ToolInput(mustJSON(lspRenameInput{
		Path: "main.go", Line: 1, Col: 7, NewName: "renamedFunc",
	})))
	c.Require().NoError(err, "Execute")
	// Assert on the output. This previously only logged the result, so it
	// passed no matter what rename did — including doing nothing at all,
	// which is exactly what rename did against real gopls.
	c.False(!strings.Contains(result.Text, "main.go") || !strings.Contains(result.Text, "other.go"), "rename must report every modified file, got:\n%s", result.Text)
	c.NotStrContains(result.Text, "no files were modified", "rename reported success while modifying nothing:\n")
}

func TestLSPRename_Materialize_Declines(t *testing.T) {
	c := assert.NewCollecting(t)
	bp := LSPRenameBlueprint{}
	tool, err := bp.Materialize(ToolOpts{LSP: nil, Cwd: "/tmp"})
	c.Require().NoError(err, "Materialize")
	c.Nil(tool, "expected nil tool when LSP is nil")
}

func TestLSPRestart_Execute(t *testing.T) {
	c := assert.NewCollecting(t)
	fake := &fakeLSPMutClient{
		fakeLSPClient: fakeLSPClient{diags: map[string][]LSPDiagnostic{}},
	}

	tool, err := LSPRestartBlueprint{}.Materialize(ToolOpts{LSP: fake, Cwd: "/tmp"})
	c.Require().NoError(err, "Materialize")

	result, err := tool.Execute(context.Background(), ToolInput(mustJSON(lspRestartInput{Path: "main.go"})))
	c.Require().NoError(err, "Execute")
	c.True(fake.restarted, "lsp_restart did not reach the client's Restart method")
	c.NotEq("", result.Text, "lsp_restart must report what it did")
}

func TestLSPReplaceSymbol_Replace(t *testing.T) {
	c := assert.NewCollecting(t)
	dir := t.TempDir()
	p := filepath.Join(dir, "main.go")
	c.Require().NoError(os.WriteFile(p, []byte("package main\n\nfunc old() {\n\tprintln(1)\n}\n\nfunc keep() {}\n"), 0o644))

	fake := &fakeLSPMutClient{
		fakeLSPClient: fakeLSPClient{diags: map[string][]LSPDiagnostic{}},
		symbols: []LSPLocation{
			{Name: "old", Line: 2, Col: 5, EndLine: 4, EndCol: 1, Kind: "function"},
			{Name: "keep", Line: 6, Col: 5, EndLine: 6, EndCol: 12, Kind: "function"},
		},
	}
	tool, err := LSPReplaceSymbolBlueprint{}.Materialize(ToolOpts{
		LSP: fake, Cwd: dir, FileTracker: NewFileTracker(),
	})
	c.Require().NoError(err, "Materialize")

	result, err := tool.Execute(context.Background(), ToolInput(mustJSON(lspReplaceSymbolInput{
		Path: "main.go", Symbol: "old", Action: "replace",
		Replacement: "func old() {\n\tprintln(2)\n}",
	})))
	c.Require().NoError(err, "Execute")
	b, _ := os.ReadFile(p)
	c.Eq("package main\n\nfunc old() {\n\tprintln(2)\n}\n\nfunc keep() {}\n", string(b), "replace got")
	c.StrContains(result.Text, "Replaced symbol", "result =")
}

func TestLSPReplaceSymbol_AddBeforeAddAfterDelete(t *testing.T) {
	c := assert.NewCollecting(t)
	base := "package main\n\nfunc target() {\n\tprintln(1)\n}\n"

	run := func(action, replacement, want string) {
		dir := t.TempDir()
		p := filepath.Join(dir, "main.go")
		c.Require().NoError(os.WriteFile(p, []byte(base), 0o644))
		fake := &fakeLSPMutClient{
			fakeLSPClient: fakeLSPClient{diags: map[string][]LSPDiagnostic{}},
			symbols:       []LSPLocation{{Name: "target", Line: 2, Col: 5, EndLine: 4, EndCol: 1, Kind: "function"}},
		}
		tool, err := LSPReplaceSymbolBlueprint{}.Materialize(ToolOpts{LSP: fake, Cwd: dir, FileTracker: NewFileTracker()})
		c.Require().NoError(err, "Materialize")
		_, err = tool.Execute(context.Background(), ToolInput(mustJSON(lspReplaceSymbolInput{
			Path: "main.go", Symbol: "target", Action: action, Replacement: replacement,
		})))
		c.Require().NoError(err, "Execute %s", action)
		b, _ := os.ReadFile(p)
		c.Eq(want, string(b), "%s got", action)
	}

	run("add_before", "// note", "package main\n\n// note\nfunc target() {\n\tprintln(1)\n}\n")
	run("add_after", "// note", "package main\n\nfunc target() {\n\tprintln(1)\n}\n// note\n")
	run("delete", "", "package main\n\n")
}

func TestLSPReplaceSymbol_Errors(t *testing.T) {
	c := assert.NewCollecting(t)
	dir := t.TempDir()
	p := filepath.Join(dir, "main.go")
	c.Require().NoError(os.WriteFile(p, []byte("package main\n"), 0o644))
	fake := &fakeLSPMutClient{
		fakeLSPClient: fakeLSPClient{diags: map[string][]LSPDiagnostic{}},
		symbols:       []LSPLocation{{Name: "present", Line: 0, Col: 0, EndLine: 0, EndCol: 0}},
	}
	tool, err := LSPReplaceSymbolBlueprint{}.Materialize(ToolOpts{LSP: fake, Cwd: dir, FileTracker: NewFileTracker()})
	c.Require().NoError(err, "Materialize")

	// Unknown symbol.
	_, err = tool.Execute(context.Background(), ToolInput(mustJSON(lspReplaceSymbolInput{Path: "main.go", Symbol: "missing", Replacement: "x"})))
	c.Error(err, "expected an error for an unknown symbol")

	// Replacement required for replace.
	_, err = tool.Execute(context.Background(), ToolInput(mustJSON(lspReplaceSymbolInput{Path: "main.go", Symbol: "present", Action: "replace"})))
	c.Error(err, "expected an error when replacement is missing")

	// Invalid action.
	_, err = tool.Execute(context.Background(), ToolInput(mustJSON(lspReplaceSymbolInput{Path: "main.go", Symbol: "present", Action: "bogus", Replacement: "x"})))
	c.Error(err, "expected an error for an invalid action")
}

func TestLSPReplaceSymbol_Materialize_Declines(t *testing.T) {
	c := assert.NewCollecting(t)
	tool, err := LSPReplaceSymbolBlueprint{}.Materialize(ToolOpts{LSP: nil, Cwd: "/tmp"})
	c.Require().NoError(err, "Materialize")
	c.Nil(tool, "expected nil tool when LSP is nil")
}
