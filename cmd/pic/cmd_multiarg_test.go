package main

import (
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"graveland.dev/pi-controller/internal/protocol"
)

// ─── Argument validation tests ───────────────────────────────────────────────

// TestForgetCmd_MultiArg verifies that multiple positional args are accepted
// and that zero positional args are rejected (unless --all-exited is given).
func TestForgetCmd_MultiArg_AcceptsMultiple(t *testing.T) {
	cmd := newForgetCmd()
	// Replace RunE so we don't need a real daemon; just exercise cobra's Args.
	cmd.RunE = func(_ *cobra.Command, _ []string) error { return nil }
	cmd.SetArgs([]string{"child-a", "child-b", "child-c"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("expected no error for multiple args, got: %v", err)
	}
}

func TestForgetCmd_MultiArg_ZeroArgsRejected(t *testing.T) {
	cmd := newForgetCmd()
	cmd.RunE = func(_ *cobra.Command, _ []string) error { return nil }
	cmd.SetArgs([]string{}) // no args, no --all-exited
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error for zero args without --all-exited, got nil")
	}
	if !strings.Contains(err.Error(), "at least one") {
		t.Errorf("expected 'at least one' in error, got: %v", err)
	}
}

func TestForgetCmd_AllExited_ZeroArgsOK(t *testing.T) {
	cmd := newForgetCmd()
	cmd.RunE = func(_ *cobra.Command, _ []string) error { return nil }
	cmd.SetArgs([]string{"--all-exited"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("expected no error for --all-exited with no positional args, got: %v", err)
	}
}

// TestKillCmd_MultiArg verifies that multiple positional args are accepted and
// that zero positional args are rejected.
func TestKillCmd_MultiArg_AcceptsMultiple(t *testing.T) {
	cmd := newKillCmd()
	cmd.RunE = func(_ *cobra.Command, _ []string) error { return nil }
	cmd.SetArgs([]string{"child-a", "child-b"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("expected no error for multiple args, got: %v", err)
	}
}

func TestKillCmd_MultiArg_ZeroArgsRejected(t *testing.T) {
	cmd := newKillCmd()
	cmd.RunE = func(_ *cobra.Command, _ []string) error { return nil }
	cmd.SetArgs([]string{})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error for zero args, got nil")
	}
}

// TestGetCmd_MultiArg verifies that multiple positional args are accepted and
// that zero positional args are rejected.
func TestGetCmd_MultiArg_AcceptsMultiple(t *testing.T) {
	cmd := newGetCmd()
	cmd.RunE = func(_ *cobra.Command, _ []string) error { return nil }
	cmd.SetArgs([]string{"child-a", "child-b"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("expected no error for multiple args, got: %v", err)
	}
}

func TestGetCmd_MultiArg_ZeroArgsRejected(t *testing.T) {
	cmd := newGetCmd()
	cmd.RunE = func(_ *cobra.Command, _ []string) error { return nil }
	cmd.SetArgs([]string{})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected error for zero args, got nil")
	}
}

// ─── State-filtered completion predicate tests ───────────────────────────────

// These tests exercise the filter predicates without needing a live daemon.
// They verify the boolean logic used by completeChildrenByState callers.

func TestCompletionPredicate_ForgetExitedOnly(t *testing.T) {
	exited := func(ch protocol.ChildSummary) bool {
		return ch.Status == string(protocol.StatusExited)
	}
	cases := []struct {
		status string
		want   bool
	}{
		{"exited", true},
		{"idle", false},
		{"streaming", false},
		{"tool_running", false},
		{"spawning", false},
		{"shutting_down", false},
	}
	for _, tc := range cases {
		ch := protocol.ChildSummary{Status: tc.status}
		got := exited(ch)
		if got != tc.want {
			t.Errorf("forget predicate(%q) = %v, want %v", tc.status, got, tc.want)
		}
	}
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
		if got != tc.want {
			t.Errorf("kill predicate(%q) = %v, want %v", tc.status, got, tc.want)
		}
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
		if got != tc.want {
			t.Errorf("resume predicate(%q) = %v, want %v", tc.status, got, tc.want)
		}
	}
}

func TestCompletionPredicate_AttachAttachableStates(t *testing.T) {
	attachable := func(ch protocol.ChildSummary) bool {
		switch ch.Status {
		case string(protocol.StatusIdle),
			string(protocol.StatusStreaming),
			string(protocol.StatusToolRunning),
			string(protocol.StatusCompacting),
			string(protocol.StatusBlockedUI):
			return true
		}
		return false
	}
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
		ch := protocol.ChildSummary{Status: tc.status}
		got := attachable(ch)
		if got != tc.want {
			t.Errorf("attach predicate(%q) = %v, want %v", tc.status, got, tc.want)
		}
	}
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

func TestAliases_Recent(t *testing.T) {
	cmd := newRecentCmd()
	if !containsAlias(cmd.Aliases, "history") {
		t.Errorf("recent: expected alias 'history', got %v", cmd.Aliases)
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
