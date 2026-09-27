// SPDX-License-Identifier: Apache-2.0

package main

import (
	"strings"
	"testing"

	"github.com/multigres/testkit/assert"
)

func TestAttachCmdShape(t *testing.T) {
	c := assert.NewAborting(t)
	cmd := newAttachCmd()
	c.Eq("attach [id|name]", cmd.Use, "Use")
	// The exit-behaviour flags TestCLI_AttachHelp asserts on.
	for _, flag := range []string{"kill-on-exit", "keep-on-exit"} {
		c.NotNil(cmd.Flags().Lookup(flag), "attach is missing the --%s flag", flag)
	}
	// Declaring them is not enough. Before C1b, attach declared both and read
	// neither -- create read them, attach did not -- and this test asserted only
	// that they existed, which is how a flag that did nothing survived review.
	c.False(!attachReadsExitFlags, "attach must READ --kill-on-exit/--keep-on-exit, not merely declare them")
}

// C1b makes bare `rafiki attach` the cockpit entry point: it opens over every
// child the caller can see, with nothing focused.
func TestAttachAcceptsZeroArgs(t *testing.T) {
	cmd := newAttachCmd()
	assert.NewAborting(t).NoError(cmd.Args(cmd, []string{}), "bare `rafiki attach` must be accepted")
}

func TestAttachRejectsTwoArgs(t *testing.T) {
	cmd := newAttachCmd()
	assert.NewAborting(t).Error(cmd.Args(cmd, []string{"c_1", "c_2"}), "want an error for two arguments")
}

func TestSubjectForBareAttachIsAll(t *testing.T) {
	s := subjectFor("")
	assert.NewAborting(t).True(s.GetAll(), "bare attach subject = %+v, want all", s)
}

func TestSubjectForAChildIsSubtreePlusSelf(t *testing.T) {
	c := assert.NewCollecting(t)
	s := subjectFor("c_1")
	c.Require().Eq("c_1", s.GetSubtree(), "subject = %+v, want subtree c_1", s)
	if !s.GetIncludeSelf() {
		t.Fatal("attach <id> must set include_self: ScopeSubtree never includes the root, " +
			"so without it the attached child's own rail row freezes the moment you hop off")
	}
	c.Eq(0, s.GetMaxDepth(), "max_depth")
}

func TestAttachAcceptsOneArg(t *testing.T) {
	cmd := newAttachCmd()
	assert.NewAborting(t).NoError(cmd.Args(cmd, []string{"c_01ABC"}), "want one argument accepted, got")
}

func TestAttachIsRegistered(t *testing.T) {
	root := newRootCmd()
	var found bool
	for _, c := range root.Commands() {
		if strings.HasPrefix(c.Use, "attach") {
			found = true
		}
	}
	assert.NewAborting(t).True(found, "attach is not registered on the root command")
}

// `tui` was a B2 placeholder. The verbs are create and attach; no alias.
func TestTUIVerbIsGone(t *testing.T) {
	root := newRootCmd()
	for _, c := range root.Commands() {
		if strings.HasPrefix(c.Use, "tui") {
			t.Fatalf("the tui verb is still registered: %q", c.Use)
		}
		for _, a := range c.Aliases {
			assert.NewAborting(t).NotEq("tui", a, "%q still aliases tui", c.Use)
		}
	}
}
