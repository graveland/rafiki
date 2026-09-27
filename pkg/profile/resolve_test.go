// SPDX-License-Identifier: Apache-2.0

package profile

import (
	"os"
	"strings"
	"testing"

	"go.graveland.dev/rafiki/pkg/paths"

	"github.com/multigres/testkit/assert"
)

// seed writes a two-profile manifest and returns nothing; every resolution
// test starts from the same fixture so only the selection varies.
func seed(t *testing.T) {
	t.Helper()
	err := Save(Set{Profiles: map[string]Profile{
		"work":     {Name: "work", Socket: "/tmp/work.sock"},
		"personal": {Name: "personal", URL: "https://rafiki.example.net"},
	}})
	assert.NewAborting(t).NoError(err, "seed")
}

func TestResolvePrefersFlagThenEnvThenPointer(t *testing.T) {
	setXDG(t)
	seed(t)
	assert.NewAborting(t).NoError(SavePointer("work"), "SavePointer")

	cases := []struct {
		name string
		sel  Selection
		want string
	}{
		{"flag beats env and pointer", Selection{Flag: "personal", Env: "work", EnvSet: true}, "personal"},
		{"env beats pointer", Selection{Env: "personal", EnvSet: true}, "personal"},
		{"pointer when nothing else", Selection{}, "work"},
		{"empty env is not a selection", Selection{Env: "", EnvSet: true}, "work"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := assert.NewAborting(t)
			got, err := Resolve(tc.sel)
			c.NoError(err, "Resolve")
			c.Eq(tc.want, got.Name, "resolved")
		})
	}
}

func TestResolveCarriesTheProfilesOwnToken(t *testing.T) {
	c := assert.NewAborting(t)
	setXDG(t)
	seed(t)
	c.NoError(WriteToken("personal", "sk-personal"), "WriteToken")
	c.NoError(WriteToken("work", "sk-work"), "WriteToken")

	p, err := Resolve(Selection{Flag: "personal"})
	c.NoError(err, "Resolve")
	c.Eq("sk-personal", p.Token, "token")
}

func TestResolveRejectsAnUnknownName(t *testing.T) {
	c := assert.NewCollecting(t)
	setXDG(t)
	seed(t)
	_, err := Resolve(Selection{Flag: "nope"})
	c.Require().Error(err, "Resolve(nope) = nil error")
	for _, want := range []string{"nope", "work", "personal"} {
		c.StrContains(err.Error(), want, "error %q does not mention", err)
	}
}

func TestResolveWithAManifestButNothingSelectedIsAnError(t *testing.T) {
	c := assert.NewAborting(t)
	setXDG(t)
	seed(t)
	// No pointer file written.
	_, err := Resolve(Selection{})
	c.Error(err, "Resolve with no selection = nil error; it must not silently pick one")
	c.StrContains(err.Error(), "rafiki profile use", "error %q does not tell the user how to fix it", err)
}

func TestResolveBootstrapsWhenThereIsNoManifestAtAll(t *testing.T) {
	c := assert.NewAborting(t)
	setXDG(t)

	got, err := Resolve(Selection{})
	c.NoError(err, "Resolve")
	c.True(got.Bootstrapped, "Bootstrapped = false; the caller needs this to print its notice")
	c.Eq(DefaultName, got.Name, "bootstrapped profile")
	c.Eq(paths.SocketPath(), got.Socket, "bootstrapped socket")
	c.NotEq("", got.Proxy, "bootstrapped profile has no proxy; `rafiki claude` would have no default URL")

	// It must be durable, not computed fresh each time.
	if _, err := os.Stat(ProfilesFile()); err != nil {
		t.Fatalf("bootstrap did not write %s: %v", ProfilesFile(), err)
	}
	c.Eq(DefaultName, LoadPointer(), "bootstrap did not write the pointer (got")

	// A second call must NOT re-report a bootstrap.
	again, err := Resolve(Selection{})
	c.NoError(err, "second Resolve")
	c.False(again.Bootstrapped, "Bootstrapped = true on the second call; the notice would print forever")
}

func TestResolveWithAnExplicitSelectionOnABareMachineErrorsRatherThanBootstrapping(t *testing.T) {
	setXDG(t)
	// No manifest at all; no pointer file.

	cases := []struct {
		name string
		sel  Selection
	}{
		{"explicit flag", Selection{Flag: "somename"}},
		{"explicit env", Selection{Env: "somename", EnvSet: true}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := assert.NewCollecting(t)
			// Clear any leftover manifest.
			os.Remove(ProfilesFile())

			_, err := Resolve(tc.sel)
			c.Require().Error(err, "Resolve with explicit selection on a bare machine = nil error; it must refuse to bootstrap")
			c.StrContains(err.Error(), "somename", "error %q does not name the requested profile", err)
			c.StrContains(err.Error(), "rafiki profile add", "error %q does not point at the fix", err)

			// Verify bootstrap did NOT run as a side effect.
			if _, err := os.Stat(ProfilesFile()); err == nil {
				t.Fatal("profiles.toml was created; Bootstrap() should not have run")
			}
		})
	}
}

func TestResolveErrorMessageUsesCorrectPrecedence(t *testing.T) {
	c := assert.NewCollecting(t)
	setXDG(t)
	// No manifest at all; no pointer file.
	// When both Flag and Env are set on a bare machine, the error should
	// name the Flag value (which wins by precedence), not concatenate both.

	os.Remove(ProfilesFile())
	_, err := Resolve(Selection{Flag: "foo", Env: "bar", EnvSet: true})
	c.Require().Error(err, "Resolve with both Flag and Env on a bare machine = nil error")
	c.StrContains(err.Error(), "foo", "error %q does not name the Flag value", err)
	// Verify it does NOT say "foobar" or "foo" + "bar" concatenated.
	c.NotStrContains(err.Error(), "foobar", "error %q incorrectly concatenates Flag and Env", err)
	c.NotStrContains(err.Error(), "bar", "error %q should not mention the Env value when Flag is set", err)
}

func TestCheckRetiredEnvNamesTheVariableAndTheFix(t *testing.T) {
	for _, name := range []string{
		"RAFIKI_URL", "RAFIKI_TOKEN", "RAFIKI_SOCKET",
		"RAFIKI_DEFAULT_MODEL", "RAFIKI_DEFAULT_PRESET", "RAFIKI_DEFAULT_LABELS",
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv(name, "something")
			c := assert.NewCollecting(t)
			err := CheckRetiredEnv()
			c.Require().Error(err, "CheckRetiredEnv with %s set = nil error", name)
			c.StrContains(err.Error(), name, "error %q does not name", err)
			c.StrContains(err.Error(), "rafiki profile", "error %q does not point at the replacement", err)
		})
	}
}

func TestCheckRetiredEnvIsQuietWhenNoneAreSet(t *testing.T) {
	for _, name := range []string{
		"RAFIKI_URL", "RAFIKI_TOKEN", "RAFIKI_SOCKET",
		"RAFIKI_DEFAULT_MODEL", "RAFIKI_DEFAULT_PRESET", "RAFIKI_DEFAULT_LABELS",
	} {
		t.Setenv(name, "")
		os.Unsetenv(name)
	}
	assert.NewAborting(t).NoError(CheckRetiredEnv(), "CheckRetiredEnv")
}

// TestCheckRetiredEnvTreatsPresentButEmptyAsUnset pins the distinction the
// previous test does NOT exercise: os.Unsetenv makes a variable fully
// ABSENT, but isolateProfiles (t.Setenv(v, "")) and test/integration's
// cliCmd ("RAFIKI_URL=" in cmd.Env) both rely on a variable being PRESENT
// with an empty value also reading as unset. Without this, either
// hermeticity mechanism could silently stop working and nothing would catch
// it -- every test using isolateProfiles would start failing with "these
// variables no longer configure the rafiki client", but the assertion
// belongs here, on the function whose contract this is.
func TestCheckRetiredEnvTreatsPresentButEmptyAsUnset(t *testing.T) {
	names := []string{
		"RAFIKI_URL", "RAFIKI_TOKEN", "RAFIKI_SOCKET",
		"RAFIKI_DEFAULT_MODEL", "RAFIKI_DEFAULT_PRESET", "RAFIKI_DEFAULT_LABELS",
	}
	// Every variable present-but-empty, all at once: a developer's own shell
	// may genuinely export e.g. RAFIKI_URL (this repo's own .env, sourced by
	// `make check`, sets several of these), and t.Setenv on just one variable
	// would leave the others at whatever real value the ambient environment
	// happens to have -- failing this test for a reason unrelated to the
	// present-but-empty distinction it exists to pin.
	for _, name := range names {
		t.Setenv(name, "")
	}
	assert.NewAborting(t).NoError(CheckRetiredEnv(), "CheckRetiredEnv with every retired var present-but-empty (via t.Setenv, not os.Unsetenv)")
}

// TestResolveDerivesProxyFromURL pins Fix 5 (design spec: "For a url profile
// it defaults to that same URL -- one TLS listener serves the control plane
// and the proxy face"). No task implemented this, so `rafiki claude` against
// a freshly-added url profile with no --proxy errored "profile has no proxy
// URL" even though docs/MIGRATING.md's worked example for a remote profile
// never passes --proxy.
func TestResolveDerivesProxyFromURL(t *testing.T) {
	c := assert.NewAborting(t)
	setXDG(t)
	c.NoError(Save(Set{Profiles: map[string]Profile{
		"personal": {Name: "personal", URL: "https://rafiki.example.net"},
	}}), "Save")

	got, err := Resolve(Selection{Flag: "personal"})
	c.NoError(err, "Resolve")
	c.Eq("https://rafiki.example.net", got.Proxy, "Proxy")
}

// TestResolveExplicitProxyWinsOverDerivation checks that a genuine --proxy
// choice is never overridden by the url-derivation default.
func TestResolveExplicitProxyWinsOverDerivation(t *testing.T) {
	c := assert.NewAborting(t)
	setXDG(t)
	c.NoError(Save(Set{Profiles: map[string]Profile{
		"personal": {Name: "personal", URL: "https://rafiki.example.net", Proxy: "https://proxy.example.net"},
	}}), "Save")

	got, err := Resolve(Selection{Flag: "personal"})
	c.NoError(err, "Resolve")
	c.Eq("https://proxy.example.net", got.Proxy, "Proxy")
}

// TestResolveDoesNotDeriveProxyForASocketProfile checks that a local daemon
// with no `proxy` set stays proxy-less: there is no url to derive one from,
// and deriving from the socket path would be nonsense.
func TestResolveDoesNotDeriveProxyForASocketProfile(t *testing.T) {
	c := assert.NewAborting(t)
	setXDG(t)
	c.NoError(Save(Set{Profiles: map[string]Profile{
		"work": {Name: "work", Socket: "/tmp/work.sock"},
	}}), "Save")

	got, err := Resolve(Selection{Flag: "work"})
	c.NoError(err, "Resolve")
	c.Eq("", got.Proxy, "Proxy")
}

func TestDescribeNamesTheEndpoint(t *testing.T) {
	local := Resolved{Profile: Profile{Name: "work", Socket: "/tmp/ctl.sock"}}
	if got := local.Describe(); !strings.Contains(got, "work") || !strings.Contains(got, "/tmp/ctl.sock") {
		t.Errorf("Describe() = %q", got)
	}
	remote := Resolved{Profile: Profile{Name: "personal", URL: "https://h"}}
	if got := remote.Describe(); !strings.Contains(got, "personal") || !strings.Contains(got, "https://h") {
		t.Errorf("Describe() = %q", got)
	}
}
