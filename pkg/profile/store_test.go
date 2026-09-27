// SPDX-License-Identifier: Apache-2.0

package profile

import (
	"os"
	"testing"

	"github.com/multigres/testkit/assert"
)

func TestLoadWithNoManifestSaysSo(t *testing.T) {
	setXDG(t)
	_, err := Load()
	assert.NewAborting(t).ErrorIs(err, ErrNoManifest, "Load() error")
}

func TestSaveThenLoadRoundTrips(t *testing.T) {
	c := assert.NewAborting(t)
	setXDG(t)

	in := Set{Profiles: map[string]Profile{
		"work": {
			Name: "work", Socket: "/tmp/ctl.sock", Proxy: "http://localhost:8035",
			Kind: "claude", Model: "claude-opus-5",
			Labels: map[string]string{"env": "work"},
		},
		"personal": {Name: "personal", URL: "https://rafiki.example.net", Preset: "cheap"},
	}}
	c.NoError(Save(in), "Save")

	out, err := Load()
	c.NoError(err, "Load")
	w, ok := out.Get("work")
	c.True(ok, "work missing after round trip")
	c.False(w.Socket != "/tmp/ctl.sock" || w.Kind != "claude" || w.Labels["env"] != "work", "work round-tripped as %+v", w)
	p, _ := out.Get("personal")
	c.False(p.URL != "https://rafiki.example.net" || p.Preset != "cheap", "personal round-tripped as %+v", p)
}

func TestManifestIsNotWorldReadable(t *testing.T) {
	c := assert.NewAborting(t)
	setXDG(t)
	c.NoError(Save(Set{Profiles: map[string]Profile{"a": {Name: "a", Socket: "/s"}}}), "Save")
	fi, err := os.Stat(ProfilesFile())
	c.NoError(err, "stat")
	c.Eq(0, fi.Mode().Perm()&0o077, "profiles.toml mode = %v, want no group/other bits", fi.Mode().Perm())
}

func TestPointerRoundTripsAndDegradesQuietly(t *testing.T) {
	c := assert.NewAborting(t)
	setXDG(t)
	c.Eq("", LoadPointer(), "LoadPointer with no file")
	c.NoError(SavePointer("work"), "SavePointer")
	c.Eq("work", LoadPointer(), "LoadPointer")
}

func TestTokenRoundTripsAt0600(t *testing.T) {
	c := assert.NewAborting(t)
	setXDG(t)
	c.Eq("", ReadToken("work"), "ReadToken with no file")
	c.NoError(WriteToken("work", "sk-test\n"), "WriteToken")
	c.Eq("sk-test", ReadToken("work"), "ReadToken")
	fi, err := os.Stat(TokenFile("work"))
	c.NoError(err, "stat")
	c.Eq(0o600, fi.Mode().Perm(), "token mode")
}
