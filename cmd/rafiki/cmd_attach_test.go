// SPDX-License-Identifier: Apache-2.0

package main

import (
	"strings"
	"testing"
)

func TestAttachCmdShape(t *testing.T) {
	cmd := newAttachCmd()
	if cmd.Use != "attach [id|name]" {
		t.Fatalf("Use = %q, want %q", cmd.Use, "attach [id|name]")
	}
	// The exit-behaviour flags TestCLI_AttachHelp asserts on.
	for _, flag := range []string{"kill-on-exit", "keep-on-exit"} {
		if cmd.Flags().Lookup(flag) == nil {
			t.Fatalf("attach is missing the --%s flag", flag)
		}
	}
}

// Phase C0 requires a child argument. Bare `rafiki attach` becomes the cockpit
// entry point in C1; until then it must say so rather than guessing.
func TestAttachWithNoArgsIsAnError(t *testing.T) {
	cmd := newAttachCmd()
	if err := cmd.Args(cmd, []string{}); err == nil {
		t.Fatal("want an error for `rafiki attach` with no arguments")
	}
}

func TestAttachAcceptsOneArg(t *testing.T) {
	cmd := newAttachCmd()
	if err := cmd.Args(cmd, []string{"c_01ABC"}); err != nil {
		t.Fatalf("want one argument accepted, got: %v", err)
	}
}

func TestAttachIsRegistered(t *testing.T) {
	root := newRootCmd()
	var found bool
	for _, c := range root.Commands() {
		if strings.HasPrefix(c.Use, "attach") {
			found = true
		}
	}
	if !found {
		t.Fatal("attach is not registered on the root command")
	}
}
// `tui` was a B2 placeholder. The verbs are create and attach; no alias.
func TestTUIVerbIsGone(t *testing.T) {
	root := newRootCmd()
	for _, c := range root.Commands() {
		if strings.HasPrefix(c.Use, "tui") {
			t.Fatalf("the tui verb is still registered: %q", c.Use)
		}
		for _, a := range c.Aliases {
			if a == "tui" {
				t.Fatalf("%q still aliases tui", c.Use)
			}
		}
	}
}
