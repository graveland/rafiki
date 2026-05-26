package main

import (
	"os"
	"strings"
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

// ─── Mutual-exclusivity tests ─────────────────────────────────────────────────

// executeWithFlags runs cmd with the given flag args and returns any error.
// The RunE function is replaced with a no-op so the test only exercises Cobra's
// flag validation (mutual-exclusivity checks) without executing real business
// logic.
func executeWithFlags(cmd *cobra.Command, flagArgs ...string) error {
	// Replace RunE so we don't need a real daemon.
	cmd.RunE = func(_ *cobra.Command, _ []string) error { return nil }
	cmd.SetArgs(flagArgs)
	return cmd.Execute()
}

func TestCreateCmd_KillAndKeepAreMutuallyExclusive(t *testing.T) {
	cmd := newCreateCmd()
	err := executeWithFlags(cmd, "--kill-on-exit", "--keep-on-exit")
	if err == nil {
		t.Fatal("expected error when both --kill-on-exit and --keep-on-exit are set, got nil")
	}
	// Cobra's message contains "if any flags in the group" when mutual exclusion fires.
	if !strings.Contains(err.Error(), "kill-on-exit") || !strings.Contains(err.Error(), "keep-on-exit") {
		t.Errorf("expected flag names in error, got: %v", err)
	}
}

func TestCreateCmd_KillOnExitAlone_OK(t *testing.T) {
	cmd := newCreateCmd()
	if err := executeWithFlags(cmd, "--kill-on-exit"); err != nil {
		t.Errorf("unexpected error with only --kill-on-exit: %v", err)
	}
}

func TestCreateCmd_KeepOnExitAlone_OK(t *testing.T) {
	cmd := newCreateCmd()
	if err := executeWithFlags(cmd, "--keep-on-exit"); err != nil {
		t.Errorf("unexpected error with only --keep-on-exit: %v", err)
	}
}

func TestAttachCmd_KillAndKeepAreMutuallyExclusive(t *testing.T) {
	cmd := newAttachCmd()
	err := executeWithFlags(cmd, "--kill-on-exit", "--keep-on-exit")
	if err == nil {
		t.Fatal("expected error when both --kill-on-exit and --keep-on-exit are set, got nil")
	}
	// Cobra's message contains "if any flags in the group" when mutual exclusion fires.
	if !strings.Contains(err.Error(), "kill-on-exit") || !strings.Contains(err.Error(), "keep-on-exit") {
		t.Errorf("expected flag names in error, got: %v", err)
	}
}

func TestAttachCmd_KillOnExitAlone_OK(t *testing.T) {
	cmd := newAttachCmd()
	if err := executeWithFlags(cmd, "--kill-on-exit"); err != nil {
		t.Errorf("unexpected error with only --kill-on-exit: %v", err)
	}
}

func TestAttachCmd_KeepOnExitAlone_OK(t *testing.T) {
	cmd := newAttachCmd()
	if err := executeWithFlags(cmd, "--keep-on-exit"); err != nil {
		t.Errorf("unexpected error with only --keep-on-exit: %v", err)
	}
}
