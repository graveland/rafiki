package main

import (
	"slices"
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
	for _, flag := range []string{"connect-socket", "binary", "child-id", "append-system-prompt"} {
		if serve.Flags().Lookup(flag) == nil {
			t.Errorf("daraja serve is missing the --%s flag", flag)
		}
	}
	// The old --socket flag must be gone.
	if serve.Flags().Lookup("socket") != nil {
		t.Error("daraja serve still has the deprecated --socket flag")
	}
}

// The executor's Launch appends a bare "--" before the spec's ExtraArgs (see
// TestLaunchCarriesAppendSystemPromptAndExtraArgs). pflag must treat that
// separator as end-of-flags and hand the rest back as POSITIONAL args — a
// flag-shaped extra parsed as a serve flag would mangle the flags Launch maps
// or die on an unknown one, which is exactly what the separator exists to
// prevent. pflag (v1.0.9) drops the separator itself, so Flags().Args() is
// exactly the positionals.
func TestDarajaServeDropsTheSeparatorAndKeepsThePositionals(t *testing.T) {
	cmd := newDarajaServeCmd()
	if err := cmd.ParseFlags([]string{"--", "--foo", "bar"}); err != nil {
		t.Fatalf("ParseFlags: %v", err)
	}
	got := cmd.Flags().Args()
	if !slices.Equal(got, []string{"--foo", "bar"}) {
		t.Fatalf("Flags().Args() = %v, want exactly [--foo bar]", got)
	}
}
