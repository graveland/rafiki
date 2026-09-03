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

func TestDarajaServeRequiresSocketAndBinary(t *testing.T) {
	cmd := newDarajaCmd()
	serve := cmd.Commands()
	if len(serve) == 0 {
		t.Fatal("daraja has no subcommands; expected `serve`")
	}
	for _, flag := range []string{"socket", "binary", "cwd", "kind", "model", "resume", "permission-mode"} {
		if serve[0].Flags().Lookup(flag) == nil {
			t.Errorf("daraja serve is missing the --%s flag", flag)
		}
	}
}
