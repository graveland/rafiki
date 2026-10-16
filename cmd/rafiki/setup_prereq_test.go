// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os/exec"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// ---- fakeRunner: the subprocess seam for every setup test ----

type fakeScript struct {
	stdout, stderr []byte
	err            error
}

type fakeStreamScript struct {
	lines []string
	err   error
}

type fakeCall struct {
	env  []string
	argv []string
}

// fakeRunner scripts subprocess responses, keyed by the full joined argv (so
// `docker inspect -f {{.State.Status}}` and `...{{.State.Health.Status}}` are
// distinct), and records every call for assertions.
type fakeRunner struct {
	paths   map[string]string
	scripts map[string]fakeScript
	streams map[string]fakeStreamScript

	runCalls    []fakeCall
	interCalls  []fakeCall
	streamCalls []fakeCall
}

func newFakeRunner() *fakeRunner {
	return &fakeRunner{
		paths:   map[string]string{},
		scripts: map[string]fakeScript{},
		streams: map[string]fakeStreamScript{},
	}
}

func runKey(argv []string) string { return strings.Join(argv, " ") }

// script registers a successful Run response.
func (f *fakeRunner) script(argv []string, stdout string) {
	f.scripts[runKey(argv)] = fakeScript{stdout: []byte(stdout)}
}

// scriptErr registers a failing Run response.
func (f *fakeRunner) scriptErr(argv []string, err error) {
	f.scripts[runKey(argv)] = fakeScript{err: err}
}

// scriptFail registers a failing Run response that carries stderr.
func (f *fakeRunner) scriptFail(argv []string, err error, stderr string) {
	f.scripts[runKey(argv)] = fakeScript{stderr: []byte(stderr), err: err}
}

func (f *fakeRunner) LookPath(name string) (string, error) {
	if p, ok := f.paths[name]; ok && p != "" {
		return p, nil
	}
	return "", exec.ErrNotFound
}

func (f *fakeRunner) Run(_ context.Context, env []string, argv ...string) ([]byte, []byte, error) {
	f.runCalls = append(f.runCalls, fakeCall{env: env, argv: argv})
	if s, ok := f.scripts[runKey(argv)]; ok {
		return s.stdout, s.stderr, s.err
	}
	return nil, nil, nil
}

func (f *fakeRunner) RunInteractive(_ context.Context, argv ...string) error {
	f.interCalls = append(f.interCalls, fakeCall{argv: argv})
	if s, ok := f.scripts["interactive "+runKey(argv)]; ok {
		return s.err
	}
	return nil
}

func (f *fakeRunner) Stream(ctx context.Context, argv ...string) (io.ReadCloser, error) {
	f.streamCalls = append(f.streamCalls, fakeCall{argv: argv})
	s, ok := f.streams[runKey(argv)]
	if ok && s.err != nil {
		return nil, s.err
	}
	var lines []string
	if ok {
		lines = s.lines
	}
	pr, pw := io.Pipe()
	go func() {
		for _, l := range lines {
			if _, err := io.WriteString(pw, l+"\n"); err != nil {
				return
			}
		}
		<-ctx.Done()
		_ = pw.Close()
	}()
	return pr, nil
}

// newSetupWizard builds a non-interactive wizard carrying the flags setup uses.
func newSetupWizard(t *testing.T) *wizard {
	t.Helper()
	return newTestWizard(t, func(cmd *cobra.Command) {
		cmd.Flags().String("dsn", "", "")
		cmd.Flags().String("container-runtime", "", "")
		cmd.Flags().StringSlice("with", nil, "")
	})
}

// ---- execRunner: the production seam must actually work ----

func TestExecRunnerRun(t *testing.T) {
	out, _, err := execRunner{}.Run(context.Background(), []string{"RAFINNI=bar"}, "sh", "-c", `printf %s "$RAFINNI"`)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if string(out) != "bar" {
		t.Errorf("stdout = %q, want %q", out, "bar")
	}
}

func TestExecRunnerRunCapturesStderrAndExit(t *testing.T) {
	_, stderr, err := execRunner{}.Run(context.Background(), nil, "sh", "-c", "echo boom >&2; exit 3")
	if err == nil {
		t.Fatal("Run: want error, got nil")
	}
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		t.Errorf("error %v is not an *exec.ExitError", err)
	}
	if !strings.Contains(string(stderr), "boom") {
		t.Errorf("stderr = %q, want it to contain boom", stderr)
	}
}

func TestExecRunnerStream(t *testing.T) {
	rc, err := execRunner{}.Stream(context.Background(), "sh", "-c", "echo hello")
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer rc.Close()
	b, _ := io.ReadAll(rc)
	if strings.TrimSpace(string(b)) != "hello" {
		t.Errorf("stream = %q, want hello", b)
	}
}

func TestExecRunnerLookPath(t *testing.T) {
	if _, err := (execRunner{}).LookPath("sh"); err != nil {
		t.Errorf("LookPath(sh): %v", err)
	}
	if _, err := (execRunner{}).LookPath("rafiki-definitely-not-real-zzz"); err == nil {
		t.Error("LookPath of a missing binary: want error, got nil")
	}
}

func TestExecRunnerRunInteractive(t *testing.T) {
	if err := (execRunner{}).RunInteractive(context.Background(), "sh", "-c", "exit 0"); err != nil {
		t.Errorf("RunInteractive: %v", err)
	}
}

// ---- checkPrereqs ----

func TestSetupPrereqDockerMissingDarwinOffersOrbstack(t *testing.T) {
	r := newFakeRunner()
	r.paths["brew"] = "/opt/homebrew/bin/brew"
	r.scriptErr([]string{"docker", "info"}, errors.New("exit 1"))
	w := newSetupWizard(t)

	if _, err := checkPrereqs(context.Background(), r, w, "darwin", nil); err == nil {
		t.Fatal("checkPrereqs: want error on declined install, got nil")
	}
	if got := w.out.(*bytes.Buffer).String(); !strings.Contains(got, "brew install --cask orbstack") {
		t.Errorf("output %q must offer the orbstack cask", got)
	}
}

func TestSetupPrereqDeclineStopsWithCommand(t *testing.T) {
	r := newFakeRunner()
	r.script([]string{"docker", "info"}, "26.0")
	r.scriptErr([]string{"claude", "auth", "status", "--json"}, errors.New("exit 1"))
	w := newSetupWizard(t)

	_, err := checkPrereqs(context.Background(), r, w, "linux", nil)
	if err == nil {
		t.Fatal("checkPrereqs: want error, got nil")
	}
	for _, want := range []string{"claude is required", "bash -c"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q must contain %q", err.Error(), want)
		}
	}
}

func TestSetupPrereqClaudeLoggedOutOffersLogin(t *testing.T) {
	r := newFakeRunner()
	r.paths["claude"] = "/usr/local/bin/claude"
	r.script([]string{"docker", "info"}, "26.0")
	r.scriptErr([]string{"claude", "auth", "status", "--json"}, errors.New("exit 1"))
	w := newSetupWizard(t)

	_, err := checkPrereqs(context.Background(), r, w, "linux", nil)
	if err == nil {
		t.Fatal("checkPrereqs: want error on declined login, got nil")
	}
	if got := w.out.(*bytes.Buffer).String(); !strings.Contains(got, "claude auth login") {
		t.Errorf("output %q must offer `claude auth login`", got)
	}
}

func TestSetupPrereqLinuxDockerGroupStops(t *testing.T) {
	r := newFakeRunner()
	r.scriptErr([]string{"docker", "info"}, errors.New("exit 1"))
	w := newSetupWizard(t)
	w.yes = true // accept the offered commands so the group step is reached

	_, err := checkPrereqs(context.Background(), r, w, "linux", nil)
	if err == nil {
		t.Fatal("checkPrereqs: want the group stop, got nil")
	}
	if !strings.Contains(err.Error(), "log out and back in") {
		t.Errorf("error %q must tell the user to log out and back in", err.Error())
	}
	var ranApt, ranUsermod bool
	for _, c := range r.interCalls {
		if strings.HasPrefix(runKey(c.argv), "sudo apt-get install -y docker.io") {
			ranApt = true
		}
		if strings.HasPrefix(runKey(c.argv), "sudo usermod -aG docker") {
			ranUsermod = true
		}
	}
	if !ranApt || !ranUsermod {
		t.Errorf("expected apt-get and usermod installs, got %v", r.interCalls)
	}
}

// A user who is not in the docker group gets `permission denied` from
// `docker info`, which used to be reported as "the daemon is not running; sudo
// systemctl start docker" — the wrong remedy. It must name the group fix.
func TestSetupPrereqDockerPermissionDeniedNamesGroup(t *testing.T) {
	r := newFakeRunner()
	r.scriptFail([]string{"docker", "info"}, errors.New("exit 1"),
		"Got permission denied while trying to connect to the Docker daemon socket at unix:///var/run/docker.sock")
	w := newSetupWizard(t)

	_, err := checkPrereqs(context.Background(), r, w, "linux", nil)
	if err == nil {
		t.Fatal("checkPrereqs: want the permission stop, got nil")
	}
	if !strings.Contains(err.Error(), "usermod -aG docker") {
		t.Errorf("error %q must name the docker-group fix", err.Error())
	}
	if strings.Contains(err.Error(), "daemon is not running") {
		t.Errorf("error %q must not be the misleading daemon-down message", err.Error())
	}
}

// A genuinely stopped daemon must still read as daemon-down (and get the
// "start it" remedy), not as a permission or install problem.
func TestSetupPrereqDockerDaemonDownNamesStart(t *testing.T) {
	r := newFakeRunner()
	r.paths["docker"] = "/usr/bin/docker"
	r.scriptFail([]string{"docker", "info"}, errors.New("exit 1"),
		"Cannot connect to the Docker daemon at unix:///var/run/docker.sock. Is the docker daemon running?")
	w := newSetupWizard(t)

	_, err := checkPrereqs(context.Background(), r, w, "linux", nil)
	if err == nil {
		t.Fatal("checkPrereqs: want the daemon-down stop, got nil")
	}
	if !strings.Contains(err.Error(), "daemon is not running") {
		t.Errorf("error %q must be the daemon-down message", err.Error())
	}
	if strings.Contains(err.Error(), "usermod -aG docker") {
		t.Errorf("error %q must not be the permission message", err.Error())
	}
}

// A reachable daemon yields the present result and no error.
func TestSetupPrereqDockerPresent(t *testing.T) {
	r := newFakeRunner()
	r.script([]string{"docker", "info"}, "Server Version: 26.0")

	var docker *prereq
	for _, p := range prereqs() {
		if p.Name == "docker" {
			p := p
			docker = &p
			break
		}
	}
	if docker == nil {
		t.Fatal("no docker prereq registered")
	}
	ok, detail, err := docker.Present(context.Background(), r)
	if err != nil {
		t.Fatalf("Present: %v", err)
	}
	if !ok || detail != "" {
		t.Errorf("Present = (%v, %q), want (true, \"\")", ok, detail)
	}
}

func TestSetupPrereqOptionalChosenInstalled(t *testing.T) {
	r := newFakeRunner()
	r.paths["brew"] = "/opt/homebrew/bin/brew"
	r.paths["uv"] = "/opt/homebrew/bin/uv"
	r.script([]string{"docker", "info"}, "26.0")
	r.script([]string{"claude", "auth", "status", "--json"}, `{"loggedIn":true}`)
	r.paths["rg"] = "/opt/homebrew/bin/rg"
	w := newSetupWizard(t)
	w.yes = true

	chosen, err := checkPrereqs(context.Background(), r, w, "darwin", []string{"uv", "rtk"})
	if err != nil {
		t.Fatalf("checkPrereqs: %v", err)
	}
	if !containsString(chosen, "uv") || !containsString(chosen, "rtk") {
		t.Errorf("chosen = %v, want uv and rtk", chosen)
	}
}

func TestSetupPrereqDarwinWithoutBrewStops(t *testing.T) {
	r := newFakeRunner()
	r.scriptErr([]string{"docker", "info"}, errors.New("exit 1"))
	r.script([]string{"claude", "auth", "status", "--json"}, `{"loggedIn":true}`)
	r.paths["rg"] = "/usr/local/bin/rg"
	w := newSetupWizard(t)

	_, err := checkPrereqs(context.Background(), r, w, "darwin", nil)
	if err == nil {
		t.Fatal("checkPrereqs: want the Homebrew stop, got nil")
	}
	if !strings.Contains(err.Error(), "homebrew is required") {
		t.Errorf("error %q must explain Homebrew is required", err.Error())
	}
}

func TestSetupPrereqDarwinWithoutBrewNoInstallNeeded(t *testing.T) {
	r := newFakeRunner()
	r.script([]string{"docker", "info"}, "26.0")
	r.script([]string{"claude", "auth", "status", "--json"}, `{"loggedIn":true}`)
	r.paths["rg"] = "/usr/local/bin/rg"
	w := newSetupWizard(t)

	chosen, err := checkPrereqs(context.Background(), r, w, "darwin", nil)
	if err != nil {
		t.Fatalf("a fully-provisioned Mac with no brew must not be blocked: %v", err)
	}
	if len(chosen) != 0 {
		t.Errorf("chosen = %v, want none", chosen)
	}
}

func TestWizardSecretPrecedence(t *testing.T) {
	w := newTestWizard(t, func(cmd *cobra.Command) {
		cmd.Flags().String("api-key", "", "")
	})
	if err := w.cmd.Flags().Set("api-key", "from-flag"); err != nil {
		t.Fatal(err)
	}
	got, err := w.askSecret("API key", "api-key", "saved", "detected", true)
	if err != nil {
		t.Fatalf("askSecret: %v", err)
	}
	if got != "from-flag" {
		t.Errorf("askSecret = %q, want the flag value", got)
	}

	w2 := newTestWizard(t, func(cmd *cobra.Command) {
		cmd.Flags().String("api-key", "", "")
	})
	got2, err := w2.askSecret("API key", "api-key", "saved", "detected", true)
	if err != nil {
		t.Fatalf("askSecret: %v", err)
	}
	if got2 != "saved" {
		t.Errorf("askSecret = %q, want the saved value", got2)
	}
}

func TestWizardSecretRequiredMissingUnderYes(t *testing.T) {
	w := newTestWizard(t, func(cmd *cobra.Command) {
		cmd.Flags().String("api-key", "", "")
	})
	w.yes = true

	_, err := w.askSecret("API key", "api-key", "", "", true)
	if err == nil || !strings.Contains(err.Error(), "--api-key") {
		t.Errorf("askSecret required-empty: err = %v, want it to name --api-key", err)
	}
}

func TestShellJoinQuotesSpaces(t *testing.T) {
	got := shellJoin([]string{"bash", "-c", "curl x | bash"})
	if got != "bash -c 'curl x | bash'" {
		t.Errorf("shellJoin = %q", got)
	}
}
