package main

import (
	"testing"

	"github.com/spf13/cobra"
	"go.graveland.dev/rafiki/pkg/protocol"

	"github.com/multigres/testkit/assert"
)

// ─── Argument validation tests ───────────────────────────────────────────────

// TestCloseCmd_MultiArg verifies that multiple positional args are accepted
// and that zero positional args are rejected (unless --all-exited is given).
func TestCloseCmd_MultiArg_AcceptsMultiple(t *testing.T) {
	cmd := newCloseCmd()
	// Replace RunE so we don't need a real daemon; just exercise cobra's Args.
	cmd.RunE = func(_ *cobra.Command, _ []string) error { return nil }
	cmd.SetArgs([]string{"child-a", "child-b", "child-c"})
	assert.NewAborting(t).NoError(cmd.Execute(), "expected no error for multiple args, got")
}

func TestCloseCmd_MultiArg_ZeroArgsRejected(t *testing.T) {
	c := assert.NewCollecting(t)
	cmd := newCloseCmd()
	cmd.RunE = func(_ *cobra.Command, _ []string) error { return nil }
	cmd.SetArgs([]string{}) // no args, no --all-exited
	err := cmd.Execute()
	c.Require().Error(err, "expected error for zero args without --all-exited, got nil")
	c.StrContains(err.Error(), "at least one", "expected 'at least one' in error, got: %v", err)
}

func TestCloseCmd_AllExited_ZeroArgsOK(t *testing.T) {
	cmd := newCloseCmd()
	cmd.RunE = func(_ *cobra.Command, _ []string) error { return nil }
	cmd.SetArgs([]string{"--all-exited"})
	assert.NewAborting(t).NoError(cmd.Execute(), "expected no error for --all-exited with no positional args, got")
}

// TestStopCmd_MultiArg verifies that multiple positional args are accepted and
// that zero positional args are rejected.
func TestStopCmd_MultiArg_AcceptsMultiple(t *testing.T) {
	cmd := newStopCmd()
	cmd.RunE = func(_ *cobra.Command, _ []string) error { return nil }
	cmd.SetArgs([]string{"child-a", "child-b"})
	assert.NewAborting(t).NoError(cmd.Execute(), "expected no error for multiple args, got")
}

func TestStopCmd_MultiArg_ZeroArgsRejected(t *testing.T) {
	cmd := newStopCmd()
	cmd.RunE = func(_ *cobra.Command, _ []string) error { return nil }
	cmd.SetArgs([]string{})
	err := cmd.Execute()
	assert.NewAborting(t).Error(err, "expected error for zero args, got nil")
}

// TestGetCmd_MultiArg verifies that multiple positional args are accepted and
// that zero positional args are rejected.
func TestGetCmd_MultiArg_AcceptsMultiple(t *testing.T) {
	cmd := newGetCmd()
	cmd.RunE = func(_ *cobra.Command, _ []string) error { return nil }
	cmd.SetArgs([]string{"child-a", "child-b"})
	assert.NewAborting(t).NoError(cmd.Execute(), "expected no error for multiple args, got")
}

func TestGetCmd_MultiArg_ZeroArgsRejected(t *testing.T) {
	cmd := newGetCmd()
	cmd.RunE = func(_ *cobra.Command, _ []string) error { return nil }
	cmd.SetArgs([]string{})
	err := cmd.Execute()
	assert.NewAborting(t).Error(err, "expected error for zero args, got nil")
}

// ─── State-filtered completion predicate tests ───────────────────────────────

// These tests exercise the filter predicates without needing a live daemon.
// They verify the boolean logic used by completeChildrenByState callers.

// TestCloseCmd_ValidArgsFunctionUnfiltered pins that close completion offers
// every child, not just exited ones: close stops a live target first, so
// restricting completion to exited children (the pre-split behavior, when
// close could only ever target something already stopped) hides exactly the
// targets `rafiki close` is now meant to handle in one step.
func TestCloseCmd_ValidArgsFunctionUnfiltered(t *testing.T) {
	c := assert.NewCollecting(t)
	cmd := newCloseCmd()
	c.Require().NotNil(cmd.ValidArgsFunction, "ValidArgsFunction not set")
	// completeChildren (unfiltered) is what newCloseCmd wires up now; this
	// smoke-checks it doesn't crash without a daemon rather than re-deriving
	// completeChildren's own behavior, which has its own tests.
	_, directive := cmd.ValidArgsFunction(cmd, nil, "")
	c.Eq(cobra.ShellCompDirectiveNoFileComp, directive, "directive")
}

func TestCompletionPredicate_KillNotExited(t *testing.T) {
	notExited := func(ch protocol.ChildSummary) bool {
		return ch.Status != string(protocol.StatusExited)
	}
	cases := []struct {
		status string
		want   bool
	}{
		{"exited", false},
		{"idle", true},
		{"streaming", true},
		{"tool_running", true},
		{"spawning", true},
		{"shutting_down", true},
		{"blocked_ui", true},
	}
	for _, tc := range cases {
		ch := protocol.ChildSummary{Status: tc.status}
		got := notExited(ch)
		assert.NewCollecting(t).Eq(tc.want, got, "kill predicate(%q) = %v, want", tc.status, got)
	}
}

func TestCompletionPredicate_ResumeExitedOnly(t *testing.T) {
	resumable := func(ch protocol.ChildSummary) bool {
		return ch.Status == string(protocol.StatusExited)
	}
	cases := []struct {
		status string
		want   bool
	}{
		{"exited", true},
		{"idle", false},
		{"streaming", false},
	}
	for _, tc := range cases {
		ch := protocol.ChildSummary{Status: tc.status}
		got := resumable(ch)
		assert.NewCollecting(t).Eq(tc.want, got, "resume predicate(%q) = %v, want", tc.status, got)
	}
}

// TestCompletionPredicate_AttachAttachableStates pins isAttachable itself,
// not a local copy of its logic — newAttachCmd went a while without wiring
// ValidArgsFunction to any predicate at all, and a test against a standalone
// closure passed the whole time without catching that the real command
// offered no completions whatsoever.
func TestCompletionPredicate_AttachAttachableStates(t *testing.T) {
	cases := []struct {
		status string
		want   bool
	}{
		{"idle", true},
		{"streaming", true},
		{"tool_running", true},
		{"compacting", true},
		{"blocked_ui", true},
		{"spawning", false},
		{"shutting_down", false},
		{"exited", false},
	}
	for _, tc := range cases {
		ch := completionChild{Status: tc.status}
		got := isAttachable(ch)
		assert.NewCollecting(t).Eq(tc.want, got, "isAttachable(%q) = %v, want", tc.status, got)
	}
}

// TestAttachCmd_ValidArgsFunctionWired pins that newAttachCmd actually wires
// a ValidArgsFunction — the regression this file's isAttachable test alone
// could not catch, since it never called through the real command.
func TestAttachCmd_ValidArgsFunctionWired(t *testing.T) {
	c := assert.NewCollecting(t)
	cmd := newAttachCmd()
	c.Require().NotNil(cmd.ValidArgsFunction, "ValidArgsFunction not set — `rafiki attach <TAB>` completes nothing")
	_, directive := cmd.ValidArgsFunction(cmd, nil, "")
	c.Eq(cobra.ShellCompDirectiveNoFileComp, directive, "directive")
}

// ─── Aliases ─────────────────────────────────────────────────────────────────

func TestAliases_List(t *testing.T) {
	cmd := newListCmd()
	if !containsAlias(cmd.Aliases, "ls") {
		t.Errorf("list: expected alias 'ls', got %v", cmd.Aliases)
	}
}

func TestAliases_Get(t *testing.T) {
	cmd := newGetCmd()
	for _, alias := range []string{"show", "info"} {
		if !containsAlias(cmd.Aliases, alias) {
			t.Errorf("get: expected alias %q, got %v", alias, cmd.Aliases)
		}
	}
}

func TestAliases_Status(t *testing.T) {
	cmd := newStatusCmd()
	if !containsAlias(cmd.Aliases, "st") {
		t.Errorf("status: expected alias 'st', got %v", cmd.Aliases)
	}
}

func TestAliases_Logs(t *testing.T) {
	cmd := newLogsCmd()
	if !containsAlias(cmd.Aliases, "log") {
		t.Errorf("logs: expected alias 'log', got %v", cmd.Aliases)
	}
}

func TestAliases_Service(t *testing.T) {
	cmd := newServiceCmd()
	if !containsAlias(cmd.Aliases, "svc") {
		t.Errorf("service: expected alias 'svc', got %v", cmd.Aliases)
	}
}

func containsAlias(aliases []string, target string) bool {
	for _, a := range aliases {
		if a == target {
			return true
		}
	}
	return false
}
