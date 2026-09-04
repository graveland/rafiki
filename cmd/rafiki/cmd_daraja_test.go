package main

import (
	"strings"
	"testing"

	"github.com/spf13/cobra"
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
	var serve *cobra.Command
	for _, c := range cmd.Commands() {
		if c.Use == "serve" {
			serve = c
			break
		}
	}
	if serve == nil {
		t.Fatal("darja has no `serve` subcommand")
	}
	for _, flag := range []string{"connect-socket", "binary", "child-id"} {
		if serve.Flags().Lookup(flag) == nil {
			t.Errorf("daraja serve is missing the --%s flag", flag)
		}
	}
	// The old --socket flag must be gone.
	if serve.Flags().Lookup("socket") != nil {
		t.Error("daraja serve still has the deprecated --socket flag")
	}
}
