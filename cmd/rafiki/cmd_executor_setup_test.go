package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	"go.graveland.dev/rafiki/pkg/installstate"
	"go.graveland.dev/rafiki/pkg/paths"

	"github.com/multigres/testkit/assert"
)

// isolateSetup points every XDG directory at a fresh temp dir so an executor
// setup test never reads or writes the developer's real profiles, install.toml
// or executor.env, and redirects every executor-related environment variable
// the setup flow consults into that same temp dir, so an operator's exported
// RAFIKI_EXECUTOR_ENV_FILE / RAFIKI_EXECUTOR_NAME cannot leak in and steer a
// test's writes at their real files. It also clears the profile selector so
// resolveProfile bootstraps into the temp dir rather than inheriting a real one.
func isolateSetup(t *testing.T) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("XDG_DATA_HOME", t.TempDir())
	t.Setenv("XDG_RUNTIME_DIR", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	t.Setenv("RAFIKI_PROFILE", "")
	// The executor commands resolve these outside the XDG dirs; an ambient value
	// would point a test at the developer's own files.
	t.Setenv(paths.ExecutorEnvFileEnv, filepath.Join(t.TempDir(), "executor.env"))
	t.Setenv(paths.ExecutorOverridesFileEnv, filepath.Join(t.TempDir(), "executor-overrides.env"))
	t.Setenv(paths.ExecutorName, "rafiki-setup-test-host")
	t.Setenv("RAFIKI_EXECUTOR_CREDENTIAL", "")
}

// seedExecutorCredential writes a non-empty credential file, so the setup flow
// treats this machine as already enrolled and neither mints nor exchanges a
// token.
func seedExecutorCredential(t *testing.T) {
	t.Helper()
	dir := paths.DataDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "executor.cred"), []byte("durable-credential\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func seedMachineName(t *testing.T, name string) {
	t.Helper()
	if err := paths.SetMachineName(name); err != nil {
		t.Fatal(err)
	}
}

// fakeExecutorBackend records that Install ran without touching a real service
// manager.
type fakeExecutorBackend struct{ installed bool }

func (f *fakeExecutorBackend) Install(serviceSpec) error { f.installed = true; return nil }
func (f *fakeExecutorBackend) Uninstall() error          { return nil }
func (f *fakeExecutorBackend) Start() error              { return nil }
func (f *fakeExecutorBackend) Stop() error               { return nil }
func (f *fakeExecutorBackend) Restart() error            { return nil }
func (f *fakeExecutorBackend) Status() (serviceStatus, error) {
	return serviceStatus{}, nil
}
func (f *fakeExecutorBackend) LogPath() string { return "/dev/null" }

// stubExecutorSetup swaps in a fake service backend and a no-op liveness wait,
// restoring both afterwards.
func stubExecutorSetup(t *testing.T) *fakeExecutorBackend {
	t.Helper()
	fb := &fakeExecutorBackend{}
	origBackend := executorServiceBackendFor
	executorServiceBackendFor = func() serviceBackend { return fb }
	origWait := executorSetupWaitLive
	executorSetupWaitLive = func(context.Context, *cobra.Command, string) error { return nil }
	t.Cleanup(func() {
		executorServiceBackendFor = origBackend
		executorSetupWaitLive = origWait
	})
	return fb
}

// stubEnroll replaces the token-minter with a stub returning token/err.
func stubEnroll(t *testing.T, token string, err error) {
	t.Helper()
	orig := executorEnrollClient
	executorEnrollClient = func(context.Context, *cobra.Command, string, []string) (string, error) {
		return token, err
	}
	t.Cleanup(func() { executorEnrollClient = orig })
}

// fakeClaude puts an executable named claude on PATH so a --launch claude run's
// LookPath check passes without a real Claude Code install.
func fakeClaude(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "claude"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// runExecutorSetupForTest drives runExecutorSetup through a real setup command
// with args parsed, capturing everything written to the output.
func runExecutorSetupForTest(t *testing.T, args ...string) (string, error) {
	t.Helper()
	cmd := newExecutorSetupCmd()
	if err := cmd.ParseFlags(args); err != nil {
		t.Fatalf("parse %v: %v", args, err)
	}
	var buf strings.Builder
	w := newWizard(cmd)
	w.out = &buf
	err := runExecutorSetup(cmdCtx(cmd), w, &buf)
	return buf.String(), err
}

func readExecutorEnvFile(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(paths.ExecutorEnvFile())
	if err != nil {
		t.Fatalf("read executor.env: %v", err)
	}
	return string(data)
}

// TestExecutorSetupEnvNamesNotSnapshot pins that an unattended --yes run carries
// only the detected default names, not a snapshot of every variable in the
// installing shell: a secret-shaped variable that happens to be set must not
// reach the file.
func TestExecutorSetupEnvNamesNotSnapshot(t *testing.T) {
	isolateSetup(t)
	seedExecutorCredential(t)
	seedMachineName(t, "test-box")
	stubExecutorSetup(t)
	t.Setenv("LANG", "C")
	t.Setenv("SECRET_THING", "hunter2")
	t.Setenv("EDITOR", "vi")

	if _, err := runExecutorSetupForTest(t, "--yes"); err != nil {
		t.Fatalf("runExecutorSetup: %v", err)
	}

	c := assert.NewCollecting(t)
	got := readExecutorEnvFile(t)
	c.StrContains(got, "LANG=\"C\"", "a detected default must be carried")
	c.StrContains(got, "EDITOR=\"vi\"", "a detected default must be carried")
	c.NotStrContains(got, "SECRET_THING", "a non-selected variable must not be carried")
}

// TestExecutorSetupReplayRereadsValues pins the fix MergeEnvFile's append-only
// semantics cannot give: a second run with no flags re-reads each carried
// name's CURRENT value and updates the file in place, rather than leaving the
// first run's stale value beside it.
func TestExecutorSetupReplayRereadsValues(t *testing.T) {
	isolateSetup(t)
	seedExecutorCredential(t)
	seedMachineName(t, "test-box")
	stubExecutorSetup(t)
	t.Setenv("EDITOR", "vi")

	if _, err := runExecutorSetupForTest(t, "--yes"); err != nil {
		t.Fatalf("first run: %v", err)
	}

	t.Setenv("EDITOR", "nano")
	if _, err := runExecutorSetupForTest(t, "--yes"); err != nil {
		t.Fatalf("second run: %v", err)
	}

	c := assert.NewCollecting(t)
	got := readExecutorEnvFile(t)
	c.StrContains(got, "EDITOR=\"nano\"", "a replay must re-read the value")
	c.NotStrContains(got, "EDITOR=\"vi\"", "the stale value must be replaced, not duplicated")
}

// TestExecutorSetupNeverPrintsValues pins that the flow never writes a carried
// variable's VALUE to its output — only names. A terminal scrollback is
// readable.
func TestExecutorSetupNeverPrintsValues(t *testing.T) {
	isolateSetup(t)
	seedExecutorCredential(t)
	seedMachineName(t, "test-box")
	stubExecutorSetup(t)
	const canary = "pr1nt-canary-editor-value"
	t.Setenv("EDITOR", canary)
	t.Setenv("SECRET_THING", "pr1nt-canary-secret-value")

	out, err := runExecutorSetupForTest(t, "--yes")
	if err != nil {
		t.Fatalf("runExecutorSetup: %v", err)
	}

	c := assert.NewCollecting(t)
	c.NotStrContains(out, canary, "output must never contain a carried variable's value")
	c.NotStrContains(out, "pr1nt-canary-secret-value", "output must never contain an environment value")
	c.StrContains(out, "EDITOR", "output should name the carried variable")
}

// TestExecutorSetupYesWithoutTokenOrMintFails pins the unattended failure: no
// credential file, no --enroll-token, and a token mint refused for lack of
// admin, so --yes must error with the remedy (pass --enroll-token) rather than
// hanging on a form.
func TestExecutorSetupYesWithoutTokenOrMintFails(t *testing.T) {
	isolateSetup(t)
	seedMachineName(t, "test-box")
	stubExecutorSetup(t)
	stubEnroll(t, "", connect.NewError(connect.CodePermissionDenied, errors.New("not an admin")))

	_, err := runExecutorSetupForTest(t, "--yes")
	if err == nil {
		t.Fatal("expected an error when the token cannot be minted and none was given")
	}
	if !strings.Contains(err.Error(), "--enroll-token") {
		t.Fatalf("error must tell the operator to pass --enroll-token, got: %v", err)
	}
}

// TestExecutorSetupSavesAnswers pins that the chosen answers land in
// install.toml for a later replay.
func TestExecutorSetupSavesAnswers(t *testing.T) {
	isolateSetup(t)
	seedExecutorCredential(t)
	seedMachineName(t, "test-box")
	stubExecutorSetup(t)
	fakeClaude(t)
	t.Setenv("LANG", "C")
	t.Setenv("EDITOR", "vi")
	root := t.TempDir()

	if _, err := runExecutorSetupForTest(t,
		"--yes", "--root", root, "--launch", "claude",
		"--env", "LANG", "--env", "EDITOR"); err != nil {
		t.Fatalf("runExecutorSetup: %v", err)
	}

	st, ok, err := installstate.Load()
	c := assert.NewCollecting(t)
	c.Require().NoError(err, "Load")
	c.True(ok, "install state must have been written")
	c.Eq(root, st.Executor.Root, "root")
	c.True(containsString(st.Executor.Launch, "claude"), "launch must contain claude, got %v", st.Executor.Launch)
	c.True(containsString(st.Executor.Env, "LANG"), "env must contain LANG, got %v", st.Executor.Env)
	c.True(containsString(st.Executor.Env, "EDITOR"), "env must contain EDITOR, got %v", st.Executor.Env)
}

// TestExecutorSetupIgnoresAmbientExecutorEnvFile pins the isolation guard: even
// when the developer's shell points RAFIKI_EXECUTOR_ENV_FILE at a real file,
// isolateSetup must redirect the write into a temp dir, so a test never modifies
// the developer's executor.env. Against an un-isolated setup the run would write
// to the sentinel and the equality check would fail.
func TestExecutorSetupIgnoresAmbientExecutorEnvFile(t *testing.T) {
	// Simulate a developer whose shell names their real executor environment
	// file — the exact leak isolateSetup exists to defeat.
	sentinel := filepath.Join(t.TempDir(), "real-executor.env")
	const knownLine = "KNOWN_OPERATOR_SETTING=line\n"
	if err := os.WriteFile(sentinel, []byte(knownLine), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(paths.ExecutorEnvFileEnv, sentinel)

	isolateSetup(t) // must override the ambient value with a temp path
	seedExecutorCredential(t)
	seedMachineName(t, "test-box")
	stubExecutorSetup(t)

	if _, err := runExecutorSetupForTest(t, "--yes"); err != nil {
		t.Fatalf("runExecutorSetup: %v", err)
	}

	got, err := os.ReadFile(sentinel)
	if err != nil {
		t.Fatalf("read sentinel: %v", err)
	}
	c := assert.NewCollecting(t)
	c.Eq(knownLine, string(got), "the ambient executor.env must be left untouched")
	c.NotStrContains(readExecutorEnvFile(t), "KNOWN_OPERATOR_SETTING",
		"the run must write to the isolated file, not the ambient one")
}

// TestInstallExecutorServiceNilEnvNamesKeepsLegacyCapture pins that EnvNames
// nil keeps the old whole-shell capture, so `rafiki executor service install`
// behaves exactly as before.
func TestInstallExecutorServiceNilEnvNamesKeepsLegacyCapture(t *testing.T) {
	isolateSetup(t)
	fb := stubExecutorSetup(t)
	t.Setenv("NIX_PATH", "darwin=https://example.com/nixpkgs.tar.gz")

	var buf strings.Builder
	err := installExecutorService(executorInstallOptions{
		ConnectSocket: filepath.Join(t.TempDir(), "executor.sock"),
		Credential:    "durable-credential",
		Root:          t.TempDir(),
		EnvNames:      nil,
	}, &buf)
	if err != nil {
		t.Fatalf("installExecutorService: %v", err)
	}

	c := assert.NewCollecting(t)
	c.True(fb.installed, "the backend must have been asked to install")
	got := readExecutorEnvFile(t)
	c.StrContains(got, "NIX_PATH=", "a nil EnvNames must keep capturing the whole shell")
}
