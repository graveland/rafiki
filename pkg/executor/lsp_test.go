package executor

import (
	"os"
	"path/filepath"
	"testing"

	"go.graveland.dev/rafiki/pkg/fundi/tools"
)

// With no language server configured or installed, the LSP tools must be
// absent from the executor's registry rather than present and always failing.
// This is the same decline-by-nil rule the rest of the tool layer uses.
func TestNoLSPMeansNoLSPTools(t *testing.T) {
	s := NewServer(Options{Root: t.TempDir(), NoLSP: true})
	defer func() { _ = s.Close() }()

	for _, name := range []string{"lsp_definition", "lsp_diagnostics", "lsp_rename"} {
		if registryHas(s.reg, name) {
			t.Errorf("%q is in the executor's registry with NoLSP set", name)
		}
	}
}

// A config naming a server that is not installed must also yield no tools:
// HasInstalledServer is what keeps eight tools that can only answer
// "executable file not found in $PATH" out of the model's tools[].
func TestUninstalledServerMeansNoLSPTools(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "lsp.json")
	if err := os.WriteFile(cfgPath, []byte(`{"servers":{"nope":{"command":"definitely-not-installed-xyzzy"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	s := NewServer(Options{Root: dir, LSPConfig: cfgPath})
	defer func() { _ = s.Close() }()

	if registryHas(s.reg, "lsp_definition") {
		t.Error("lsp_definition is registered though its language server is not installed")
	}
}

func registryHas(r *tools.Registry, name string) bool {
	for _, def := range r.Definitions() {
		if def.OfTool != nil && def.OfTool.Name == name {
			return true
		}
	}
	return false
}
