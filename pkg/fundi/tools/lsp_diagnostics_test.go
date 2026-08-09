package tools

import (
	"context"
	"encoding/json"
	"os/exec"
	"strings"
	"testing"
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

func TestLSPDiagnostics_Materialize_DeclinesWhenNoLSP(t *testing.T) {
	bp := LSPDiagnosticsBlueprint{}
	tool, err := bp.Materialize(ToolOpts{LSP: nil, Cwd: "/tmp"})
	if err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	if tool != nil {
		t.Error("expected nil tool when LSP is nil")
	}
}

func TestLSPDiagnostics_Materialize_SucceedsWhenLSP(t *testing.T) {
	bp := LSPDiagnosticsBlueprint{}
	tool, err := bp.Materialize(ToolOpts{LSP: &fakeLSPClient{}, Cwd: "/tmp"})
	if err != nil {
		t.Fatalf("Materialize: %v", err)
	}
	if tool == nil {
		t.Fatal("expected non-nil tool when LSP is set")
	}
}

func TestLSPDiagnostics_Execute_ReturnsDiagnostics(t *testing.T) {
	fake := &fakeLSPClient{
		diags: map[string][]LSPDiagnostic{
			"/tmp/main.go": {
				{Path: "/tmp/main.go", Line: 5, Column: 0, Severity: "error", Message: "undefined: foo"},
				{Path: "/tmp/main.go", Line: 10, Column: 2, Severity: "warning", Message: "unused variable"},
			},
		},
	}

	tool, err := LSPDiagnosticsBlueprint{}.Materialize(ToolOpts{LSP: fake, Cwd: "/tmp"})
	if err != nil {
		t.Fatalf("Materialize: %v", err)
	}

	result, err := tool.Execute(context.Background(), ToolInput(mustJSON(map[string]any{"path": "main.go"})))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	text := result.Text
	t.Logf("output:\n%s", text)

	if !strings.Contains(text, "undefined: foo") {
		t.Error("expected output to contain diagnostic message")
	}
	if !strings.Contains(text, "unused variable") {
		t.Error("expected output to contain second diagnostic")
	}
	if !strings.Contains(text, "error") {
		t.Error("expected output to contain severity 'error'")
	}
	if !strings.Contains(text, "warning") {
		t.Error("expected output to contain severity 'warning'")
	}
}

func TestLSPDiagnostics_Execute_NoDiagnostics(t *testing.T) {
	fake := &fakeLSPClient{
		diags: map[string][]LSPDiagnostic{
			"/tmp/clean.go": {},
		},
	}

	tool, err := LSPDiagnosticsBlueprint{}.Materialize(ToolOpts{LSP: fake, Cwd: "/tmp"})
	if err != nil {
		t.Fatalf("Materialize: %v", err)
	}

	result, err := tool.Execute(context.Background(), ToolInput(mustJSON(map[string]any{"path": "clean.go"})))
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}

	text := result.Text
	t.Logf("output:\n%s", text)

	if !strings.Contains(text, "no diagnostics") {
		t.Error("expected 'no diagnostics' message")
	}
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

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}
