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

// TokenFromEnv is the single credential resolver for BOTH surfaces — the
// control plane's ctrl_auth frame and the proxy face's Authorization header —
// and had no direct coverage. Its precedence is load-bearing: an empty result
// is what puts a client into the daemon's bootstrap path, so a resolver that
// wrongly returned a stale file value would silently turn a bootstrap dial
// into an authenticated one that then fails.
func TestTokenFromEnv(t *testing.T) {
	write := func(t *testing.T, content string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(TokenFile()), 0o700); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(TokenFile(), []byte(content), 0o600); err != nil {
			t.Fatalf("write token file: %v", err)
		}
	}

	t.Run("env wins over the file", func(t *testing.T) {
		t.Setenv("XDG_CONFIG_HOME", t.TempDir())
		t.Setenv(Token, "from-env")
		write(t, "from-file\n")
		if got := TokenFromEnv(); got != "from-env" {
			t.Errorf("TokenFromEnv() = %q, want %q", got, "from-env")
		}
	})

	t.Run("falls back to the file when env is empty", func(t *testing.T) {
		t.Setenv("XDG_CONFIG_HOME", t.TempDir())
		t.Setenv(Token, "")
		write(t, "from-file\n")
		if got := TokenFromEnv(); got != "from-file" {
			t.Errorf("TokenFromEnv() = %q, want %q", got, "from-file")
		}
	})

	// The file is written with a trailing newline; a token carrying one would
	// be rejected by the daemon as an unknown credential.
	t.Run("trims surrounding whitespace from the file", func(t *testing.T) {
		t.Setenv("XDG_CONFIG_HOME", t.TempDir())
		t.Setenv(Token, "")
		write(t, "  rfk_padded\n\n")
		if got := TokenFromEnv(); got != "rfk_padded" {
			t.Errorf("TokenFromEnv() = %q, want %q", got, "rfk_padded")
		}
	})

	// Empty is not an error — it is the signal that puts a client on the
	// bootstrap path, where it sends no ctrl_auth frame at all.
	t.Run("no env and no file is empty, not an error", func(t *testing.T) {
		t.Setenv("XDG_CONFIG_HOME", t.TempDir())
		t.Setenv(Token, "")
		if got := TokenFromEnv(); got != "" {
			t.Errorf("TokenFromEnv() = %q, want empty", got)
		}
	})

	// A file containing only whitespace must resolve empty rather than to a
	// blank token, which would be presented as a credential and rejected.
	t.Run("a whitespace-only file resolves empty", func(t *testing.T) {
		t.Setenv("XDG_CONFIG_HOME", t.TempDir())
		t.Setenv(Token, "")
		write(t, "   \n\t\n")
		if got := TokenFromEnv(); got != "" {
			t.Errorf("TokenFromEnv() = %q, want empty", got)
		}
	})
}
