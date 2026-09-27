package paths

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/multigres/testkit/assert"
)

func TestMachineNamePrefersTheEnvVar(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Setenv(ExecutorName, "pod-7")
	c := assert.NewAborting(t)
	c.NoError(SetMachineName("laptop"))
	name, source, err := MachineName()
	c.NoError(err)
	c.Eq("pod-7", name, "name = %q, want the env var to win over the file (a container "+
		"gets its name from the downward API, not from a writable data dir)", name)
	c.Eq("env", source, "source")
}

func TestMachineNameFallsBackToTheFile(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	c := assert.NewAborting(t)
	c.NoError(SetMachineName("laptop"))
	name, source, err := MachineName()
	c.NoError(err)
	c.Eq("laptop", name, "name")
	c.Eq(MachineNameFile(), source, "source")
}

// No name is an ANSWER, not a default. A hostname fallback is what the deleted
// machine id existed to escape: on darwin it changes with the active network
// interface, so switching networks mid-session orphaned the running workspace.
func TestMachineNameWithNothingSetIsAnError(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	c := assert.NewAborting(t)
	_, _, err := MachineName()
	c.ErrorIs(err, ErrNoMachineName, "err")
	c.False(!strings.Contains(err.Error(), "rafiki executor name") ||
		!strings.Contains(err.Error(), ExecutorName), "the error must name BOTH ways to set it, got: %v", err)
}

func TestMachineNameTreatsABlankFileAsUnset(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	c := assert.NewAborting(t)
	c.NoError(os.MkdirAll(filepath.Dir(MachineNameFile()), 0o700))
	c.NoError(os.WriteFile(MachineNameFile(), []byte("   \n"), 0o600))
	_, _, err := MachineName()
	c.ErrorIs(err, ErrNoMachineName, "a whitespace-only file is corruption, not a name")
}

func TestSetMachineNameWritesAtomicallyAndNotWorldReadable(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	c := assert.NewAborting(t)
	c.NoError(SetMachineName("laptop"))
	fi, err := os.Stat(MachineNameFile())
	c.NoError(err)
	c.Eq(0, fi.Mode().Perm()&0o077, "mode = %v, want no group/other bits", fi.Mode().Perm())
	entries, err := os.ReadDir(filepath.Dir(MachineNameFile()))
	c.NoError(err)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".executor-name") {
			t.Fatalf("temp file %q left behind; the rename did not complete", e.Name())
		}
	}
}

func TestValidateMachineNameRejectsWhatASelectorCannotCarry(t *testing.T) {
	c := assert.NewCollecting(t)
	for _, bad := range []string{"", "has space", "has,comma", "has=equals", strings.Repeat("x", 64)} {
		c.Error(ValidateMachineName(bad), "ValidateMachineName(%q) = nil, want an error", bad)
	}
	for _, ok := range []string{"laptop", "build-01", "pod_7", "a"} {
		err := ValidateMachineName(ok)
		c.NoError(err, "ValidateMachineName(%q) = %v, want nil", ok, err)
	}
}
