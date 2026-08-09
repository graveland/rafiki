// SPDX-License-Identifier: Apache-2.0

package main

import (
	"reflect"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func TestClaudeCmd_FlagDefaults(t *testing.T) {
	// Isolate from any RAFIKI_* set in the developer's shell/.env.
	t.Setenv("RAFIKI_URL", "")
	t.Setenv("RAFIKI_TOKEN", "")
	t.Setenv("RAFIKI_MODEL", "")
	t.Setenv("RAFIKI_SESSION", "")

	cmd := newClaudeCmd()

	tests := []struct {
		flag string
		want string
	}{
		{"url", "http://localhost:8035"},
		{"token", "dev"},
		{"model", ""},
		{"session", ""},
	}
	for _, tt := range tests {
		got, err := cmd.Flags().GetString(tt.flag)
		if err != nil {
			t.Fatalf("--%s not registered: %v", tt.flag, err)
		}
		if got != tt.want {
			t.Errorf("--%s default = %q, want %q", tt.flag, got, tt.want)
		}
	}
}

func TestClaudeCmd_FlagDefaultsFromEnv(t *testing.T) {
	t.Setenv("RAFIKI_URL", "http://example:9000")
	t.Setenv("RAFIKI_TOKEN", "secret")
	t.Setenv("RAFIKI_MODEL", "glm-5.2")
	t.Setenv("RAFIKI_SESSION", "sess-123")

	cmd := newClaudeCmd()

	want := map[string]string{
		"url":     "http://example:9000",
		"token":   "secret",
		"model":   "glm-5.2",
		"session": "sess-123",
	}
	for flag, wantVal := range want {
		got, _ := cmd.Flags().GetString(flag)
		if got != wantVal {
			t.Errorf("--%s default = %q, want %q (from env)", flag, got, wantVal)
		}
	}
}

// pflag treats a bare "--" as the flag/arg terminator (unlike stdlib flag,
// which the old FlagSet also honored the same way), so everything after it
// must reach RunE as positional args untouched. This is what lets
// `rafiki claude --model foo -- --permission-mode plan` forward
// --permission-mode straight to the claude binary instead of rafiki trying
// to parse it as its own flag.
func TestClaudeCmd_DashDashPassesArgsThrough(t *testing.T) {
	cmd := newClaudeCmd()

	var gotArgs []string
	cmd.RunE = func(_ *cobra.Command, args []string) error {
		gotArgs = args
		return nil
	}
	cmd.SetArgs([]string{"--model", "foo", "--", "--permission-mode", "plan"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if model, _ := cmd.Flags().GetString("model"); model != "foo" {
		t.Errorf("--model = %q, want foo", model)
	}
	want := []string{"--permission-mode", "plan"}
	if !reflect.DeepEqual(gotArgs, want) {
		t.Errorf("args passed to RunE = %v, want %v", gotArgs, want)
	}
}

func TestRunClaude_EmptyURLIsError(t *testing.T) {
	cmd := newClaudeCmd()
	if err := cmd.Flags().Set("url", ""); err != nil {
		t.Fatal(err)
	}

	err := runClaude(cmd, nil)
	if err == nil {
		t.Fatal("expected error for empty --url, got nil")
	}
	if !strings.Contains(err.Error(), "--url") {
		t.Errorf("error = %v, want it to mention --url", err)
	}
}
