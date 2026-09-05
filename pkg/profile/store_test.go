// SPDX-License-Identifier: Apache-2.0

package profile

import (
	"errors"
	"os"
	"testing"
)

func TestLoadWithNoManifestSaysSo(t *testing.T) {
	setXDG(t)
	_, err := Load()
	if !errors.Is(err, ErrNoManifest) {
		t.Fatalf("Load() error = %v, want it to wrap ErrNoManifest", err)
	}
}

func TestSaveThenLoadRoundTrips(t *testing.T) {
	setXDG(t)

	in := Set{Profiles: map[string]Profile{
		"work": {
			Name: "work", Socket: "/tmp/ctl.sock", Proxy: "http://localhost:8035",
			Kind: "claude", Model: "claude-opus-5",
			Labels: map[string]string{"env": "work"},
		},
		"personal": {Name: "personal", URL: "https://rafiki.example.net", Preset: "cheap"},
	}}
	if err := Save(in); err != nil {
		t.Fatalf("Save: %v", err)
	}

	out, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	w, ok := out.Get("work")
	if !ok {
		t.Fatal("work missing after round trip")
	}
	if w.Socket != "/tmp/ctl.sock" || w.Kind != "claude" || w.Labels["env"] != "work" {
		t.Fatalf("work round-tripped as %+v", w)
	}
	p, _ := out.Get("personal")
	if p.URL != "https://rafiki.example.net" || p.Preset != "cheap" {
		t.Fatalf("personal round-tripped as %+v", p)
	}
}

func TestManifestIsNotWorldReadable(t *testing.T) {
	setXDG(t)
	if err := Save(Set{Profiles: map[string]Profile{"a": {Name: "a", Socket: "/s"}}}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	fi, err := os.Stat(ProfilesFile())
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if fi.Mode().Perm()&0o077 != 0 {
		t.Fatalf("profiles.toml mode = %v, want no group/other bits", fi.Mode().Perm())
	}
}

func TestPointerRoundTripsAndDegradesQuietly(t *testing.T) {
	setXDG(t)
	if got := LoadPointer(); got != "" {
		t.Fatalf("LoadPointer with no file = %q, want empty", got)
	}
	if err := SavePointer("work"); err != nil {
		t.Fatalf("SavePointer: %v", err)
	}
	if got := LoadPointer(); got != "work" {
		t.Fatalf("LoadPointer = %q, want work", got)
	}
}

func TestTokenRoundTripsAt0600(t *testing.T) {
	setXDG(t)
	if got := ReadToken("work"); got != "" {
		t.Fatalf("ReadToken with no file = %q, want empty", got)
	}
	if err := WriteToken("work", "sk-test\n"); err != nil {
		t.Fatalf("WriteToken: %v", err)
	}
	if got := ReadToken("work"); got != "sk-test" {
		t.Fatalf("ReadToken = %q, want sk-test (trimmed)", got)
	}
	fi, err := os.Stat(TokenFile("work"))
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("token mode = %v, want 0600", fi.Mode().Perm())
	}
}
