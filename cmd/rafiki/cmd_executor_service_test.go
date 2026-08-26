package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"go.graveland.dev/rafiki/pkg/paths"
)

func TestAppendProxyArgsNoopWhenEmpty(t *testing.T) {
	args := []string{"executor", "serve"}
	got, err := appendProxyArgs(args, nil)
	if err != nil {
		t.Fatalf("appendProxyArgs: %v", err)
	}
	if !reflect.DeepEqual(got, args) {
		t.Fatalf("got %v, want unchanged %v", got, args)
	}
}

func TestAppendProxyArgsAppendsEachAsARepeatedFlag(t *testing.T) {
	args := []string{"executor", "serve"}
	got, err := appendProxyArgs(args, []string{"vmlx=http://localhost:8005", "ollama=http://localhost:11434"})
	if err != nil {
		t.Fatalf("appendProxyArgs: %v", err)
	}
	want := []string{
		"executor", "serve",
		"--proxy", "vmlx=http://localhost:8005",
		"--proxy", "ollama=http://localhost:11434",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestAppendProxyArgsRejectsMalformedEntryBeforeInstalling(t *testing.T) {
	_, err := appendProxyArgs([]string{"executor", "serve"}, []string{"not-a-pair"})
	if err == nil {
		t.Fatal("expected an error for a proxy flag with no '=name'")
	}
}

// captureExecutorEnv is deliberately broad — the executor runs the operator's
// toolchain, so GOPATH or a proxy variable is load-bearing there — and narrow
// only where capture is inert (the unit owns HOME/PATH), leaks (RAFIKI_* and
// provider keys must not reach every bash child), or is stale-on-arrival
// session/GUI residue. SSH_AUTH_SOCK is captured on purpose: operators fix
// agent socket paths so they survive reboots.
func TestCaptureExecutorEnvKeepsToolchainVarsAndDropsSessionOnes(t *testing.T) {
	environ := []string{
		"HOME=/Users/you",
		"PATH=/usr/bin:/bin",
		"GOPATH=/Users/you/go",
		"GITHUB_TOKEN=ghp_x",
		"http_proxy=http://localhost:8888",
		"NIX_PATH=darwin=https://example.com/nixpkgs.tar.gz",
		"SSH_AUTH_SOCK=/Users/you/.ssh/.agent",
		// Reserved: the daemon's own configuration must not ride along.
		"RAFIKI_DB=postgres://user:pw@db.example.com/rafiki",
		"ANTHROPIC_API_KEY=sk-ant-...",
		"OPENROUTER_API_KEY=sk-or-...",
		"FUNDI_SOMETHING=old-spelling",
		// Shell-session and GUI residue from a real capture.
		"PWD=/Users/you/src",
		"OLDPWD=/tmp",
		"SHLVL=1",
		"_=/usr/local/bin/something",
		"GPG_TTY=/dev/ttys003",
		"DIRENV_DIFF=eJx0kkuTqjgUgP9",
		"DIRENV_DIR=-/Users/you/src",
		"TERM=xterm-256color",
		"COLORTERM=truecolor",
		"COLORFGBG=7;0",
		"TERM_SESSION_ID=w0t5p0:uuid",
		"ITERM_PROFILE=default",
		"LC_TERMINAL=iTerm2",
		"DISPLAY=:0",
		"SECURITYSESSIONID=186bb",
		"XPC_FLAGS=0x0",
		"__CFBundleIdentifier=com.googlecode.iterm2",
		"TMPDIR=/var/folders/xx/T/",
		"EMPTY=",
	}
	got := captureExecutorEnv(environ)
	for _, k := range []string{"GOPATH", "GITHUB_TOKEN", "http_proxy", "NIX_PATH", "SSH_AUTH_SOCK"} {
		if _, ok := got[k]; !ok {
			t.Errorf("captureExecutorEnv dropped %s; toolchain vars must survive", k)
		}
	}
	for _, k := range []string{
		"HOME", "PATH", "TERM", "COLORTERM", "COLORFGBG", "TERM_SESSION_ID",
		"ITERM_PROFILE", "LC_TERMINAL", "DISPLAY", "SECURITYSESSIONID", "XPC_FLAGS",
		"__CFBundleIdentifier", "TMPDIR", "PWD", "OLDPWD", "SHLVL", "_", "GPG_TTY",
		"DIRENV_DIFF", "DIRENV_DIR", "EMPTY",
		// Reserved rafiki variables and provider keys.
		"RAFIKI_DB", "ANTHROPIC_API_KEY", "OPENROUTER_API_KEY", "FUNDI_SOMETHING",
	} {
		if _, ok := got[k]; ok {
			t.Errorf("captureExecutorEnv kept %s; want it excluded", k)
		}
	}
}

func TestExecutorEnvReportConflictSaysTheFileWins(t *testing.T) {
	res := paths.MergeResult{Added: []string{"NEWVAR"}, Existing: []string{"A", "B"}, Conflict: []string{"OLDVAR"}}
	out := executorEnvReport("/tmp/executor.env", map[string]string{"NEWVAR": "v", "OLDVAR": "x"}, res, nil)
	if !strings.Contains(out, "OLDVAR") {
		t.Error("report should name conflicting variables")
	}
	if !strings.Contains(out, "file wins") {
		t.Error("report should state that the file wins at serve time")
	}
	if strings.Contains(out, "ghp_") {
		t.Error("report must never contain values")
	}
	if !strings.Contains(out, "/tmp/executor.env") {
		t.Error("report should name the environment file")
	}
}

// loadExecutorEnv is what makes the supervised executor see the installing
// shell's environment: absent a login shell, the 0600 file is all it gets.
// Precedence matches the daemon: the process environment overrides the file.
func TestLoadExecutorEnvAppliesFileWithoutOverridingProcess(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "executor.env")
	content := "EXECUTOR_ENV_TEST_FILEVAR=filevalue\nEXECUTOR_ENV_TEST_PROCVAR=filevalue\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(paths.ExecutorEnvFileEnv, path)
	t.Setenv("EXECUTOR_ENV_TEST_PROCVAR", "processvalue")

	loadExecutorEnv()

	if got := os.Getenv("EXECUTOR_ENV_TEST_FILEVAR"); got != "filevalue" {
		t.Errorf("file var not applied: got %q", got)
	}
	if got := os.Getenv("EXECUTOR_ENV_TEST_PROCVAR"); got != "processvalue" {
		t.Errorf("process env must win over the file: got %q", got)
	}
}
