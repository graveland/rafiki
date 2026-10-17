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

// TestExecRunnerStreamIncludesStderr: `docker logs -f` replays a container's
// stderr on its own stderr, and postgres's real server logs its "ready" line
// there — a stdout-only Stream never delivers it.
func TestExecRunnerStreamIncludesStderr(t *testing.T) {
	rc, err := execRunner{}.Stream(context.Background(), "sh", "-c", "echo out; echo err >&2")
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	defer rc.Close()
	b, _ := io.ReadAll(rc)
	for _, want := range []string{"out", "err"} {
		if !strings.Contains(string(b), want) {
			t.Errorf("Stream output %q lacks %q", b, want)
		}
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

	if err := ensureDocker(context.Background(), r, w, "darwin"); err == nil {
		t.Fatal("ensureDocker: want error on declined install, got nil")
	}
	if got := w.out.(*bytes.Buffer).String(); !strings.Contains(got, "brew install --cask orbstack") {
		t.Errorf("output %q must offer the orbstack cask", got)
	}
}

func TestSetupPrereqDeclineStopsWithCommand(t *testing.T) {
	r := newFakeRunner()
	r.paths["rg"] = "/usr/bin/rg"
	w := newSetupWizard(t)
	if err := w.cmd.Flags().Set("with", "claude"); err != nil {
		t.Fatal(err)
	}

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

// claude is optional: installed but logged out is a note, never a stop.
func TestSetupPrereqClaudeLoggedOutIsANote(t *testing.T) {
	r := newFakeRunner()
	r.paths["rg"] = "/usr/bin/rg"
	r.paths["claude"] = "/usr/local/bin/claude"
	r.scriptErr([]string{"claude", "auth", "status", "--json"}, errors.New("exit 1"))
	w := newSetupWizard(t)

	chosen, err := checkPrereqs(context.Background(), r, w, "linux", nil)
	if err != nil {
		t.Fatalf("checkPrereqs: a logged-out optional claude must not stop setup: %v", err)
	}
	if !containsString(chosen, "claude") {
		t.Errorf("chosen = %v, want the installed claude", chosen)
	}
	if got := w.out.(*bytes.Buffer).String(); !strings.Contains(got, "claude auth login") {
		t.Errorf("output %q must tell the user to run `claude auth login`", got)
	}
}

func TestSetupPrereqLinuxDockerGroupStops(t *testing.T) {
	r := newFakeRunner()
	r.scriptErr([]string{"docker", "info"}, errors.New("exit 1"))
	w := newSetupWizard(t)
	w.nonInteractive = true // accept the offered commands so the group step is reached

	err := ensureDocker(context.Background(), r, w, "linux")
	if err == nil {
		t.Fatal("ensureDocker: want the group stop, got nil")
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

	err := ensureDocker(context.Background(), r, w, "linux")
	if err == nil {
		t.Fatal("ensureDocker: want the permission stop, got nil")
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

	err := ensureDocker(context.Background(), r, w, "linux")
	if err == nil {
		t.Fatal("ensureDocker: want the daemon-down stop, got nil")
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

// Everything installed: nothing is asked, nothing is installed, and the
// installed optional tools are the chosen set.
func TestSetupPrereqInstalledOptionalToolsAreChosenWithoutAsking(t *testing.T) {
	r := newFakeRunner()
	for _, n := range []string{"brew", "rg", "docker", "claude", "uv", "ollama", "rtk"} {
		r.paths[n] = "/opt/homebrew/bin/" + n
	}
	r.script([]string{"claude", "auth", "status", "--json"}, `{"loggedIn":true}`)
	w := newSetupWizard(t)

	chosen, err := checkPrereqs(context.Background(), r, w, "darwin", nil)
	if err != nil {
		t.Fatalf("checkPrereqs: %v", err)
	}
	if got := strings.Join(chosen, ","); got != "docker,claude,uv,ollama,rtk" {
		t.Errorf("chosen = %q, want every installed optional tool", got)
	}
	if len(r.interCalls) != 0 {
		t.Errorf("installed tools must not be reinstalled, got %v", r.interCalls)
	}
}

// A missing optional tool is installed only when picked; an installed one is
// chosen regardless and is never offered.
func TestSetupPrereqOnlyMissingToolsAreOffered(t *testing.T) {
	r := newFakeRunner()
	r.paths["brew"] = "/opt/homebrew/bin/brew"
	r.paths["rg"] = "/opt/homebrew/bin/rg"
	r.paths["uv"] = "/opt/homebrew/bin/uv"
	w := newSetupWizard(t)
	w.nonInteractive = true

	chosen, err := checkPrereqs(context.Background(), r, w, "darwin", []string{"rtk", "uv"})
	if err != nil {
		t.Fatalf("checkPrereqs: %v", err)
	}
	if got := strings.Join(chosen, ","); got != "uv,rtk" {
		t.Errorf("chosen = %q, want the installed uv and the picked rtk", got)
	}
	var installs []string
	for _, c := range r.interCalls {
		installs = append(installs, runKey(c.argv))
	}
	if len(installs) != 1 || installs[0] != "brew install rtk" {
		t.Errorf("installs = %v, want only brew install rtk", installs)
	}
}

func TestSetupPrereqDarwinWithoutBrewStopsWhenARequiredToolIsMissing(t *testing.T) {
	r := newFakeRunner()
	w := newSetupWizard(t)

	_, err := checkPrereqs(context.Background(), r, w, "darwin", nil)
	if err == nil {
		t.Fatal("checkPrereqs: want the Homebrew stop, got nil")
	}
	if !strings.Contains(err.Error(), "homebrew is required") {
		t.Errorf("error %q must explain Homebrew is required", err.Error())
	}
}

// No docker, no claude and no brew is a fine machine for a bring-your-own
// database and an OpenRouter key: setup is not blocked.
func TestSetupPrereqWithoutDockerOrClaudeIsNotBlocked(t *testing.T) {
	r := newFakeRunner()
	r.paths["rg"] = "/usr/local/bin/rg"
	w := newSetupWizard(t)

	chosen, err := checkPrereqs(context.Background(), r, w, "darwin", nil)
	if err != nil {
		t.Fatalf("a machine without docker or claude must not be blocked: %v", err)
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

func TestWizardSecretRequiredMissingUnderNonInteractive(t *testing.T) {
	w := newTestWizard(t, func(cmd *cobra.Command) {
		cmd.Flags().String("api-key", "", "")
	})
	w.nonInteractive = true

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
