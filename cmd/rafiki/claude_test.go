// SPDX-License-Identifier: Apache-2.0

package main

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"go.graveland.dev/rafiki/pkg/profile"
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
		if err != nil {
			t.Fatalf("--%s not registered: %v", tt.flag, err)
		}
		if got != tt.want {
			t.Errorf("--%s default = %q, want %q", tt.flag, got, tt.want)
		}
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
		if got != wantVal {
			t.Errorf("--%s default = %q, want %q (from env)", flag, got, wantVal)
		}
	}
}

// resolveClaudeToken must not consult profileToken when an explicit --token
// was given: an explicit flag always wins. It is a pure function of its two
// arguments now — the profile lookup that used to happen via
// paths.TokenFromEnv (RAFIKI_TOKEN, then the global token file) happens one
// layer up, in runClaude, via mustProfile.
func TestResolveClaudeToken_FlagWins(t *testing.T) {
	got, err := resolveClaudeToken("from-flag", "from-profile")
	if err != nil {
		t.Fatalf("resolveClaudeToken: %v", err)
	}
	if got != "from-flag" {
		t.Errorf("token = %q, want %q", got, "from-flag")
	}
}

// With no --token, the resolved profile's token is the fallback — this is
// what makes `rafiki user create` + `rafiki claude` work with nothing
// exported.
func TestResolveClaudeToken_FallsBackToProfileToken(t *testing.T) {
	got, err := resolveClaudeToken("", "from-profile")
	if err != nil {
		t.Fatalf("resolveClaudeToken: %v", err)
	}
	if got != "from-profile" {
		t.Errorf("token = %q, want %q", got, "from-profile")
	}
}

// No flag and no profile token: this must be a clear, actionable error, not
// the old "dev" literal silently authenticating against nothing and failing
// as a confusing 401 several steps later.
func TestResolveClaudeToken_NoneResolvesIsAnError(t *testing.T) {
	_, err := resolveClaudeToken("", "")
	if err == nil {
		t.Fatal("expected an error when no token resolves, got nil")
	}
	if !strings.Contains(err.Error(), "rafiki user create") {
		t.Errorf("error = %v, want it to name `rafiki user create`", err)
	}
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
			got, err := resolveClaudeToken(tt.flagToken, tt.profileToken)
			if err != nil {
				t.Fatalf("resolveClaudeToken: %v", err)
			}
			if got != tt.want {
				t.Errorf("token = %q, want %q", got, tt.want)
			}
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
	isolateProfiles(t)
	resetProfileCache()
	if err := profile.Save(profile.Set{Profiles: map[string]profile.Profile{
		"work": {Name: "work", Socket: filepath.Join(t.TempDir(), "d.sock")},
	}}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := profile.SavePointer("work"); err != nil {
		t.Fatalf("SavePointer: %v", err)
	}

	// Construct the command before any token exists. A correct implementation
	// does nothing with the profile here — the flag's default stays "".
	cmd := newClaudeCmd()

	// The token is minted (or rotated) AFTER the command was built — e.g. a
	// developer runs `rafiki user create` in another shell while `rafiki
	// claude` is being invoked for the first time in a fresh process.
	if err := profile.WriteToken("work", "fresh-token"); err != nil {
		t.Fatalf("WriteToken: %v", err)
	}

	p, err := resolveProfile(cmd)
	if err != nil {
		t.Fatalf("resolveProfile: %v", err)
	}
	flagToken, err := cmd.Flags().GetString("token")
	if err != nil {
		t.Fatalf("--token not registered: %v", err)
	}
	got, err := resolveClaudeToken(flagToken, p.Token)
	if err != nil {
		t.Fatalf("resolveClaudeToken: %v", err)
	}
	if got != "fresh-token" {
		t.Errorf("token = %q, want %q (resolution must happen at RunE time, not command-construction time)", got, "fresh-token")
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

// An empty --url with a profile carrying no `proxy` field is an error: there
// is nowhere left to look.
func TestRunClaude_EmptyURLIsError(t *testing.T) {
	isolateProfiles(t)
	resetProfileCache()
	if err := profile.Save(profile.Set{Profiles: map[string]profile.Profile{
		"noproxy": {Name: "noproxy", Socket: filepath.Join(t.TempDir(), "d.sock")},
	}}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := profile.SavePointer("noproxy"); err != nil {
		t.Fatalf("SavePointer: %v", err)
	}

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

func TestClaudeCmd_PassthroughFlagDefaultsAuto(t *testing.T) {
	t.Setenv("RAFIKI_CLAUDE_PASSTHROUGH", "")

	cmd := newClaudeCmd()
	got, err := cmd.Flags().GetString("passthrough-auth")
	if err != nil {
		t.Fatalf("--passthrough-auth not registered: %v", err)
	}
	if got != "auto" {
		t.Errorf("--passthrough-auth default = %q, want %q", got, "auto")
	}
}

func TestClaudeCmd_PassthroughFlagFromEnv(t *testing.T) {
	t.Setenv("RAFIKI_CLAUDE_PASSTHROUGH", "1")

	cmd := newClaudeCmd()
	got, err := cmd.Flags().GetString("passthrough-auth")
	if err != nil {
		t.Fatalf("--passthrough-auth not registered: %v", err)
	}
	mode, err := parsePassthroughMode(got)
	if err != nil {
		t.Fatalf("parsePassthroughMode(%q): %v", got, err)
	}
	if mode != passthroughOn {
		t.Errorf("RAFIKI_CLAUDE_PASSTHROUGH=1 resolved to %q, want on", mode)
	}
}

// "0" and "false" mean off. Treating any non-empty value as true would make a
// user who exports RAFIKI_CLAUDE_PASSTHROUGH=0 to disable the feature silently
// start billing their personal subscription instead.
func TestClaudeCmd_PassthroughFlagFalseyEnv(t *testing.T) {
	for _, v := range []string{"0", "false", "no"} {
		t.Run(v, func(t *testing.T) {
			t.Setenv("RAFIKI_CLAUDE_PASSTHROUGH", v)
			got, err := newClaudeCmd().Flags().GetString("passthrough-auth")
			if err != nil {
				t.Fatalf("--passthrough-auth not registered: %v", err)
			}
			mode, err := parsePassthroughMode(got)
			if err != nil {
				t.Fatalf("parsePassthroughMode(%q): %v", got, err)
			}
			if mode != passthroughOff {
				t.Errorf("RAFIKI_CLAUDE_PASSTHROUGH=%q resolved to %q, want off", v, mode)
			}
		})
	}
}

// A bare --passthrough-auth (no "=value") must keep meaning "on", matching the
// flag's old boolean ergonomics and every existing example in README/Long.
func TestClaudeCmd_PassthroughFlagBareMeansOn(t *testing.T) {
	cmd := newClaudeCmd()
	cmd.SetArgs([]string{"--passthrough-auth"})
	if err := cmd.ParseFlags([]string{"--passthrough-auth"}); err != nil {
		t.Fatalf("ParseFlags: %v", err)
	}
	got, _ := cmd.Flags().GetString("passthrough-auth")
	mode, err := parsePassthroughMode(got)
	if err != nil {
		t.Fatalf("parsePassthroughMode(%q): %v", got, err)
	}
	if mode != passthroughOn {
		t.Errorf("bare --passthrough-auth resolved to %q, want on", mode)
	}
}

func TestParsePassthroughMode(t *testing.T) {
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
		if err != nil {
			t.Fatalf("parsePassthroughMode(%q): %v", tt.in, err)
		}
		if got != tt.want {
			t.Errorf("parsePassthroughMode(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// A typo must fail loudly rather than quietly falling back to auto: this
// switch decides who gets billed.
func TestParsePassthroughMode_InvalidIsError(t *testing.T) {
	_, err := parsePassthroughMode("onn")
	if err == nil {
		t.Fatal("expected error for invalid --passthrough-auth value, got nil")
	}
	if !strings.Contains(err.Error(), "auto, on, or off") {
		t.Errorf("error = %v, want it to list the valid values", err)
	}
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
			if got := passthroughAuthFor(passthroughAuto, tt.model); got != tt.want {
				t.Errorf("passthroughAuthFor(auto, %q) = %v, want %v", tt.model, got, tt.want)
			}
		})
	}
}

// on/off force the choice regardless of model; the OpenRouter+on combination
// is rejected later by runClaude's guard, not by passthroughAuthFor itself.
func TestPassthroughAuthFor_OnOffForceRegardlessOfModel(t *testing.T) {
	if got := passthroughAuthFor(passthroughOn, "openai/gpt-4o"); !got {
		t.Error("passthroughAuthFor(on, openrouter-model) = false, want true")
	}
	if got := passthroughAuthFor(passthroughOff, "claude-opus-5"); got {
		t.Error("passthroughAuthFor(off, anthropic-model) = true, want false")
	}
}

// A subscription credential cannot buy an OpenRouter model. The proxy rejects
// it too, but only on the first turn — by which point claude owns the TTY and
// the failure reads as a hung session.
func TestRunClaude_PassthroughRejectsNonAnthropicModel(t *testing.T) {
	isolateProfiles(t)
	resetProfileCache()

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

// An unrecognised --passthrough-auth value must fail before ever dialing the
// proxy, same as the OpenRouter+on guard below — a typo here decides who gets
// billed, so it must not resolve to auto silently.
func TestRunClaude_InvalidPassthroughModeIsError(t *testing.T) {
	isolateProfiles(t)
	resetProfileCache()

	cmd := newClaudeCmd()
	for flag, val := range map[string]string{"url": "http://localhost:8035", "token": "dev", "passthrough-auth": "onn"} {
		if err := cmd.Flags().Set(flag, val); err != nil {
			t.Fatal(err)
		}
	}

	err := runClaude(cmd, nil)
	if err == nil {
		t.Fatal("expected error for invalid --passthrough-auth value, got nil")
	}
	if !strings.Contains(err.Error(), "auto, on, or off") {
		t.Errorf("error = %v, want it to list the valid values", err)
	}
}

// With no token resolvable from any source, runClaude must fail before ever
// dialing the proxy — for ANY invocation, not just --passthrough-auth: a
// literal-default token that authenticates against nothing (the old "dev")
// is worse than refusing outright, because it turns a missing credential
// into a confusing 401 several steps later instead of a clear error here.
func TestRunClaude_NoTokenIsError(t *testing.T) {
	isolateProfiles(t) // no profile token file either
	resetProfileCache()

	cmd := newClaudeCmd()
	for flag, val := range map[string]string{"url": "http://localhost:8035", "token": "", "model": "claude-opus-5"} {
		if err := cmd.Flags().Set(flag, val); err != nil {
			t.Fatal(err)
		}
	}

	err := runClaude(cmd, nil)
	if err == nil {
		t.Fatal("expected error with no token resolvable from any source, got nil")
	}
	if !strings.Contains(err.Error(), "rafiki user create") {
		t.Errorf("error = %v, want it to name `rafiki user create`", err)
	}
}
