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

func TestClaudeCmd_PassthroughFlagDefaultsOff(t *testing.T) {
	t.Setenv("RAFIKI_CLAUDE_PASSTHROUGH", "")

	cmd := newClaudeCmd()
	got, err := cmd.Flags().GetBool("passthrough-auth")
	if err != nil {
		t.Fatalf("--passthrough-auth not registered: %v", err)
	}
	if got {
		t.Error("--passthrough-auth default = true, want false: billing must never change by accident")
	}
}

func TestClaudeCmd_PassthroughFlagFromEnv(t *testing.T) {
	t.Setenv("RAFIKI_CLAUDE_PASSTHROUGH", "1")

	cmd := newClaudeCmd()
	got, err := cmd.Flags().GetBool("passthrough-auth")
	if err != nil {
		t.Fatalf("--passthrough-auth not registered: %v", err)
	}
	if !got {
		t.Error("--passthrough-auth default = false with RAFIKI_CLAUDE_PASSTHROUGH=1, want true")
	}
}

// "0" and "false" mean off. Treating any non-empty value as true would make a
// user who exports RAFIKI_CLAUDE_PASSTHROUGH=0 to disable the feature silently
// start billing their personal subscription instead.
func TestClaudeCmd_PassthroughFlagFalseyEnv(t *testing.T) {
	for _, v := range []string{"0", "false", "no"} {
		t.Run(v, func(t *testing.T) {
			t.Setenv("RAFIKI_CLAUDE_PASSTHROUGH", v)
			got, err := newClaudeCmd().Flags().GetBool("passthrough-auth")
			if err != nil {
				t.Fatalf("--passthrough-auth not registered: %v", err)
			}
			if got {
				t.Errorf("RAFIKI_CLAUDE_PASSTHROUGH=%q enabled passthrough, want off", v)
			}
		})
	}
}

// A subscription credential cannot buy an OpenRouter model. The proxy rejects
// it too, but only on the first turn — by which point claude owns the TTY and
// the failure reads as a hung session.
func TestRunClaude_PassthroughRejectsNonAnthropicModel(t *testing.T) {
	cmd := newClaudeCmd()
	for flag, val := range map[string]string{"url": "http://localhost:8035", "token": "dev", "model": "openai/gpt-4o"} {
		if err := cmd.Flags().Set(flag, val); err != nil {
			t.Fatal(err)
		}
	}
	if err := cmd.Flags().Set("passthrough-auth", "true"); err != nil {
		t.Fatal(err)
	}

	err := runClaude(cmd, nil)
	if err == nil {
		t.Fatal("expected error for --passthrough-auth with an OpenRouter model, got nil")
	}
	if !strings.Contains(err.Error(), "--passthrough-auth") {
		t.Errorf("error = %v, want it to name the conflicting flag", err)
	}
}

// Without a rafiki token there is no X-Rafiki-Token header, so the proxy sees
// only the OAuth bearer, tries it as rafiki's own token and 401s. Catch it here
// where the message can say what is actually wrong.
func TestRunClaude_PassthroughRequiresToken(t *testing.T) {
	cmd := newClaudeCmd()
	for flag, val := range map[string]string{"url": "http://localhost:8035", "token": "", "model": "claude-opus-5"} {
		if err := cmd.Flags().Set(flag, val); err != nil {
			t.Fatal(err)
		}
	}
	if err := cmd.Flags().Set("passthrough-auth", "true"); err != nil {
		t.Fatal(err)
	}

	err := runClaude(cmd, nil)
	if err == nil {
		t.Fatal("expected error for --passthrough-auth without a token, got nil")
	}
	if !strings.Contains(err.Error(), "--token") {
		t.Errorf("error = %v, want it to mention --token", err)
	}
}
