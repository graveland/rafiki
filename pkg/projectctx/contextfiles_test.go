package projectctx

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/multigres/testkit/assert"
)

// The project tier is CLAUDE.md and AGENTS.md at the git root and at cwd. The
// user's global instructions file is NOT part of it: that belongs to whoever
// runs the agent loop, which is a different machine from the workspace.
func TestLoadProjectContextReadsCwdFiles(t *testing.T) {
	c := assert.NewCollecting(t)
	dir := t.TempDir()
	c.Require().NoError(os.WriteFile(filepath.Join(dir, "CLAUDE.md"), []byte("project rules"), 0o600))

	got, err := LoadProjectContext(dir)
	c.Require().NoError(err, "LoadProjectContext")
	c.StrContains(got, "project rules", "got")
}

// A directory with neither file is the common case and must not be an error.
func TestLoadProjectContextEmptyIsNotAnError(t *testing.T) {
	c := assert.NewCollecting(t)
	got, err := LoadProjectContext(t.TempDir())
	c.Require().NoError(err, "LoadProjectContext")
	c.Eq("", got, "got")
}

// Includes are expanded relative to the including file, on this side of the
// boundary. They must not travel to the daemon as unexpanded @refs: the daemon
// cannot resolve a path on the executor's filesystem.
func TestLoadProjectContextExpandsIncludes(t *testing.T) {
	c := assert.NewCollecting(t)
	dir := t.TempDir()
	c.Require().NoError(os.WriteFile(filepath.Join(dir, "extra.md"), []byte("included body"), 0o600))
	c.Require().NoError(os.WriteFile(filepath.Join(dir, "CLAUDE.md"), []byte("head\n@extra.md\n"), 0o600))

	got, err := LoadProjectContext(dir)
	c.Require().NoError(err, "LoadProjectContext")
	c.StrContains(got, "included body", "got")
}
