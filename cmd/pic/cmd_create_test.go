package main

import (
	"os"
	"testing"

	"github.com/spf13/cobra"
)

// newTestCreateCmd returns a cobra.Command with spawn flags registered, suitable
// for use in buildSpawnRequest unit tests.
func newTestCreateCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "create"}
	addSpawnFlags(cmd)
	return cmd
}

func TestBuildSpawnRequest_ExplicitCwd(t *testing.T) {
	cmd := newTestCreateCmd()
	if err := cmd.Flags().Set("cwd", "/explicit/path"); err != nil {
		t.Fatal(err)
	}

	req, err := buildSpawnRequest(cmd, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if req.Cwd != "/explicit/path" {
		t.Errorf("Cwd = %q, want /explicit/path", req.Cwd)
	}
}

func TestBuildSpawnRequest_DefaultCwd(t *testing.T) {
	// When --cwd is omitted, buildSpawnRequest should use os.Getwd().
	wantCwd, err := os.Getwd()
	if err != nil {
		t.Skip("os.Getwd() failed — skipping:", err)
	}

	cmd := newTestCreateCmd()
	// cwd left at its zero value ("") intentionally.

	req, err := buildSpawnRequest(cmd, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if req.Cwd != wantCwd {
		t.Errorf("Cwd = %q, want %q", req.Cwd, wantCwd)
	}
}

func TestBuildSpawnRequest_RelativeCwdRejected(t *testing.T) {
	cmd := newTestCreateCmd()
	if err := cmd.Flags().Set("cwd", "relative/path"); err != nil {
		t.Fatal(err)
	}

	_, err := buildSpawnRequest(cmd, nil)
	if err == nil {
		t.Fatal("expected error for relative --cwd, got nil")
	}
}

func TestBuildSpawnRequest_NameFromArgs(t *testing.T) {
	cmd := newTestCreateCmd()
	if err := cmd.Flags().Set("cwd", "/tmp"); err != nil {
		t.Fatal(err)
	}

	req, err := buildSpawnRequest(cmd, []string{"my-session"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if req.Name != "my-session" {
		t.Errorf("Name = %q, want my-session", req.Name)
	}
}
