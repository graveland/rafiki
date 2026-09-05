// SPDX-License-Identifier: Apache-2.0

package profile

import (
	"os"
	"strings"
	"testing"

	"go.graveland.dev/rafiki/pkg/paths"
)

// seed writes a two-profile manifest and returns nothing; every resolution
// test starts from the same fixture so only the selection varies.
func seed(t *testing.T) {
	t.Helper()
	err := Save(Set{Profiles: map[string]Profile{
		"work":     {Name: "work", Socket: "/tmp/work.sock"},
		"personal": {Name: "personal", URL: "https://rafiki.example.net"},
	}})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
}

func TestResolvePrefersFlagThenEnvThenPointer(t *testing.T) {
	setXDG(t)
	seed(t)
	if err := SavePointer("work"); err != nil {
		t.Fatalf("SavePointer: %v", err)
	}

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
			got, err := Resolve(tc.sel)
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			if got.Name != tc.want {
				t.Fatalf("resolved %q, want %q", got.Name, tc.want)
			}
		})
	}
}

func TestResolveCarriesTheProfilesOwnToken(t *testing.T) {
	setXDG(t)
	seed(t)
	if err := WriteToken("personal", "sk-personal"); err != nil {
		t.Fatalf("WriteToken: %v", err)
	}
	if err := WriteToken("work", "sk-work"); err != nil {
		t.Fatalf("WriteToken: %v", err)
	}

	p, err := Resolve(Selection{Flag: "personal"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if p.Token != "sk-personal" {
		t.Fatalf("token = %q, want sk-personal — the whole point is that the credential travels with the endpoint", p.Token)
	}
}

func TestResolveRejectsAnUnknownName(t *testing.T) {
	setXDG(t)
	seed(t)
	_, err := Resolve(Selection{Flag: "nope"})
	if err == nil {
		t.Fatal("Resolve(nope) = nil error")
	}
	for _, want := range []string{"nope", "work", "personal"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

func TestResolveWithAManifestButNothingSelectedIsAnError(t *testing.T) {
	setXDG(t)
	seed(t)
	// No pointer file written.
	_, err := Resolve(Selection{})
	if err == nil {
		t.Fatal("Resolve with no selection = nil error; it must not silently pick one")
	}
	if !strings.Contains(err.Error(), "rafiki profile use") {
		t.Fatalf("error %q does not tell the user how to fix it", err)
	}
}

func TestResolveBootstrapsWhenThereIsNoManifestAtAll(t *testing.T) {
	setXDG(t)

	got, err := Resolve(Selection{})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if !got.Bootstrapped {
		t.Fatal("Bootstrapped = false; the caller needs this to print its notice")
	}
	if got.Name != DefaultName {
		t.Fatalf("bootstrapped profile = %q, want %q", got.Name, DefaultName)
	}
	if got.Socket != paths.SocketPath() {
		t.Fatalf("bootstrapped socket = %q, want the XDG default %q", got.Socket, paths.SocketPath())
	}
	if got.Proxy == "" {
		t.Fatal("bootstrapped profile has no proxy; `rafiki claude` would have no default URL")
	}

	// It must be durable, not computed fresh each time.
	if _, err := os.Stat(ProfilesFile()); err != nil {
		t.Fatalf("bootstrap did not write %s: %v", ProfilesFile(), err)
	}
	if LoadPointer() != DefaultName {
		t.Fatalf("bootstrap did not write the pointer (got %q)", LoadPointer())
	}

	// A second call must NOT re-report a bootstrap.
	again, err := Resolve(Selection{})
	if err != nil {
		t.Fatalf("second Resolve: %v", err)
	}
	if again.Bootstrapped {
		t.Fatal("Bootstrapped = true on the second call; the notice would print forever")
	}
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
			// Clear any leftover manifest.
			os.Remove(ProfilesFile())

			_, err := Resolve(tc.sel)
			if err == nil {
				t.Fatal("Resolve with explicit selection on a bare machine = nil error; it must refuse to bootstrap")
			}
			if !strings.Contains(err.Error(), "somename") {
				t.Errorf("error %q does not name the requested profile", err)
			}
			if !strings.Contains(err.Error(), "rafiki profile add") {
				t.Errorf("error %q does not point at the fix", err)
			}

			// Verify bootstrap did NOT run as a side effect.
			if _, err := os.Stat(ProfilesFile()); err == nil {
				t.Fatal("profiles.toml was created; Bootstrap() should not have run")
			}
		})
	}
}

func TestResolveErrorMessageUsesCorrectPrecedence(t *testing.T) {
	setXDG(t)
	// No manifest at all; no pointer file.
	// When both Flag and Env are set on a bare machine, the error should
	// name the Flag value (which wins by precedence), not concatenate both.

	os.Remove(ProfilesFile())
	_, err := Resolve(Selection{Flag: "foo", Env: "bar", EnvSet: true})
	if err == nil {
		t.Fatal("Resolve with both Flag and Env on a bare machine = nil error")
	}
	if !strings.Contains(err.Error(), "foo") {
		t.Errorf("error %q does not name the Flag value", err)
	}
	// Verify it does NOT say "foobar" or "foo" + "bar" concatenated.
	if strings.Contains(err.Error(), "foobar") {
		t.Errorf("error %q incorrectly concatenates Flag and Env", err)
	}
	if strings.Contains(err.Error(), "bar") {
		t.Errorf("error %q should not mention the Env value when Flag is set", err)
	}
}

func TestCheckRetiredEnvNamesTheVariableAndTheFix(t *testing.T) {
	for _, name := range []string{
		"RAFIKI_URL", "RAFIKI_TOKEN", "RAFIKI_SOCKET",
		"RAFIKI_DEFAULT_MODEL", "RAFIKI_DEFAULT_PRESET", "RAFIKI_DEFAULT_LABELS",
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv(name, "something")
			err := CheckRetiredEnv()
			if err == nil {
				t.Fatalf("CheckRetiredEnv with %s set = nil error", name)
			}
			if !strings.Contains(err.Error(), name) {
				t.Errorf("error %q does not name %s", err, name)
			}
			if !strings.Contains(err.Error(), "rafiki profile") {
				t.Errorf("error %q does not point at the replacement", err)
			}
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
	if err := CheckRetiredEnv(); err != nil {
		t.Fatalf("CheckRetiredEnv = %v, want nil", err)
	}
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
	if err := CheckRetiredEnv(); err != nil {
		t.Fatalf("CheckRetiredEnv with every retired var present-but-empty (via t.Setenv, not os.Unsetenv) = %v, want nil", err)
	}
}

// TestResolveDerivesProxyFromURL pins Fix 5 (design spec: "For a url profile
// it defaults to that same URL -- one TLS listener serves the control plane
// and the proxy face"). No task implemented this, so `rafiki claude` against
// a freshly-added url profile with no --proxy errored "profile has no proxy
// URL" even though docs/MIGRATING.md's worked example for a remote profile
// never passes --proxy.
func TestResolveDerivesProxyFromURL(t *testing.T) {
	setXDG(t)
	if err := Save(Set{Profiles: map[string]Profile{
		"personal": {Name: "personal", URL: "https://rafiki.example.net"},
	}}); err != nil {
		t.Fatalf("Save: %v", err)
	}

	got, err := Resolve(Selection{Flag: "personal"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Proxy != "https://rafiki.example.net" {
		t.Fatalf("Proxy = %q, want the derived url", got.Proxy)
	}
}

// TestResolveExplicitProxyWinsOverDerivation checks that a genuine --proxy
// choice is never overridden by the url-derivation default.
func TestResolveExplicitProxyWinsOverDerivation(t *testing.T) {
	setXDG(t)
	if err := Save(Set{Profiles: map[string]Profile{
		"personal": {Name: "personal", URL: "https://rafiki.example.net", Proxy: "https://proxy.example.net"},
	}}); err != nil {
		t.Fatalf("Save: %v", err)
	}

	got, err := Resolve(Selection{Flag: "personal"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Proxy != "https://proxy.example.net" {
		t.Fatalf("Proxy = %q, want the explicit proxy unchanged", got.Proxy)
	}
}

// TestResolveDoesNotDeriveProxyForASocketProfile checks that a local daemon
// with no `proxy` set stays proxy-less: there is no url to derive one from,
// and deriving from the socket path would be nonsense.
func TestResolveDoesNotDeriveProxyForASocketProfile(t *testing.T) {
	setXDG(t)
	if err := Save(Set{Profiles: map[string]Profile{
		"work": {Name: "work", Socket: "/tmp/work.sock"},
	}}); err != nil {
		t.Fatalf("Save: %v", err)
	}

	got, err := Resolve(Selection{Flag: "work"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Proxy != "" {
		t.Fatalf("Proxy = %q, want empty for a socket profile with none set", got.Proxy)
	}
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
