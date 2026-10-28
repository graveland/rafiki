// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"connectrpc.com/connect"
	"github.com/spf13/cobra"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/gen/rafiki/v1/rafikiv1connect"
	"go.graveland.dev/rafiki/pkg/installstate"
	"go.graveland.dev/rafiki/pkg/paths"
	"go.graveland.dev/rafiki/pkg/profile"
	"go.graveland.dev/rafiki/pkg/secretbox"

	"github.com/multigres/testkit/assert"
)

// constSetupDSN is the DSN the fixtures write to service.env. Its password must
// never reach argv or stdout.
const constSetupDSN = "postgres://postgres:secretpw@127.0.0.1:5432/rafiki?sslmode=disable"

// constSetupSecretsKey is what a fully configured machine's service.env holds
// for RAFIKI_SECRETS_KEY: base64 of 32 bytes, the shape setup generates. No test
// seals anything with it.
const constSetupSecretsKey = "QUFBQUFBQUFBQUFBQUFBQUFBQUFBQUFBQUFBQUFBQUE="

// ── the fake Control server ──────────────────────────────────────────────────

// setupStubControl serves only the Control RPCs `rafiki setup` uses — Status,
// Spawn, Send, Close, ListExecutors and the event stream. Every other RPC
// answers Unimplemented via the embed.
type setupStubControl struct {
	rafikiv1connect.UnimplementedControlHandler

	mu sync.Mutex

	statusErr    error
	liveChildren int32
	executors    []*rafikiv1.ExecutorRow

	spawnErr     error
	spawnChildID string
	sendErr      error
	closeErr     error
	events       []*rafikiv1.Event

	spawnReqs []*rafikiv1.SpawnRequest

	// latestOrdinal is what GetChild reports; nil = unset.
	latestOrdinal *int32
	// calls records the order of Spawn, GetChild, Send and StreamEvents;
	// streamReqs every StreamEvents request.
	calls      []string
	streamReqs []*rafikiv1.StreamEventsRequest

	statusCalls int
	spawnCalls  int
	sendCalls   int
	closeCalls  int
	killCalls   int
	listCalls   int
}

func (s *setupStubControl) Status(context.Context, *connect.Request[rafikiv1.StatusRequest]) (*connect.Response[rafikiv1.StatusResponse], error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.statusCalls++
	if s.statusErr != nil {
		return nil, s.statusErr
	}
	return connect.NewResponse(&rafikiv1.StatusResponse{
		Children: &rafikiv1.StatusResponse_ChildCounts{Live: s.liveChildren},
	}), nil
}

func (s *setupStubControl) Spawn(_ context.Context, req *connect.Request[rafikiv1.SpawnRequest]) (*connect.Response[rafikiv1.SpawnResponse], error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.spawnCalls++
	s.spawnReqs = append(s.spawnReqs, req.Msg)
	s.calls = append(s.calls, "Spawn")
	if s.spawnErr != nil {
		return nil, s.spawnErr
	}
	id := s.spawnChildID
	if id == "" {
		id = "c_smoke"
	}
	return connect.NewResponse(&rafikiv1.SpawnResponse{ChildId: id}), nil
}

func (s *setupStubControl) GetChild(_ context.Context, req *connect.Request[rafikiv1.GetChildRequest]) (*connect.Response[rafikiv1.GetChildResponse], error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, "GetChild")
	return connect.NewResponse(&rafikiv1.GetChildResponse{Child: &rafikiv1.ChildSummary{
		ChildId: req.Msg.GetChildId(), LatestOrdinal: s.latestOrdinal,
	}}), nil
}

func (s *setupStubControl) Send(_ context.Context, _ *connect.Request[rafikiv1.SendRequest]) (*connect.Response[rafikiv1.SendResponse], error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sendCalls++
	s.calls = append(s.calls, "Send")
	if s.sendErr != nil {
		return nil, s.sendErr
	}
	return connect.NewResponse(&rafikiv1.SendResponse{}), nil
}

func (s *setupStubControl) Kill(_ context.Context, req *connect.Request[rafikiv1.KillRequest]) (*connect.Response[rafikiv1.KillResponse], error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.killCalls++
	return connect.NewResponse(&rafikiv1.KillResponse{}), nil
}

func (s *setupStubControl) Close(_ context.Context, req *connect.Request[rafikiv1.CloseRequest]) (*connect.Response[rafikiv1.CloseResponse], error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closeCalls++
	if s.closeErr != nil {
		return nil, s.closeErr
	}
	return connect.NewResponse(&rafikiv1.CloseResponse{ChildId: req.Msg.GetChildId()}), nil
}

func (s *setupStubControl) ListExecutors(context.Context, *connect.Request[rafikiv1.ListExecutorsRequest]) (*connect.Response[rafikiv1.ListExecutorsResponse], error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.listCalls++
	return connect.NewResponse(&rafikiv1.ListExecutorsResponse{Rows: s.executors}), nil
}

func (s *setupStubControl) StreamEvents(_ context.Context, req *connect.Request[rafikiv1.StreamEventsRequest], stream *connect.ServerStream[rafikiv1.Event]) error {
	s.mu.Lock()
	s.calls = append(s.calls, "StreamEvents")
	s.streamReqs = append(s.streamReqs, req.Msg)
	events := append([]*rafikiv1.Event(nil), s.events...)
	s.mu.Unlock()
	for _, ev := range events {
		if err := stream.Send(ev); err != nil {
			return err
		}
	}
	return nil
}

// ── the fixture ──────────────────────────────────────────────────────────────

type setupFixture struct {
	t       *testing.T
	cmd     *cobra.Command
	out     bytes.Buffer
	run     *fakeRunner
	fb      *fakeServiceBackend
	execFB  *fakeServiceBackend
	stub    *setupStubControl
	rafikid string
}

// newSetupFixture wires an isolated environment: XDG dirs, a fake rafikid on
// PATH, a stubbed service backend, and a fake Control server on the profile's
// own socket. It seeds a baseline "everything done" state that individual tests
// override.
func newSetupFixture(t *testing.T) *setupFixture {
	t.Helper()
	c := assert.NewAborting(t)

	isolateProfiles(t)
	resetProfileCache()

	// A short runtime dir: unix socket paths are capped (~104 bytes), and
	// t.TempDir() nests under the full test name.
	runDir, err := os.MkdirTemp("", "rafset")
	c.NoError(err, "MkdirTemp")
	t.Cleanup(func() { os.RemoveAll(runDir) })
	t.Setenv("XDG_RUNTIME_DIR", runDir)
	t.Setenv("XDG_STATE_HOME", filepath.Join(runDir, "state"))
	// Point setup's providers.toml at the scratch dir: paths.Get(paths.Providers)
	// reads RAFIKI_PROVIDERS first, and a developer's exported value would send
	// the step's writes to their real file.
	t.Setenv("RAFIKI_PROVIDERS", filepath.Join(runDir, "providers.toml"))
	// A key exported in the developer's shell is ADOPTED into service.env (the
	// daemon's real precedence puts the environment above the file), which would
	// make every seeded fixture depend on that shell. Empty it: no value is not
	// an exported value, and t.Setenv restores whatever was there.
	t.Setenv(secretbox.EnvKey, "")
	// Never let a probe reach a real local Ollama.
	origOllama := ollamaBaseURL
	ollamaBaseURL = "http://127.0.0.1:1"
	t.Cleanup(func() { ollamaBaseURL = origOllama })
	c.NoError(os.MkdirAll(filepath.Dir(paths.SocketPath()), 0o700), "MkdirAll socket dir")

	f := &setupFixture{t: t, cmd: newSetupCmd(), run: newFakeRunner()}

	// A rafikid binary findDaemonBinary can locate without a real build.
	binDir := t.TempDir()
	f.rafikid = filepath.Join(binDir, "rafikid")
	c.NoError(os.WriteFile(f.rafikid, []byte("#!/bin/sh\n"), 0o755), "write rafikid")
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	f.run.paths["rg"] = "/usr/bin/rg"
	f.run.script([]string{"docker", "info", "--format", "{{.ServerVersion}}"}, "26.0")
	f.run.script([]string{"claude", "auth", "status", "--json"}, `{"loggedIn":true}`)
	f.run.script([]string{"docker", "inspect", "-f", "{{.State.Status}}", managedPGContainer}, "running")

	f.fb = &fakeServiceBackend{status: serviceStatus{Installed: true}}
	stubServiceBackend(t, f.fb)

	// The executor unit is a separate service; without this stub the real
	// launchd/systemd unit on the developer's machine would be read, and
	// repointed.
	f.execFB = &fakeServiceBackend{}
	origExec := executorServiceBackendFor
	executorServiceBackendFor = func() serviceBackend { return f.execFB }
	t.Cleanup(func() { executorServiceBackendFor = origExec })

	f.stub = &setupStubControl{executors: []*rafikiv1.ExecutorRow{{
		Id: "ex1", Machine: "machine", Eligible: true, LaunchKinds: []string{"claude"},
	}}}
	f.serveStub()

	// Baseline: a prior successful install, resumable and complete.
	no := false
	c.NoError(installstate.Save(installstate.State{
		Version:  installstate.SchemaVersion,
		Channel:  installstate.ChannelSource,
		LastStep: stepExecutor,
		DB:       installstate.DB{Mode: installstate.DBModeManaged, Container: managedPGContainer, Volume: managedPGVolume, Image: managedPGImage},
		Daemon:   installstate.Daemon{Expose: &no},
		Identity: installstate.Identity{User: "bob", Profile: "default"},
	}), "installstate.Save")
	f.seedServiceEnv(map[string]string{
		"RAFIKI_DB":           constSetupDSN,
		"RAFIKI_PROXY_LISTEN": setupProxyListenLoopback,
		secretbox.EnvKey:      constSetupSecretsKey,
	})
	f.seedProfile("default", "sk-live")

	// The happy subprocess answers: no scripted docker run, a successful
	// migrate (unscripted → no error), and no user command needed on the
	// baseline (the token is already valid).
	return f
}

func (f *setupFixture) serveStub() {
	routePath, handler := rafikiv1connect.NewControlHandler(f.stub)
	serveConnectOnUnixSocket(f.t, paths.SocketPath(), routePath, handler)
}

func (f *setupFixture) seedServiceEnv(vars map[string]string) {
	f.t.Helper()
	_ = os.Remove(paths.ServiceEnvFile())
	_, err := paths.SetEnvFile(paths.ServiceEnvFile(), vars, "test fixture")
	assert.NewAborting(f.t).NoError(err, "SetEnvFile")
}

func (f *setupFixture) seedProfile(name, token string) {
	f.t.Helper()
	c := assert.NewAborting(f.t)
	c.NoError(profile.Save(profile.Set{Profiles: map[string]profile.Profile{
		name: {Name: name, Socket: paths.SocketPath(), Proxy: "http://127.0.0.1:8035", Kind: "claude"},
	}}), "profile.Save")
	if token != "" {
		c.NoError(profile.WriteToken(name, token), "WriteToken")
	} else {
		_ = os.Remove(profile.TokenFile(name))
	}
	c.NoError(profile.SavePointer(name), "SavePointer")
}

func (f *setupFixture) setFlag(name, value string) {
	f.t.Helper()
	assert.NewAborting(f.t).NoError(f.cmd.Flags().Set(name, value), "set --%s", name)
}

// run drives runSetup with a non-interactive wizard built AFTER the test set
// its flags (newWizard reads --non-interactive at construction).
func (f *setupFixture) runSetup() error {
	w := newWizard(f.cmd)
	w.in = &bytes.Buffer{}
	w.out = &f.out
	w.interactive = false
	return runSetup(context.Background(), f.cmd, w, f.run, &f.out)
}

func (f *setupFixture) serviceEnvBytes() string {
	b, _ := os.ReadFile(paths.ServiceEnvFile())
	return string(b)
}

func (f *setupFixture) lastStep() string {
	st, _, err := installstate.Load()
	assert.NewAborting(f.t).NoError(err, "installstate.Load")
	return st.LastStep
}

// ranArg reports whether any recorded Run call carried an argv element equal to
// arg.
func (f *setupFixture) ranArg(arg string) bool {
	for _, c := range f.run.runCalls {
		for _, a := range c.argv {
			if a == arg {
				return true
			}
		}
	}
	return false
}

// ranRunKey reports whether any Run call's joined argv STARTS WITH prefix.
func (f *setupFixture) ranRunKey(prefix string) bool {
	for _, c := range f.run.runCalls {
		if strings.HasPrefix(runKey(c.argv), prefix) {
			return true
		}
	}
	return false
}

// ── event helpers ────────────────────────────────────────────────────────────

func assistantEvent(id, text string) *rafikiv1.Event {
	return &rafikiv1.Event{ChildId: id, Payload: &rafikiv1.Event_AssistantMessage{
		AssistantMessage: &rafikiv1.AssistantMessage{Content: []*rafikiv1.ContentBlock{{
			Index: 0, Block: &rafikiv1.ContentBlock_Text{Text: &rafikiv1.TextBlock{Text: text}},
		}}},
	}}
}

func turnEndEvent(id string) *rafikiv1.Event {
	return &rafikiv1.Event{ChildId: id, Payload: &rafikiv1.Event_TurnEnd{TurnEnd: &rafikiv1.TurnEnd{}}}
}

// stubKeyServer points keyCheckBaseURLs at a server answering status.
func stubKeyServer(t *testing.T, status int) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
	}))
	t.Cleanup(srv.Close)
	orig := keyCheckBaseURLs
	t.Cleanup(func() { keyCheckBaseURLs = orig })
	keyCheckBaseURLs = map[string]string{"openrouter": srv.URL, "anthropic": srv.URL}
}

// stubKeyServerCounting points keyCheckBaseURLs at a counting server that
// answers status, and returns the request counter.
func stubKeyServerCounting(t *testing.T, status int) *atomic.Int64 {
	t.Helper()
	var count atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		count.Add(1)
		w.WriteHeader(status)
	}))
	t.Cleanup(srv.Close)
	orig := keyCheckBaseURLs
	t.Cleanup(func() { keyCheckBaseURLs = orig })
	keyCheckBaseURLs = map[string]string{"openrouter": srv.URL, "anthropic": srv.URL}
	return &count
}

// ── tests ────────────────────────────────────────────────────────────────────

// TestSetupSecondRunIsNoop: a fully-configured machine spawns nothing, mints
// nothing and touches no service.
func TestSetupSecondRunIsNoop(t *testing.T) {
	c := assert.NewAborting(t)
	f := newSetupFixture(t)

	c.NoError(f.runSetup(), "a second setup run must succeed")

	if f.ranRunKey("docker run") {
		t.Error("a no-op re-run ran `docker run`")
	}
	if f.ranArg("create") && f.ranArg("--admin") {
		t.Error("a no-op re-run created a user")
	}
	if f.ranArg("mint") {
		t.Error("a no-op re-run minted a token")
	}
	if f.fb.installed.DaemonBinary != "" {
		t.Error("a no-op re-run reinstalled the service")
	}
	if f.fb.restarted {
		t.Error("a no-op re-run restarted the service")
	}
}

// TestSetupSavesProgressPerStep: a failure at migrate leaves last_step pointing
// at the last COMPLETED step.
func TestSetupSavesProgressPerStep(t *testing.T) {
	c := assert.NewAborting(t)
	f := newSetupFixture(t)
	f.run.scriptErr([]string{f.rafikid, "migrate"}, errors.New("boom"))

	if err := f.runSetup(); err == nil {
		t.Fatal("setup must fail when migrate fails")
	}
	c.Eq(stepDatabase, f.lastStep(), "last_step after failing at migrate")
}

// TestSetupKeyInvalidUnderNonInteractiveFails: a rejected key is fatal when there is no
// terminal to re-prompt.
func TestSetupKeyInvalidUnderNonInteractiveFails(t *testing.T) {
	c := assert.NewAborting(t)
	f := newSetupFixture(t)
	f.setFlag("non-interactive", "true")
	f.setFlag("openrouter-key", "bad-key")
	stubKeyServer(t, http.StatusUnauthorized)

	err := f.runSetup()
	c.Error(err, "an invalid key under --non-interactive must fail")
	c.StrContains(err.Error(), "invalid", "the error must say the key is invalid:\n")
}

// TestSetupKeyValidIsWritten: a key that validates lands in service.env.
func TestSetupKeyValidIsWritten(t *testing.T) {
	c := assert.NewAborting(t)
	f := newSetupFixture(t)
	f.setFlag("openrouter-key", "sk-good")
	stubKeyServer(t, http.StatusOK)

	c.NoError(f.runSetup(), "setup with a valid key")
	c.StrContains(f.serviceEnvBytes(), "OPENROUTER_API_KEY", "a valid key must be written to service.env:\n")

	pb, err := os.ReadFile(paths.ProvidersFile())
	c.NoError(err, "read providers.toml")
	c.StrContains(string(pb), `default_provider = "openrouter"`, "an openrouter-only install must default to openrouter:\n")
}

// TestSetupNoProviderPrintsNote: with no provider, the operator sees the exact note.
func TestSetupNoProviderPrintsNote(t *testing.T) {
	c := assert.NewAborting(t)
	f := newSetupFixture(t)

	c.NoError(f.runSetup(), "setup with no provider")
	c.StrContains(f.out.String(), noProviderNote, "the no-provider note is missing or altered:\n")
}

// TestSetupExposeDefaultsToLoopback: without --expose the proxy binds loopback.
func TestSetupExposeDefaultsToLoopback(t *testing.T) {
	c := assert.NewAborting(t)
	f := newSetupFixture(t)
	f.seedServiceEnv(map[string]string{"RAFIKI_DB": constSetupDSN})

	c.NoError(f.runSetup(), "setup")
	c.StrContains(f.serviceEnvBytes(), "RAFIKI_PROXY_LISTEN=\"127.0.0.1:8035\"", "the default bind must be loopback:\n")
	c.NotStrContains(f.out.String(), "cleartext", "a loopback bind must not warn about cleartext:\n")
}

// TestSetupExposeWarnsCleartext: --expose binds every interface and warns.
func TestSetupExposeWarnsCleartext(t *testing.T) {
	c := assert.NewAborting(t)
	f := newSetupFixture(t)
	f.seedServiceEnv(map[string]string{"RAFIKI_DB": constSetupDSN})
	f.setFlag("expose", "true")
	f.setFlag("non-interactive", "true")

	c.NoError(f.runSetup(), "setup --expose")
	c.StrContains(f.serviceEnvBytes(), "RAFIKI_PROXY_LISTEN=\":8035\"", "an exposed bind must be all-interfaces:\n")
	c.StrContains(f.out.String(), "cleartext", "exposing without the cleartext warning:\n")
}

// TestSetupIdentityExistingUserMintsToken: create saying "already exists" falls
// through to token mint.
func TestSetupIdentityExistingUserMintsToken(t *testing.T) {
	c := assert.NewAborting(t)
	f := newSetupFixture(t)
	f.seedProfile("default", "") // no token yet
	f.run.scriptFail([]string{f.rafikid, "user", "create", "bob", "--admin", "--token"},
		errors.New("exit 1"), `user "bob" already exists`)
	f.run.script([]string{f.rafikid, "user", "token", "mint", "bob"}, "token: sk-minted\n")

	c.NoError(f.runSetup(), "setup with an existing user")
	c.True(f.ranRunKey(f.rafikid+" user token mint bob"), "mint was not called after create reported an existing user")
	c.Eq("sk-minted", profile.ReadToken("default"), "the minted token must reach the profile's token file")
}

// TestSetupTokenNeverPrinted: the token never lands on stdout.
func TestSetupTokenNeverPrinted(t *testing.T) {
	c := assert.NewAborting(t)
	f := newSetupFixture(t)
	f.seedProfile("default", "")
	f.run.script([]string{f.rafikid, "user", "create", "bob", "--admin", "--token"},
		"username: bob\ntoken: sk-secret-token\n")

	c.NoError(f.runSetup(), "setup")
	c.NotStrContains(f.out.String(), "sk-secret-token", "the token leaked to stdout:\n")
}

// TestSetupIdentityDSNNotInArgv: the DSN travels in the environment, never argv.
func TestSetupIdentityDSNNotInArgv(t *testing.T) {
	c := assert.NewAborting(t)
	f := newSetupFixture(t)
	f.seedProfile("default", "")
	f.run.script([]string{f.rafikid, "user", "create", "bob", "--admin", "--token"},
		"username: bob\ntoken: sk-created\n")

	c.NoError(f.runSetup(), "setup")

	var userCall *fakeCall
	for i := range f.run.runCalls {
		if len(f.run.runCalls[i].argv) > 1 && f.run.runCalls[i].argv[1] == "user" {
			userCall = &f.run.runCalls[i]
			break
		}
	}
	c.Require().NotNil(userCall, "no `rafikid user ...` call was made")
	c.Contains(userCall.env, "RAFIKI_DB="+constSetupDSN, "the DSN must reach rafikid in the environment")
	for _, a := range userCall.argv {
		c.NotStrContains(a, constSetupDSN, "the DSN must not appear in argv")
		c.NotStrContains(a, "secretpw", "the DSN password must not appear in argv")
	}
}

// TestSetupRestartWarnsAboutLiveChildren: a service.env change with live
// children prints the interruption warning before restarting.
func TestSetupRestartWarnsAboutLiveChildren(t *testing.T) {
	c := assert.NewAborting(t)
	f := newSetupFixture(t)
	f.seedServiceEnv(map[string]string{"RAFIKI_DB": constSetupDSN}) // no proxy-listen yet → change
	f.stub.liveChildren = 2

	c.NoError(f.runSetup(), "setup")
	c.StrContains(f.out.String(), "restarting rafikid will interrupt 2 running agent(s)", "the live-children warning is missing:\n")
	c.True(f.fb.restarted, "the daemon was not restarted after a service.env change")
}

// TestSetupLastServiceLogLines: the timeout path prints the log tail, and a
// missing log is empty rather than an error.
func TestSetupLastServiceLogLines(t *testing.T) {
	c := assert.NewAborting(t)
	isolateProfiles(t)

	c.Eq("", lastServiceLogLines(20), "a missing log must read as empty")

	c.NoError(os.MkdirAll(filepath.Dir(paths.ServiceLogPath()), 0o700), "mkdir log dir")
	var b strings.Builder
	for i := 1; i <= 25; i++ {
		fmt.Fprintf(&b, "line %d\n", i)
	}
	c.NoError(os.WriteFile(paths.ServiceLogPath(), []byte(b.String()), 0o600), "write log")

	got := lastServiceLogLines(20)
	c.StrContains(got, "line 25", "the tail must include the newest line:\n")
	c.NotStrContains(got, "line 5\n", "the tail must be bounded to the last N lines:\n")
}

// TestSetupValidateKeyAnthropic exercises the Anthropic branch of validateKey.
func TestSetupValidateKeyAnthropic(t *testing.T) {
	c := assert.NewAborting(t)
	stubKeyServer(t, http.StatusOK)
	c.NoError(validateKey(context.Background(), "anthropic", "sk-ant"), "a 200 must validate")

	stubKeyServer(t, http.StatusForbidden)
	c.Error(validateKey(context.Background(), "anthropic", "sk-ant"), "a non-200 must not validate")

	c.Error(validateKey(context.Background(), "nope", "k"), "an unknown provider must error")
}

// TestSetupUnchangedKeyNotValidated: a saved key that is not changed must not
// trigger a live validation call — a second run must not need the network, and
// must not hard-fail under --non-interactive on an offline machine. The server answers 500,
// so a validation call would also fail the run.
func TestSetupUnchangedKeyNotValidated(t *testing.T) {
	c := assert.NewAborting(t)
	f := newSetupFixture(t)
	f.seedServiceEnv(map[string]string{
		"RAFIKI_DB":           constSetupDSN,
		"RAFIKI_PROXY_LISTEN": setupProxyListenLoopback,
		"OPENROUTER_API_KEY":  "sk-unchanged",
	})
	count := stubKeyServerCounting(t, http.StatusInternalServerError)

	c.NoError(f.runSetup(), "a re-run with an unchanged key must not fail")
	c.Eq(int64(0), count.Load(), "an unchanged saved key must not be validated with a live call")
	c.StrContains(f.serviceEnvBytes(), "OPENROUTER_API_KEY", "the saved key must survive the re-run:\n")
}

// TestSetupProvidersChangeRestartsDaemon: a new provider key rewrites service.env
// (and providers.toml) through the providers step, which must ride the existing
// envChanged → restart path.
func TestSetupProvidersChangeRestartsDaemon(t *testing.T) {
	c := assert.NewAborting(t)
	f := newSetupFixture(t)
	f.seedServiceEnv(map[string]string{
		"RAFIKI_DB":           constSetupDSN,
		"RAFIKI_PROXY_LISTEN": setupProxyListenLoopback,
	})
	f.setFlag("openrouter-key", "sk-good")
	stubKeyServer(t, http.StatusOK)

	c.NoError(f.runSetup(), "setup adding a provider key")
	c.True(f.fb.restarted, "a provider change must restart the installed daemon")
}

// TestSetupInstallCapturesShellDaemonEnv: launchd and systemd run the daemon
// without a login shell, so a fresh service install carries the invoking shell's
// daemon settings into the unit, while setup's own service.env answers stay.
func TestSetupInstallCapturesShellDaemonEnv(t *testing.T) {
	c := assert.NewAborting(t)
	f := newSetupFixture(t)
	f.fb.status = serviceStatus{Installed: false}
	t.Setenv("RAFIKI_DEFAULT_MODEL", "some/model")
	t.Setenv("RAFIKI_DB", "postgres://shell@elsewhere/other")

	c.NoError(f.runSetup(), "setup")

	c.Eq("some/model", f.fb.installed.ExtraEnv["RAFIKI_DEFAULT_MODEL"])
	vars, err := readEnvFile(paths.ServiceEnvFile())
	c.NoError(err, "readEnvFile")
	c.Eq(constSetupDSN, vars["RAFIKI_DB"])
}

// TestSetupReinstallsServicePointingAtAnotherBinary: an installed unit whose
// program is not the rafikid next to this client (a source install replaced by a
// release) is rewritten, with the shell's daemon env captured, rather than left
// running the old binary.
func TestSetupReinstallsServicePointingAtAnotherBinary(t *testing.T) {
	c := assert.NewAborting(t)
	f := newSetupFixture(t)
	f.fb.status = serviceStatus{Installed: true, Binary: "/old/source/rafikid"}
	t.Setenv("RAFIKI_DEFAULT_MODEL", "some/model")

	c.NoError(f.runSetup(), "setup")

	c.Eq(f.rafikid, f.fb.installed.DaemonBinary)
	c.Eq("some/model", f.fb.installed.ExtraEnv["RAFIKI_DEFAULT_MODEL"])
}

// TestSetupLeavesServiceOnCurrentBinaryAlone: a unit already pointing at this
// rafikid is not rewritten.
func TestSetupLeavesServiceOnCurrentBinaryAlone(t *testing.T) {
	c := assert.NewAborting(t)
	f := newSetupFixture(t)
	f.fb.status = serviceStatus{Installed: true, Binary: f.rafikid}

	c.NoError(f.runSetup(), "setup")

	c.Eq("", f.fb.installed.DaemonBinary)
}

// TestSetupRepointsExecutorServiceOnAnotherBinary: an installed executor unit
// that launches a different rafiki is moved onto this one, args untouched.
func TestSetupRepointsExecutorServiceOnAnotherBinary(t *testing.T) {
	c := assert.NewAborting(t)
	f := newSetupFixture(t)
	f.execFB.status = serviceStatus{Installed: true, Binary: "/old/source/rafiki"}
	c.NoError(f.cmd.Flags().Set("non-interactive", "true"), "set flag")

	c.NoError(f.runSetup(), "setup")

	self, err := os.Executable()
	c.NoError(err, "os.Executable")
	c.Eq(self, f.execFB.repointed)
}

// TestSetupLeavesExecutorServiceOnThisBinaryAlone: nothing to repoint.
func TestSetupLeavesExecutorServiceOnThisBinaryAlone(t *testing.T) {
	c := assert.NewAborting(t)
	f := newSetupFixture(t)
	self, err := os.Executable()
	c.NoError(err, "os.Executable")
	f.execFB.status = serviceStatus{Installed: true, Binary: self}

	c.NoError(f.runSetup(), "setup")

	c.Eq("", f.execFB.repointed)
}

// TestSetupGeneratesSecretsKey: a service.env with no RAFIKI_SECRETS_KEY gets
// one, in the shape pkg/secretbox.FromEnv accepts, and the operator is told to
// back it up. A generated key is a service.env change, so an installed daemon is
// restarted onto it.
func TestSetupGeneratesSecretsKey(t *testing.T) {
	c := assert.NewAborting(t)
	f := newSetupFixture(t)
	f.seedServiceEnv(map[string]string{
		"RAFIKI_DB":           constSetupDSN,
		"RAFIKI_PROXY_LISTEN": setupProxyListenLoopback,
	})

	c.NoError(f.runSetup(), "setup with no secrets key")

	vars, err := readEnvFile(paths.ServiceEnvFile())
	c.NoError(err, "readEnvFile")
	raw := vars[secretbox.EnvKey]
	c.NotEq("", raw, "setup must generate a key:\n%s", f.serviceEnvBytes())
	key, derr := base64.StdEncoding.DecodeString(raw)
	c.NoError(derr, "the generated key must be base64 of 32 bytes")
	c.Len(key, 32, "the generated key must be 32 bytes")

	c.StrContains(f.out.String(), "generated RAFIKI_SECRETS_KEY in", "the operator must be told a key was generated:\n")
	c.StrContains(f.out.String(), "back it up: losing it logs every user out of ChatGPT providers", "the backup warning is missing:\n")
	c.True(f.fb.restarted, "a generated key must reach an installed daemon")
}

// TestSetupKeepsExistingSecretsKey: an existing key is never replaced. A new key
// cannot open a single stored login, so replacing it would log every user out of
// the ChatGPT-subscription providers.
func TestSetupKeepsExistingSecretsKey(t *testing.T) {
	c := assert.NewAborting(t)
	f := newSetupFixture(t) // the baseline service.env already carries one

	c.NoError(f.runSetup(), "setup")

	vars, err := readEnvFile(paths.ServiceEnvFile())
	c.NoError(err, "readEnvFile")
	c.Eq(constSetupSecretsKey, vars[secretbox.EnvKey], "an existing key must be left alone")
	c.NotStrContains(f.out.String(), "generated RAFIKI_SECRETS_KEY", "an existing key must not be reported as generated:\n")
}

// TestSetupAdoptsSecretsKeyFromEnv: a key EXPORTED in the shell is the one the
// daemon is using (the environment outranks service.env), so setup carries that
// value into service.env rather than generating a second key, which would leave
// every login sealed under the exported one unopenable.
func TestSetupAdoptsSecretsKeyFromEnv(t *testing.T) {
	c := assert.NewAborting(t)
	f := newSetupFixture(t) // the baseline file already carries a key
	f.seedServiceEnv(map[string]string{
		"RAFIKI_DB":           constSetupDSN,
		"RAFIKI_PROXY_LISTEN": setupProxyListenLoopback,
	})
	exported := constSetupSecretsKey
	t.Setenv(secretbox.EnvKey, exported)

	c.NoError(f.runSetup(), "setup with the key exported in the shell")

	vars, err := readEnvFile(paths.ServiceEnvFile())
	c.NoError(err, "readEnvFile")
	c.Eq(exported, vars[secretbox.EnvKey], "the exported key must be the one written")
	c.StrContains(f.out.String(), "adopted RAFIKI_SECRETS_KEY from the environment", "the operator must be told the key was adopted:\n")
	c.NotStrContains(f.out.String(), "generated RAFIKI_SECRETS_KEY", "an adopted key is not a generated one:\n")
}
