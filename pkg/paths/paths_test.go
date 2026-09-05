package paths

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
)

// The XDG env vars must win when set — that is the whole point of the spec, and
// it is also how the tests below stay off the developer's real home directory.
func TestXDGEnvOverrides(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "/tmp/xc")
	t.Setenv("XDG_DATA_HOME", "/tmp/xd")
	t.Setenv("XDG_STATE_HOME", "/tmp/xs")
	t.Setenv("XDG_RUNTIME_DIR", "/tmp/xr")

	for _, tc := range []struct {
		name string
		got  string
		want string
	}{
		{"ConfigDir", ConfigDir(), "/tmp/xc/rafiki"},
		{"DataDir", DataDir(), "/tmp/xd/rafiki"},
		{"StateDir", StateDir(), "/tmp/xs/rafiki"},
		{"RuntimeDir", RuntimeDir(), "/tmp/xr/rafiki"},
	} {
		if tc.got != tc.want {
			t.Errorf("%s = %q, want %q", tc.name, tc.got, tc.want)
		}
	}
}

// With the env unset, the spec's documented defaults apply — NOT ~/.pi, which
// belongs to the pi agent and is not rafiki's to write its runtime state into.
func TestDefaultsFollowXDGSpec(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("XDG_DATA_HOME", "")
	t.Setenv("XDG_STATE_HOME", "")
	t.Setenv("XDG_RUNTIME_DIR", "")
	t.Setenv("HOME", "/home/tester")

	for _, tc := range []struct {
		name string
		got  string
		want string
	}{
		{"ConfigDir", ConfigDir(), "/home/tester/.config/rafiki"},
		{"DataDir", DataDir(), "/home/tester/.local/share/rafiki"},
		{"StateDir", StateDir(), "/home/tester/.local/state/rafiki"},
		// No XDG_RUNTIME_DIR (the normal case on macOS): fall back to the state
		// dir rather than inventing a path outside the spec's directories.
		{"RuntimeDir", RuntimeDir(), "/home/tester/.local/state/rafiki"},
	} {
		if tc.got != tc.want {
			t.Errorf("%s = %q, want %q", tc.name, tc.got, tc.want)
		}
	}
}

// Nothing rafiki owns may live under ~/.pi — that directory belongs to pi, and
// a daemon must not write its own state into another program's config directory.
func TestNoPathLandsUnderDotPi(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("XDG_DATA_HOME", "")
	t.Setenv("XDG_STATE_HOME", "")
	t.Setenv("XDG_RUNTIME_DIR", "")
	t.Setenv("HOME", "/home/tester")

	for name, p := range map[string]string{
		"ConfigDir":      ConfigDir(),
		"DataDir":        DataDir(),
		"StateDir":       StateDir(),
		"RuntimeDir":     RuntimeDir(),
		"SocketPath":     SocketPath(),
		"RecordsDir":     RecordsDir(),
		"LogsDir":        LogsDir(),
		"ServiceLogPath": ServiceLogPath(),
	} {
		if strings.Contains(p, "/.pi/") || strings.HasSuffix(p, "/.pi") {
			t.Errorf("%s = %q, must not live under ~/.pi", name, p)
		}
	}
}

// A unix socket path is bounded by sun_path (104 bytes on darwin, 108 on
// linux). The default location must leave comfortable headroom, because a
// too-long path fails at bind() with a confusing "invalid argument".
func TestSocketPathFitsSunPath(t *testing.T) {
	t.Setenv("XDG_RUNTIME_DIR", "")
	t.Setenv("XDG_STATE_HOME", "")
	t.Setenv("HOME", "/Users/a-reasonably-long-username")

	if got := len(SocketPath()); got > 104 {
		t.Errorf("SocketPath() is %d bytes (%q), exceeds sun_path", got, SocketPath())
	}
}

// The derived paths must sit under their respective base dirs, so a caller that
// overrides one env var moves everything that belongs to it.
func TestDerivedPathsNestUnderBases(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", "/tmp/d")
	t.Setenv("XDG_STATE_HOME", "/tmp/s")
	t.Setenv("XDG_RUNTIME_DIR", "/tmp/r")

	if got, want := RecordsDir(), filepath.Join("/tmp/d/rafiki", "state"); got != want {
		t.Errorf("RecordsDir = %q, want %q (session records are persistent data)", got, want)
	}
	if got, want := LogsDir(), filepath.Join("/tmp/s/rafiki", "logs"); got != want {
		t.Errorf("LogsDir = %q, want %q", got, want)
	}
	if got, want := SocketPath(), filepath.Join("/tmp/r/rafiki", "controller.sock"); got != want {
		t.Errorf("SocketPath = %q, want %q", got, want)
	}
	if got, want := ServiceLogPath(), filepath.Join("/tmp/s/rafiki", "controller.log"); got != want {
		t.Errorf("ServiceLogPath = %q, want %q", got, want)
	}
}

func TestSkillsDirs_DefaultIsConfigDir(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "/tmp/cfg")
	t.Setenv("RAFIKI_SKILLS_DIRS", "")
	got := SkillsDirs()
	want := []string{"/tmp/cfg/rafiki/skills"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("SkillsDirs() = %v, want %v", got, want)
	}
}

func TestSkillsDirs_SplitsPathList(t *testing.T) {
	t.Setenv("RAFIKI_SKILLS_DIRS", "/a"+string(os.PathListSeparator)+"/b")
	got := SkillsDirs()
	want := []string{"/a", "/b"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("SkillsDirs() = %v, want %v", got, want)
	}
}

func TestSkillsDirs_DropsEmptySegments(t *testing.T) {
	sep := string(os.PathListSeparator)
	t.Setenv("RAFIKI_SKILLS_DIRS", sep+"/a"+sep+sep+"/b"+sep)
	got := SkillsDirs()
	want := []string{"/a", "/b"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("SkillsDirs() = %v, want %v", got, want)
	}
}

func TestSkillsDirs_AllEmptySegmentsFallsBackToDefault(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "/tmp/cfg")
	sep := string(os.PathListSeparator)
	t.Setenv("RAFIKI_SKILLS_DIRS", sep+sep+sep)

	got := SkillsDirs()
	want := []string{"/tmp/cfg/rafiki/skills"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("SkillsDirs() = %v, want %v — a variable of only separators must fall back to the default, not yield an empty slice", got, want)
	}
}

func TestInstructionsFile_EnvWins(t *testing.T) {
	t.Setenv("RAFIKI_INSTRUCTIONS", "/custom/inst.md")
	if got := InstructionsFile(); got != "/custom/inst.md" {
		t.Fatalf("InstructionsFile() = %q, want /custom/inst.md", got)
	}
}

func TestNoClaudeOrPiPathsLeak(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "/tmp/cfg")
	for _, env := range []string{"RAFIKI_INSTRUCTIONS", "RAFIKI_SKILLS_DIRS", "RAFIKI_MCP_CONFIG"} {
		t.Setenv(env, "")
	}
	all := append(SkillsDirs(),
		InstructionsFile(), PresetsFile(), GlobalMCPConfig())
	for _, p := range all {
		if strings.Contains(p, "/.claude") || strings.Contains(p, "/.pi/") {
			t.Errorf("fundi config path leaks into a foreign tool's directory: %s", p)
		}
	}
}

// When HOME cannot be resolved, base() must not silently hand back a relative
// path with no trace anywhere — that silence is exactly the I10 defect this
// pins down. Setting HOME to the empty string is enough to force the failure
// deterministically: os.UserHomeDir on Unix reads only $HOME (no getpwuid
// fallback), so this never touches the real HOME outside the test.
func TestBase_HomeDirUnresolvable_FallsBackToRelativePath(t *testing.T) {
	homeDirWarnOnce = sync.Once{}
	t.Setenv("HOME", "")
	t.Setenv("XDG_CONFIG_HOME", "")

	got := ConfigDir()
	want := filepath.Join(".config", "rafiki")
	if got != want {
		t.Fatalf("ConfigDir() = %q, want %q", got, want)
	}
	if filepath.IsAbs(got) {
		t.Fatalf("ConfigDir() = %q, must be relative to the caller's cwd when home cannot be resolved — that is the pre-existing fallback behaviour this task makes observable, not a new one", got)
	}
}

// The invariant is two-sided: the failure must be observable in the log, and
// it must not spam — base() runs on every path resolution (SocketPath(),
// InstructionsFile(), etc.), so logging unconditionally would flood a
// long-lived rafikid with the same fact on every request.
func TestBase_HomeDirUnresolvable_WarnsExactlyOnceAcrossRepeatedCalls(t *testing.T) {
	homeDirWarnOnce = sync.Once{}
	t.Setenv("HOME", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("XDG_DATA_HOME", "")
	t.Setenv("XDG_STATE_HOME", "")

	var buf bytes.Buffer
	prevLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prevLogger) })

	// Three distinct call sites, all funnelling through base(): the guard must
	// be process-wide, not re-armed per envVar or per exported function.
	_ = ConfigDir()
	_ = DataDir()
	_ = StateDir()

	if got := strings.Count(buf.String(), "cannot determine home directory"); got != 1 {
		t.Fatalf("warning logged %d times across 3 calls, want exactly 1 (sync.Once must guard the whole process, not fire per call or per envVar)\nlog output:\n%s", got, buf.String())
	}
}

func TestProvidersFile(t *testing.T) {
	t.Setenv(Providers, "")
	t.Setenv("XDG_CONFIG_HOME", "/tmp/xdg-config-test")
	if got, want := ProvidersFile(), "/tmp/xdg-config-test/rafiki/providers.toml"; got != want {
		t.Errorf("ProvidersFile() = %q, want %q", got, want)
	}
	t.Setenv(Providers, "/etc/rafiki/p.toml")
	if got, want := ProvidersFile(), "/etc/rafiki/p.toml"; got != want {
		t.Errorf("ProvidersFile() with RAFIKI_PROVIDERS = %q, want %q", got, want)
	}
}

func TestConnectSocketPathIsBesideTheControlSocket(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", dir)

	got := ConnectSocketPath()
	want := filepath.Join(dir, "rafiki", "connect.sock")
	if got != want {
		t.Fatalf("ConnectSocketPath() = %q, want %q", got, want)
	}
	if filepath.Dir(got) != filepath.Dir(SocketPath()) {
		t.Fatalf("connect socket %q is not beside the control socket %q", got, SocketPath())
	}
}
