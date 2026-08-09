package fundi

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.graveland.dev/rafiki/pkg/fundi/lsp"
	"go.graveland.dev/rafiki/pkg/fundi/tools"
)

// TestIntegration_RenameAgainstGopls is the end-to-end proof for the two
// defects that made lsp_rename worse than useless:
//
//   - Only WorkspaceEdit.changes was decoded, but gopls answers with
//     documentChanges. The map was always nil, nothing was applied, and the
//     tool returned "no files were modified" as a SUCCESS string — so the
//     model believed the rename had happened and carried on editing code
//     that still used the old name.
//   - LSP columns are UTF-16 code units, not bytes, so once the shape was
//     fixed the apply path cut multi-byte runes in half.
//
// The fixture deliberately puts a CJK string literal on the same line as the
// symbol being renamed, so both defects would fail this test.
func TestIntegration_RenameAgainstGopls(t *testing.T) {
	goplsPath, err := exec.LookPath("gopls")
	if err != nil {
		t.Skipf("gopls not found on PATH: %v", err)
	}

	dir := t.TempDir()
	write := func(name, content string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", "module example\n\ngo 1.21\n")
	// Add is defined in main.go and used in both files. The CJK literal sits
	// before the call on the same line.
	write("main.go", "package main\n\nfunc Add(a, b int) int { return a + b }\n\nfunc main() {\n\tprintln(\"日本語\", Add(1, 2))\n}\n")
	write("other.go", "package main\n\nvar _ = Add(3, 4)\n")

	mgr := lsp.NewManager(lsp.Config{
		Servers: map[string]lsp.ServerConfig{
			"go": {Command: goplsPath, Extensions: []string{".go"}},
		},
	}, dir)
	defer mgr.Shutdown(context.Background())

	adapter := &lspClientAdapter{mgr: mgr, tracker: tools.NewFileTracker()}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	mainPath := filepath.Join(dir, "main.go")
	if err := adapter.DidOpen(ctx, mainPath, ""); err != nil {
		t.Fatalf("DidOpen: %v", err)
	}
	_ = adapter.WaitForDiagnostics(ctx, mainPath, 15)

	// "func Add" -> Add starts at line 2 (0-based), column 5.
	modified, err := adapter.Rename(ctx, mainPath, 2, 5, "Sum")
	if err != nil {
		t.Fatalf("Rename: %v", err)
	}
	if len(modified) == 0 {
		t.Fatal("rename modified no files: the documentChanges shape is not being decoded")
	}

	gotMain, err := os.ReadFile(mainPath)
	if err != nil {
		t.Fatal(err)
	}
	gotOther, err := os.ReadFile(filepath.Join(dir, "other.go"))
	if err != nil {
		t.Fatal(err)
	}

	if strings.Contains(string(gotMain), "Add") || strings.Contains(string(gotOther), "Add") {
		t.Errorf("old name survived the rename:\nmain.go:\n%s\nother.go:\n%s", gotMain, gotOther)
	}
	if !strings.Contains(string(gotMain), "func Sum(") {
		t.Errorf("definition not renamed:\n%s", gotMain)
	}
	if !strings.Contains(string(gotOther), "Sum(3, 4)") {
		t.Errorf("cross-file reference not renamed:\n%s", gotOther)
	}
	// The CJK literal must survive intact. Byte-indexed UTF-16 columns cut
	// it mid-rune and produced invalid UTF-8.
	if !strings.Contains(string(gotMain), `"日本語"`) {
		t.Errorf("the CJK literal was corrupted by the edit:\n%q", gotMain)
	}

	// Regression check for Finding 6: applyWorkspaceEdit wrote the rename to
	// disk but never told the server, so its OPEN buffer for main.go still
	// held the pre-rename content. "Add" -> "Sum" is exactly the
	// line-count-and-column-preserving case that made this silent: querying
	// DocumentSymbols at the same position gopls last indexed would still
	// report the OLD name with no error at all if the didChange notify were
	// missing. If this starts failing, check that Rename in lsp_adapter.go
	// still calls a.mgr.NotifyChange for every modified path.
	syms, err := adapter.DocumentSymbols(ctx, mainPath)
	if err != nil {
		t.Fatalf("DocumentSymbols: %v", err)
	}
	var sawOld, sawNew bool
	for _, s := range syms {
		switch s.Name {
		case "Add":
			sawOld = true
		case "Sum":
			sawNew = true
		}
	}
	if sawOld {
		t.Error("server still reports the pre-rename symbol \"Add\" -- it was never notified of the rename (no didChange sent)")
	}
	if !sawNew {
		t.Error("server does not report the renamed symbol \"Sum\"; expected DocumentSymbols to reflect the rename")
	}
}
