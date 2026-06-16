package main

import (
	"reflect"
	"testing"

	"git.graveland.dev/brent/pi-controller/protocol"
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
		"--dangerously-skip-permissions",
		"--disallowedTools", "AskUserQuestion",
		"--model", "claude-opus-4-8",
		"--resume", "sess-abc",
		"--append-system-prompt", "be brief",
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
