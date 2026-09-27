package paths

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/multigres/testkit/assert"
)

func TestDaemonIDPrefersEnv(t *testing.T) {
	t.Setenv(DaemonIDVar, "from-env")
	c := assert.NewCollecting(t)
	id, source, err := DaemonID()
	c.Require().NoError(err, "DaemonID")
	c.Eq("from-env", id, "id")
	c.Eq("env", source, "source")
}

func TestDaemonIDGeneratesAndPersists(t *testing.T) {
	c := assert.NewCollecting(t)
	dir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dir)
	t.Setenv(DaemonIDVar, "")

	first, source, err := DaemonID()
	c.Require().NoError(err, "first DaemonID")
	c.Require().NotEq("", first, "first DaemonID returned an empty id")
	c.NotEq("env", source, "source")

	second, _, err := DaemonID()
	c.Require().NoError(err, "second DaemonID")
	c.Eq(first, second, "id changed across calls")
}

func TestDaemonIDIgnoresBlankFile(t *testing.T) {
	c := assert.NewCollecting(t)
	dir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dir)
	t.Setenv(DaemonIDVar, "")

	c.Require().NoError(os.MkdirAll(DataDir(), 0o700), "mkdir")
	c.Require().NoError(os.WriteFile(DaemonIDFile(), []byte("   \n"), 0o600), "write blank file")

	id, _, err := DaemonID()
	c.Require().NoError(err, "DaemonID")
	c.Require().NotEq("", id, "a whitespace-only file must be replaced, not returned")
	raw, err := os.ReadFile(DaemonIDFile())
	c.Require().NoError(err, "read back")
	c.Eq(id, string(raw), "file holds")
}

var _ = filepath.Join
