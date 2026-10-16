// SPDX-License-Identifier: Apache-2.0

package installstate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/multigres/testkit/assert"
)

func isolate(t *testing.T) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
}

func TestLoadMissingFileIsNotAnError(t *testing.T) {
	isolate(t)
	c := assert.NewCollecting(t)

	_, ok, err := Load()
	c.NoError(err, "Load of a missing file must not error")
	c.False(ok, "Load of a missing file reports ok == false")
}

func TestSaveLoadRoundTrip(t *testing.T) {
	isolate(t)
	c := assert.NewCollecting(t)

	no := false
	want := State{
		Version:  SchemaVersion,
		Channel:  ChannelBrew,
		LastStep: "executor",
		DB: DB{
			Mode:      DBModeManaged,
			Container: "rafiki-pg",
			Volume:    "rafiki-pgdata",
			Port:      5432,
			Image:     "postgres:17",
		},
		Daemon:   Daemon{Expose: &no},
		Identity: Identity{User: "alice", Profile: "default"},
		Executor: Executor{
			Connect:    ExecutorConnectTCP,
			Address:    "127.0.0.1:9000",
			PinCert:    "sha256:abc",
			ServerName: "executor.local",
			Root:       "/var/lib/rafiki",
			Launch:     []string{"rafiki", "executor", "serve"},
			Env:        []string{"FOO", "BAR"},
			RTK:        "rtk",
		},
		Optional: []string{"web", "recall"},
	}

	c.NoError(Save(want), "Save")

	info, err := os.Stat(Path())
	c.NoError(err, "Stat of the saved file")
	if err == nil {
		c.Eq(os.FileMode(0o600), info.Mode().Perm(), "install.toml must be 0600")
	}

	got, ok, err := Load()
	c.NoError(err, "Load")
	c.True(ok, "Load of a saved file reports ok == true")
	c.EqDiff(want, got, "round trip")
}

// TestProvidersSHARoundTrip pins that the providers step's recorded file hash
// survives a Save/Load round trip, so a re-run can recognize its own file.
func TestProvidersSHARoundTrip(t *testing.T) {
	isolate(t)
	c := assert.NewCollecting(t)

	want := State{Version: SchemaVersion, Channel: ChannelSource, Providers: Providers{FileSHA256: "abc123def456"}}
	c.NoError(Save(want), "Save")

	got, ok, err := Load()
	c.Require().NoError(err, "Load")
	c.True(ok, "Load of a saved file reports ok == true")
	c.Eq(want.Providers.FileSHA256, got.Providers.FileSHA256, "FileSHA256 must survive")
}

func TestExposeNilSurvivesRoundTrip(t *testing.T) {
	isolate(t)
	c := assert.NewCollecting(t)

	want := State{Version: SchemaVersion, Channel: ChannelSource}
	c.NoError(Save(want), "Save")

	got, ok, err := Load()
	c.NoError(err, "Load")
	c.True(ok, "ok")
	c.True(got.Daemon.Expose == nil, "a never-asked Expose must survive as nil, got %v", got.Daemon.Expose)
}

// TestSaveRejectsInvalidState: Save validates before writing, so an invalid
// State is refused AND leaves no file behind — a later Load must not read back
// something Save would have refused.
func TestSaveRejectsInvalidState(t *testing.T) {
	isolate(t)
	c := assert.NewCollecting(t)

	err := Save(State{DB: DB{Port: 70000}})
	c.Error(err, "Save must refuse an out-of-range db.port")
	c.NoFileExists(Path(), "a refused Save must write nothing")
}

// TestValidatePortBounds pins DB.Port's accepted range: 0 (unset) and 1..65535
// are valid; -1 and 65536 are not.
func TestValidatePortBounds(t *testing.T) {
	isolate(t)
	c := assert.NewCollecting(t)

	ok := []int{0, 1, 65535}
	for _, port := range ok {
		c.NoError(State{DB: DB{Port: port}}.Validate(), "db.port %d must be valid", port)
	}
	bad := []int{-1, 65536}
	for _, port := range bad {
		c.Error(State{DB: DB{Port: port}}.Validate(), "db.port %d must be refused", port)
	}
}

// TestEnvNilVsEmptyRoundTrip pins the tri-state: a never-asked Env is nil, a
// deliberately-empty one is a non-nil empty slice, and both survive a
// Save/Load round trip distinctly.
func TestEnvNilVsEmptyRoundTrip(t *testing.T) {
	isolate(t)
	c := assert.NewCollecting(t)

	c.NoError(Save(State{Executor: Executor{Env: []string{}}}), "Save with an explicit empty Env")
	got, ok, err := Load()
	c.Require().NoError(err, "Load")
	c.True(ok, "ok")
	c.True(got.Executor.Env != nil, "an explicit empty Env must stay non-nil, got %v", got.Executor.Env)
	c.Empty(got.Executor.Env, "an explicit empty Env must stay empty")

	c.NoError(Save(State{}), "Save with a nil Env")
	got, ok, err = Load()
	c.Require().NoError(err, "Load")
	c.True(ok, "ok")
	c.True(got.Executor.Env == nil, "a never-asked Env must stay nil, got %v", got.Executor.Env)
}

func TestLoadRejectsUnknownKey(t *testing.T) {
	isolate(t)
	c := assert.NewCollecting(t)

	if err := os.MkdirAll(filepath.Dir(Path()), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(Path(), []byte("version = 1\n[db]\nmdoe = \"managed\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, _, err := Load()
	c.Error(err, "a typo key must not decode silently")
	if err != nil {
		c.True(strings.Contains(err.Error(), "db.mdoe"), "error must name the unknown key, got: %v", err)
	}
}

func TestLoadRejectsUnsupportedVersion(t *testing.T) {
	isolate(t)
	c := assert.NewCollecting(t)

	for _, content := range []string{"version = 2\n", "channel = \"source\"\n"} {
		if err := os.MkdirAll(filepath.Dir(Path()), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(Path(), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		_, _, err := Load()
		c.Error(err, "Load(%q) must refuse", content)
	}
}

func TestValidateRejectsUnknownEnum(t *testing.T) {
	isolate(t)
	c := assert.NewCollecting(t)

	tests := []struct {
		name  string
		state State
		field string
	}{
		{"channel", State{Channel: "postgres"}, "channel"},
		{"db.mode", State{DB: DB{Mode: "postgres"}}, "db.mode"},
		{"executor.connect", State{Executor: Executor{Connect: "pipe"}}, "executor.connect"},
	}
	for _, tt := range tests {
		err := tt.state.Validate()
		c.Error(err, "Validate(%s) must refuse an unknown value", tt.name)
		if err != nil {
			c.True(strings.Contains(err.Error(), tt.field), "error must name %s, got: %v", tt.field, err)
		}
	}

	// The unset constant and every real value are accepted.
	c.NoError(State{}.Validate(), "the zero State is valid (all unset)")
	c.NoError(State{Channel: ChannelBrew, DB: DB{Mode: DBModeDSN}, Executor: Executor{Connect: ExecutorConnectSocket}}.Validate(), "real values are valid")
}

func TestDetectChannel(t *testing.T) {
	c := assert.NewCollecting(t)

	tests := []struct {
		exe     string
		version string
		want    Channel
	}{
		{"/opt/homebrew/Cellar/rafiki/0.1.0/bin/rafiki", "v0.1.0", ChannelBrew},
		{"/opt/homebrew/Caskroom/rafiki/0.1.0/rafiki", "v0.1.0", ChannelBrew},
		{"/home/u/.local/bin/rafiki", "v0.1.0", ChannelTarball},
		{"/home/u/.local/bin/rafiki", "c161fbe4-dirty", ChannelSource},
		{"/home/u/.local/bin/rafiki", "v0.1.0-3-gabc", ChannelSource},
	}
	for _, tt := range tests {
		c.Eq(tt.want, DetectChannel(tt.exe, tt.version), "DetectChannel(%q, %q)", tt.exe, tt.version)
	}
}
