package main

import (
	"encoding/json"
	"io"
	"reflect"
	"strings"
	"testing"

	"go.graveland.dev/rafiki/pkg/child"
	"go.graveland.dev/rafiki/pkg/paths"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/proxyenv"
)

func TestBuildClaudeArgv_Defaults(t *testing.T) {
	got := buildClaudeArgv(protocol.SpawnRequest{}, proxyenv.Values{})
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
// exactly one builder), so this pins the delegation rather than a second,
// independent flag order. vals is the shape a proxied child actually gets
// (proxyChildEnv → buildEnv → resolveSpawnPlan): ModelArgs REPLACES the plain
// --model pair req.Model would emit, and --mcp-config sits between the model
// and --resume — Build's canonical position, not appended at the end the way
// the old post-hoc append did it.
func TestBuildClaudeArgv_ModelResumeAndAppend(t *testing.T) {
	_, vals := proxyenv.ClaudeEnv(nil, proxyenv.ClaudeOptions{
		URL: "http://localhost:8035", Token: "tok", Model: "glm-5.2",
	})
	got := buildClaudeArgv(protocol.SpawnRequest{
		Model:              "glm-5.2",
		ResumeSession:      "sess-abc",
		AppendSystemPrompt: "be brief",
		ExtraArgs:          []string{"--foo"},
	}, vals)
	want := []string{
		"-p",
		"--input-format", "stream-json",
		"--output-format", "stream-json",
		"--verbose",
		"--model", "glm-5.2", // vals.ModelArgs — exactly one --model
		"--mcp-config=" + vals.MCPConfig, // Build renders the flag against the bare-JSON Values
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

// TestBuildClaudeArgvCarriesMCPConfig pins the proxied child's argv against
// the double-emission failure class this rewiring exists to kill: a request
// with a proxy configured must yield EXACTLY ONE argv element beginning
// --mcp-config=, and its value must be the inline JSON document (Build
// prepends the flag to Params.MCPConfig, so a full element fed in verbatim
// would come out doubled).
func TestBuildClaudeArgvCarriesMCPConfig(t *testing.T) {
	t.Setenv(paths.URL, "")
	ctl := &Controller{proxyURL: "http://localhost:8035", proxyToken: "tok"}
	req := protocol.SpawnRequest{Kind: protocol.KindClaude, Model: "glm-5.2"}
	_, vals := ctl.proxyChildEnv(req, "c_abc")
	argv := buildClaudeArgv(req, vals)
	count := 0
	for _, a := range argv {
		if strings.HasPrefix(a, "--mcp-config=") {
			count++
			if !json.Valid([]byte(strings.TrimPrefix(a, "--mcp-config="))) {
				t.Errorf("--mcp-config value is not valid JSON: %q", a)
			}
		}
	}
	if count != 1 {
		t.Fatalf("argv = %v, want exactly one --mcp-config= element, got %d", argv, count)
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

	bin, _, prov, err := resolveSpawnPlan(protocol.SpawnRequest{Kind: protocol.KindClaude}, "c1", t.TempDir(), proxyenv.Values{})
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
