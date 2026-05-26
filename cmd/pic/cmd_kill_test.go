package main

import (
	"testing"
)

func TestKillCmd_NoForgetFlag(t *testing.T) {
	cmd := newKillCmd()

	// --no-forget must be parseable and default to false.
	noForget, err := cmd.Flags().GetBool("no-forget")
	if err != nil {
		t.Fatalf("--no-forget flag not registered: %v", err)
	}
	if noForget {
		t.Error("--no-forget default should be false")
	}

	// Setting the flag must be accepted.
	if err := cmd.Flags().Set("no-forget", "true"); err != nil {
		t.Fatalf("could not set --no-forget: %v", err)
	}
	noForget, _ = cmd.Flags().GetBool("no-forget")
	if !noForget {
		t.Error("--no-forget should be true after Set")
	}
}
