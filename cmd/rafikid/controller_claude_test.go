package main

import (
	"encoding/json"
	"io"
	"strings"
	"testing"

	"go.graveland.dev/rafiki/pkg/child"
	"go.graveland.dev/rafiki/pkg/claudeargv"
	"go.graveland.dev/rafiki/pkg/paths"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/proxyenv"

	"github.com/multigres/testkit/assert"
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
	assert.NewAborting(t).EqDiff(want, got, "argv")
}

// Order matches pkg/claudeargv.Build's canonical order — buildClaudeArgv is
// now a thin wrapper over it (see that package's doc comment on why there is
// exactly one builder), so this pins the delegation rather than a second,
// independent flag order. vals is the shape a proxied child actually gets
// (proxyChildEnv → buildEnv → resolveSpawnPlan): ModelArgs REPLACES the plain
// --model pair req.Model would emit, and --mcp-config sits between the model
// and --resume — Build's canonical position, not appended at the end the way
// the old post-hoc append did it. A proxied child also carries the
// coordination prompt merged into its --append-system-prompt element (the
// caller's text after it, one element — the flag is last-wins).
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
		"--append-system-prompt", claudeargv.CoordinationPrompt + "\n\nbe brief",
		"--dangerously-skip-permissions",
		"--disallowedTools", "AskUserQuestion",
		"--foo",
	}
	assert.NewAborting(t).EqDiff(want, got, "argv")
}

// TestBuildClaudeArgvCarriesMCPConfig pins the proxied child's argv against
// the double-emission failure class this rewiring exists to kill: a request
// with a proxy configured must yield EXACTLY ONE argv element beginning
// --mcp-config=, and its value must be the inline JSON document (Build
// prepends the flag to Params.MCPConfig, so a full element fed in verbatim
// would come out doubled).
func TestBuildClaudeArgvCarriesMCPConfig(t *testing.T) {
	t.Setenv(paths.URL, "")
	c := assert.NewCollecting(t)
	ctl := &Controller{proxyURL: "http://localhost:8035", proxyToken: "tok"}
	req := protocol.SpawnRequest{Kind: protocol.KindClaude, Model: "glm-5.2"}
	_, vals := ctl.proxyChildEnv(req, "c_abc")
	argv := buildClaudeArgv(req, vals)
	count := 0
	for _, a := range argv {
		if strings.HasPrefix(a, "--mcp-config=") {
			count++
			c.True(json.Valid([]byte(strings.TrimPrefix(a, "--mcp-config="))), "--mcp-config value is not valid JSON: %q", a)
		}
	}
	c.Require().Eq(1, count, "argv = %v, want exactly one --mcp-config= element, got", argv)
}

func TestResolveClaudeBinary_Override(t *testing.T) {
	got, err := resolveClaudeBinary("/custom/claude")
	assert.NewAborting(t).False(err != nil || got != "/custom/claude", "got %q err %v, want /custom/claude", got, err)
}

func TestResolveClaudeBinary_EnvVar(t *testing.T) {
	t.Setenv("CLAUDE_BINARY", "/env/claude")
	got, err := resolveClaudeBinary("")
	assert.NewAborting(t).False(err != nil || got != "/env/claude", "got %q err %v, want /env/claude", got, err)
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
	c := assert.NewCollecting(t)

	got, err := resolveClaudeBinaryIfNeeded(
		protocol.SpawnRequest{Kind: protocol.KindClaude}, nopRunner{})
	c.Require().NoError(err, "resolveClaudeBinaryIfNeeded with a daraja-backed runner")
	c.Eq("", got, "bin")
}

// TestResolveClaudeBinaryIfNeeded_ResolvesForLocalSubprocessFallback is the
// other half: a nil runner (agentRunner's claudeRunner declined — no executor
// pool configured at all) means the local-subprocess path really will exec
// this binary, so it must still be resolved and a genuine failure must still
// surface as an error.
func TestResolveClaudeBinaryIfNeeded_ResolvesForLocalSubprocessFallback(t *testing.T) {
	got, err := resolveClaudeBinaryIfNeeded(
		protocol.SpawnRequest{Kind: protocol.KindClaude, PiBinary: "/custom/claude"}, nil)
	assert.NewAborting(t).False(err != nil || got != "/custom/claude", "got %q err %v, want /custom/claude", got, err)
}

// A non-claude kind never needs the claude binary at all, runner or not.
func TestResolveClaudeBinaryIfNeeded_NoopForOtherKinds(t *testing.T) {
	got, err := resolveClaudeBinaryIfNeeded(protocol.SpawnRequest{Kind: protocol.KindFundi}, nil)
	assert.NewAborting(t).False(err != nil || got != "", "got %q err %v, want empty/nil for kind=fundi", got, err)
}

// TestResolveSpawnPlan_ClaudeNeverFailsOnMissingBinary pins the actual bug:
// resolveSpawnPlan must succeed for kind=claude regardless of whether claude
// is anywhere on the daemon's PATH — that question belongs entirely to
// resolveClaudeBinaryIfNeeded, called later, after agentRunner has decided
// whether daraja will handle this spawn instead.
func TestResolveSpawnPlan_ClaudeNeverFailsOnMissingBinary(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	t.Setenv("CLAUDE_BINARY", "")
	c := assert.NewCollecting(t)

	bin, _, prov, err := resolveSpawnPlan(protocol.SpawnRequest{Kind: protocol.KindClaude}, "c1", t.TempDir(), proxyenv.Values{})
	c.Require().NoError(err, "resolveSpawnPlan(claude) with no claude on PATH")
	c.Eq("", bin, "bin")
	_, ok := prov.(child.ClaudeProvider)
	c.True(ok, "provider = %T, want child.ClaudeProvider", prov)
}
