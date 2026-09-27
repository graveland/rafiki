package main

import (
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/multigres/testkit/assert"
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
	ck := assert.NewCollecting(t)
	cmd := newDarajaCmd()
	var serve *cobra.Command
	for _, c := range cmd.Commands() {
		if c.Use == "serve" {
			serve = c
			break
		}
	}
	ck.Require().NotNil(serve, "darja has no `serve` subcommand")
	for _, flag := range []string{"connect-socket", "binary", "child-id", "append-system-prompt"} {
		ck.NotNil(serve.Flags().Lookup(flag), "daraja serve is missing the --%s flag", flag)
	}
	// The old --socket flag must be gone.
	ck.Nil(serve.Flags().Lookup("socket"), "daraja serve still has the deprecated --socket flag")
}

// The executor's Launch appends a bare "--" before the spec's ExtraArgs (see
// TestLaunchCarriesAppendSystemPromptAndExtraArgs). pflag must treat that
// separator as end-of-flags and hand the rest back as POSITIONAL args — a
// flag-shaped extra parsed as a serve flag would mangle the flags Launch maps
// or die on an unknown one, which is exactly what the separator exists to
// prevent. pflag (v1.0.9) drops the separator itself, so Flags().Args() is
// exactly the positionals.
func TestDarajaServeDropsTheSeparatorAndKeepsThePositionals(t *testing.T) {
	c := assert.NewAborting(t)
	cmd := newDarajaServeCmd()
	c.NoError(cmd.ParseFlags([]string{"--", "--foo", "bar"}), "ParseFlags")
	got := cmd.Flags().Args()
	c.EqDiff([]string{"--foo", "bar"}, got, "Flags().Args()")
}
