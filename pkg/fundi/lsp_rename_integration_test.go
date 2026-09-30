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
	"go.graveland.dev/rafiki/pkg/fundi/lspadapter"
	"go.graveland.dev/rafiki/pkg/fundi/tools"

	"github.com/multigres/testkit/assert"
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
	c := assert.NewCollecting(t)
	goplsPath, err := exec.LookPath("gopls")
	if err != nil {
		t.Skipf("gopls not found on PATH: %v", err)
	}

	dir := t.TempDir()
	write := func(name, content string) {
		t.Helper()
		c.Require().NoError(os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644))
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
	}, dir, nil)
	defer mgr.Shutdown(context.Background())

	adapter := lspadapter.New(mgr, tools.NewFileTracker())

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	mainPath := filepath.Join(dir, "main.go")
	c.Require().NoError(adapter.DidOpen(ctx, mainPath, ""), "DidOpen")
	_ = adapter.WaitForDiagnostics(ctx, mainPath, 15)

	// "func Add" -> Add starts at line 2 (0-based), column 5.
	modified, err := adapter.Rename(ctx, mainPath, 2, 5, "Sum")
	c.Require().NoError(err, "Rename")
	c.Require().NotEmpty(modified, "rename modified no files: the documentChanges shape is not being decoded")

	gotMain, err := os.ReadFile(mainPath)
	c.Require().NoError(err)
	gotOther, err := os.ReadFile(filepath.Join(dir, "other.go"))
	c.Require().NoError(err)

	c.False(strings.Contains(string(gotMain), "Add") || strings.Contains(string(gotOther), "Add"), "old name survived the rename:\nmain.go:\n%s\nother.go:\n%s", gotMain, gotOther)
	c.StrContains(string(gotMain), "func Sum(", "definition not renamed:\n%s", gotMain)
	c.StrContains(string(gotOther), "Sum(3, 4)", "cross-file reference not renamed:\n%s", gotOther)
	// The CJK literal must survive intact. Byte-indexed UTF-16 columns cut
	// it mid-rune and produced invalid UTF-8.
	c.StrContains(string(gotMain), `"日本語"`, "the CJK literal was corrupted by the edit:\n%q", gotMain)

	// Regression check for Finding 6: applyWorkspaceEdit wrote the rename to
	// disk but never told the server, so its OPEN buffer for main.go still
	// held the pre-rename content. "Add" -> "Sum" is exactly the
	// line-count-and-column-preserving case that made this silent: querying
	// DocumentSymbols at the same position gopls last indexed would still
	// report the OLD name with no error at all if the didChange notify were
	// missing. If this starts failing, check that Rename in lspadapter/adapter.go
	// still calls a.mgr.NotifyChange for every modified path.
	syms, err := adapter.DocumentSymbols(ctx, mainPath)
	c.Require().NoError(err, "DocumentSymbols")
	var sawOld, sawNew bool
	for _, s := range syms {
		switch s.Name {
		case "Add":
			sawOld = true
		case "Sum":
			sawNew = true
		}
	}
	c.False(sawOld, "server still reports the pre-rename symbol \"Add\" -- it was never notified of the rename (no didChange sent)")
	c.True(sawNew, "server does not report the renamed symbol \"Sum\"; expected DocumentSymbols to reflect the rename")
}
