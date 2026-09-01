package main

import (
	"testing"
)

func TestKillCmd_NoCloseFlag(t *testing.T) {
	cmd := newKillCmd()

	// --no-close must be parseable and default to false.
	noClose, err := cmd.Flags().GetBool("no-close")
	if err != nil {
		t.Fatalf("--no-close flag not registered: %v", err)
	}
	if noClose {
		t.Error("--no-close default should be false")
	}

	// Setting the flag must be accepted.
	if err := cmd.Flags().Set("no-close", "true"); err != nil {
		t.Fatalf("could not set --no-close: %v", err)
	}
	noClose, _ = cmd.Flags().GetBool("no-close")
	if !noClose {
		t.Error("--no-close should be true after Set")
	}
}
