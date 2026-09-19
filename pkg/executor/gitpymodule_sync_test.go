// SPDX-License-Identifier: Apache-2.0

package executor

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"connectrpc.com/connect"

	executorpb "go.graveland.dev/rafiki/pkg/executorpb"
)

// gitSyncServer builds a Server whose git-source sync targets a temp cache
// dir, by pointing XDG_CACHE_HOME at it. Same mechanism as pymoduleServer:
// paths.CacheDir resolves the same variable on this machine. PyModulesSync is
// left ON even in the refusing test, so a green refusal proves the two
// option gates are independent rather than one shared switch.
func gitSyncServer(t *testing.T, optIn bool) *Server {
	t.Helper()
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	return &Server{opts: Options{PyModulesSync: true, PymoduleGitSync: optIn}}
}

// gitRun runs one git command in dir, failing the test with the command's
// combined output when it exits non-zero.
func gitRun(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s (in %s): %v: %s", strings.Join(args, " "), dir, err, out)
	}
	return string(out)
}

// gitCommit commits dir's working tree with a fixed identity.
func gitCommit(t *testing.T, dir, msg string) {
	t.Helper()
	gitRun(t, dir, "-c", "user.name=rafiki-test", "-c", "user.email=rafiki-test@example.invalid", "commit", "-m", msg)
}

// gitFixture creates a local git repo under a fresh temp dir -- no network,
// no remote, exactly the posture the global constraints demand -- writes the
// given files, commits them, and returns the repo path for use as a clone
// URL. Global and system git config are nulled so a developer's own
// commit.gpgsign or insteadOf rewrite cannot leak into either the fixture or
// the git subprocesses the handler itself runs (they inherit this process's
// environment), and the branch is pinned to `main` so a test can name Ref
// without guessing git's init.defaultBranch.
func gitFixture(t *testing.T, files map[string]string) string {
	t.Helper()
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_SYSTEM", "/dev/null")
	repo := t.TempDir()
	gitRun(t, repo, "init", "-b", "main")
	for name, content := range files {
		path := filepath.Join(repo, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	gitRun(t, repo, "add", "-A")
	gitCommit(t, repo, "fixture")
	return repo
}

// syncGitSource runs one SyncPyModuleGitSource against s and returns the
// response's message, failing the test on an RPC-level error.
func syncGitSource(t *testing.T, s *Server, name, url, ref string) *executorpb.SyncPyModuleGitSourceResponse {
	t.Helper()
	resp, err := s.SyncPyModuleGitSource(context.Background(),
		connect.NewRequest(&executorpb.SyncPyModuleGitSourceRequest{Name: name, Url: url, Ref: ref}))
	if err != nil {
		t.Fatalf("SyncPyModuleGitSource: %v", err)
	}
	return resp.Msg
}

// assertScripts asserts the response's scripts exactly match want (name ->
// description), order-insensitively.
func assertScripts(t *testing.T, got []*executorpb.GitSourceScript, want map[string]string) {
	t.Helper()
	names := make([]string, 0, len(got))
	for _, sc := range got {
		names = append(names, sc.GetName())
		if sc.GetDescription() != want[sc.GetName()] {
			t.Errorf("script %q description = %q, want %q", sc.GetName(), sc.GetDescription(), want[sc.GetName()])
		}
	}
	wantNames := make([]string, 0, len(want))
	for n := range want {
		wantNames = append(wantNames, n)
	}
	slices.Sort(names)
	slices.Sort(wantNames)
	if !slices.Equal(names, wantNames) {
		t.Errorf("scripts = %v, want %v", names, wantNames)
	}
}

// assertPackages is assertScripts for the response's packages.
func assertPackages(t *testing.T, got []*executorpb.GitSourcePackage, want map[string]string) {
	t.Helper()
	names := make([]string, 0, len(got))
	for _, p := range got {
		names = append(names, p.GetName())
		if p.GetDescription() != want[p.GetName()] {
			t.Errorf("package %q description = %q, want %q", p.GetName(), p.GetDescription(), want[p.GetName()])
		}
	}
	wantNames := make([]string, 0, len(want))
	for n := range want {
		wantNames = append(wantNames, n)
	}
	slices.Sort(names)
	slices.Sort(wantNames)
	if !slices.Equal(names, wantNames) {
		t.Errorf("packages = %v, want %v", names, wantNames)
	}
}

// assertNoStaging asserts the checkout holds no leftover staging temp dir --
// a failed or finished build must never leave one behind for git clean to
// trip over.
func assertNoStaging(t *testing.T, checkout string) {
	t.Helper()
	entries, err := os.ReadDir(checkout)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".rafiki-venv-staging") {
			t.Errorf("staging dir %s survived the sync", filepath.Join(checkout, e.Name()))
		}
	}
}

func TestSyncPyModuleGitSourceRefusesWhenNotOptedIn(t *testing.T) {
	s := gitSyncServer(t, false)
	_, err := s.SyncPyModuleGitSource(context.Background(),
		connect.NewRequest(&executorpb.SyncPyModuleGitSourceRequest{Name: "ops-tools", Url: "/tmp/nowhere", Ref: "main"}))
	if err == nil {
		t.Fatal("SyncPyModuleGitSource ran on an executor that never opted in")
	}
	if connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Errorf("got %v, want PermissionDenied", connect.CodeOf(err))
	}
	if _, derr := os.Stat(filepath.Dir(gitPymoduleRepoDir("ops-tools"))); !os.IsNotExist(derr) {
		t.Errorf("a refused sync created its cache root (err=%v)", derr)
	}
}

// The name is a path segment under the cache root. Anything that could escape
// it must be refused before a single byte is written.
func TestSyncPyModuleGitSourceRejectsInvalidName(t *testing.T) {
	s := gitSyncServer(t, true)
	_, err := s.SyncPyModuleGitSource(context.Background(),
		connect.NewRequest(&executorpb.SyncPyModuleGitSourceRequest{Name: "../evil", Url: "/tmp/nowhere", Ref: "main"}))
	if err == nil {
		t.Fatal("accepted a name that can escape the pymodule-repo directory")
	}
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("got code %v, want InvalidArgument", connect.CodeOf(err))
	}
	if _, derr := os.Stat(filepath.Dir(gitPymoduleRepoDir("ops-tools"))); !os.IsNotExist(derr) {
		t.Errorf("a rejected sync wrote to disk (err=%v)", derr)
	}
}

func TestSyncPyModuleGitSourceClonesAndDiscovers(t *testing.T) {
	s := gitSyncServer(t, true)
	repo := gitFixture(t, map[string]string{
		"scripts/rotate.py":     "# rotate the keys\nprint('rotate')\n",
		"ops_tools/__init__.py": "# operator tooling\n",
		"README.md":             "# repo readme, not an entry\n",
	})

	resp := syncGitSource(t, s, "ops-tools", repo, "main")

	assertScripts(t, resp.GetScripts(), map[string]string{"rotate": "rotate the keys"})
	assertPackages(t, resp.GetPackages(), map[string]string{"ops_tools": "operator tooling"})

	// No pyproject.toml in the fixture: nothing to build, and the response's
	// venv_ready contract promises true for that, not a failed build.
	if !resp.GetVenvReady() || resp.GetVenvError() != "" {
		t.Errorf("venv: got {ready: %v, error: %q}, want {ready: true, error: \"\"}",
			resp.GetVenvReady(), resp.GetVenvError())
	}

	checkout := gitPymoduleRepoDir("ops-tools")
	if _, err := os.Stat(filepath.Join(checkout, ".git")); err != nil {
		t.Errorf("the checkout does not exist at %s: %v", checkout, err)
	}
}

// A refresh must land the repo's new tip and sweep everything untracked: a
// leftover file would otherwise be invisible cruft -- or, in scripts/, a
// callable nobody committed.
func TestSyncPyModuleGitSourceRefreshResetsAndCleans(t *testing.T) {
	s := gitSyncServer(t, true)
	repo := gitFixture(t, map[string]string{
		"scripts/a.py": "# first script\n",
	})
	first := syncGitSource(t, s, "ops-tools", repo, "main")
	assertScripts(t, first.GetScripts(), map[string]string{"a": "first script"})

	// A NEW commit in the fixture repo: the refresh must bring it in.
	if err := os.WriteFile(filepath.Join(repo, "scripts", "b.py"), []byte("# second script\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, repo, "add", "-A")
	gitCommit(t, repo, "add b.py")

	checkout := gitPymoduleRepoDir("ops-tools")
	// Leftover cruft, in two shapes: an untracked file at the checkout root
	// and an untracked .py in scripts/ (the dangerous kind -- it would become
	// callable if git clean did not run).
	stray := filepath.Join(checkout, "stray.txt")
	if err := os.WriteFile(stray, []byte("cruft"), 0o644); err != nil {
		t.Fatal(err)
	}
	strayScript := filepath.Join(checkout, "scripts", "stray.py")
	if err := os.WriteFile(strayScript, []byte("# never committed\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	second := syncGitSource(t, s, "ops-tools", repo, "main")

	assertScripts(t, second.GetScripts(), map[string]string{
		"a": "first script",
		"b": "second script", // the new commit arrived
	})
	for _, p := range []string{stray, strayScript} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("untracked %s survived the refresh (err=%v)", p, err)
		}
	}
}

// A repo whose only dependency manifest is a bare requirements.txt cannot be
// served by `uv sync`: reporting venv_ready=true would promise a venv that
// was never built, its dependencies missing at run time. The failure reports
// exactly like a failed build -- VenvReady=false, one clear sentence naming
// the gap -- while the discovered inventory still reports (design §5).
func TestSyncPyModuleGitSourceRequirementsOnlyRepoReportsNotReady(t *testing.T) {
	s := gitSyncServer(t, true)
	repo := gitFixture(t, map[string]string{
		"requirements.txt":  "requests\n",
		"scripts/rotate.py": "# rotate\n",
	})

	resp := syncGitSource(t, s, "ops-tools", repo, "main")

	if resp.GetVenvReady() {
		t.Fatal("a requirements.txt-only repo reported VenvReady")
	}
	if !strings.Contains(resp.GetVenvError(), "requirements.txt") {
		t.Errorf("venv error %q does not name requirements.txt", resp.GetVenvError())
	}
	assertScripts(t, resp.GetScripts(), map[string]string{"rotate": "rotate"})
	if _, err := os.Stat(filepath.Join(gitPymoduleRepoDir("ops-tools"), ".venv")); !os.IsNotExist(err) {
		t.Errorf("a skipped build published a .venv (err=%v)", err)
	}
}

// The fake uv for the git-source sync tests: implements exactly the one
// subcommand buildRepoVenv invokes, following writeFakeUV's technique from
// pymodule_venv_test.go so no test here touches a real uv or the network.
//
//   - uv sync --python <interp>  creates <UV_PROJECT_ENVIRONMENT>/bin/ with
//     an executable stub python3. The staged venv path MUST arrive via
//     UV_PROJECT_ENVIRONMENT -- the fake refuses to run without it, pinning
//     the contract that buildRepoVenv stages the build.
//   - Fails with "simulated sync failure" on stderr iff the checkout (the
//     fake's working directory) holds a tracked FAIL_THIS_SYNC file.
//
// Every invocation appends one line to $FAKE_UV_LOG, read via the shared
// fakeUVInvocations helper.
func writeFakeUVSync(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	script := `#!/bin/sh
# Fake uv for rafiki's git-source sync tests -- see writeFakeUVSync.
: "${FAKE_UV_LOG:=/dev/null}"
printf '%s\n' "$*" >> "$FAKE_UV_LOG"
case "$1" in
sync)
	path="${UV_PROJECT_ENVIRONMENT:?UV_PROJECT_ENVIRONMENT must be set}"
	if [ -f ./FAIL_THIS_SYNC ]; then
		echo "simulated sync failure" >&2
		exit 1
	fi
	mkdir -p "$path/bin" || exit 1
	printf '#!/bin/sh\nexit 0\n' > "$path/bin/python3" || exit 1
	chmod +x "$path/bin/python3"
	;;
*)
	echo "fake uv: unexpected invocation: $*" >&2
	exit 64
	;;
esac
exit 0
`
	path := filepath.Join(dir, "uv")
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestSyncPyModuleGitSourceBuildsVenvWithFakeUv(t *testing.T) {
	uvDir := writeFakeUVSync(t)
	t.Setenv("RAFIKI_PYMODULE_UV", filepath.Join(uvDir, "uv"))
	log := setFakeUVLog(t)
	s := gitSyncServer(t, true)
	repo := gitFixture(t, map[string]string{
		"pyproject.toml":    "[project]\nname = \"ops-tools\"\nversion = \"0.1.0\"\ndependencies = [\"requests\"]\n",
		"scripts/rotate.py": "# rotate\n",
	})

	resp := syncGitSource(t, s, "ops-tools", repo, "main")

	if !resp.GetVenvReady() || resp.GetVenvError() != "" {
		t.Fatalf("venv: got {ready: %v, error: %q}, want {ready: true, error: \"\"}",
			resp.GetVenvReady(), resp.GetVenvError())
	}
	checkout := gitPymoduleRepoDir("ops-tools")
	if _, err := os.Stat(filepath.Join(checkout, ".venv", "bin", "python3")); err != nil {
		t.Errorf("the repo's shared venv was not built into the checkout: %v", err)
	}
	if n := fakeUVInvocations(t, log); n != 1 {
		t.Errorf("uv ran %d times, want 1", n)
	}
	assertNoStaging(t, checkout)
}

// A broken dependency build must not hide the inventory (design §5): the
// discovery still reports, only the venv fails.
func TestSyncPyModuleGitSourceReportsDiscoveryEvenOnVenvFailure(t *testing.T) {
	uvDir := writeFakeUVSync(t)
	t.Setenv("RAFIKI_PYMODULE_UV", filepath.Join(uvDir, "uv"))
	s := gitSyncServer(t, true)
	repo := gitFixture(t, map[string]string{
		"pyproject.toml":        "[project]\nname = \"ops-tools\"\nversion = \"0.1.0\"\n",
		"FAIL_THIS_SYNC":        "\n",
		"scripts/rotate.py":     "# rotate\n",
		"ops_tools/__init__.py": "# operator tooling\n",
	})

	resp := syncGitSource(t, s, "ops-tools", repo, "main")

	if resp.GetVenvReady() {
		t.Fatal("a failed uv sync reported VenvReady")
	}
	if !strings.Contains(resp.GetVenvError(), "simulated sync failure") {
		t.Errorf("venv error %q does not name the failure", resp.GetVenvError())
	}
	assertScripts(t, resp.GetScripts(), map[string]string{"rotate": "rotate"})
	assertPackages(t, resp.GetPackages(), map[string]string{"ops_tools": "operator tooling"})

	checkout := gitPymoduleRepoDir("ops-tools")
	if _, err := os.Stat(filepath.Join(checkout, ".venv")); !os.IsNotExist(err) {
		t.Errorf("a failed build published a .venv (err=%v)", err)
	}
	assertNoStaging(t, checkout)
}

// A ref that does not exist must come back as a clean failure REPORT -- the
// RPC itself succeeds, VenvReady is false with git's own output, the
// inventory is empty, and nothing panics or hangs. The clone itself succeeds,
// so the checkout stays in place for a later refresh with a good ref.
func TestSyncPyModuleGitSourceFailsCleanlyOnBadRef(t *testing.T) {
	s := gitSyncServer(t, true)
	repo := gitFixture(t, map[string]string{
		"scripts/rotate.py": "# rotate\n",
	})

	resp := syncGitSource(t, s, "ops-tools", repo, "no-such-ref")

	if resp.GetVenvReady() {
		t.Fatal("a failed checkout reported VenvReady")
	}
	if resp.GetVenvError() == "" {
		t.Fatal("VenvError is empty for a failed checkout")
	}
	if !strings.Contains(resp.GetVenvError(), "no-such-ref") {
		t.Errorf("VenvError %q does not name the bad ref", resp.GetVenvError())
	}
	if len(resp.GetScripts()) != 0 || len(resp.GetPackages()) != 0 {
		t.Errorf("a failed checkout reported inventory: %v/%v", resp.GetScripts(), resp.GetPackages())
	}
	if _, err := os.Stat(filepath.Join(gitPymoduleRepoDir("ops-tools"), ".git")); err != nil {
		t.Errorf("the failed checkout destroyed the clone: %v", err)
	}
}

// Describe self-reports the option, mirroring how pymodules_sync is reported.
func TestDescribeReportsPymoduleGitSync(t *testing.T) {
	for _, tc := range []struct{ optIn, want bool }{{true, true}, {false, false}} {
		s := NewServer(Options{Root: t.TempDir(), Concurrency: 1, Version: "test", PymoduleGitSync: tc.optIn})
		resp, err := s.Describe(context.Background(), connect.NewRequest(&executorpb.DescribeRequest{}))
		if err != nil {
			t.Fatalf("Describe: %v", err)
		}
		if got := resp.Msg.GetPymoduleGitSync(); got != tc.want {
			t.Errorf("optIn=%v: Describe reported pymodule_git_sync=%v, want %v", tc.optIn, got, tc.want)
		}
	}
}
