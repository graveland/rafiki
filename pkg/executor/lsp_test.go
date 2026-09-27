package executor

import (
	"os"
	"path/filepath"
	"testing"

	"go.graveland.dev/rafiki/pkg/fundi/tools"

	"github.com/multigres/testkit/assert"
)

// With no language server configured or installed, the LSP tools must be
// absent from the executor's registry rather than present and always failing.
// This is the same decline-by-nil rule the rest of the tool layer uses.
func TestNoLSPMeansNoLSPTools(t *testing.T) {
	s := NewServer(Options{Root: t.TempDir(), NoLSP: true})
	defer func() { _ = s.Close() }()

	for _, name := range []string{"lsp_definition", "lsp_diagnostics", "lsp_rename"} {
		assert.NewCollecting(t).False(registryHas(s.reg, name), "%q is in the executor's registry with NoLSP set", name)
	}
}

// A config naming a server that is not installed must also yield no tools:
// HasInstalledServer is what keeps eight tools that can only answer
// "executable file not found in $PATH" out of the model's tools[].
func TestUninstalledServerMeansNoLSPTools(t *testing.T) {
	c := assert.NewCollecting(t)
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "lsp.json")
	c.Require().NoError(os.WriteFile(cfgPath, []byte(`{"servers":{"nope":{"command":"definitely-not-installed-xyzzy"}}}`), 0o600))

	s := NewServer(Options{Root: dir, LSPConfig: cfgPath})
	defer func() { _ = s.Close() }()

	c.False(registryHas(s.reg, "lsp_definition"), "lsp_definition is registered though its language server is not installed")
}

func registryHas(r *tools.Registry, name string) bool {
	for _, def := range r.Definitions() {
		if def.OfTool != nil && def.OfTool.Name == name {
			return true
		}
	}
	return false
}

// Describe advertises servedTools(), which must be exactly what Execute can
// run: the file tools, minus anything the executor does not host. Parent-side
// RPCs are never in the registry, and lsp_* is absent with NoLSP set.
func TestServedToolsReflectsTheRegistry(t *testing.T) {
	c := assert.NewCollecting(t)
	s := NewServer(Options{Root: t.TempDir(), NoLSP: true})
	defer func() { _ = s.Close() }()

	got := map[string]bool{}
	for _, name := range s.servedTools() {
		got[name] = true
	}
	for _, name := range []string{"read", "write", "edit", "glob", "grep", "ls", "bash"} {
		c.False(!got[name], "servedTools omits %q, which the executor always serves", name)
	}
	for _, name := range []string{"lsp_definition", "lsp_diagnostics", "lsp_rename", "bash_start", "bash_output", "bash_kill"} {
		c.False(got[name], "servedTools claims %q, which the executor's registry does not contain", name)
	}
}
