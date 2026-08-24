package paths

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMachineIDIsStableAcrossCalls(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	first, err := MachineID()
	if err != nil {
		t.Fatal(err)
	}
	second, err := MachineID()
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("machine id must be stable: %q then %q", first, second)
	}
	if strings.TrimSpace(first) == "" {
		t.Fatal("machine id must not be empty")
	}
}

func TestMachineIDPersistsToDisk(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dir)

	id, err := MachineID()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(MachineIDFile())
	if err != nil {
		t.Fatalf("the id must survive a process restart: %v", err)
	}
	if strings.TrimSpace(string(raw)) != id {
		t.Fatalf("file holds %q, MachineID returned %q", strings.TrimSpace(string(raw)), id)
	}
}

func TestMachineIDFileIsNotWorldReadable(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	if _, err := MachineID(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(MachineIDFile())
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("want mode 0600, got %o", perm)
	}
}

func TestMachineIDRejectsGarbage(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dir)
	if err := os.MkdirAll(filepath.Dir(MachineIDFile()), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(MachineIDFile(), []byte("   \n"), 0o600); err != nil {
		t.Fatal(err)
	}

	id, err := MachineID()
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(id) == "" {
		t.Fatal("a blank file must be replaced with a fresh id, not returned as-is")
	}
}
