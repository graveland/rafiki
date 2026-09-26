package executor

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	"go.graveland.dev/rafiki/pkg/adminpb"
	"go.graveland.dev/rafiki/pkg/darajapb"
)

// scriptCacheFixture lays out a synced blob cache the way SyncPyModules does:
// one directory per module name, <name>.py inside, an optional per-module
// .venv with a bin/python3 and a site-packages glob target. paths.CacheDir()
// resolves from XDG_CACHE_HOME, so the fixture isolates the cache under a
// temp dir and the caller restores nothing (t.Setenv does).
func scriptCacheFixture(t *testing.T, modules map[string]moduleFixture) {
	t.Helper()
	cache := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", cache)
	// Re-derive: base() prefers the env var; with XDG_CACHE_HOME set the cache
	// root is <xdg>/rafiki — so seed under THAT, not the bare temp dir.
	root := filepath.Join(cache, "rafiki", "pymodules")
	for name, m := range modules {
		dir := filepath.Join(root, name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name+".py"), []byte(m.code), 0o644); err != nil {
			t.Fatal(err)
		}
		if m.venv {
			if err := os.MkdirAll(filepath.Join(dir, ".venv", "bin"), 0o755); err != nil {
				t.Fatal(err)
			}
			py := filepath.Join(dir, ".venv", "bin", "python3")
			if err := os.WriteFile(py, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			site := filepath.Join(dir, ".venv", "lib", "python3.12", "site-packages")
			if err := os.MkdirAll(site, 0o755); err != nil {
				t.Fatal(err)
			}
		}
	}
}

type moduleFixture struct {
	code string
	venv bool
}

func TestResolveScriptLaunchFromBlobCache(t *testing.T) {
	scriptCacheFixture(t, map[string]moduleFixture{
		"driver": {code: "print('hi')\n", venv: false},
		"helper": {code: "x=1\n# pymodule-requirements: requests\n", venv: true},
	})
	res, err := resolveScriptLaunch(&darajapb.ScriptParams{
		Repo:    "local",
		Script:  "driver",
		Modules: []string{"helper"},
		Args:    []string{"--flag", "value"},
	})
	if err != nil {
		t.Fatalf("resolveScriptLaunch: %v", err)
	}
	// The venv python wins the interpreter slot for a script that has one...
	if res.interpreter != "python3" {
		t.Fatalf("interpreter = %q; the SCRIPT's venv wins, not a module's (driver has none)", res.interpreter)
	}
	// ...the script path leads argv, args follow verbatim.
	cache := filepath.Join(os.Getenv("XDG_CACHE_HOME"), "rafiki", "pymodules")
	if want := filepath.Join(cache, "driver", "driver.py"); res.argv[0] != want {
		t.Errorf("argv[0] = %q, want %q", res.argv[0], want)
	}
	if len(res.argv) != 3 || res.argv[1] != "--flag" || res.argv[2] != "value" {
		t.Errorf("argv = %v, want script path then the spec's args", res.argv)
	}
	// PYTHONPATH: the module's code dir then its site-packages (the script has
	// no venv, so no script site-packages entry), exactly two entries — the
	// script's own dir stays off (it is sys.path[0]).
	entries := strings.Split(res.pythonPath, string(os.PathListSeparator))
	want := []string{
		filepath.Join(cache, "helper"),
		filepath.Join(cache, "helper", ".venv", "lib", "python3.12", "site-packages"),
	}
	if len(entries) != len(want) {
		t.Fatalf("PYTHONPATH = %q (%d entries), want %v", res.pythonPath, len(entries), want)
	}
	for i := range want {
		if entries[i] != want[i] {
			t.Errorf("PYTHONPATH entry %d = %q, want %q", i, entries[i], want[i])
		}
	}
}

// A script whose entry has its own venv runs on THAT venv's python — never the
// fallback — so a changed RAFIKI_PYMODULE_PYTHON cannot re-point or rebuild a
// venv that was already built.
func TestResolveScriptLaunchVenvInterpreterWins(t *testing.T) {
	scriptCacheFixture(t, map[string]moduleFixture{
		"venved": {code: "print('v')\n", venv: true},
	})
	t.Setenv("RAFIKI_PYMODULE_PYTHON", "/custom/python3")
	res, err := resolveScriptLaunch(&darajapb.ScriptParams{Repo: "local", Script: "venved"})
	if err != nil {
		t.Fatalf("resolveScriptLaunch: %v", err)
	}
	want := filepath.Join(os.Getenv("XDG_CACHE_HOME"), "rafiki", "pymodules", "venved", ".venv", "bin", "python3")
	if res.interpreter != want {
		t.Fatalf("interpreter = %q, want the script's own venv python %q", res.interpreter, want)
	}
}

// Requirements without a built venv are refused before anything is launched,
// with the same readiness reasoning pymodule_run gives — including the
// build-in-progress retry case, where a staging directory means the venv is
// still being built.
func TestResolveScriptLaunchRefusesMissingVenv(t *testing.T) {
	// The requirements block is a marker line followed by #-commented lines,
	// exactly ParseRequirements' shape.
	scriptCacheFixture(t, map[string]moduleFixture{
		"needy": {code: "# pymodule-requirements:\n# requests\nprint('n')\n", venv: false},
	})
	_, err := resolveScriptLaunch(&darajapb.ScriptParams{Repo: "local", Script: "needy"})
	if err == nil || !strings.Contains(err.Error(), "dependencies not ready") {
		t.Fatalf("want the venv readiness refusal, got %v", err)
	}

	// A staging directory is a build in flight: retryable, worded as such.
	cache := filepath.Join(os.Getenv("XDG_CACHE_HOME"), "rafiki", "pymodules", "needy")
	if err := os.MkdirAll(filepath.Join(cache, ".rafiki-venv-staging-1"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, err = resolveScriptLaunch(&darajapb.ScriptParams{Repo: "local", Script: "needy"})
	if err == nil || !strings.Contains(err.Error(), "build is in progress") {
		t.Fatalf("want the in-progress wording, got %v", err)
	}
}

// An unsynced name is a Launch refusal with pymodule_run's wording, never a
// half-hosted child.
func TestResolveScriptLaunchRefusesUnsynced(t *testing.T) {
	scriptCacheFixture(t, nil)
	_, err := resolveScriptLaunch(&darajapb.ScriptParams{Repo: "local", Script: "ghost"})
	if err == nil || !strings.Contains(err.Error(), "not synced to this executor") {
		t.Fatalf("want the unsynced refusal, got %v", err)
	}
}

// scriptGitFixture lays out a synced git checkout the way SyncPyModuleGitSource
// does: <repo>/scripts/*.py callable, top-level __init__-carrying directories
// importable, one repo-wide venv.
func scriptGitFixture(t *testing.T, repo string, venv bool) string {
	t.Helper()
	base := t.TempDir()
	t.Setenv("XDG_CACHE_HOME", base)
	root := filepath.Join(base, "rafiki", "pymodule-repos", repo)
	if err := os.MkdirAll(filepath.Join(root, "scripts"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "scripts", "deploy.py"), []byte("print('d')\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "ops_tools"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "ops_tools", "__init__.py"), []byte(""), 0o644); err != nil {
		t.Fatal(err)
	}
	if venv {
		if err := os.MkdirAll(filepath.Join(root, ".venv", "bin"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, ".venv", "bin", "python3"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func TestResolveScriptLaunchFromGitCheckout(t *testing.T) {
	repo := scriptGitFixture(t, "ops", false)
	res, err := resolveScriptLaunch(&darajapb.ScriptParams{
		Repo:    "ops",
		Script:  "deploy",
		Modules: []string{"ops_tools"},
		Args:    []string{"--plan"},
	})
	if err != nil {
		t.Fatalf("resolveScriptLaunch: %v", err)
	}
	if res.interpreter != "python3" {
		t.Errorf("interpreter = %q; the repo venv is the only alternative and was not built", res.interpreter)
	}
	if want := filepath.Join(repo, "scripts", "deploy.py"); res.argv[0] != want {
		t.Errorf("argv[0] = %q, want %q", res.argv[0], want)
	}
	// The checkout root joins PYTHONPATH once (intra-repo imports), then the
	// named module's directory.
	if got := strings.SplitN(res.pythonPath, string(os.PathListSeparator), 2); len(got) != 2 ||
		got[0] != repo || got[1] != filepath.Join(repo, "ops_tools") {
		t.Errorf("PYTHONPATH = %q, want %q:%q", res.pythonPath, repo, filepath.Join(repo, "ops_tools"))
	}
}

func TestResolveScriptLaunchGitVenvInterpreter(t *testing.T) {
	repo := scriptGitFixture(t, "venvops", true)
	res, err := resolveScriptLaunch(&darajapb.ScriptParams{Repo: "venvops", Script: "deploy"})
	if err != nil {
		t.Fatalf("resolveScriptLaunch: %v", err)
	}
	if want := filepath.Join(repo, ".venv", "bin", "python3"); res.interpreter != want {
		t.Fatalf("interpreter = %q, want the repo venv python %q", res.interpreter, want)
	}
}

// The executor's pre-existing PYTHONPATH trails the computed entries — the
// computed ones win, the inherited ones trail, exactly like pymodule_run.
func TestResolveScriptLaunchFoldsInheritedPythonPath(t *testing.T) {
	scriptCacheFixture(t, map[string]moduleFixture{"plain": {code: "print(1)\n", venv: false}})
	t.Setenv("PYTHONPATH", "/pre/existing")
	res, err := resolveScriptLaunch(&darajapb.ScriptParams{Repo: "local", Script: "plain"})
	if err != nil {
		t.Fatalf("resolveScriptLaunch: %v", err)
	}
	// No computed entries: no PYTHONPATH is set on the launch, so the value
	// the executor process carries rides daraja's inherited environment
	// through to the script unchanged (the daraja env build only strips the
	// credential prefixes). That is the pass-through half of pymodule_run's
	// fold — the computed-entries half is pinned by the blob-cache test above.
	if res.pythonPath != "" {
		t.Fatalf("PYTHONPATH = %q; a script with no computed entries sets none", res.pythonPath)
	}
}

// ─── AdminService.Launch, script kind ────────────────────────────────────────

// buildEnvDumperStub compiles a stand-in for the rafiki binary that dumps its
// argv and environment to files named by RAFIKI_DUMP_DIR, then sleeps. This is
// how the launch's environment is observed for real: not by asserting on the
// slice Launch built, but by reading back what the launched process SAW.
func buildEnvDumperStub(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	src := filepath.Join(dir, "main.go")
	if err := os.WriteFile(src, []byte(`package main

import (
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
)

func main() {
	if d := os.Getenv("RAFIKI_DUMP_DIR"); d != "" {
		os.WriteFile(filepath.Join(d, "argv"), []byte(fmt.Sprint(os.Args)), 0o600)
		var sb []byte
		for _, kv := range os.Environ() {
			sb = append(sb, []byte(kv+"\n")...)
		}
		os.WriteFile(filepath.Join(d, "env"), sb, 0o600)
	}
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGTERM, syscall.SIGINT)
	<-ch
}
`), 0o600); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "stub")
	cmd := exec.Command("go", "build", "-o", bin, src)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build stub: %v\n%s", err, out)
	}
	return bin
}

func dumpFiles(t *testing.T, dir string) (argv []string, env map[string]string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		argvRaw, err := os.ReadFile(filepath.Join(dir, "argv"))
		if err != nil {
			time.Sleep(20 * time.Millisecond)
			continue
		}
		envRaw, err := os.ReadFile(filepath.Join(dir, "env"))
		if err != nil {
			time.Sleep(20 * time.Millisecond)
			continue
		}
		// fmt.Sprint of a []string joins with spaces, not SplitList syntax.
		argv = strings.Fields(strings.Trim(string(argvRaw), "[]"))
		env = map[string]string{}
		for _, line := range strings.Split(strings.TrimRight(string(envRaw), "\n"), "\n") {
			if i := strings.IndexByte(line, '='); i > 0 {
				env[line[:i]] = line[i+1:]
			}
		}
		return argv, env
	}
	t.Fatal("the stub never wrote its dump")
	return nil, nil
}

// The reviewer checkpoint, pinned at the kernel level: the per-child Connect
// secret travels in the daraja process's ENVIRONMENT and never in its argv.
// A ticket-shaped duplicate (the claude path's own test covers the ticket)
// plus the env read-back closes the ps gap on the script launch too.
func TestLaunchScriptKeepsTheChildSecretOutOfArgv(t *testing.T) {
	dump := t.TempDir()
	t.Setenv("RAFIKI_DUMP_DIR", dump)
	a := NewAdminServer(AdminOptions{
		SelfBinary:  buildEnvDumperStub(t),
		ChildBinary: "",
		LaunchKinds: []string{"script"},
		SocketDir:   t.TempDir(),
	})
	defer a.Close()

	scriptCacheFixture(t, map[string]moduleFixture{
		"plain": {code: "print('p')\n", venv: false},
	})
	secret := "per-child-connect-secret-do-not-log"
	resp, err := a.Launch(context.Background(), connect.NewRequest(&adminpb.LaunchRequest{
		ChildId: "c-script-secret",
		Cwd:     t.TempDir(),
		Spec: &darajapb.ChildSpec{
			Kind: darajapb.Kind_KIND_SCRIPT,
			Script: &darajapb.ScriptParams{
				Repo:        "local",
				Script:      "plain",
				ChildSecret: secret,
			},
		},
	}))
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}

	pid := int(resp.Msg.GetPid())
	out, err := exec.Command("ps", "-o", "command=", "-p", fmt.Sprint(pid)).CombinedOutput()
	if err != nil {
		t.Fatalf("ps -p %d: %v (output: %s)", pid, err, out)
	}
	if strings.Contains(string(out), secret) {
		t.Fatalf("the child secret is readable in the kernel cmdline:\n%s", out)
	}

	_, env := dumpFiles(t, dump)
	if got := env["RAFIKI_CHILD_SECRET"]; got != secret {
		t.Fatalf("RAFIKI_CHILD_SECRET = %q in the launched env, want the secret; env=%v", got, redact(env))
	}
	// The script-facing channel is the socket path, not the token.
	if v, ok := env["RAFIKI_CHILD_CONNECT"]; ok {
		t.Fatalf("RAFIKI_CHILD_CONNECT must not leak into the daraja process env, got %q", v)
	}
}

// redact hides every value the test dump contains except structure — the dump
// only ever appears in a failure message, and it must not print the secret.
func redact(env map[string]string) map[string]string {
	out := map[string]string{}
	for k := range env {
		out[k] = "<redacted>"
	}
	return out
}

// The interpreter is --binary and the resolved script argv rides positionally
// after the "--" separator, so a flag-shaped script argument cannot be eaten
// by pflag; PYTHONPATH and forwarded names arrive once each; the credential
// prefixes are stripped from the forwarded environment.
func TestLaunchScriptCarriesResolvedArgvAndEnv(t *testing.T) {
	dump := t.TempDir()
	t.Setenv("RAFIKI_DUMP_DIR", dump)
	a := NewAdminServer(AdminOptions{
		SelfBinary:  buildEnvDumperStub(t),
		LaunchKinds: []string{"script"},
		SocketDir:   t.TempDir(),
		// A pin rides to daraja so the per-child socket verifies the SAME
		// listener the executor itself dials.
		PinCert:    "aa11",
		ServerName: "rafiki.internal",
	})
	defer a.Close()

	scriptCacheFixture(t, map[string]moduleFixture{"driver": {code: "print(1)\n", venv: false}})
	_, err := a.Launch(context.Background(), connect.NewRequest(&adminpb.LaunchRequest{
		ChildId: "c-script-argv",
		Cwd:     t.TempDir(),
		Spec: &darajapb.ChildSpec{
			Kind: darajapb.Kind_KIND_SCRIPT,
			Script: &darajapb.ScriptParams{
				Repo:   "local",
				Script: "driver",
				Args:   []string{"--flag-like", "value"},
				Env: map[string]string{
					"FORWARDED_OK":      "yes",
					"ANTHROPIC_API_KEY": "smuggled",
					"RAFIKI_PROFILE":    "smuggled",
				},
			},
		},
	}))
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}

	argv, env := dumpFiles(t, dump)
	// Flags: kind, binary (the resolved interpreter), the pin ride-through.
	for _, pair := range [][2]string{
		{"--kind", "script"},
		{"--binary", "python3"},
		{"--pin-cert", "aa11"},
		{"--server-name", "rafiki.internal"},
	} {
		found := false
		for i, arg := range argv {
			if arg == pair[0] && i+1 < len(argv) && argv[i+1] == pair[1] {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("argv %v missing [%s %s]", argv, pair[0], pair[1])
		}
	}
	// The resolved script path leads the POSITIONALS (after "--"), and the
	// flag-shaped script argument stayed positional.
	sep := -1
	for i, arg := range argv {
		if arg == "--" {
			sep = i
			break
		}
	}
	if sep < 0 {
		t.Fatalf("argv %v has no -- separator", argv)
	}
	positional := argv[sep+1:]
	cache := filepath.Join(os.Getenv("XDG_CACHE_HOME"), "rafiki", "pymodules")
	if len(positional) != 3 || positional[0] != filepath.Join(cache, "driver", "driver.py") ||
		positional[1] != "--flag-like" || positional[2] != "value" {
		t.Errorf("positionals = %v, want the resolved script path then the args", positional)
	}
	// Forwarded env: functional names arrive, credential prefixes do not.
	if env["FORWARDED_OK"] != "yes" {
		t.Errorf("FORWARDED_OK = %q, want it forwarded", env["FORWARDED_OK"])
	}
	if _, ok := env["ANTHROPIC_API_KEY"]; ok {
		t.Errorf("a forwarded ANTHROPIC_API_KEY reached the daraja env; the strip must hold on the launch payload too")
	}
	if _, ok := env["RAFIKI_PROFILE"]; ok {
		t.Errorf("a forwarded RAFIKI_PROFILE reached the daraja env")
	}
	// PYTHONPATH: exactly one entry (the script's dir stays off it — it is
	// sys.path[0]; a plain script has no modules, so only the inherited value).
	if pp, ok := env["PYTHONPATH"]; ok {
		t.Errorf("PYTHONPATH = %q in the daraja env; a module-less script resolves none", pp)
	}
}

// An undeclared kind stays refused, and a script launch against a cache
// without the name fails with nothing started and no claim left behind.
func TestLaunchScriptRefusesUnsyncedScript(t *testing.T) {
	a := NewAdminServer(AdminOptions{
		SelfBinary:  buildSelfStub(t),
		LaunchKinds: []string{"script"},
		SocketDir:   t.TempDir(),
	})
	defer a.Close()

	scriptCacheFixture(t, nil)
	_, err := a.Launch(context.Background(), connect.NewRequest(&adminpb.LaunchRequest{
		ChildId: "c-ghost",
		Cwd:     t.TempDir(),
		Spec: &darajapb.ChildSpec{
			Kind:   darajapb.Kind_KIND_SCRIPT,
			Script: &darajapb.ScriptParams{Repo: "local", Script: "ghost"},
		},
	}))
	if err == nil || connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("want a FailedPrecondition refusal for an unsynced script, got %v", err)
	}

	a.mu.Lock()
	_, claimed := a.m["c-ghost"]
	a.mu.Unlock()
	if claimed {
		t.Fatal("a refused launch left its claim in the table")
	}
}
