// SPDX-License-Identifier: Apache-2.0

package profile

import (
	"strings"
	"testing"

	"github.com/multigres/testkit/assert"
)

func TestParseReadsBothEndpointKinds(t *testing.T) {
	c := assert.NewCollecting(t)
	s, err := Parse([]byte(`
[profile.work]
socket = "/tmp/ctl.sock"
proxy  = "http://localhost:8035"
kind   = "claude"
model  = "claude-opus-5"
labels = { env = "work" }

[profile.personal]
url    = "https://rafiki.example.net"
kind   = "fundi"
preset = "cheap"
`))
	c.Require().NoError(err, "Parse")
	if got := s.Names(); len(got) != 2 || got[0] != "personal" || got[1] != "work" {
		t.Fatalf("Names() = %v, want [personal work]", got)
	}

	w, ok := s.Get("work")
	c.Require().True(ok, "Get(work): not found")
	c.Eq("work", w.Name, "Name")
	c.False(w.Socket != "/tmp/ctl.sock" || w.URL != "", "endpoint = socket:%q url:%q", w.Socket, w.URL)
	c.False(w.Proxy != "http://localhost:8035" || w.Kind != "claude" || w.Model != "claude-opus-5", "defaults = %+v", w)
	c.Eq("work", w.Labels["env"], "Labels = %v, want env=work", w.Labels)

	p, _ := s.Get("personal")
	c.False(p.URL != "https://rafiki.example.net" || p.Socket != "", "personal endpoint = socket:%q url:%q", p.Socket, p.URL)
	c.Eq("cheap", p.Preset, "Preset")
}

func TestParseRejectsBadProfiles(t *testing.T) {
	cases := []struct {
		name string
		toml string
		want string
	}{
		{
			name: "both endpoints",
			toml: "[profile.x]\nsocket = \"/a\"\nurl = \"https://b\"\n",
			want: "exactly one of",
		},
		{
			name: "neither endpoint",
			toml: "[profile.x]\nproxy = \"http://localhost:8035\"\n",
			want: "exactly one of",
		},
		{
			name: "unknown key",
			toml: "[profile.x]\nsocket = \"/a\"\nsoket = \"/b\"\n",
			want: "unknown key",
		},
		{
			name: "unknown kind",
			toml: "[profile.x]\nsocket = \"/a\"\nkind = \"wombat\"\n",
			want: "kind",
		},
		{
			name: "url is not https",
			toml: "[profile.x]\nurl = \"http://insecure\"\n",
			want: "https",
		},
		{
			name: "empty name",
			toml: "[profile.\"\"]\nsocket = \"/a\"\n",
			want: "empty",
		},
		{
			name: "traversal name",
			toml: "[profile.\"..\"]\nsocket = \"/a\"\n",
			want: "reserved",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := assert.NewAborting(t)
			_, err := Parse([]byte(tc.toml))
			c.Error(err, "Parse(%q) = nil error, want one containing %q", tc.toml, tc.want)
			c.StrContains(err.Error(), tc.want, "Parse error = %q, want it to contain", err)
		})
	}
}

func TestParseAcceptsAnEmptyFile(t *testing.T) {
	c := assert.NewAborting(t)
	s, err := Parse(nil)
	c.NoError(err, "Parse(nil)")
	c.Empty(s.Names(), "Names()")
	_, ok := s.Get("anything")
	c.False(ok, "Get on an empty Set returned ok")
}

// TestValidNameRejectsTraversal pins the guard that stops `rafiki profile
// remove ..` from deleting the whole config directory: filepath.Join(dir, "..")
// cleans to dir itself, so a profile literally named ".." must never reach
// profile.Dir at all.
func TestValidNameRejectsTraversal(t *testing.T) {
	cases := []struct {
		name    string
		wantErr bool
	}{
		{"", true},
		{".", true},
		{"..", true},
		{"a/b", true},
		{"../etc", true},
		{"/etc", true},
		{"work", false},
		{"my-profile", false},
		{"personal_2", false},
	}
	for _, tc := range cases {
		t.Run("name="+tc.name, func(t *testing.T) {
			c := assert.NewAborting(t)
			err := ValidName(tc.name)
			c.False(tc.wantErr && err == nil, "ValidName(%q) = nil error, want one", tc.name)
			c.False(!tc.wantErr && err != nil, "ValidName(%q) = %v, want nil", tc.name, err)
			c.False(tc.wantErr && !strings.Contains(err.Error(), tc.name) && tc.name != "", "ValidName error %q does not name the rejected name %q", err, tc.name)
		})
	}
}
