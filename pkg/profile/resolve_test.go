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
