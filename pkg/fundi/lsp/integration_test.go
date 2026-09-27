package lsp

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/multigres/testkit/assert"
)

// TestIntegration_Gopls is a smoke test against a real gopls binary.
// It is skipped when gopls is not on PATH.
func TestIntegration_Gopls(t *testing.T) {
	c := assert.NewAborting(t)
	goplsPath, err := exec.LookPath("gopls")
	if err != nil {
		t.Skipf("gopls not found on PATH: %v", err)
	}
	t.Logf("gopls at %s", goplsPath)

	// Create a minimal Go module in a temp dir.
	dir := t.TempDir()
	c.NoError(os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example\n\ngo 1.21\n"), 0o644))
	// A DELIBERATE compile error, as the plan required. The fixture used to
	// be clean and the test only logged whatever came back, so it could not
	// tell "gopls found no problems" apart from "gopls never saw the file"
	// — it passed either way.
	src := "package main\n\nfunc main() {\n\tunusedVar := 42\n\tprintln(\"hello\")\n}\n"
	c.NoError(os.WriteFile(filepath.Join(dir, "main.go"), []byte(src), 0o644))

	// Run gopls in the temp dir.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	client, err := NewClient(ctx, ClientConfig{
		Name:    "go",
		Command: goplsPath,
		Cwd:     dir,
	})
	c.NoError(err, "NewClient")
	defer func() { _ = client.Shutdown(ctx) }()

	c.NoError(client.Initialize(ctx, dir), "Initialize")

	// Open the main.go file.
	content, err := os.ReadFile(filepath.Join(dir, "main.go"))
	c.NoError(err)
	startVer := client.DiagnosticsVersion()
	c.NoError(client.DidOpen(ctx, filepath.Join(dir, "main.go"), string(content)), "DidOpen")
	c.NoError(client.WaitForInitialDiagnostics(ctx, startVer, 15*time.Second), "WaitForInitialDiagnostics")

	diags, err := client.Diagnostics(ctx, filepath.Join(dir, "main.go"))
	c.NoError(err, "Diagnostics")
	for _, d := range diags {
		t.Logf("  L%d:%d: %s: %s", d.Range.Start.Line, d.Range.Start.Character, d.Severity, d.Message)
	}
	c.NotEmpty(diags, "gopls reported no diagnostics for a file with an unused variable; "+
		"the document never reached the server")
	var found bool
	for _, d := range diags {
		if strings.Contains(d.Message, "unusedVar") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected a diagnostic naming unusedVar, got %d others", len(diags))
	}

	c.NoError(client.DidClose(ctx, filepath.Join(dir, "main.go")), "DidClose")
}
