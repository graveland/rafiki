// SPDX-License-Identifier: Apache-2.0

package main

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"go.graveland.dev/rafiki/pkg/profile"

	"github.com/multigres/testkit/assert"
)

func TestClaudeCmd_FlagDefaults(t *testing.T) {
	// Isolate from any RAFIKI_* set in the developer's shell/.env.
	t.Setenv("RAFIKI_URL", "")
	t.Setenv("RAFIKI_TOKEN", "")
	t.Setenv("RAFIKI_MODEL", "")
	t.Setenv("RAFIKI_SESSION", "")
	c := assert.NewCollecting(t)

	cmd := newClaudeCmd()

	tests := []struct {
		flag string
		want string
	}{
		// url has no flag-construction-time default at all: it falls back to
		// the resolved profile's `proxy` field, resolved at RunE time — see
		// runClaude. RAFIKI_URL no longer feeds it (that variable is retired
		// client-side; see profile.CheckRetiredEnv).
		{"url", ""},
		// token has no flag-construction-time default at all, not even from
		// the profile's token — see TestResolveClaudeToken. A cobra flag
		// default is computed once, when newClaudeCmd runs, so baking the
		// profile's token in here would miss a token minted later in the
		// process's life; resolveClaudeToken runs at RunE time instead.
		{"token", ""},
		{"model", ""},
		{"session", ""},
	}
	for _, tt := range tests {
		got, err := cmd.Flags().GetString(tt.flag)
		c.Require().NoError(err, "--%s not registered", tt.flag)
		c.Eq(tt.want, got, "--%s default = %q, want", tt.flag, got)
	}
}

func TestClaudeCmd_FlagDefaultsFromEnv(t *testing.T) {
	t.Setenv("RAFIKI_MODEL", "glm-5.2")
	t.Setenv("RAFIKI_SESSION", "sess-123")

	cmd := newClaudeCmd()

	// url and token deliberately excluded: neither feeds a flag default any
	// more (see TestClaudeCmd_FlagDefaults) — url falls back to the resolved
	// profile's `proxy`, and RAFIKI_URL no longer configures the client at
	// all (profile.CheckRetiredEnv rejects it outright); token is read at
	// RunE time by resolveClaudeToken instead.
	want := map[string]string{
		"model":   "glm-5.2",
		"session": "sess-123",
	}
	for flag, wantVal := range want {
		got, _ := cmd.Flags().GetString(flag)
		assert.NewCollecting(t).Eq(wantVal, got, "--%s default = %q, want %q (from env)", flag, got, wantVal)
	}
}

// resolveClaudeToken must not consult profileToken when an explicit --token
// was given: an explicit flag always wins. It is a pure function of its two
// arguments now — the profile lookup that used to happen via
// paths.TokenFromEnv (RAFIKI_TOKEN, then the global token file) happens one
// layer up, in runClaude, via mustProfile.
func TestResolveClaudeToken_FlagWins(t *testing.T) {
	c := assert.NewCollecting(t)
	got, err := resolveClaudeToken("from-flag", "from-profile")
	c.Require().NoError(err, "resolveClaudeToken")
	c.Eq("from-flag", got, "token")
}

// With no --token, the resolved profile's token is the fallback — this is
// what makes `rafiki user create` + `rafiki claude` work with nothing
// exported.
func TestResolveClaudeToken_FallsBackToProfileToken(t *testing.T) {
	c := assert.NewCollecting(t)
	got, err := resolveClaudeToken("", "from-profile")
	c.Require().NoError(err, "resolveClaudeToken")
	c.Eq("from-profile", got, "token")
}

// No flag and no profile token: this must be a clear, actionable error, not
// the old "dev" literal silently authenticating against nothing and failing
// as a confusing 401 several steps later.
func TestResolveClaudeToken_NoneResolvesIsAnError(t *testing.T) {
	c := assert.NewCollecting(t)
	_, err := resolveClaudeToken("", "")
	c.Require().Error(err, "expected an error when no token resolves, got nil")
	c.StrContains(err.Error(), "rafiki user create", "error = %v, want it to name `rafiki user create`", err)
}

// The two sources can be present and conflicting at once; only short-circuit
// control flow guarantees the precedence order in that case.
func TestResolveClaudeToken_Precedence(t *testing.T) {
	tests := []struct {
		name         string
		flagToken    string
		profileToken string
		want         string
	}{
		{
			name:         "flag wins over the profile token when both are set",
			flagToken:    "from-flag",
			profileToken: "from-profile",
			want:         "from-flag",
		},
		{
			name:         "profile token is the fallback when the flag is empty",
			flagToken:    "",
			profileToken: "from-profile",
			want:         "from-profile",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := assert.NewCollecting(t)
			got, err := resolveClaudeToken(tt.flagToken, tt.profileToken)
			c.Require().NoError(err, "resolveClaudeToken")
			c.Eq(tt.want, got, "token")
		})
	}
}

// Regression test for the original Critical: a token resolved at
// newClaudeCmd's construction time (a cobra flag default is computed once,
// when the command tree is built) freezes in whatever the token file held at
// THAT moment. The fix moved resolution to RunE time, and profiles carry it
// further: mustProfile — which reads the profile's token file — is called
// inside runClaude's body, never at command-construction time. This proves
// that ordering end to end: construct the command, THEN mint the token, and
// confirm resolution still sees it.
func TestResolveClaudeToken_ReflectsProfileTokenWrittenAfterConstruction(t *testing.T) {
	c := assert.NewCollecting(t)
	isolateProfiles(t)
	resetProfileCache()
	c.Require().NoError(profile.Save(profile.Set{Profiles: map[string]profile.Profile{
		"work": {Name: "work", Socket: filepath.Join(t.TempDir(), "d.sock")},
	}}), "Save")
	c.Require().NoError(profile.SavePointer("work"), "SavePointer")

	// Construct the command before any token exists. A correct implementation
	// does nothing with the profile here — the flag's default stays "".
	cmd := newClaudeCmd()

	// The token is minted (or rotated) AFTER the command was built — e.g. a
	// developer runs `rafiki user create` in another shell while `rafiki
	// claude` is being invoked for the first time in a fresh process.
	c.Require().NoError(profile.WriteToken("work", "fresh-token"), "WriteToken")

	p, err := resolveProfile(cmd)
	c.Require().NoError(err, "resolveProfile")
	flagToken, err := cmd.Flags().GetString("token")
	c.Require().NoError(err, "--token not registered")
	got, err := resolveClaudeToken(flagToken, p.Token)
	c.Require().NoError(err, "resolveClaudeToken")
	c.Eq("fresh-token", got, "token")
}

// pflag treats a bare "--" as the flag/arg terminator (unlike stdlib flag,
// which the old FlagSet also honored the same way), so everything after it
// must reach RunE as positional args untouched. This is what lets
// `rafiki claude --model foo -- --permission-mode plan` forward
// --permission-mode straight to the claude binary instead of rafiki trying
// to parse it as its own flag.
func TestClaudeCmd_DashDashPassesArgsThrough(t *testing.T) {
	c := assert.NewCollecting(t)
	cmd := newClaudeCmd()

	var gotArgs []string
	cmd.RunE = func(_ *cobra.Command, args []string) error {
		gotArgs = args
		return nil
	}
	cmd.SetArgs([]string{"--model", "foo", "--", "--permission-mode", "plan"})
	c.Require().NoError(cmd.Execute(), "unexpected error")

	model, _ := cmd.Flags().GetString("model")
	c.Eq("foo", model, "--model")
	want := []string{"--permission-mode", "plan"}
	c.EqDiff(want, gotArgs, "args passed to RunE")
}

// An empty --url with a profile carrying no `proxy` field is an error: there
// is nowhere left to look.
func TestRunClaude_EmptyURLIsError(t *testing.T) {
	c := assert.NewCollecting(t)
	isolateProfiles(t)
	resetProfileCache()
	c.Require().NoError(profile.Save(profile.Set{Profiles: map[string]profile.Profile{
		"noproxy": {Name: "noproxy", Socket: filepath.Join(t.TempDir(), "d.sock")},
	}}), "Save")
	c.Require().NoError(profile.SavePointer("noproxy"), "SavePointer")

	cmd := newClaudeCmd()
	c.Require().NoError(cmd.Flags().Set("url", ""))

	err := runClaude(cmd, nil)
	c.Require().Error(err, "expected error for empty --url, got nil")
	c.StrContains(err.Error(), "--url", "error = %v, want it to mention --url", err)
}

func TestClaudeCmd_PassthroughFlagDefaultsAuto(t *testing.T) {
	t.Setenv("RAFIKI_CLAUDE_PASSTHROUGH", "")
	c := assert.NewCollecting(t)

	cmd := newClaudeCmd()
	got, err := cmd.Flags().GetString("passthrough-auth")
	c.Require().NoError(err, "--passthrough-auth not registered")
	c.Eq("auto", got, "--passthrough-auth default")
}

func TestClaudeCmd_PassthroughFlagFromEnv(t *testing.T) {
	t.Setenv("RAFIKI_CLAUDE_PASSTHROUGH", "1")
	c := assert.NewCollecting(t)

	cmd := newClaudeCmd()
	got, err := cmd.Flags().GetString("passthrough-auth")
	c.Require().NoError(err, "--passthrough-auth not registered")
	mode, err := parsePassthroughMode(got)
	c.Require().NoError(err, "parsePassthroughMode(%q)", got)
	c.Eq(passthroughOn, mode, "RAFIKI_CLAUDE_PASSTHROUGH=1 resolved to")
}

// "0" and "false" mean off. Treating any non-empty value as true would make a
// user who exports RAFIKI_CLAUDE_PASSTHROUGH=0 to disable the feature silently
// start billing their personal subscription instead.
func TestClaudeCmd_PassthroughFlagFalseyEnv(t *testing.T) {
	for _, v := range []string{"0", "false", "no"} {
		t.Run(v, func(t *testing.T) {
			t.Setenv("RAFIKI_CLAUDE_PASSTHROUGH", v)
			c := assert.NewCollecting(t)
			got, err := newClaudeCmd().Flags().GetString("passthrough-auth")
			c.Require().NoError(err, "--passthrough-auth not registered")
			mode, err := parsePassthroughMode(got)
			c.Require().NoError(err, "parsePassthroughMode(%q)", got)
			c.Eq(passthroughOff, mode, "RAFIKI_CLAUDE_PASSTHROUGH=%q resolved to %q, want off", v, mode)
		})
	}
}

// A bare --passthrough-auth (no "=value") must keep meaning "on", matching the
// flag's old boolean ergonomics and every existing example in README/Long.
func TestClaudeCmd_PassthroughFlagBareMeansOn(t *testing.T) {
	c := assert.NewCollecting(t)
	cmd := newClaudeCmd()
	cmd.SetArgs([]string{"--passthrough-auth"})
	c.Require().NoError(cmd.ParseFlags([]string{"--passthrough-auth"}), "ParseFlags")
	got, _ := cmd.Flags().GetString("passthrough-auth")
	mode, err := parsePassthroughMode(got)
	c.Require().NoError(err, "parsePassthroughMode(%q)", got)
	c.Eq(passthroughOn, mode, "bare --passthrough-auth resolved to")
}

func TestParsePassthroughMode(t *testing.T) {
	c := assert.NewCollecting(t)
	tests := []struct {
		in   string
		want passthroughMode
	}{
		{"", passthroughAuto},
		{"auto", passthroughAuto},
		{"AUTO", passthroughAuto},
		{"on", passthroughOn},
		{"true", passthroughOn},
		{"1", passthroughOn},
		{"off", passthroughOff},
		{"false", passthroughOff},
		{"0", passthroughOff},
		{"no", passthroughOff},
	}
	for _, tt := range tests {
		got, err := parsePassthroughMode(tt.in)
		c.Require().NoError(err, "parsePassthroughMode(%q)", tt.in)
		c.Eq(tt.want, got, "parsePassthroughMode(%q) = %q, want", tt.in, got)
	}
}

// A typo must fail loudly rather than quietly falling back to auto: this
// switch decides who gets billed.
func TestParsePassthroughMode_InvalidIsError(t *testing.T) {
	c := assert.NewCollecting(t)
	_, err := parsePassthroughMode("onn")
	c.Require().Error(err, "expected error for invalid --passthrough-auth value, got nil")
	c.StrContains(err.Error(), "auto, on, or off", "error = %v, want it to list the valid values", err)
}

// With mode=auto, passthrough follows the model: on for an Anthropic id
// (including no --model at all), off for an OpenRouter slash id.
func TestPassthroughAuthFor_Auto(t *testing.T) {
	tests := []struct {
		name  string
		model string
		want  bool
	}{
		{"no model", "", true},
		{"anthropic id", "claude-opus-5", true},
		{"family-latest alias", "opus-latest", true},
		{"openrouter slash id", "openai/gpt-4o", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := passthroughAuthFor(passthroughAuto, tt.model)
			assert.NewCollecting(t).Eq(tt.want, got, "passthroughAuthFor(auto, %q) = %v, want", tt.model, got)
		})
	}
}

// on/off force the choice regardless of model; the OpenRouter+on combination
// is rejected later by runClaude's guard, not by passthroughAuthFor itself.
func TestPassthroughAuthFor_OnOffForceRegardlessOfModel(t *testing.T) {
	c := assert.NewCollecting(t)
	c.True(passthroughAuthFor(passthroughOn, "openai/gpt-4o"), "passthroughAuthFor(on, openrouter-model) = false, want true")
	c.False(passthroughAuthFor(passthroughOff, "claude-opus-5"), "passthroughAuthFor(off, anthropic-model) = true, want false")
}

// A subscription credential cannot buy an OpenRouter model. The proxy rejects
// it too, but only on the first turn — by which point claude owns the TTY and
// the failure reads as a hung session.
func TestRunClaude_PassthroughRejectsNonAnthropicModel(t *testing.T) {
	c := assert.NewCollecting(t)
	isolateProfiles(t)
	resetProfileCache()

	cmd := newClaudeCmd()
	for flag, val := range map[string]string{"url": "http://localhost:8035", "token": "dev", "model": "openai/gpt-4o"} {
		c.Require().NoError(cmd.Flags().Set(flag, val))
	}
	c.Require().NoError(cmd.Flags().Set("passthrough-auth", "true"))

	err := runClaude(cmd, nil)
	c.Require().Error(err, "expected error for --passthrough-auth with an OpenRouter model, got nil")
	c.StrContains(err.Error(), "--passthrough-auth", "error = %v, want it to name the conflicting flag", err)
}

// An unrecognised --passthrough-auth value must fail before ever dialing the
// proxy, same as the OpenRouter+on guard below — a typo here decides who gets
// billed, so it must not resolve to auto silently.
func TestRunClaude_InvalidPassthroughModeIsError(t *testing.T) {
	c := assert.NewCollecting(t)
	isolateProfiles(t)
	resetProfileCache()

	cmd := newClaudeCmd()
	for flag, val := range map[string]string{"url": "http://localhost:8035", "token": "dev", "passthrough-auth": "onn"} {
		c.Require().NoError(cmd.Flags().Set(flag, val))
	}

	err := runClaude(cmd, nil)
	c.Require().Error(err, "expected error for invalid --passthrough-auth value, got nil")
	c.StrContains(err.Error(), "auto, on, or off", "error = %v, want it to list the valid values", err)
}

func TestRunClaudeArgvHasNoHeadlessFlags(t *testing.T) {
	c := assert.NewCollecting(t)
	isolateProfiles(t)
	resetProfileCache()

	// claudePreflight must see a live /healthz before runClaude gets as far
	// as building argv; without this the test would measure the preflight
	// failure, not the argv.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)

	var captured claudeInvocation
	origExec := execClaude
	execClaude = func(inv claudeInvocation) error {
		captured = inv
		return nil
	}
	t.Cleanup(func() { execClaude = origExec })

	cmd := newClaudeCmd()
	for flag, val := range map[string]string{
		"url":     srv.URL,
		"token":   "tok",
		"model":   "claude-opus-5",
		"session": "sess-1",
	} {
		c.Require().NoError(cmd.Flags().Set(flag, val))
	}

	// What pflag hands back for everything after `--`: claude-side flags
	// rafiki never parses. --resume doubles as the "resume arg it was given"
	// of the review finding; with a proxied model the builder routes the model
	// through vals.ModelArgs, so this also proves the pair survived the switch
	// from pre-rendered argv to claudeargv.Params.
	userArgs := []string{"--resume", "abc-123", "--permission-mode", "plan"}
	c.Require().NoError(runClaude(cmd, userArgs), "runClaude")

	argv := captured.Args
	c.Require().Greater(len(userArgs), len(argv), "argv %v is degenerate: an interactive build must still carry the model, mcp-config and user args it was given", argv)
	for _, unwanted := range []string{
		"-p",
		"--input-format",
		"--output-format",
		"--verbose",
		"--dangerously-skip-permissions",
		"--disallowedTools",
	} {
		c.NotContains(argv, unwanted, "interactive argv")
	}
	// (a) the model pair it was given is still there...
	assertArgvPair(t, argv, "--model", "claude-opus-5")
	// ...and so is the MCP config, emitted as a single --mcp-config=<json>
	// element (the variadic flag makes a two-element pair swallow the next one).
	mcpCount := 0
	for _, a := range argv {
		if strings.HasPrefix(a, "--mcp-config=") {
			mcpCount++
			c.NotEq("--mcp-config=", a, "argv %v carries an empty --mcp-config=", argv)
		}
	}
	c.Eq(1, mcpCount, "argv %v: want exactly one --mcp-config element, got", argv)
	// (b) the user's own args come last, after everything the builder emits.
	c.EqDiff(userArgs, argv[len(argv)-len(userArgs):], "argv %v: want user args %v last, got tail", argv, userArgs)
	c.NotContains(argv[:len(argv)-len(userArgs)], "--resume", "argv %v: a user arg leaked ahead of the user tail", argv)

	// ClaudeEnv (the only proxyenv entry point) must not have dropped the
	// environment half: the proxy wiring and the session correlation header
	// still arrive.
	envJoined := strings.Join(captured.Env, "\n")
	c.Contains(captured.Env, "ANTHROPIC_BASE_URL="+srv.URL, "env does not point ANTHROPIC_BASE_URL at the proxy; got:\n%s", envJoined)
	c.StrContains(envJoined, "X-Rafiki-Session: sess-1", "env is missing the X-Rafiki-Session correlation header; got:\n")
}

// assertArgvPair asserts argv carries flag immediately followed by value.
func assertArgvPair(t *testing.T, argv []string, flag, value string) {
	t.Helper()
	for i, a := range argv {
		if a == flag {
			assert.NewCollecting(t).False(i+1 >= len(argv) || argv[i+1] != value, "argv %v: %s is not followed by %q", argv, flag, value)
			return
		}
	}
	t.Errorf("argv %v: missing %s", argv, flag)
}

// With no token resolvable from any source, runClaude must fail before ever
// dialing the proxy — for ANY invocation, not just --passthrough-auth: a
// literal-default token that authenticates against nothing (the old "dev")
// is worse than refusing outright, because it turns a missing credential
// into a confusing 401 several steps later instead of a clear error here.
func TestRunClaude_NoTokenIsError(t *testing.T) {
	c := assert.NewCollecting(t)
	isolateProfiles(t) // no profile token file either
	resetProfileCache()

	cmd := newClaudeCmd()
	for flag, val := range map[string]string{"url": "http://localhost:8035", "token": "", "model": "claude-opus-5"} {
		c.Require().NoError(cmd.Flags().Set(flag, val))
	}

	err := runClaude(cmd, nil)
	c.Require().Error(err, "expected error with no token resolvable from any source, got nil")
	c.StrContains(err.Error(), "rafiki user create", "error = %v, want it to name `rafiki user create`", err)
}
