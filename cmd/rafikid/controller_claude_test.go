package main

import (
	"reflect"
	"testing"

	"go.graveland.dev/rafiki/pkg/protocol"
)

func TestBuildClaudeArgv_Defaults(t *testing.T) {
	got := buildClaudeArgv(protocol.SpawnRequest{})
	want := []string{
		"-p",
		"--input-format", "stream-json",
		"--output-format", "stream-json",
		"--verbose",
		"--dangerously-skip-permissions",
		"--disallowedTools", "AskUserQuestion",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("argv = %v\nwant %v", got, want)
	}
}

// Order matches pkg/claudeargv.Build's canonical order — buildClaudeArgv is
// now a thin wrapper over it (see that package's doc comment on why there is
// exactly one builder), so this pins the delegation rather than a
// second, independent flag order.
func TestBuildClaudeArgv_ModelResumeAndAppend(t *testing.T) {
	got := buildClaudeArgv(protocol.SpawnRequest{
		Model:              "claude-opus-4-8",
		ResumeSession:      "sess-abc",
		AppendSystemPrompt: "be brief",
		ExtraArgs:          []string{"--foo"},
	})
	want := []string{
		"-p",
		"--input-format", "stream-json",
		"--output-format", "stream-json",
		"--verbose",
		"--model", "claude-opus-4-8",
		"--resume", "sess-abc",
		"--append-system-prompt", "be brief",
		"--dangerously-skip-permissions",
		"--disallowedTools", "AskUserQuestion",
		"--foo",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("argv = %v\nwant %v", got, want)
	}
}

func TestResolveClaudeBinary_Override(t *testing.T) {
	got, err := resolveClaudeBinary("/custom/claude")
	if err != nil || got != "/custom/claude" {
		t.Fatalf("got %q err %v, want /custom/claude", got, err)
	}
}

func TestResolveClaudeBinary_EnvVar(t *testing.T) {
	t.Setenv("CLAUDE_BINARY", "/env/claude")
	got, err := resolveClaudeBinary("")
	if err != nil || got != "/env/claude" {
		t.Fatalf("got %q err %v, want /env/claude", got, err)
	}
}
