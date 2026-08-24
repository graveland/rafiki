package paths

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMachineNamePrefersTheEnvVar(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Setenv(ExecutorName, "pod-7")
	if err := SetMachineName("laptop"); err != nil {
		t.Fatal(err)
	}
	name, source, err := MachineName()
	if err != nil {
		t.Fatal(err)
	}
	if name != "pod-7" {
		t.Fatalf("name = %q, want the env var to win over the file (a container "+
			"gets its name from the downward API, not from a writable data dir)", name)
	}
	if source != "env" {
		t.Fatalf("source = %q, want %q", source, "env")
	}
}

func TestMachineNameFallsBackToTheFile(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	if err := SetMachineName("laptop"); err != nil {
		t.Fatal(err)
	}
	name, source, err := MachineName()
	if err != nil {
		t.Fatal(err)
	}
	if name != "laptop" {
		t.Fatalf("name = %q, want %q", name, "laptop")
	}
	if source != MachineNameFile() {
		t.Fatalf("source = %q, want the file path", source)
	}
}

// No name is an ANSWER, not a default. A hostname fallback is what the deleted
// machine id existed to escape: on darwin it changes with the active network
// interface, so switching networks mid-session orphaned the running workspace.
func TestMachineNameWithNothingSetIsAnError(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	_, _, err := MachineName()
	if !errors.Is(err, ErrNoMachineName) {
		t.Fatalf("err = %v, want ErrNoMachineName", err)
	}
	if !strings.Contains(err.Error(), "rafiki executor name") ||
		!strings.Contains(err.Error(), ExecutorName) {
		t.Fatalf("the error must name BOTH ways to set it, got: %v", err)
	}
}

func TestMachineNameTreatsABlankFileAsUnset(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	if err := os.MkdirAll(filepath.Dir(MachineNameFile()), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(MachineNameFile(), []byte("   \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := MachineName(); !errors.Is(err, ErrNoMachineName) {
		t.Fatalf("a whitespace-only file is corruption, not a name: %v", err)
	}
}

func TestSetMachineNameWritesAtomicallyAndNotWorldReadable(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	if err := SetMachineName("laptop"); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(MachineNameFile())
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm()&0o077 != 0 {
		t.Fatalf("mode = %v, want no group/other bits", fi.Mode().Perm())
	}
	entries, err := os.ReadDir(filepath.Dir(MachineNameFile()))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".executor-name") {
			t.Fatalf("temp file %q left behind; the rename did not complete", e.Name())
		}
	}
}

func TestValidateMachineNameRejectsWhatASelectorCannotCarry(t *testing.T) {
	for _, bad := range []string{"", "has space", "has,comma", "has=equals", strings.Repeat("x", 64)} {
		if err := ValidateMachineName(bad); err == nil {
			t.Errorf("ValidateMachineName(%q) = nil, want an error", bad)
		}
	}
	for _, ok := range []string{"laptop", "build-01", "pod_7", "a"} {
		if err := ValidateMachineName(ok); err != nil {
			t.Errorf("ValidateMachineName(%q) = %v, want nil", ok, err)
		}
	}
}
