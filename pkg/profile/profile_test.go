// SPDX-License-Identifier: Apache-2.0

package profile

import (
	"strings"
	"testing"
)

func TestParseReadsBothEndpointKinds(t *testing.T) {
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
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got := s.Names(); len(got) != 2 || got[0] != "personal" || got[1] != "work" {
		t.Fatalf("Names() = %v, want [personal work]", got)
	}

	w, ok := s.Get("work")
	if !ok {
		t.Fatal("Get(work): not found")
	}
	if w.Name != "work" {
		t.Errorf("Name = %q, want work", w.Name)
	}
	if w.Socket != "/tmp/ctl.sock" || w.URL != "" {
		t.Errorf("endpoint = socket:%q url:%q", w.Socket, w.URL)
	}
	if w.Proxy != "http://localhost:8035" || w.Kind != "claude" || w.Model != "claude-opus-5" {
		t.Errorf("defaults = %+v", w)
	}
	if w.Labels["env"] != "work" {
		t.Errorf("Labels = %v, want env=work", w.Labels)
	}

	p, _ := s.Get("personal")
	if p.URL != "https://rafiki.example.net" || p.Socket != "" {
		t.Errorf("personal endpoint = socket:%q url:%q", p.Socket, p.URL)
	}
	if p.Preset != "cheap" {
		t.Errorf("Preset = %q, want cheap", p.Preset)
	}
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
			_, err := Parse([]byte(tc.toml))
			if err == nil {
				t.Fatalf("Parse(%q) = nil error, want one containing %q", tc.toml, tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Parse error = %q, want it to contain %q", err, tc.want)
			}
		})
	}
}

func TestParseAcceptsAnEmptyFile(t *testing.T) {
	s, err := Parse(nil)
	if err != nil {
		t.Fatalf("Parse(nil): %v", err)
	}
	if len(s.Names()) != 0 {
		t.Fatalf("Names() = %v, want empty", s.Names())
	}
	if _, ok := s.Get("anything"); ok {
		t.Fatal("Get on an empty Set returned ok")
	}
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
			err := ValidName(tc.name)
			if tc.wantErr && err == nil {
				t.Fatalf("ValidName(%q) = nil error, want one", tc.name)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("ValidName(%q) = %v, want nil", tc.name, err)
			}
			if tc.wantErr && !strings.Contains(err.Error(), tc.name) && tc.name != "" {
				t.Fatalf("ValidName error %q does not name the rejected name %q", err, tc.name)
			}
		})
	}
}
