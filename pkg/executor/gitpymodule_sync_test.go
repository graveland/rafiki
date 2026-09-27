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

	"github.com/multigres/testkit/assert"
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
	assert.NewAborting(t).NoError(err, "git %s (in %s): %v: %s", strings.Join(args, " "), dir, err, out)
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
	c := assert.NewAborting(t)
	repo := t.TempDir()
	gitRun(t, repo, "init", "-b", "main")
	for name, content := range files {
		path := filepath.Join(repo, name)
		c.NoError(os.MkdirAll(filepath.Dir(path), 0o755))
		c.NoError(os.WriteFile(path, []byte(content), 0o644))
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
	assert.NewAborting(t).NoError(err, "SyncPyModuleGitSource")
	return resp.Msg
}

// assertScripts asserts the response's scripts exactly match want (name ->
// description), order-insensitively.
func assertScripts(t *testing.T, got []*executorpb.GitSourceScript, want map[string]string) {
	t.Helper()
	c := assert.NewCollecting(t)
	names := make([]string, 0, len(got))
	for _, sc := range got {
		names = append(names, sc.GetName())
		c.Eq(want[sc.GetName()], sc.GetDescription(), "script %q description = %q, want", sc.GetName(), sc.GetDescription())
	}
	wantNames := make([]string, 0, len(want))
	for n := range want {
		wantNames = append(wantNames, n)
	}
	slices.Sort(names)
	slices.Sort(wantNames)
	c.EqDiff(wantNames, names, "scripts")
}

// assertPackages is assertScripts for the response's packages.
func assertPackages(t *testing.T, got []*executorpb.GitSourcePackage, want map[string]string) {
	t.Helper()
	c := assert.NewCollecting(t)
	names := make([]string, 0, len(got))
	for _, p := range got {
		names = append(names, p.GetName())
		c.Eq(want[p.GetName()], p.GetDescription(), "package %q description = %q, want", p.GetName(), p.GetDescription())
	}
	wantNames := make([]string, 0, len(want))
	for n := range want {
		wantNames = append(wantNames, n)
	}
	slices.Sort(names)
	slices.Sort(wantNames)
	c.EqDiff(wantNames, names, "packages")
}

// assertNoStaging asserts the checkout holds no leftover staging temp dir --
// a failed or finished build must never leave one behind for git clean to
// trip over.
func assertNoStaging(t *testing.T, checkout string) {
	t.Helper()
	entries, err := os.ReadDir(checkout)
	assert.NewAborting(t).NoError(err)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".rafiki-venv-staging") {
			t.Errorf("staging dir %s survived the sync", filepath.Join(checkout, e.Name()))
		}
	}
}

func TestSyncPyModuleGitSourceRefusesWhenNotOptedIn(t *testing.T) {
	c := assert.NewCollecting(t)
	s := gitSyncServer(t, false)
	_, err := s.SyncPyModuleGitSource(context.Background(),
		connect.NewRequest(&executorpb.SyncPyModuleGitSourceRequest{Name: "ops-tools", Url: "/tmp/nowhere", Ref: "main"}))
	c.Require().Error(err, "SyncPyModuleGitSource ran on an executor that never opted in")
	c.Eq(connect.CodePermissionDenied, connect.CodeOf(err), "got")
	_, derr := os.Stat(filepath.Dir(gitPymoduleRepoDir("ops-tools")))
	c.True(os.IsNotExist(derr), "a refused sync created its cache root (err=%v)", derr)
}

// The name is a path segment under the cache root. Anything that could escape
// it must be refused before a single byte is written.
func TestSyncPyModuleGitSourceRejectsInvalidName(t *testing.T) {
	c := assert.NewCollecting(t)
	s := gitSyncServer(t, true)
	_, err := s.SyncPyModuleGitSource(context.Background(),
		connect.NewRequest(&executorpb.SyncPyModuleGitSourceRequest{Name: "../evil", Url: "/tmp/nowhere", Ref: "main"}))
	c.Require().Error(err, "accepted a name that can escape the pymodule-repo directory")
	c.Eq(connect.CodeInvalidArgument, connect.CodeOf(err), "got code")
	_, derr := os.Stat(filepath.Dir(gitPymoduleRepoDir("ops-tools")))
	c.True(os.IsNotExist(derr), "a rejected sync wrote to disk (err=%v)", derr)
}

// A leading dash makes git parse the argument as an option rather than a
// value: `git fetch origin --upload-pack=<cmd>` executes <cmd> locally on
// this executor. The executor re-checks both wire values at its own boundary
// before any subprocess runs -- the same shape validSegment applies to the
// name. One test per field, so a pass line names exactly which gate held.
func TestSyncPyModuleGitSourceRejectsLeadingDashRef(t *testing.T) {
	c := assert.NewCollecting(t)
	s := gitSyncServer(t, true)
	_, err := s.SyncPyModuleGitSource(context.Background(),
		connect.NewRequest(&executorpb.SyncPyModuleGitSourceRequest{
			Name: "ops-tools", Url: "/tmp/nowhere", Ref: "--upload-pack=touch /tmp/pwned",
		}))
	c.Require().Error(err, "accepted a ref that begins with a dash")
	c.Eq(connect.CodeInvalidArgument, connect.CodeOf(err), "got code")
	c.StrContains(err.Error(), "ref", "error")
	_, derr := os.Stat(filepath.Dir(gitPymoduleRepoDir("ops-tools")))
	c.True(os.IsNotExist(derr), "a rejected sync wrote to disk (err=%v)", derr)
}

func TestSyncPyModuleGitSourceRejectsLeadingDashUrl(t *testing.T) {
	c := assert.NewCollecting(t)
	s := gitSyncServer(t, true)
	_, err := s.SyncPyModuleGitSource(context.Background(),
		connect.NewRequest(&executorpb.SyncPyModuleGitSourceRequest{
			Name: "ops-tools", Url: "--upload-pack=touch /tmp/pwned", Ref: "main",
		}))
	c.Require().Error(err, "accepted a url that begins with a dash")
	c.Eq(connect.CodeInvalidArgument, connect.CodeOf(err), "got code")
	c.StrContains(err.Error(), "url", "error")
	_, derr := os.Stat(filepath.Dir(gitPymoduleRepoDir("ops-tools")))
	c.True(os.IsNotExist(derr), "a rejected sync wrote to disk (err=%v)", derr)
}

// The pymodule-repos cache root is lazily created by the first sync, and a
// root this executor never created must be REFUSED, not adopted: a pre-existing
// non-rafiki directory would otherwise have clone, fetch, reset --hard and
// clean -fdx run inside it. Same mechanism as the blob path's
// assertManagedOrAbsent guard, one failure shape earlier in the handler.
func TestSyncPyModuleGitSourceRefusesUnmanagedRoot(t *testing.T) {
	c := assert.NewCollecting(t)
	s := gitSyncServer(t, true)
	repo := gitFixture(t, map[string]string{
		"scripts/rotate.py": "# rotate\n",
	})

	root := filepath.Dir(gitPymoduleRepoDir("ops-tools"))
	c.Require().NoError(os.MkdirAll(root, 0o755))
	stray := filepath.Join(root, "operator-notes")
	c.Require().NoError(os.WriteFile(stray, []byte("not rafiki's"), 0o644))

	_, err := s.SyncPyModuleGitSource(context.Background(),
		connect.NewRequest(&executorpb.SyncPyModuleGitSourceRequest{Name: "ops-tools", Url: repo, Ref: "main"}))
	c.Require().Error(err, "synced into an unmanaged pymodule-repos root")
	c.Eq(connect.CodeFailedPrecondition, connect.CodeOf(err), "got")
	if _, serr := os.Stat(gitPymoduleRepoDir("ops-tools")); !os.IsNotExist(serr) {
		t.Errorf("a refused sync wrote a checkout into the unmanaged root (err=%v)", serr)
	}
	if _, serr := os.Stat(filepath.Join(root, managedMarker)); !os.IsNotExist(serr) {
		t.Errorf("a refused sync marked the unmanaged root as its own (err=%v)", serr)
	}
	_, serr := os.Stat(stray)
	c.NoError(serr, "the refusal disturbed the root's own contents")
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
	// The lazily created cache root is marked, so a later sync recognises it
	// as rafiki's — the same managedMarker mechanism the blob path's
	// SyncPyModules applies to its own cache dir.
	_, err := os.Stat(filepath.Join(filepath.Dir(checkout), managedMarker))
	assert.NewCollecting(t).NoError(err, "the first sync did not drop the managed marker on the pymodule-repos root")
}

// A refresh must land the repo's new tip and sweep everything untracked: a
// leftover file would otherwise be invisible cruft -- or, in scripts/, a
// callable nobody committed.
func TestSyncPyModuleGitSourceRefreshResetsAndCleans(t *testing.T) {
	c := assert.NewCollecting(t)
	s := gitSyncServer(t, true)
	repo := gitFixture(t, map[string]string{
		"scripts/a.py": "# first script\n",
	})
	first := syncGitSource(t, s, "ops-tools", repo, "main")
	assertScripts(t, first.GetScripts(), map[string]string{"a": "first script"})

	// A NEW commit in the fixture repo: the refresh must bring it in.
	c.Require().NoError(os.WriteFile(filepath.Join(repo, "scripts", "b.py"), []byte("# second script\n"), 0o644))
	gitRun(t, repo, "add", "-A")
	gitCommit(t, repo, "add b.py")

	checkout := gitPymoduleRepoDir("ops-tools")
	// Leftover cruft, in two shapes: an untracked file at the checkout root
	// and an untracked .py in scripts/ (the dangerous kind -- it would become
	// callable if git clean did not run).
	stray := filepath.Join(checkout, "stray.txt")
	c.Require().NoError(os.WriteFile(stray, []byte("cruft"), 0o644))
	strayScript := filepath.Join(checkout, "scripts", "stray.py")
	c.Require().NoError(os.WriteFile(strayScript, []byte("# never committed\n"), 0o644))

	second := syncGitSource(t, s, "ops-tools", repo, "main")

	assertScripts(t, second.GetScripts(), map[string]string{
		"a": "first script",
		"b": "second script", // the new commit arrived
	})
	for _, p := range []string{stray, strayScript} {
		_, err := os.Stat(p)
		c.True(os.IsNotExist(err), "untracked %s survived the refresh (err=%v)", p, err)
	}
}

// A repo whose only dependency manifest is a bare requirements.txt cannot be
// served by `uv sync`: reporting venv_ready=true would promise a venv that
// was never built, its dependencies missing at run time. The failure reports
// exactly like a failed build -- VenvReady=false, one clear sentence naming
// the gap -- while the discovered inventory still reports (design §5).
func TestSyncPyModuleGitSourceRequirementsOnlyRepoReportsNotReady(t *testing.T) {
	c := assert.NewCollecting(t)
	s := gitSyncServer(t, true)
	repo := gitFixture(t, map[string]string{
		"requirements.txt":  "requests\n",
		"scripts/rotate.py": "# rotate\n",
	})

	resp := syncGitSource(t, s, "ops-tools", repo, "main")

	c.Require().False(resp.GetVenvReady(), "a requirements.txt-only repo reported VenvReady")
	c.StrContains(resp.GetVenvError(), "requirements.txt", "venv error")
	assertScripts(t, resp.GetScripts(), map[string]string{"rotate": "rotate"})
	_, err := os.Stat(filepath.Join(gitPymoduleRepoDir("ops-tools"), ".venv"))
	c.True(os.IsNotExist(err), "a skipped build published a .venv (err=%v)", err)
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
	assert.NewAborting(t).NoError(os.WriteFile(path, []byte(script), 0o755))
	return dir
}

func TestSyncPyModuleGitSourceBuildsVenvWithFakeUv(t *testing.T) {
	c := assert.NewCollecting(t)
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
	_, err := os.Stat(filepath.Join(checkout, ".venv", "bin", "python3"))
	c.NoError(err, "the repo's shared venv was not built into the checkout")
	c.Eq(1, fakeUVInvocations(t, log), "uv ran")
	assertNoStaging(t, checkout)
}

// A broken dependency build must not hide the inventory (design §5): the
// discovery still reports, only the venv fails.
func TestSyncPyModuleGitSourceReportsDiscoveryEvenOnVenvFailure(t *testing.T) {
	c := assert.NewCollecting(t)
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

	c.Require().False(resp.GetVenvReady(), "a failed uv sync reported VenvReady")
	c.StrContains(resp.GetVenvError(), "simulated sync failure", "venv error")
	assertScripts(t, resp.GetScripts(), map[string]string{"rotate": "rotate"})
	assertPackages(t, resp.GetPackages(), map[string]string{"ops_tools": "operator tooling"})

	checkout := gitPymoduleRepoDir("ops-tools")
	_, err := os.Stat(filepath.Join(checkout, ".venv"))
	c.True(os.IsNotExist(err), "a failed build published a .venv (err=%v)", err)
	assertNoStaging(t, checkout)
}

// A ref that does not exist must come back as a clean failure REPORT -- the
// RPC itself succeeds, VenvReady is false with git's own output, the
// inventory is empty, and nothing panics or hangs. The clone itself succeeds,
// so the checkout stays in place for a later refresh with a good ref.
func TestSyncPyModuleGitSourceFailsCleanlyOnBadRef(t *testing.T) {
	c := assert.NewCollecting(t)
	s := gitSyncServer(t, true)
	repo := gitFixture(t, map[string]string{
		"scripts/rotate.py": "# rotate\n",
	})

	resp := syncGitSource(t, s, "ops-tools", repo, "no-such-ref")

	c.Require().False(resp.GetVenvReady(), "a failed checkout reported VenvReady")
	c.Require().NotEq("", resp.GetVenvError(), "VenvError is empty for a failed checkout")
	c.StrContains(resp.GetVenvError(), "no-such-ref", "VenvError")
	if len(resp.GetScripts()) != 0 || len(resp.GetPackages()) != 0 {
		t.Errorf("a failed checkout reported inventory: %v/%v", resp.GetScripts(), resp.GetPackages())
	}
	_, err := os.Stat(filepath.Join(gitPymoduleRepoDir("ops-tools"), ".git"))
	c.NoError(err, "the failed checkout destroyed the clone")
}

// Describe self-reports the option, mirroring how pymodules_sync is reported.
func TestDescribeReportsPymoduleGitSync(t *testing.T) {
	c := assert.NewCollecting(t)
	for _, tc := range []struct{ optIn, want bool }{{true, true}, {false, false}} {
		s := NewServer(Options{Root: t.TempDir(), Concurrency: 1, Version: "test", PymoduleGitSync: tc.optIn})
		resp, err := s.Describe(context.Background(), connect.NewRequest(&executorpb.DescribeRequest{}))
		c.Require().NoError(err, "Describe")
		got := resp.Msg.GetPymoduleGitSync()
		c.Eq(tc.want, got, "optIn=%v: Describe reported pymodule_git_sync=%v, want", tc.optIn, got)
	}
}
