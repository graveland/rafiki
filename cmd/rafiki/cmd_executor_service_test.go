package main

import (
	"os"
	"path/filepath"
	"testing"

	"strings"

	"go.graveland.dev/rafiki/pkg/paths"

	"github.com/multigres/testkit/assert"
)

func TestAppendProxyArgsNoopWhenEmpty(t *testing.T) {
	c := assert.NewAborting(t)
	args := []string{"executor", "serve"}
	got, err := appendProxyArgs(args, nil)
	c.NoError(err, "appendProxyArgs")
	c.EqDiff(args, got, "got")
}

func TestAppendProxyArgsAppendsEachAsARepeatedFlag(t *testing.T) {
	c := assert.NewAborting(t)
	args := []string{"executor", "serve"}
	got, err := appendProxyArgs(args, []string{"vmlx=http://localhost:8005", "ollama=http://localhost:11434"})
	c.NoError(err, "appendProxyArgs")
	want := []string{
		"executor", "serve",
		"--proxy", "vmlx=http://localhost:8005",
		"--proxy", "ollama=http://localhost:11434",
	}
	c.EqDiff(want, got, "got")
}

func TestAppendProxyArgsRejectsMalformedEntryBeforeInstalling(t *testing.T) {
	_, err := appendProxyArgs([]string{"executor", "serve"}, []string{"not-a-pair"})
	assert.NewAborting(t).Error(err, "expected an error for a proxy flag with no '=name'")
}

// captureExecutorEnv is deliberately broad — the executor runs the operator's
// toolchain, so GOPATH or a proxy variable is load-bearing there — and narrow
// only where capture is inert (the unit owns HOME/PATH), leaks (RAFIKI_* and
// provider keys must not reach every bash child), or is stale-on-arrival
// session/GUI residue. SSH_AUTH_SOCK is captured on purpose: operators fix
// agent socket paths so they survive reboots.
func TestCaptureExecutorEnvKeepsToolchainVarsAndDropsSessionOnes(t *testing.T) {
	c := assert.NewCollecting(t)
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
		_, ok := got[k]
		c.True(ok, "captureExecutorEnv dropped %s; toolchain vars must survive", k)
	}
	for _, k := range []string{
		"HOME", "PATH", "TERM", "COLORTERM", "COLORFGBG", "TERM_SESSION_ID",
		"ITERM_PROFILE", "LC_TERMINAL", "DISPLAY", "SECURITYSESSIONID", "XPC_FLAGS",
		"__CFBundleIdentifier", "TMPDIR", "PWD", "OLDPWD", "SHLVL", "_", "GPG_TTY",
		"DIRENV_DIFF", "DIRENV_DIR", "EMPTY",
		// Reserved rafiki variables and provider keys.
		"RAFIKI_DB", "ANTHROPIC_API_KEY", "OPENROUTER_API_KEY", "FUNDI_SOMETHING",
	} {
		_, ok := got[k]
		c.False(ok, "captureExecutorEnv kept %s; want it excluded", k)
	}
}

func TestExecutorEnvReportConflictSaysTheFileWins(t *testing.T) {
	c := assert.NewCollecting(t)
	res := paths.MergeResult{Added: []string{"NEWVAR"}, Existing: []string{"A", "B"}, Conflict: []string{"OLDVAR"}}
	out := executorEnvReport("/tmp/executor.env", map[string]string{"NEWVAR": "v", "OLDVAR": "x"}, res, nil)
	c.StrContains(out, "OLDVAR", "report should name conflicting variables")
	c.StrContains(out, "file wins", "report should state that the file wins at serve time")
	c.NotStrContains(out, "ghp_", "report must never contain values")
	c.StrContains(out, "/tmp/executor.env", "report should name the environment file")
}

// loadExecutorEnv is what makes the supervised executor see the installing
// shell's environment: absent a login shell, the 0600 file is all it gets.
// Precedence matches the daemon: the process environment overrides the file.
func TestLoadExecutorEnvAppliesFileWithoutOverridingProcess(t *testing.T) {
	c := assert.NewCollecting(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "executor.env")
	content := "EXECUTOR_ENV_TEST_FILEVAR=filevalue\nEXECUTOR_ENV_TEST_PROCVAR=filevalue\n"
	c.Require().NoError(os.WriteFile(path, []byte(content), 0o600))
	t.Setenv(paths.ExecutorEnvFileEnv, path)
	t.Setenv("EXECUTOR_ENV_TEST_PROCVAR", "processvalue")

	loadExecutorEnv()

	c.Eq("filevalue", os.Getenv("EXECUTOR_ENV_TEST_FILEVAR"), "file var not applied: got")
	c.Eq("processvalue", os.Getenv("EXECUTOR_ENV_TEST_PROCVAR"), "process env must win over the file: got")
}

// TestExecutorPinnedEnvCarriesOverridesAndShrugsOffDrift pins the snapshot
// contract: executorPinnedEnv applies the environment files and snapshots the
// result, so the overrides file's values — PATH above all, the variable
// launchd seeds itself and executor-overrides.env exists to fix — are in the
// snapshot even when the process environment LATER drifts to something else.
// Every subprocess the executor spawns (tools via ToolOpts.Env, language
// servers, launched children) runs under that snapshot, never under the
// drifted process environment.
func TestExecutorPinnedEnvCarriesOverridesAndShrugsOffDrift(t *testing.T) {
	dir := t.TempDir()

	overrides := filepath.Join(dir, "executor-overrides.env")
	c := assert.NewCollecting(t)
	c.Require().NoError(os.WriteFile(overrides,
		[]byte("PATH=/pinned/bin\nEXECUTOR_PIN_TEST_VAR=pinned\n"), 0o600))
	t.Setenv(paths.ExecutorOverridesFileEnv, overrides)
	// Keep the fill-gaps file out of the way for a deterministic environ.
	t.Setenv(paths.ExecutorEnvFileEnv, filepath.Join(dir, "absent-executor.env"))

	pinned := executorPinnedEnv()
	c.Eq("/pinned/bin", envValue(pinned, "PATH"), "overrides PATH must be in the snapshot")
	c.Eq("pinned", envValue(pinned, "EXECUTOR_PIN_TEST_VAR"), "overrides var must be in the snapshot")

	// Whatever mutates a long-lived process's environment later, the
	// snapshot — and therefore every child spawned from it — is unaffected.
	c.Require().NoError(os.Setenv("PATH", "/drifted/bin"))
	c.Require().NoError(os.Setenv("EXECUTOR_PIN_TEST_VAR", "drifted"))
	c.Eq("/pinned/bin", envValue(pinned, "PATH"), "later drift must not touch the snapshot")
	c.Eq("pinned", envValue(pinned, "EXECUTOR_PIN_TEST_VAR"), "later drift must not touch the snapshot")
}

// envValue reads a KEY=VALUE slice like exec.Cmd.Env.
func envValue(env []string, key string) string {
	for _, kv := range env {
		if k, v, ok := strings.Cut(kv, "="); ok && k == key {
			return v
		}
	}
	return ""
}
