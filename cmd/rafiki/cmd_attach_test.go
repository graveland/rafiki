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
	// Declaring them is not enough. Before C1b, attach declared both and read
	// neither -- create read them, attach did not -- and this test asserted only
	// that they existed, which is how a flag that did nothing survived review.
	if !attachReadsExitFlags {
		t.Fatal("attach must READ --kill-on-exit/--keep-on-exit, not merely declare them")
	}
}

// C1b makes bare `rafiki attach` the cockpit entry point: it opens over every
// child the caller can see, with nothing focused.
func TestAttachAcceptsZeroArgs(t *testing.T) {
	cmd := newAttachCmd()
	if err := cmd.Args(cmd, []string{}); err != nil {
		t.Fatalf("bare `rafiki attach` must be accepted: %v", err)
	}
}

func TestAttachRejectsTwoArgs(t *testing.T) {
	cmd := newAttachCmd()
	if err := cmd.Args(cmd, []string{"c_1", "c_2"}); err == nil {
		t.Fatal("want an error for two arguments")
	}
}

func TestSubjectForBareAttachIsAll(t *testing.T) {
	if s := subjectFor(""); !s.GetAll() {
		t.Fatalf("bare attach subject = %+v, want all", s)
	}
}

func TestSubjectForAChildIsSubtreePlusSelf(t *testing.T) {
	s := subjectFor("c_1")
	if s.GetSubtree() != "c_1" {
		t.Fatalf("subject = %+v, want subtree c_1", s)
	}
	if !s.GetIncludeSelf() {
		t.Fatal("attach <id> must set include_self: ScopeSubtree never includes the root, " +
			"so without it the attached child's own rail row freezes the moment you hop off")
	}
	if s.GetMaxDepth() != 0 {
		t.Errorf("max_depth = %d, want 0 (UNLIMITED) -- a watcher wants a complete model",
			s.GetMaxDepth())
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
