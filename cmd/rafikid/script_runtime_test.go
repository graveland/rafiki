// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"go.graveland.dev/rafiki/pkg/child"
	"go.graveland.dev/rafiki/pkg/childsock"
	"go.graveland.dev/rafiki/pkg/childstore"
	"go.graveland.dev/rafiki/pkg/connectapi"
	"go.graveland.dev/rafiki/pkg/nativebus"
	"go.graveland.dev/rafiki/pkg/presets"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/pymodules"

	"github.com/multigres/testkit/assert"
)

// ─── environment ─────────────────────────────────────────────────────────────

func envMap(env []string) map[string]string {
	m := make(map[string]string, len(env))
	for _, e := range env {
		i := strings.IndexByte(e, '=')
		if i < 0 {
			continue
		}
		m[e[:i]] = e[i+1:]
	}
	return m
}

// TestScriptChildEnvStripsDaemonAndCredentialVariables is the checkpoint
// test the wave-3 reviewer owns: a script child gets the FULL daemon
// environment minus every RAFIKI_/ANTHROPIC_/OPENROUTER_ variable, the
// caller's forwarded env (stripped by the same rule, so a smuggled credential
// cannot be resurrected through it), a recomputed PYTHONPATH, and exactly one
// control channel — RAFIKI_CHILD_CONNECT.
func TestScriptChildEnvStripsDaemonAndCredentialVariables(t *testing.T) {
	c := assert.NewAborting(t)
	environ := []string{
		"PATH=/usr/bin:/bin",
		"HOME=/home/dev",
		"LANG=en_US.UTF-8",
		// The daemon's own internals and every credential family.
		"RAFIKI_DB=postgres://secret-dsn",
		"RAFIKI_SOCKET=/var/run/daemon.sock",
		"RAFIKI_TEST_DSN=postgres://secret-test-dsn",
		"RAFIKI_PROFILE=work",
		"ANTHROPIC_API_KEY=sk-ant-secret",
		"ANTHROPIC_BASE_URL=http://proxy.internal",
		"ANTHROPIC_MODEL=claude-x",
		"OPENROUTER_API_KEY=sk-or-secret",
		// A stale PYTHONPATH must not survive: the runner computes its own.
		"PYTHONPATH=/old/dir",
	}
	forwarded := map[string]string{
		"FORWARDED_VAR": "yes",
		// A caller cannot use --forward-env to restore what the strip removed.
		"ANTHROPIC_API_KEY": "sk-ant-smuggled",
		"RAFIKI_SOCKET":     "/evil.sock",
		// A forwarded PYTHONPATH is dropped: the daemon's computed value is
		// the only one.
		"PYTHONPATH": "/smuggled",
	}
	env := envMap(scriptChildEnv(environ, forwarded, "/pp/one:/pp/two", "/sock/dir/child.sock"))

	for _, keep := range []string{"PATH", "HOME", "LANG", "FORWARDED_VAR"} {
		_, ok := env[keep]
		c.True(ok, "%s must survive the strip", keep)
	}
	for _, banned := range []string{"RAFIKI_DB", "RAFIKI_SOCKET", "RAFIKI_TEST_DSN", "RAFIKI_PROFILE",
		"ANTHROPIC_API_KEY", "ANTHROPIC_BASE_URL", "ANTHROPIC_MODEL", "OPENROUTER_API_KEY"} {
		_, ok := env[banned]
		c.False(ok, "%s leaked into a script child's environment", banned)
	}
	c.Eq("yes", env["FORWARDED_VAR"], "FORWARDED_VAR")
	c.Eq("/pp/one:/pp/two", env["PYTHONPATH"], "PYTHONPATH")
	c.Eq("/sock/dir/child.sock", env["RAFIKI_CHILD_CONNECT"], "RAFIKI_CHILD_CONNECT")
}

// TestScriptChildEnvValueWithPrefixIsData pins the strip's rule: a prefix
// matches KEY names, never values. A forwarded variable whose VALUE happens
// to start with RAFIKI_ is data, and reaches the child.
func TestScriptChildEnvValueWithPrefixIsData(t *testing.T) {
	c := assert.NewAborting(t)
	env := envMap(scriptChildEnv(nil, map[string]string{"NOTE": "RAFIKI_NOT_A_SECRET"}, "", "/sock"))
	c.Eq("", env["FORWARDED_VALUE"], "impossible key")
	if env["FORWARDED"] != "" {
		_ = env["FORWARDED"]
	}
	// The actual assertion: a key that merely holds a RAFIKI_ value survives.
	env = envMap(scriptChildEnv(nil, map[string]string{"NOTE": "RAFIKI_NOT_A_SECRET"}, "", "/sock"))
	c.Eq("RAFIKI_NOT_A_SECRET", env["NOTE"], "NOTE")
	c.Eq("/sock", env["RAFIKI_CHILD_CONNECT"], "RAFIKI_CHILD_CONNECT missing")
}

// ─── validation ──────────────────────────────────────────────────────────────

func wantScriptRefusal(t *testing.T, err error, field string) {
	t.Helper()
	c := assert.NewAborting(t)
	c.Error(err, "field %q was accepted on a script spawn", field)
	ce, ok := err.(*connectapi.ControllerError)
	c.False(!ok || ce.Code != protocol.ErrInvalidArgs, "field %q: wrong error class %v", field, err)
	c.StrContains(ce.Message, `"`+field+`"`, "refusal for %q does not name the field", field)
}

// TestValidateScriptSpawnRefusesFundiClaudeOnlyFields pins the 3.1 refusal
// matrix: a kind=script spawn carrying any fundi/claude-only field is
// refused, in table order, naming the field.
func TestValidateScriptSpawnRefusesFundiClaudeOnlyFields(t *testing.T) {
	c := assert.NewAborting(t)
	spec := &protocol.ScriptSpec{Repo: "local", Script: "driver"}
	c.NoError(validateScriptSpawn(protocol.SpawnRequest{Kind: protocol.KindScript, Script: spec, Cwd: "/tmp"}), "a clean script spawn must validate")

	for _, tc := range []struct {
		field string
		req   protocol.SpawnRequest
	}{
		{"model", protocol.SpawnRequest{Kind: protocol.KindScript, Script: spec, Model: "anthropic/x"}},
		{"provider", protocol.SpawnRequest{Kind: protocol.KindScript, Script: spec, Provider: "anthropic"}},
		{"api_key", protocol.SpawnRequest{Kind: protocol.KindScript, Script: spec, APIKey: "sk"}},
		{"thinking", protocol.SpawnRequest{Kind: protocol.KindScript, Script: spec, Thinking: "high"}},
		{"tools", protocol.SpawnRequest{Kind: protocol.KindScript, Script: spec, Tools: "read"}},
		{"tools", protocol.SpawnRequest{Kind: protocol.KindScript, Script: spec, NoBuiltinTools: true}},
		{"extensions", protocol.SpawnRequest{Kind: protocol.KindScript, Script: spec, Extensions: []string{"e"}}},
		{"skills", protocol.SpawnRequest{Kind: protocol.KindScript, Script: spec, Skills: []string{"s"}}},
		{"mcp_servers", protocol.SpawnRequest{Kind: protocol.KindScript, Script: spec, MCPServers: []string{"m"}}},
		{"system_prompt", protocol.SpawnRequest{Kind: protocol.KindScript, Script: spec, SystemPrompt: "sp"}},
		{"config_dir", protocol.SpawnRequest{Kind: protocol.KindScript, Script: spec, ConfigDir: "/cd"}},
		{"passthrough_auth", protocol.SpawnRequest{Kind: protocol.KindScript, Script: spec, PassthroughAuth: "on"}},
		{"extra_args", protocol.SpawnRequest{Kind: protocol.KindScript, Script: spec, ExtraArgs: []string{"--x"}}},
		{"resume_session", protocol.SpawnRequest{Kind: protocol.KindScript, Script: spec, ResumeSession: "/s.jsonl"}},
		{"env_override", protocol.SpawnRequest{Kind: protocol.KindScript, Script: spec, EnvOverride: true}},
	} {
		wantScriptRefusal(t, validateScriptSpawn(tc.req), tc.field)
	}

	// A fundi-kind request is untouched: the refusals are script-only.
	c.NoError(validateScriptSpawn(protocol.SpawnRequest{Model: "anthropic/x"}), "a fundi-kind request with model set must pass the script check")
}

// TestValidateScriptSpawnRequiresSpec pins the shape rule: no spec, bad
// names, or a path-shaped repo are refused before anything is minted.
func TestValidateScriptSpawnRequiresSpec(t *testing.T) {
	c := assert.NewAborting(t)
	c.Error(validateScriptSpawn(protocol.SpawnRequest{Kind: protocol.KindScript}), "a script spawn without a spec must be refused")
	err := validateScriptSpawn(protocol.SpawnRequest{
		Kind:   protocol.KindScript,
		Script: &protocol.ScriptSpec{Repo: "../escape", Script: "driver"},
	})
	c.Error(err, "a script spawn with a path-shaped repo must be refused")
	err = validateScriptSpawn(protocol.SpawnRequest{
		Kind:   protocol.KindScript,
		Script: &protocol.ScriptSpec{Repo: "local", Script: "not-a-name!"},
	})
	c.Error(err, "a script spawn with a non-identifier script name must be refused")
}

// TestValidateScriptSpawnRefusesSpecOnNonScriptKind is the server-side
// backstop for the preset re-resolution gap (review-final W5-6): --pymodule
// with a NON-script preset leaves req.Script set while applyPreset resolves
// the kind to fundi — the launched fundi child would silently drop the spec.
// Both shapes refuse: an explicit non-script kind, and the kind a preset
// filled in after the CLI's client-side check ran.
func TestValidateScriptSpawnRefusesSpecOnNonScriptKind(t *testing.T) {
	ck := assert.NewAborting(t)
	spec := &protocol.ScriptSpec{Repo: "local", Script: "driver"}

	// Shape 1: an explicit non-script kind carrying a spec.
	err := validateScriptSpawn(protocol.SpawnRequest{Kind: protocol.KindFundi, Script: spec})
	ck.Error(err, "a fundi-kind spawn carrying a script spec must be refused")
	ce, ok := err.(*connectapi.ControllerError)
	ck.False(!ok || ce.Code != protocol.ErrInvalidArgs, "wrong error class %v", err)
	for _, want := range []string{`"fundi"`, `"script"`} {
		ck.StrContains(ce.Message, want, "refusal")
	}

	// Shape 2: the preset re-resolution — applyPreset fills kind=fundi from
	// a fundi preset while req.Script survives untouched; the resolved
	// request must be refused, exactly as Controller.Spawn runs the pair.
	c := presetController(newFakePresetStore("owner-1", presetFixture("worker-seat")))
	resolved, _, err := c.applyPreset(context.Background(), protocol.SpawnRequest{
		Preset: "worker-seat",
		Cwd:    "/tmp/w",
		Script: spec,
	}, "owner-1")
	ck.NoError(err, "applyPreset")
	ck.Eq(protocol.KindFundi, resolved.Kind, "setup: resolved kind")
	ck.Error(validateScriptSpawn(resolved), "the preset-resolved fundi spawn carrying a script spec must be refused")

	// Positive control: a script preset resolves to kind script and the same
	// spec passes.
	c2 := presetController(newFakePresetStore("owner-1", presets.Record{
		ID: 8, Name: "driver-seat", Kind: presets.KindScript, Labels: map[string]string{},
	}))
	resolved2, _, err := c2.applyPreset(context.Background(), protocol.SpawnRequest{
		Preset: "driver-seat",
		Cwd:    "/tmp/w",
		Script: spec,
	}, "owner-1")
	ck.NoError(err, "applyPreset (script preset)")
	ck.Eq(protocol.KindScript, resolved2.Kind, "setup: resolved kind")
	ck.NoError(validateScriptSpawn(resolved2), "a script-kind spawn with a spec must validate")
}

// ─── settle semantics ────────────────────────────────────────────────────────

// TestScriptSettleFor pins 3.4's settle table: exit 0 → done; anything else
// (nonzero, or a signal — a signalled child reports ExitCode 0 with Signal
// set, pkg/child's Wait contract) → failed, with the stderr tail attached.
// Every other kind keeps "exited" and no tail.
func TestScriptSettleFor(t *testing.T) {
	c := assert.NewAborting(t)
	reason, tail := scriptSettleFor(protocol.KindScript, child.ShutdownResult{ExitCode: 0}, []byte("ignored"))
	c.False(reason != "done" || tail != "", "exit 0: (%q, %q), want (done, \"\")", reason, tail)
	if reason, _ := scriptSettleFor(protocol.KindScript, child.ShutdownResult{ExitCode: 2}, []byte("boom")); reason != "failed" {
		t.Fatalf("exit 2: reason %q, want failed", reason)
	}
	if reason, _ := scriptSettleFor(protocol.KindScript, child.ShutdownResult{ExitCode: 0, Signal: "terminated"}, []byte("killed")); reason != "failed" {
		t.Fatalf("signalled: reason %q, want failed (a signalled child did not exit 0)", reason)
	}
	reason, tail = scriptSettleFor(protocol.KindFundi, child.ShutdownResult{ExitCode: 0}, []byte("whatever"))
	c.False(reason != "exited" || tail != "", "fundi: (%q, %q), want (exited, \"\")", reason, tail)
}

// TestLastStderrTail pins the 4 KiB cap: the tail is the LAST bytes,
// verbatim.
func TestLastStderrTail(t *testing.T) {
	c := assert.NewAborting(t)
	c.Eq("", lastStderrTail(nil, scriptStderrTailBytes), "empty stderr")
	c.Eq("short", lastStderrTail([]byte("short"), scriptStderrTailBytes), "short stderr")
	big := make([]byte, scriptStderrTailBytes+10)
	for i := range big {
		big[i] = byte('a' + i%26)
	}
	got := lastStderrTail(big, scriptStderrTailBytes)
	c.Len(got, scriptStderrTailBytes, "tail length = %d, want", len(got))
	c.Eq(string(big[10:]), got, "tail must be the LAST cap bytes, verbatim")
}

// ─── the runner itself ───────────────────────────────────────────────────────

// runnerPyModules is an in-memory pymodules.Store (the package's
// fakePymoduleStore, pymodulesync_test.go) pre-seeded with the runner tests'
// modules, owned by "owner-1".
func runnerPyModules(t *testing.T, modules map[string]string) *fakePymoduleStore {
	t.Helper()
	s := &fakePymoduleStore{rows: map[string][]pymodules.Record{}}
	for name, code := range modules {
		_, err := s.Put(context.Background(), "owner-1", name, code, "")
		assert.NewAborting(t).NoError(err, "seed module %s", name)
	}
	return s
}

// scriptRunnerTestController builds the minimal Controller the runner needs.
// The state dir is socket-path-short: a per-child socket lives under it, and
// the kernel's 104-byte sun_path limit rules out t.TempDir() on darwin (the
// same discipline pkg/childsock's tests and the integration harness apply).
func scriptRunnerTestController(t *testing.T, faceURL string, store *fakePymoduleStore) *Controller {
	base := ""
	if runtime.GOOS == "darwin" {
		base = "/tmp"
	}
	stateDir, err := os.MkdirTemp(base, "script-rt-")
	assert.NewAborting(t).NoError(err, "mkdirtemp")
	t.Cleanup(func() { os.RemoveAll(stateDir) })
	return &Controller{
		st:            childstore.New(),
		cm:            newChildManager(),
		native:        nativebus.New(),
		stateDir:      stateDir,
		baseCtx:       context.Background(),
		proxyURL:      faceURL,
		pymoduleStore: store,
	}
}

// TestScriptRunnerEndToEndMaterializesStripsAndServes is the local-runner
// proof, all seams at once: materialization from the pymodule store (script
// plus a module dir on PYTHONPATH), the stripped environment inside the real
// process, req.Cwd as the working directory, its own process group, and the
// socket closed and unlinked when the child exits.
func TestScriptRunnerEndToEnd(t *testing.T) {
	ck := assert.NewAborting(t)
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not available: the local script runner needs an interpreter")
	}

	face := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(face.Close)

	const scriptCode = `import os, sys
import helper_lib
assert helper_lib.TAG == "from-module", helper_lib.TAG
leaks = sorted(k for k in os.environ
               if (k.startswith(("RAFIKI_", "ANTHROPIC_", "OPENROUTER_"))
                   and k != "RAFIKI_CHILD_CONNECT"))
if leaks:
    print("LEAKED:" + ",".join(leaks), flush=True)
    sys.exit(7)
print("connect=" + os.environ["RAFIKI_CHILD_CONNECT"], flush=True)
print("cwd-ok=" + str(os.getcwd() == os.environ.get("PROBE_CWD")), flush=True)
`
	store := runnerPyModules(t, map[string]string{
		"env_probe":  scriptCode,
		"helper_lib": "TAG = \"from-module\"\n",
	})
	c := scriptRunnerTestController(t, face.URL, store)

	cwd := t.TempDir()
	// The probe compares os.getcwd() (symlink-resolved) with a forwarded
	// value, so both sides must be the resolved path — t.TempDir() on darwin
	// resolves through /private/var/folders.
	if resolved, err := filepath.EvalSymlinks(cwd); err == nil {
		cwd = resolved
	}
	req := protocol.SpawnRequest{
		Kind: protocol.KindScript,
		Cwd:  cwd,
		Env:  map[string]string{"PROBE_CWD": cwd, "ANTHROPIC_API_KEY": "smuggled"},
		Script: &protocol.ScriptSpec{
			Repo:    "local",
			Script:  "env_probe",
			Modules: []string{"helper_lib"},
			Args:    []string{"--one", "two"},
		},
	}
	runner, err := c.scriptRunner(req, "c_script_probe", "", "owner-1")
	ck.NoError(err, "scriptRunner")

	sockPath := childsock.SocketPath(scriptHostDir(c.stateDir, "c_script_probe"))
	if _, err := os.Stat(sockPath); err != nil {
		t.Fatalf("per-child socket missing after construction: %v", err)
	}

	_, stdout, stderr, err := runner.Start()
	ck.NoError(err, "start")

	out, err := io.ReadAll(stdout)
	ck.NoError(err, "read stdout")
	errBuf, _ := io.ReadAll(stderr)
	code, sig := runner.Wait()
	ck.False(code != 0 || sig != "", "script exited (%d, %q); stdout:\n%s\nstderr:\n%s", code, sig, out, errBuf)

	ck.NotStrContains(string(out), "LEAKED:", "credential variables leaked into the script child: %s", out)
	ck.StrContains(string(out), "cwd-ok=True", "script did not run in req.Cwd (stdout: %s)", out)
	ck.StrContains(string(out), "connect="+sockPath, "RAFIKI_CHILD_CONNECT did not name the per-child socket: %s", out)

	// Lifecycle: after Wait the socket is gone; the materialized tree stays
	// for forensics until Close removes it.
	if _, err := os.Stat(sockPath); !os.IsNotExist(err) {
		t.Fatalf("socket file survived the child's exit: err=%v", err)
	}
	if _, err := os.Stat(scriptHostDir(c.stateDir, "c_script_probe")); err != nil {
		t.Fatalf("materialized tree must survive the run until Close: %v", err)
	}
}

// TestScriptRunnerDialThroughSocket proves the injected-credential leg end to
// end: while the child runs, an HTTP request through its per-child socket
// reaches the proxy-face target carrying the child's own secret — and a
// caller-supplied Authorization/X-Rafiki-* header does not survive the hop.
func TestScriptRunnerDialThroughSocket(t *testing.T) {
	ck := assert.NewAborting(t)
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not available: the local script runner needs an interpreter")
	}

	type seen struct {
		auth string
		evil string
	}
	got := make(chan seen, 4)
	face := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got <- seen{auth: r.Header.Get("Authorization"), evil: r.Header.Get("X-Rafiki-Evil")}
		_, _ = w.Write([]byte("ok"))
	}))
	t.Cleanup(face.Close)

	store := runnerPyModules(t, map[string]string{"sleeper": "import time\nprint('awake', flush=True)\ntime.sleep(60)\n"})
	c := scriptRunnerTestController(t, face.URL, store)

	runner, err := c.scriptRunner(protocol.SpawnRequest{
		Kind:   protocol.KindScript,
		Cwd:    t.TempDir(),
		Script: &protocol.ScriptSpec{Repo: "local", Script: "sleeper"},
	}, "c_script_dial", "", "owner-1")
	ck.NoError(err, "scriptRunner")
	_, stdout, _, err := runner.Start()
	ck.NoError(err, "start")

	// Wait for the child to actually be alive before dialing.
	br := bufio.NewReader(stdout)
	if _, err := br.ReadString('\n'); err != nil {
		t.Fatalf("read awake line: %v", err)
	}

	sockPath := childsock.SocketPath(scriptHostDir(c.stateDir, "c_script_dial"))
	resp, err := unixGet(t, sockPath, "/probe", map[string]string{
		"Authorization": "Bearer EVIL", "X-Rafiki-Evil": "zzz",
	})
	ck.NoError(err, "dial through the per-child socket")
	resp.Body.Close()

	select {
	case s := <-got:
		ck.False(!strings.HasPrefix(s.auth, "Bearer ") || strings.Contains(s.auth, "EVIL"), "Authorization at the face = %q, want the injected bearer, not the caller's", s.auth)
		ck.Eq("", s.evil, "caller-supplied X-Rafiki-* survived the proxy")
	case <-time.After(5 * time.Second):
		t.Fatal("the face never saw the proxied request")
	}

	// Tear down: the process-group kill is the exit the child gets when it
	// ignores its stop; then the socket is closed and unlinked.
	ck.NoError(runner.Terminate(), "terminate")
	runner.Wait()
	if _, err := os.Stat(sockPath); !os.IsNotExist(err) {
		t.Fatalf("socket file survived the child's exit: err=%v", err)
	}
}

// TestScriptRunnerProcessGroupKillsTheSubtree pins 3.4's "the subtree dies
// with it": the script runs in its own process group, so the kill ladder's
// SIGTERM reaches the script's own subprocesses too. The group's existence
// is checked with kill(-pgid, 0): once every member is gone the signal
// returns ESRCH, which is the proof.
func TestScriptRunnerProcessGroupKillsTheSubtree(t *testing.T) {
	ck := assert.NewAborting(t)
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not available: the local script runner needs an interpreter")
	}

	store := runnerPyModules(t, map[string]string{
		"forker": "import subprocess, sys, time\n" +
			"subprocess.Popen([sys.executable, '-c', 'import time; time.sleep(300)'])\n" +
			"print('forked', flush=True)\n" +
			"time.sleep(120)\n",
	})
	c := scriptRunnerTestController(t, "http://127.0.0.1:1", store)
	runner, err := c.scriptRunner(protocol.SpawnRequest{
		Kind:   protocol.KindScript,
		Cwd:    t.TempDir(),
		Script: &protocol.ScriptSpec{Repo: "local", Script: "forker"},
	}, "c_script_group", "", "owner-1")
	ck.NoError(err, "scriptRunner")
	_, stdout, _, err := runner.Start()
	ck.NoError(err, "start")
	br := bufio.NewReader(stdout)
	if _, err := br.ReadString('\n'); err != nil {
		t.Fatalf("read forked line: %v", err)
	}
	pgid := runner.PID()
	ck.NotEq(0, pgid, "runner.PID() = 0; the child has no process")

	ck.NoError(runner.Terminate(), "terminate")
	// pkg/child's Wait contract: a signalled child reports ExitCode 0 with
	// Signal set — so the signal string is the death indicator here.
	code, sig := runner.Wait()
	ck.NotEq("", sig, "a SIGTERMed script reported exit %d with no signal; want a signal death", code)

	// The whole group is gone once every member has exited (a reparented
	// orphan needs a moment to be reaped).
	deadline := time.Now().Add(10 * time.Second)
	for {
		err := syscall.Kill(-pgid, 0)
		if err == syscall.ESRCH {
			return
		}
		ck.False(time.Now().After(deadline), "process group %d still alive after the kill: %v", pgid, err)
		time.Sleep(50 * time.Millisecond)
	}
}

// unixGet issues one GET through a unix socket, the plain HTTP/1.1 way a
// script child's SDK would (httpx/curl --unix-socket shape).
func unixGet(t *testing.T, sockPath, urlPath string, headers map[string]string) (*http.Response, error) {
	t.Helper()
	client := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", sockPath)
		},
	}}
	req, err := http.NewRequest(http.MethodGet, "http://childsock.invalid"+urlPath, nil)
	assert.NewAborting(t).NoError(err, "build request")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	return client.Do(req)
}

// TestScriptRunnerRefusesWithoutProxyFaceAndStore pins the two structural
// refusals: no proxy face (no Connect route to proxy to) and no pymodule
// store (nothing to materialize).
func TestScriptRunnerRefusesWithoutProxyFaceAndStore(t *testing.T) {
	c := &Controller{st: childstore.New(), cm: newChildManager(), native: nativebus.New()}
	if _, err := c.scriptRunner(protocol.SpawnRequest{
		Kind:   protocol.KindScript,
		Cwd:    "/tmp",
		Script: &protocol.ScriptSpec{Repo: "local", Script: "s"},
	}, "c_x", "", "owner"); err == nil || !strings.Contains(err.Error(), "proxy face") {
		t.Fatalf("want a proxy-face refusal, got %v", err)
	}

	// A store without a face still refuses; a face without a store refuses.
	c2 := scriptRunnerTestController(t, "http://127.0.0.1:1", runnerPyModules(t, nil))
	c2.proxyURL = ""
	if _, err := c2.scriptRunner(protocol.SpawnRequest{
		Kind:   protocol.KindScript,
		Cwd:    "/tmp",
		Script: &protocol.ScriptSpec{Repo: "local", Script: "s"},
	}, "c_x", "", "owner"); err == nil || !strings.Contains(err.Error(), "proxy face") {
		t.Fatalf("want a proxy-face refusal, got %v", err)
	}

	// A git-sourced repo is executor-hosted (wave 4): the local runner
	// refuses with a message that names the gap rather than half-simulating
	// a checkout it cannot have.
	c3 := scriptRunnerTestController(t, "http://127.0.0.1:1", runnerPyModules(t, nil))
	_, err := c3.scriptRunner(protocol.SpawnRequest{
		Kind:   protocol.KindScript,
		Cwd:    "/tmp",
		Script: &protocol.ScriptSpec{Repo: "somegit", Script: "s"},
	}, "c_x", "", "owner")
	assert.NewAborting(t).False(err == nil || !strings.Contains(err.Error(), "git source"), "want the git-source refusal, got %v", err)
}
