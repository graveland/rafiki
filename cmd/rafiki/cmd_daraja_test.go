package main

import (
	"strings"
	"testing"
)

// daraja is a SUBCOMMAND of rafiki, not a third binary: this repo ships exactly
// two artifacts and cmd/rafiki-executor was deleted to keep it that way.
func TestDarajaIsRegisteredOnRoot(t *testing.T) {
	root := newRootCmd()
	for _, c := range root.Commands() {
		if strings.HasPrefix(c.Use, "daraja") {
			return
		}
	}
	t.Fatal("daraja is not registered on the root command")
}

func TestDarajaServeRequiresConnectAndBinary(t *testing.T) {
	cmd := newDarajaCmd()
	serve := cmd.Commands()
	if len(serve) == 0 {
		t.Fatal("daraja has no subcommands; expected `serve`")
	}
	for _, flag := range []string{"connect-socket", "binary", "child-id"} {
		if serve[0].Flags().Lookup(flag) == nil {
			t.Errorf("daraja serve is missing the --%s flag", flag)
		}
	}
	// The old --socket flag must be gone.
	if serve[0].Flags().Lookup("socket") != nil {
		t.Error("daraja serve still has the deprecated --socket flag")
	}
}
