package lsp

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// TestIntegration_Gopls is a smoke test against a real gopls binary.
// It is skipped when gopls is not on PATH.
func TestIntegration_Gopls(t *testing.T) {
	goplsPath, err := exec.LookPath("gopls")
	if err != nil {
		t.Skipf("gopls not found on PATH: %v", err)
	}
	t.Logf("gopls at %s", goplsPath)

	// Create a minimal Go module in a temp dir.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example\n\ngo 1.21\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n\nfunc main() {\n\tprintln(\"hello\")\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Run gopls in the temp dir.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	client, err := NewClient(ctx, ClientConfig{
		Name:    "go",
		Command: goplsPath,
		Cwd:     dir,
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer func() { _ = client.Shutdown(ctx) }()

	if err := client.Initialize(ctx, dir); err != nil {
		t.Fatalf("Initialize: %v", err)
	}

	// Open the main.go file.
	content, err := os.ReadFile(filepath.Join(dir, "main.go"))
	if err != nil {
		t.Fatal(err)
	}
	startVer := client.DiagnosticsVersion()
	if err := client.DidOpen(ctx, filepath.Join(dir, "main.go"), string(content)); err != nil {
		t.Fatalf("DidOpen: %v", err)
	}
	if err := client.WaitForInitialDiagnostics(ctx, startVer, 15*time.Second); err != nil {
		t.Fatalf("WaitForInitialDiagnostics: %v", err)
	}

	diags, err := client.Diagnostics(ctx, filepath.Join(dir, "main.go"))
	if err != nil {
		t.Fatalf("Diagnostics: %v", err)
	}
	t.Logf("diagnostics count: %d", len(diags))
	for _, d := range diags {
		t.Logf("  L%d:%d: %s: %s", d.Range.Start.Line, d.Range.Start.Character, d.Severity, d.Message)
	}

	if err := client.DidClose(ctx, filepath.Join(dir, "main.go")); err != nil {
		t.Fatalf("DidClose: %v", err)
	}
}
