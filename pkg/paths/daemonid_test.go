package paths

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDaemonIDPrefersEnv(t *testing.T) {
	t.Setenv(DaemonIDVar, "from-env")
	id, source, err := DaemonID()
	if err != nil {
		t.Fatalf("DaemonID: %v", err)
	}
	if id != "from-env" {
		t.Errorf("id = %q, want %q", id, "from-env")
	}
	if source != "env" {
		t.Errorf("source = %q, want %q", source, "env")
	}
}

func TestDaemonIDGeneratesAndPersists(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dir)
	t.Setenv(DaemonIDVar, "")

	first, source, err := DaemonID()
	if err != nil {
		t.Fatalf("first DaemonID: %v", err)
	}
	if first == "" {
		t.Fatal("first DaemonID returned an empty id")
	}
	if source == "env" {
		t.Errorf("source = %q, want the file path", source)
	}

	second, _, err := DaemonID()
	if err != nil {
		t.Fatalf("second DaemonID: %v", err)
	}
	if second != first {
		t.Errorf("id changed across calls: %q then %q", first, second)
	}
}

func TestDaemonIDIgnoresBlankFile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_DATA_HOME", dir)
	t.Setenv(DaemonIDVar, "")

	if err := os.MkdirAll(DataDir(), 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(DaemonIDFile(), []byte("   \n"), 0o600); err != nil {
		t.Fatalf("write blank file: %v", err)
	}

	id, _, err := DaemonID()
	if err != nil {
		t.Fatalf("DaemonID: %v", err)
	}
	if id == "" {
		t.Fatal("a whitespace-only file must be replaced, not returned")
	}
	raw, err := os.ReadFile(DaemonIDFile())
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(raw) != id {
		t.Errorf("file holds %q, want %q", string(raw), id)
	}
}

var _ = filepath.Join
