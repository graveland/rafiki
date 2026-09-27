package tools

import (
	"context"
	"os/exec"
	"testing"

	"github.com/multigres/testkit/assert"
)

// fakeLSPClient implements LSPClient for testing.
type fakeLSPClient struct {
	diags       map[string][]LSPDiagnostic
	didOpenErr  error
	waitTimeout bool
}

func (f *fakeLSPClient) Diagnostics(_ context.Context, path string) ([]LSPDiagnostic, error) {
	return f.diags[path], nil
}

func (f *fakeLSPClient) DidOpen(_ context.Context, _, _ string) error {
	return f.didOpenErr
}

func (f *fakeLSPClient) DidChange(_ context.Context, _, _ string) error {
	return nil
}

func (f *fakeLSPClient) WaitForDiagnostics(_ context.Context, _ string, _ int) error {
	if f.waitTimeout {
		return context.DeadlineExceeded
	}
	return nil
}

func (f *fakeLSPClient) Definition(context.Context, string, int, int) ([]LSPLocation, error) {
	return nil, nil
}
func (f *fakeLSPClient) References(context.Context, string, int, int) ([]LSPLocation, error) {
	return nil, nil
}
func (f *fakeLSPClient) DocumentSymbols(context.Context, string) ([]LSPLocation, error) {
	return nil, nil
}
func (f *fakeLSPClient) WorkspaceSymbols(context.Context, string) ([]LSPLocation, error) {
	return nil, nil
}
func (f *fakeLSPClient) PrepareCallHierarchy(context.Context, string, int, int) ([]LSPCallHierarchyItem, error) {
	return nil, nil
}
func (f *fakeLSPClient) IncomingCalls(context.Context, LSPCallHierarchyItem) ([]LSPCallHierarchyItem, error) {
	return nil, nil
}
func (f *fakeLSPClient) OutgoingCalls(context.Context, LSPCallHierarchyItem) ([]LSPCallHierarchyItem, error) {
	return nil, nil
}
func (f *fakeLSPClient) Rename(context.Context, string, int, int, string) ([]string, error) {
	return nil, nil
}
func (f *fakeLSPClient) Restart(context.Context, string) error { return nil }

// TestLSPDiagnostics_SchemaRequiresPath is the regression test for the
// documentation promising a mode that is a hard error: the description and
// schema used to say "path" was optional ("omit or use \".\" to include all
// open files"), but resolveToolPath rejects an empty path with "path is
// required", and "." resolves to the cwd directory, which DidOpen fails to
// open (os.ReadFile on a directory). A model following the documentation
// burned a turn either way. This pins that the schema and description now
// agree with the implementation: path is required, and no omit/"." mode is
// advertised.
func TestLSPDiagnostics_SchemaRequiresPath(t *testing.T) {
	c := assert.NewCollecting(t)
	bp := LSPDiagnosticsBlueprint{}

	schema := bp.InputSchema()
	found := false
	for _, r := range schema.Required {
		if r == "path" {
			found = true
		}
	}
	c.Require().True(found, "schema.Required = %v, want it to include %q", schema.Required, "path")

	desc := bp.Description()
	for _, promise := range []string{"omit", "\".\"", "all open files", "all currently-open files"} {
		c.NotStrContains(desc, promise, "description still promises an omit/all-open-files mode (")
	}
}

func TestLSPDiagnostics_Materialize_DeclinesWhenNoLSP(t *testing.T) {
	c := assert.NewCollecting(t)
	bp := LSPDiagnosticsBlueprint{}
	tool, err := bp.Materialize(ToolOpts{LSP: nil, Cwd: "/tmp"})
	c.Require().NoError(err, "Materialize")
	c.Nil(tool, "expected nil tool when LSP is nil")
}

func TestLSPDiagnostics_Materialize_SucceedsWhenLSP(t *testing.T) {
	c := assert.NewAborting(t)
	bp := LSPDiagnosticsBlueprint{}
	tool, err := bp.Materialize(ToolOpts{LSP: &fakeLSPClient{}, Cwd: "/tmp"})
	c.NoError(err, "Materialize")
	c.NotNil(tool, "expected non-nil tool when LSP is set")
}

func TestLSPDiagnostics_Execute_ReturnsDiagnostics(t *testing.T) {
	c := assert.NewCollecting(t)
	fake := &fakeLSPClient{
		diags: map[string][]LSPDiagnostic{
			"/tmp/main.go": {
				{Path: "/tmp/main.go", Line: 5, Column: 0, Severity: "error", Message: "undefined: foo"},
				{Path: "/tmp/main.go", Line: 10, Column: 2, Severity: "warning", Message: "unused variable"},
			},
		},
	}

	tool, err := LSPDiagnosticsBlueprint{}.Materialize(ToolOpts{LSP: fake, Cwd: "/tmp"})
	c.Require().NoError(err, "Materialize")

	result, err := tool.Execute(context.Background(), ToolInput(mustJSON(map[string]any{"path": "main.go"})))
	c.Require().NoError(err, "Execute")

	text := result.Text
	t.Logf("output:\n%s", text)

	c.StrContains(text, "undefined: foo", "expected output to contain diagnostic message")
	c.StrContains(text, "unused variable", "expected output to contain second diagnostic")
	c.StrContains(text, "error", "expected output to contain severity 'error'")
	c.StrContains(text, "warning", "expected output to contain severity 'warning'")
}

func TestLSPDiagnostics_Execute_NoDiagnostics(t *testing.T) {
	c := assert.NewCollecting(t)
	fake := &fakeLSPClient{
		diags: map[string][]LSPDiagnostic{
			"/tmp/clean.go": {},
		},
	}

	tool, err := LSPDiagnosticsBlueprint{}.Materialize(ToolOpts{LSP: fake, Cwd: "/tmp"})
	c.Require().NoError(err, "Materialize")

	result, err := tool.Execute(context.Background(), ToolInput(mustJSON(map[string]any{"path": "clean.go"})))
	c.Require().NoError(err, "Execute")

	text := result.Text
	t.Logf("output:\n%s", text)

	c.StrContains(text, "no diagnostics", "expected 'no diagnostics' message")
}

// TestLSPDiagnostics_Integration_Gopls tests against real gopls.
// The full integration test is in pkg/fundi/lsp/ — see TestIntegration_Gopls.
func TestLSPDiagnostics_Integration_Gopls(t *testing.T) {
	_, err := exec.LookPath("gopls")
	if err != nil {
		t.Skipf("gopls not found: %v", err)
	}
	t.Log("gopls integration tested via lsp.Manager in pkg/fundi/lsp/integration_test.go")
}
