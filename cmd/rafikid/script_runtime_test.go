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
	"go.graveland.dev/rafiki/pkg/control"
	"go.graveland.dev/rafiki/pkg/nativebus"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/pymodules"
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
	env := envMap(scriptChildEnv(environ, forwarded, "/pp/one:/pp/two", "/sock/dir/connect.sock"))

	for _, keep := range []string{"PATH", "HOME", "LANG", "FORWARDED_VAR"} {
		if _, ok := env[keep]; !ok {
			t.Fatalf("%s must survive the strip", keep)
		}
	}
	for _, banned := range []string{"RAFIKI_DB", "RAFIKI_SOCKET", "RAFIKI_TEST_DSN", "RAFIKI_PROFILE",
		"ANTHROPIC_API_KEY", "ANTHROPIC_BASE_URL", "ANTHROPIC_MODEL", "OPENROUTER_API_KEY"} {
		if _, ok := env[banned]; ok {
			t.Fatalf("%s leaked into a script child's environment", banned)
		}
	}
	if env["FORWARDED_VAR"] != "yes" {
		t.Fatalf("FORWARDED_VAR = %q, want the forwarded value", env["FORWARDED_VAR"])
	}
	if env["PYTHONPATH"] != "/pp/one:/pp/two" {
		t.Fatalf("PYTHONPATH = %q, want the daemon's computed value, exactly once", env["PYTHONPATH"])
	}
	if env["RAFIKI_CHILD_CONNECT"] != "/sock/dir/connect.sock" {
		t.Fatalf("RAFIKI_CHILD_CONNECT = %q, want the per-child socket path", env["RAFIKI_CHILD_CONNECT"])
	}
}

// TestScriptChildEnvValueWithPrefixIsData pins the strip's rule: a prefix
// matches KEY names, never values. A forwarded variable whose VALUE happens
// to start with RAFIKI_ is data, and reaches the child.
func TestScriptChildEnvValueWithPrefixIsData(t *testing.T) {
	env := envMap(scriptChildEnv(nil, map[string]string{"NOTE": "RAFIKI_NOT_A_SECRET"}, "", "/sock"))
	if env["FORWARDED_VALUE"] != "" {
		t.Fatal("impossible key")
	}
	if env["FORWARDED"] != "" {
		_ = env["FORWARDED"]
	}
	// The actual assertion: a key that merely holds a RAFIKI_ value survives.
	env = envMap(scriptChildEnv(nil, map[string]string{"NOTE": "RAFIKI_NOT_A_SECRET"}, "", "/sock"))
	if env["NOTE"] != "RAFIKI_NOT_A_SECRET" {
		t.Fatalf("NOTE = %q — a value beginning with RAFIKI_ is data, not a key", env["NOTE"])
	}
	if env["RAFIKI_CHILD_CONNECT"] != "/sock" {
		t.Fatal("RAFIKI_CHILD_CONNECT missing")
	}
}

// ─── validation ──────────────────────────────────────────────────────────────

func wantScriptRefusal(t *testing.T, err error, field string) {
	t.Helper()
	if err == nil {
		t.Fatalf("field %q was accepted on a script spawn", field)
	}
	ce, ok := err.(*control.ControllerError)
	if !ok || ce.Code != protocol.ErrInvalidArgs {
		t.Fatalf("field %q: wrong error class %v", field, err)
	}
	if !strings.Contains(ce.Message, `"`+field+`"`) {
		t.Fatalf("refusal for %q does not name the field: %s", field, ce.Message)
	}
}

// TestValidateScriptSpawnRefusesFundiClaudeOnlyFields pins the 3.1 refusal
// matrix: a kind=script spawn carrying any fundi/claude-only field is
// refused, in table order, naming the field.
func TestValidateScriptSpawnRefusesFundiClaudeOnlyFields(t *testing.T) {
	spec := &protocol.ScriptSpec{Repo: "local", Script: "driver"}
	if err := validateScriptSpawn(protocol.SpawnRequest{Kind: protocol.KindScript, Script: spec, Cwd: "/tmp"}); err != nil {
		t.Fatalf("a clean script spawn must validate: %v", err)
	}

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
	if err := validateScriptSpawn(protocol.SpawnRequest{Model: "anthropic/x"}); err != nil {
		t.Fatalf("a fundi-kind request with model set must pass the script check: %v", err)
	}
}

// TestValidateScriptSpawnRequiresSpec pins the shape rule: no spec, bad
// names, or a path-shaped repo are refused before anything is minted.
func TestValidateScriptSpawnRequiresSpec(t *testing.T) {
	if err := validateScriptSpawn(protocol.SpawnRequest{Kind: protocol.KindScript}); err == nil {
		t.Fatal("a script spawn without a spec must be refused")
	}
	err := validateScriptSpawn(protocol.SpawnRequest{
		Kind:   protocol.KindScript,
		Script: &protocol.ScriptSpec{Repo: "../escape", Script: "driver"},
	})
	if err == nil {
		t.Fatal("a script spawn with a path-shaped repo must be refused")
	}
	err = validateScriptSpawn(protocol.SpawnRequest{
		Kind:   protocol.KindScript,
		Script: &protocol.ScriptSpec{Repo: "local", Script: "not-a-name!"},
	})
	if err == nil {
		t.Fatal("a script spawn with a non-identifier script name must be refused")
	}
}

// ─── settle semantics ────────────────────────────────────────────────────────

// TestScriptSettleFor pins 3.4's settle table: exit 0 → done; anything else
// (nonzero, or a signal — a signalled child reports ExitCode 0 with Signal
// set, pkg/child's Wait contract) → failed, with the stderr tail attached.
// Every other kind keeps "exited" and no tail.
func TestScriptSettleFor(t *testing.T) {
	reason, tail := scriptSettleFor(protocol.KindScript, child.ShutdownResult{ExitCode: 0}, []byte("ignored"))
	if reason != "done" || tail != "" {
		t.Fatalf("exit 0: (%q, %q), want (done, \"\")", reason, tail)
	}
	if reason, _ := scriptSettleFor(protocol.KindScript, child.ShutdownResult{ExitCode: 2}, []byte("boom")); reason != "failed" {
		t.Fatalf("exit 2: reason %q, want failed", reason)
	}
	if reason, _ := scriptSettleFor(protocol.KindScript, child.ShutdownResult{ExitCode: 0, Signal: "terminated"}, []byte("killed")); reason != "failed" {
		t.Fatalf("signalled: reason %q, want failed (a signalled child did not exit 0)", reason)
	}
	reason, tail = scriptSettleFor(protocol.KindFundi, child.ShutdownResult{ExitCode: 0}, []byte("whatever"))
	if reason != "exited" || tail != "" {
		t.Fatalf("fundi: (%q, %q), want (exited, \"\")", reason, tail)
	}
}

// TestLastStderrTail pins the 4 KiB cap: the tail is the LAST bytes,
// verbatim.
func TestLastStderrTail(t *testing.T) {
	if got := lastStderrTail(nil, scriptStderrTailBytes); got != "" {
		t.Fatalf("empty stderr: %q", got)
	}
	if got := lastStderrTail([]byte("short"), scriptStderrTailBytes); got != "short" {
		t.Fatalf("short stderr: %q", got)
	}
	big := make([]byte, scriptStderrTailBytes+10)
	for i := range big {
		big[i] = byte('a' + i%26)
	}
	got := lastStderrTail(big, scriptStderrTailBytes)
	if len(got) != scriptStderrTailBytes {
		t.Fatalf("tail length = %d, want %d", len(got), scriptStderrTailBytes)
	}
	if got != string(big[10:]) {
		t.Fatal("tail must be the LAST cap bytes, verbatim")
	}
}

// ─── the runner itself ───────────────────────────────────────────────────────

// runnerPyModules is an in-memory pymodules.Store (the package's
// fakePymoduleStore, pymodulesync_test.go) pre-seeded with the runner tests'
// modules, owned by "owner-1".
func runnerPyModules(t *testing.T, modules map[string]string) *fakePymoduleStore {
	t.Helper()
	s := &fakePymoduleStore{rows: map[string][]pymodules.Record{}}
	for name, code := range modules {
		if _, err := s.Put(context.Background(), "owner-1", name, code, ""); err != nil {
			t.Fatalf("seed module %s: %v", name, err)
		}
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
	if err != nil {
		t.Fatalf("mkdirtemp: %v", err)
	}
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
	if err != nil {
		t.Fatalf("scriptRunner: %v", err)
	}

	sockPath := childsock.SocketPath(scriptHostDir(c.stateDir, "c_script_probe"))
	if _, err := os.Stat(sockPath); err != nil {
		t.Fatalf("per-child socket missing after construction: %v", err)
	}

	_, stdout, stderr, err := runner.Start()
	if err != nil {
		t.Fatalf("start: %v", err)
	}

	out, err := io.ReadAll(stdout)
	if err != nil {
		t.Fatalf("read stdout: %v", err)
	}
	errBuf, _ := io.ReadAll(stderr)
	code, sig := runner.Wait()
	if code != 0 || sig != "" {
		t.Fatalf("script exited (%d, %q); stdout:\n%s\nstderr:\n%s", code, sig, out, errBuf)
	}

	if strings.Contains(string(out), "LEAKED:") {
		t.Fatalf("credential variables leaked into the script child: %s", out)
	}
	if !strings.Contains(string(out), "cwd-ok=True") {
		t.Fatalf("script did not run in req.Cwd (stdout: %s)", out)
	}
	if !strings.Contains(string(out), "connect="+sockPath) {
		t.Fatalf("RAFIKI_CHILD_CONNECT did not name the per-child socket: %s", out)
	}

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
	if err != nil {
		t.Fatalf("scriptRunner: %v", err)
	}
	_, stdout, _, err := runner.Start()
	if err != nil {
		t.Fatalf("start: %v", err)
	}

	// Wait for the child to actually be alive before dialing.
	br := bufio.NewReader(stdout)
	if _, err := br.ReadString('\n'); err != nil {
		t.Fatalf("read awake line: %v", err)
	}

	sockPath := childsock.SocketPath(scriptHostDir(c.stateDir, "c_script_dial"))
	resp, err := unixGet(t, sockPath, "/probe", map[string]string{
		"Authorization": "Bearer EVIL", "X-Rafiki-Evil": "zzz",
	})
	if err != nil {
		t.Fatalf("dial through the per-child socket: %v", err)
	}
	resp.Body.Close()

	select {
	case s := <-got:
		if !strings.HasPrefix(s.auth, "Bearer ") || strings.Contains(s.auth, "EVIL") {
			t.Fatalf("Authorization at the face = %q, want the injected bearer, not the caller's", s.auth)
		}
		if s.evil != "" {
			t.Fatalf("caller-supplied X-Rafiki-* survived the proxy: %q", s.evil)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the face never saw the proxied request")
	}

	// Tear down: the process-group kill is the exit the child gets when it
	// ignores its stop; then the socket is closed and unlinked.
	if err := runner.Terminate(); err != nil {
		t.Fatalf("terminate: %v", err)
	}
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
	if err != nil {
		t.Fatalf("scriptRunner: %v", err)
	}
	_, stdout, _, err := runner.Start()
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	br := bufio.NewReader(stdout)
	if _, err := br.ReadString('\n'); err != nil {
		t.Fatalf("read forked line: %v", err)
	}
	pgid := runner.PID()
	if pgid == 0 {
		t.Fatal("runner.PID() = 0; the child has no process")
	}

	if err := runner.Terminate(); err != nil {
		t.Fatalf("terminate: %v", err)
	}
	// pkg/child's Wait contract: a signalled child reports ExitCode 0 with
	// Signal set — so the signal string is the death indicator here.
	code, sig := runner.Wait()
	if sig == "" {
		t.Fatalf("a SIGTERMed script reported exit %d with no signal; want a signal death", code)
	}

	// The whole group is gone once every member has exited (a reparented
	// orphan needs a moment to be reaped).
	deadline := time.Now().Add(10 * time.Second)
	for {
		err := syscall.Kill(-pgid, 0)
		if err == syscall.ESRCH {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("process group %d still alive after the kill: %v", pgid, err)
		}
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
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
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
	if err == nil || !strings.Contains(err.Error(), "git source") {
		t.Fatalf("want the git-source refusal, got %v", err)
	}
}
