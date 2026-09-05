package main

import (
	"io"
	"reflect"
	"testing"

	"go.graveland.dev/rafiki/pkg/child"
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

// nopRunner is a minimal child.Runner stand-in — only its non-nilness
// matters to resolveClaudeBinaryIfNeeded, which never calls any of these.
type nopRunner struct{}

func (nopRunner) Start() (io.WriteCloser, io.ReadCloser, io.ReadCloser, error) {
	return nil, nil, nil, nil
}
func (nopRunner) Wait() (int, string) { return 0, "" }
func (nopRunner) PID() int            { return 0 }
func (nopRunner) Terminate() error    { return nil }
func (nopRunner) Kill() error         { return nil }
func (nopRunner) Interrupt() error    { return nil }

// TestResolveClaudeBinaryIfNeeded_SkipsLookupWhenDarajaRouted is the
// regression pin for the bug this function exists to fix: resolveSpawnPlan
// used to resolve the claude binary UNCONDITIONALLY, before agentRunner ever
// got a chance to route the spawn through daraja — so a daemon with no local
// claude install (a remote/k8s daemon relying entirely on an executor pool,
// exactly the topology daraja exists for) refused every claude spawn outright
// with "executable file not found in $PATH", even when a live daraja-backed
// Runner was available. A non-nil runner must short-circuit before ever
// touching the filesystem or $PATH.
func TestResolveClaudeBinaryIfNeeded_SkipsLookupWhenDarajaRouted(t *testing.T) {
	t.Setenv("PATH", t.TempDir()) // guarantees "claude" cannot be found on PATH
	t.Setenv("CLAUDE_BINARY", "")

	got, err := resolveClaudeBinaryIfNeeded(
		protocol.SpawnRequest{Kind: protocol.KindClaude}, nopRunner{})
	if err != nil {
		t.Fatalf("resolveClaudeBinaryIfNeeded with a daraja-backed runner: %v", err)
	}
	if got != "" {
		t.Errorf("bin = %q, want empty (unused once a Runner is provided)", got)
	}
}

// TestResolveClaudeBinaryIfNeeded_ResolvesForLocalSubprocessFallback is the
// other half: a nil runner (agentRunner's claudeRunner declined — no executor
// pool configured at all) means the local-subprocess path really will exec
// this binary, so it must still be resolved and a genuine failure must still
// surface as an error.
func TestResolveClaudeBinaryIfNeeded_ResolvesForLocalSubprocessFallback(t *testing.T) {
	got, err := resolveClaudeBinaryIfNeeded(
		protocol.SpawnRequest{Kind: protocol.KindClaude, PiBinary: "/custom/claude"}, nil)
	if err != nil || got != "/custom/claude" {
		t.Fatalf("got %q err %v, want /custom/claude", got, err)
	}
}

// A non-claude kind never needs the claude binary at all, runner or not.
func TestResolveClaudeBinaryIfNeeded_NoopForOtherKinds(t *testing.T) {
	got, err := resolveClaudeBinaryIfNeeded(protocol.SpawnRequest{Kind: protocol.KindFundi}, nil)
	if err != nil || got != "" {
		t.Fatalf("got %q err %v, want empty/nil for kind=fundi", got, err)
	}
}

// TestResolveSpawnPlan_ClaudeNeverFailsOnMissingBinary pins the actual bug:
// resolveSpawnPlan must succeed for kind=claude regardless of whether claude
// is anywhere on the daemon's PATH — that question belongs entirely to
// resolveClaudeBinaryIfNeeded, called later, after agentRunner has decided
// whether daraja will handle this spawn instead.
func TestResolveSpawnPlan_ClaudeNeverFailsOnMissingBinary(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	t.Setenv("CLAUDE_BINARY", "")

	bin, _, prov, err := resolveSpawnPlan(protocol.SpawnRequest{Kind: protocol.KindClaude}, "c1", t.TempDir())
	if err != nil {
		t.Fatalf("resolveSpawnPlan(claude) with no claude on PATH: %v", err)
	}
	if bin != "" {
		t.Errorf("bin = %q, want empty — resolution is deferred", bin)
	}
	if _, ok := prov.(child.ClaudeProvider); !ok {
		t.Errorf("provider = %T, want child.ClaudeProvider", prov)
	}
}
