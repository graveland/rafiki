// SPDX-License-Identifier: Apache-2.0

package profile

import (
	"path/filepath"
	"strings"
	"testing"
)

// setXDG points every base directory at t.TempDir() so path tests assert
// structure without depending on the developer's home.
func setXDG(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, "config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(root, "state"))
	t.Setenv("XDG_RUNTIME_DIR", filepath.Join(root, "run"))
	return root
}

func TestPathsAreProfileScoped(t *testing.T) {
	root := setXDG(t)

	cases := []struct {
		name string
		got  string
		want string
	}{
		{"profiles.toml", ProfilesFile(), filepath.Join(root, "config", "rafiki", "profiles.toml")},
		{"pointer", PointerFile(), filepath.Join(root, "config", "rafiki", "current-profile")},
		{"dir", Dir("work"), filepath.Join(root, "config", "rafiki", "profiles", "work")},
		{"token", TokenFile("work"), filepath.Join(root, "config", "rafiki", "profiles", "work", "token")},
		{"presets", PresetsFile("work"), filepath.Join(root, "config", "rafiki", "profiles", "work", "presets.json")},
		{"active", ActiveFile("work"), filepath.Join(root, "run", "rafiki", "profiles", "work", "active")},
		{"state", StateFile("work"), filepath.Join(root, "state", "rafiki", "profiles", "work", "client-state.json")},
	}
	for _, tc := range cases {
		if tc.got != tc.want {
			t.Errorf("%s = %q, want %q", tc.name, tc.got, tc.want)
		}
	}
}

func TestTwoProfilesNeverShareAPath(t *testing.T) {
	setXDG(t)
	for _, fn := range []func(string) string{Dir, TokenFile, PresetsFile, ActiveFile, StateFile} {
		a, b := fn("work"), fn("personal")
		if a == b {
			t.Fatalf("two profiles share a path: %q", a)
		}
		if !strings.Contains(a, "work") || !strings.Contains(b, "personal") {
			t.Fatalf("path does not name its profile: %q / %q", a, b)
		}
	}
}
