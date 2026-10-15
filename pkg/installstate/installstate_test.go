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
