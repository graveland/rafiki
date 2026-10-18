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

// gitSyncServer builds a Server whose repo sync targets a temp cache
// dir, by pointing XDG_CACHE_HOME at it. Same mechanism as pymoduleServer:
// paths.CacheDir resolves the same variable on this machine. PyModulesSync is
// left ON even in the refusing test, so a green refusal proves the two
// option gates are independent rather than one shared switch.
func gitSyncServer(t *testing.T, optIn bool) *Server {
	t.Helper()
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	return &Server{opts: Options{PyModulesSync: true, RepoSync: optIn}}
}

// skipWithoutGit skips a test when git is not installed. The repo cache is
// git's own layout, so these tests run real git against local fixtures rather
// than pretending: t.Skip (not a failure) because the environment, not the
// code, is what is missing.
func skipWithoutGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skipf("git is not on PATH, so the repo-cache layout cannot be exercised: %v", err)
	}
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
	skipWithoutGit(t)
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_SYSTEM", "/dev/null")
	repo := t.TempDir()
	gitRun(t, repo, "init", "-b", "main")
	writeFixtureFiles(t, repo, files)
	gitRun(t, repo, "add", "-A")
	gitCommit(t, repo, "fixture")
	return repo
}

// writeFixtureFiles writes files (relative path -> content) under dir.
func writeFixtureFiles(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	c := assert.NewAborting(t)
	for name, content := range files {
		path := filepath.Join(dir, name)
		c.NoError(os.MkdirAll(filepath.Dir(path), 0o755))
		c.NoError(os.WriteFile(path, []byte(content), 0o644))
	}
}

// commitFixture adds one commit to an existing fixture repo and returns its
// SHA.
func commitFixture(t *testing.T, repo string, files map[string]string) string {
	t.Helper()
	writeFixtureFiles(t, repo, files)
	gitRun(t, repo, "add", "-A")
	gitCommit(t, repo, "next")
	return strings.TrimSpace(gitRun(t, repo, "rev-parse", "HEAD"))
}

// resolveRepo asks s to resolve ref and returns the commit, failing the test
// when the executor reports a git failure instead.
func resolveRepo(t *testing.T, s *Server, name, url, ref string) string {
	t.Helper()
	c := assert.NewAborting(t)
	resp, err := s.ResolveRepoRef(context.Background(),
		connect.NewRequest(&executorpb.ResolveRepoRefRequest{Name: name, Url: url, Ref: ref}))
	c.NoError(err, "ResolveRepoRef")
	c.Eq("", resp.Msg.GetError(), "ResolveRepoRef error field for %s@%s", url, ref)
	c.Require().Len(resp.Msg.GetCommit(), 40, "resolved commit %q is not a 40-hex SHA", resp.Msg.GetCommit())
	return resp.Msg.GetCommit()
}

// syncCommit runs one SyncRepo for an explicit commit and returns the
// response's message, failing the test on an RPC-level error.
func syncCommit(t *testing.T, s *Server, name, url, ref, commit string) *executorpb.SyncRepoResponse {
	t.Helper()
	resp, err := s.SyncRepo(context.Background(), connect.NewRequest(&executorpb.SyncRepoRequest{
		Name: name, Url: url, Ref: ref, Commit: commit,
	}))
	assert.NewAborting(t).NoError(err, "SyncRepo")
	return resp.Msg
}

// syncRepo resolves ref and then syncs it — the daemon's own two-step
// sequence, against a local fixture.
func syncRepo(t *testing.T, s *Server, name, url, ref string) *executorpb.SyncRepoResponse {
	t.Helper()
	return syncCommit(t, s, name, url, ref, resolveRepo(t, s, name, url, ref))
}

// assertScripts asserts the response's scripts exactly match want (name ->
// description), order-insensitively.
func assertScripts(t *testing.T, got []*executorpb.RepoScript, want map[string]string) {
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
func assertPackages(t *testing.T, got []*executorpb.RepoPackage, want map[string]string) {
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
// a failed or finished build must never leave one behind.
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

func TestSyncRepoRefusesWhenNotOptedIn(t *testing.T) {
	c := assert.NewCollecting(t)
	s := gitSyncServer(t, false)
	_, err := s.SyncRepo(context.Background(),
		connect.NewRequest(&executorpb.SyncRepoRequest{Name: "ops-tools", Url: "/tmp/nowhere", Ref: "main"}))
	c.Require().Error(err, "SyncRepo ran on an executor that never opted in")
	c.Eq(connect.CodePermissionDenied, connect.CodeOf(err), "got")
	_, derr := os.Stat(reposRoot())
	c.True(os.IsNotExist(derr), "a refused sync created its cache root (err=%v)", derr)
}

// The name is a path segment under the cache root. Anything that could escape
// it must be refused before a single byte is written.
func TestSyncRepoRejectsInvalidName(t *testing.T) {
	c := assert.NewCollecting(t)
	s := gitSyncServer(t, true)
	_, err := s.SyncRepo(context.Background(),
		connect.NewRequest(&executorpb.SyncRepoRequest{Name: "../evil", Url: "/tmp/nowhere", Ref: "main"}))
	c.Require().Error(err, "accepted a name that can escape the repo cache directory")
	c.Eq(connect.CodeInvalidArgument, connect.CodeOf(err), "got code")
	_, derr := os.Stat(reposRoot())
	c.True(os.IsNotExist(derr), "a rejected sync wrote to disk (err=%v)", derr)
}

// A leading dash makes git parse the argument as an option rather than a
// value: `git fetch --upload-pack=<cmd>` executes <cmd> locally on this
// executor. The executor re-checks both wire values at its own boundary
// before any subprocess runs -- the same shape validSegment applies to the
// name. One test per field, so a pass line names exactly which gate held.
func TestSyncRepoRejectsLeadingDashRef(t *testing.T) {
	c := assert.NewCollecting(t)
	s := gitSyncServer(t, true)
	_, err := s.SyncRepo(context.Background(),
		connect.NewRequest(&executorpb.SyncRepoRequest{
			Name: "ops-tools", Url: "/tmp/nowhere", Ref: "--upload-pack=touch /tmp/pwned",
			Commit: strings.Repeat("a", 40),
		}))
	c.Require().Error(err, "accepted a ref that begins with a dash")
	c.Eq(connect.CodeInvalidArgument, connect.CodeOf(err), "got code")
	c.StrContains(err.Error(), "ref", "error")
	_, derr := os.Stat(reposRoot())
	c.True(os.IsNotExist(derr), "a rejected sync wrote to disk (err=%v)", derr)
}

func TestSyncRepoRejectsLeadingDashUrl(t *testing.T) {
	c := assert.NewCollecting(t)
	s := gitSyncServer(t, true)
	_, err := s.SyncRepo(context.Background(),
		connect.NewRequest(&executorpb.SyncRepoRequest{
			Name: "ops-tools", Url: "--upload-pack=touch /tmp/pwned", Ref: "main",
			Commit: strings.Repeat("a", 40),
		}))
	c.Require().Error(err, "accepted a url that begins with a dash")
	c.Eq(connect.CodeInvalidArgument, connect.CodeOf(err), "got code")
	c.StrContains(err.Error(), "url", "error")
	_, derr := os.Stat(reposRoot())
	c.True(os.IsNotExist(derr), "a rejected sync wrote to disk (err=%v)", derr)
}

// The commit becomes a path segment of its own, so a value that is not the
// SHA ResolveRepoRef promises is refused before it reaches the filesystem.
func TestSyncRepoRejectsMalformedCommit(t *testing.T) {
	c := assert.NewCollecting(t)
	s := gitSyncServer(t, true)
	for _, bad := range []string{"", "../../etc", strings.Repeat("A", 40), strings.Repeat("a", 39), "HEAD"} {
		_, err := s.SyncRepo(context.Background(),
			connect.NewRequest(&executorpb.SyncRepoRequest{Name: "ops-tools", Url: "/tmp/nowhere", Ref: "main", Commit: bad}))
		c.Require().Error(err, "accepted commit %q", bad)
		c.Eq(connect.CodeInvalidArgument, connect.CodeOf(err), "code for commit %q", bad)
	}
	_, derr := os.Stat(reposRoot())
	c.True(os.IsNotExist(derr), "a rejected sync wrote to disk (err=%v)", derr)
}

// The repo cache root is lazily created by the first sync, and a root this
// executor never created must be REFUSED, not adopted: a pre-existing
// non-rafiki directory would otherwise have fetch and worktree add run inside
// it. Same mechanism as the blob path's assertManagedOrAbsent guard, one
// failure shape earlier in the handler.
func TestSyncRepoRefusesUnmanagedRoot(t *testing.T) {
	c := assert.NewCollecting(t)
	s := gitSyncServer(t, true)
	repo := gitFixture(t, map[string]string{
		"scripts/rotate.py": "# rotate\n",
	})

	root := reposRoot()
	c.Require().NoError(os.MkdirAll(root, 0o755))
	stray := filepath.Join(root, "operator-notes")
	c.Require().NoError(os.WriteFile(stray, []byte("not rafiki's"), 0o644))

	_, err := s.SyncRepo(context.Background(),
		connect.NewRequest(&executorpb.SyncRepoRequest{
			Name: "ops-tools", Url: repo, Ref: "main", Commit: strings.Repeat("a", 40),
		}))
	c.Require().Error(err, "synced into an unmanaged repo cache root")
	c.Eq(connect.CodeFailedPrecondition, connect.CodeOf(err), "got")
	if _, serr := os.Stat(repoDir("ops-tools")); !os.IsNotExist(serr) {
		t.Errorf("a refused sync wrote a checkout into the unmanaged root (err=%v)", serr)
	}
	if _, serr := os.Stat(filepath.Join(root, managedMarker)); !os.IsNotExist(serr) {
		t.Errorf("a refused sync marked the unmanaged root as its own (err=%v)", serr)
	}
	_, serr := os.Stat(stray)
	c.NoError(serr, "the refusal disturbed the root's own contents")
}

// ResolveRepoRef is the daemon's one git call: it fetches the ref into the
// source's bare repo and reports the commit FETCH_HEAD names.
func TestResolveRepoRefReturnsSHA(t *testing.T) {
	c := assert.NewCollecting(t)
	s := gitSyncServer(t, true)
	repo := gitFixture(t, map[string]string{"scripts/rotate.py": "# rotate\n"})
	want := strings.TrimSpace(gitRun(t, repo, "rev-parse", "HEAD"))

	commit := resolveRepo(t, s, "ops-tools", repo, "main")

	c.Eq(want, commit, "resolved commit")
	// The bare repo is a bare repo: no working tree of its own, and the
	// objects it fetched are what the commit checkouts are worktrees of.
	_, err := os.Stat(filepath.Join(repoGitDir("ops-tools"), "HEAD"))
	c.NoError(err, "the bare repo was not created at %s", repoGitDir("ops-tools"))
	_, err = os.Stat(filepath.Join(repoGitDir("ops-tools"), ".git"))
	c.True(os.IsNotExist(err), "the bare repo has a working tree of its own (err=%v)", err)
}

// A source whose url was repointed must resolve against the NEW url: the
// fetch names the URL rather than a configured remote, so no stale `origin`
// can keep serving the old repository's commits.
func TestResolveRepoRefHonoursRepointedURL(t *testing.T) {
	c := assert.NewCollecting(t)
	s := gitSyncServer(t, true)
	first := gitFixture(t, map[string]string{"scripts/a.py": "# a\n"})
	second := gitFixture(t, map[string]string{"scripts/b.py": "# b\n"})

	firstCommit := resolveRepo(t, s, "ops-tools", first, "main")
	secondCommit := resolveRepo(t, s, "ops-tools", second, "main")

	c.Eq(strings.TrimSpace(gitRun(t, first, "rev-parse", "HEAD")), firstCommit, "first url's commit")
	c.Eq(strings.TrimSpace(gitRun(t, second, "rev-parse", "HEAD")), secondCommit, "second url's commit")
	c.NotEq(firstCommit, secondCommit, "the repointed url resolved to the same commit")
}

// A ref git cannot resolve is REPORTED, not raised: the response carries
// git's output in error and no commit, so the daemon can tell a bad ref from
// an unreachable executor.
func TestResolveRepoRefReportsBadRef(t *testing.T) {
	c := assert.NewCollecting(t)
	s := gitSyncServer(t, true)
	repo := gitFixture(t, map[string]string{"scripts/a.py": "# a\n"})

	resp, err := s.ResolveRepoRef(context.Background(),
		connect.NewRequest(&executorpb.ResolveRepoRefRequest{Name: "ops-tools", Url: repo, Ref: "no-such-ref"}))

	c.Require().NoError(err, "a failed resolve must not be an RPC error")
	c.Eq("", resp.Msg.GetCommit(), "commit for a failed resolve")
	c.StrContains(resp.Msg.GetError(), "no-such-ref", "resolve error")
}

// A sync checks the resolved commit out as its OWN detached worktree and
// only then points `current` at it.
func TestSyncRepoChecksOutCommitAndFlipsCurrent(t *testing.T) {
	c := assert.NewCollecting(t)
	s := gitSyncServer(t, true)
	repo := gitFixture(t, map[string]string{
		"scripts/rotate.py":     "# rotate the keys\nprint('rotate')\n",
		"ops_tools/__init__.py": "# operator tooling\n",
	})

	commit := resolveRepo(t, s, "ops-tools", repo, "main")
	resp := syncCommit(t, s, "ops-tools", repo, "main", commit)

	c.Eq(commit, resp.GetCommit(), "response commit")
	c.Eq("", resp.GetCheckoutError(), "checkout error")
	assertScripts(t, resp.GetScripts(), map[string]string{"rotate": "rotate the keys"})
	assertPackages(t, resp.GetPackages(), map[string]string{"ops_tools": "operator tooling"})
	// No pyproject.toml in the fixture: nothing to build, and the response's
	// venv_ready contract promises true for that, not a failed build.
	if !resp.GetVenvReady() || resp.GetVenvError() != "" {
		t.Errorf("venv: got {ready: %v, error: %q}, want {ready: true, error: \"\"}",
			resp.GetVenvReady(), resp.GetVenvError())
	}

	checkout := repoCommitDir("ops-tools", commit)
	if _, err := os.Stat(filepath.Join(checkout, "scripts", "rotate.py")); err != nil {
		t.Errorf("the commit checkout does not exist at %s: %v", checkout, err)
	}
	// `current` is a RELATIVE symlink to the commit directory, so the cache
	// stays movable as a tree.
	target, err := os.Readlink(repoCurrentLink("ops-tools"))
	c.NoError(err, "current is not a symlink")
	c.Eq(filepath.Join("commits", commit), target, "current target")
	c.Eq(commit, currentCommit("ops-tools"), "currentCommit")
	// The lazily created cache root is marked, so a later sync recognises it
	// as rafiki's — the same managedMarker mechanism the blob path's
	// SyncPyModules applies to its own cache dir.
	_, err = os.Stat(filepath.Join(reposRoot(), managedMarker))
	c.NoError(err, "the first sync did not drop the managed marker on the repo cache root")
}

// A refresh lands the new commit in a checkout of its own and leaves the
// previous one alone: that is what lets a job that started on the old commit
// keep running from it.
func TestSyncRepoRefreshLandsNewCommit(t *testing.T) {
	c := assert.NewCollecting(t)
	s := gitSyncServer(t, true)
	repo := gitFixture(t, map[string]string{"scripts/a.py": "# first script\n"})
	first := resolveRepo(t, s, "ops-tools", repo, "main")
	syncCommit(t, s, "ops-tools", repo, "main", first)
	assertScripts(t, syncRepo(t, s, "ops-tools", repo, "main").GetScripts(),
		map[string]string{"a": "first script"})

	second := commitFixture(t, repo, map[string]string{"scripts/b.py": "# second script\n"})
	resp := syncCommit(t, s, "ops-tools", repo, "main", second)

	assertScripts(t, resp.GetScripts(), map[string]string{
		"a": "first script",
		"b": "second script", // the new commit arrived
	})
	c.Eq(second, resp.GetCommit(), "response commit")
	c.Eq(second, currentCommit("ops-tools"), "current after the refresh")
	// The first commit's checkout is untouched — under the default max age an
	// unheld checkout is kept, which is what a running job needs.
	_, err := os.Stat(filepath.Join(repoCommitDir("ops-tools", first), "scripts", "a.py"))
	c.NoError(err, "the previous commit's checkout was removed by a refresh that only moved current on")
}

// A second sync of the SAME commit does no git and no uv work: the checkout
// is already there, so an unreachable url must not fail it.
func TestSyncRepoUnchangedSkipsFetch(t *testing.T) {
	c := assert.NewCollecting(t)
	s := gitSyncServer(t, true)
	repo := gitFixture(t, map[string]string{"scripts/a.py": "# a\n"})
	commit := resolveRepo(t, s, "ops-tools", repo, "main")
	syncCommit(t, s, "ops-tools", repo, "main", commit)

	// The url is now unusable: any fetch at all would fail.
	resp := syncCommit(t, s, "ops-tools", filepath.Join(t.TempDir(), "gone"), "main", commit)

	c.Eq("", resp.GetCheckoutError(), "checkout error on an unchanged commit")
	c.Eq(commit, resp.GetCommit(), "response commit")
	assertScripts(t, resp.GetScripts(), map[string]string{"a": "a"})
	c.True(resp.GetVenvReady(), "an unchanged commit reported a venv failure")
}

// A commit the fetch did not bring cannot be served: the response says so and
// nothing changes on this executor.
func TestSyncRepoMissingCommit(t *testing.T) {
	c := assert.NewCollecting(t)
	s := gitSyncServer(t, true)
	repo := gitFixture(t, map[string]string{"scripts/a.py": "# a\n"})
	const missing = "0123456789abcdef0123456789abcdef01234567"

	resp := syncCommit(t, s, "ops-tools", repo, "main", missing)

	c.StrContains(resp.GetCheckoutError(), "commit "+missing+" not found after fetching main", "checkout error")
	c.Eq("", resp.GetCommit(), "response commit")
	c.Len(resp.GetScripts(), 0, "scripts on a failed checkout")
	_, err := os.Stat(repoCurrentLink("ops-tools"))
	c.True(os.IsNotExist(err), "a failed checkout moved current (err=%v)", err)
}

// A repo's skills ride the same refresh: discovered against the commit's own
// checkout, with per-skill errors in their own field so a broken skill never
// fails the scripts.
func TestSyncRepoReportsSkills(t *testing.T) {
	c := assert.NewCollecting(t)
	s := gitSyncServer(t, true)
	repo := gitFixture(t, map[string]string{
		"skills/x/SKILL.md":  "---\nname: x\ndescription: does x\n---\n\nDo the x thing.\n",
		"skills/x/helper.py": "print('h')\n",
	})

	resp := syncRepo(t, s, "ops-tools", repo, "main")

	c.Len(resp.GetSkillErrors(), 0, "skill errors")
	c.Require().Len(resp.GetSkills(), 1, "skills")
	sk := resp.GetSkills()[0]
	c.Eq("ops-tools", sk.GetNamespace(), "namespace")
	c.Eq("x", sk.GetName(), "name")
	c.Eq("does x", sk.GetDescription(), "description")
	c.StrContains(sk.GetBody(), "Do the x thing.", "body")
	c.Eq("skills/x/SKILL.md", sk.GetFrom(), "from")
	c.Require().Len(sk.GetFiles(), 1, "supporting files")
	c.Eq("helper.py", sk.GetFiles()[0].GetPath(), "supporting file path")
	c.Eq(uint32(0o644), sk.GetFiles()[0].GetMode(), "supporting file mode")
	c.Eq("print('h')\n", string(sk.GetFiles()[0].GetContent()), "supporting file content")
}

// A repo whose only dependency manifest is a bare requirements.txt cannot be
// served by `uv sync`: reporting venv_ready=true would promise a venv that
// was never built, its dependencies missing at run time. The failure reports
// exactly like a failed build -- VenvReady=false, one clear sentence naming
// the gap -- while the discovered inventory still reports (design §5).
func TestSyncRepoRequirementsOnlyRepoReportsNotReady(t *testing.T) {
	c := assert.NewCollecting(t)
	s := gitSyncServer(t, true)
	repo := gitFixture(t, map[string]string{
		"requirements.txt":  "requests\n",
		"scripts/rotate.py": "# rotate\n",
	})

	resp := syncRepo(t, s, "ops-tools", repo, "main")

	c.Require().False(resp.GetVenvReady(), "a requirements.txt-only repo reported VenvReady")
	c.StrContains(resp.GetVenvError(), "requirements.txt", "venv error")
	assertScripts(t, resp.GetScripts(), map[string]string{"rotate": "rotate"})
	_, err := os.Stat(filepath.Join(repoCommitDir("ops-tools", resp.GetCommit()), ".venv"))
	c.True(os.IsNotExist(err), "a skipped build published a .venv (err=%v)", err)
}

// The fake uv for the repo sync tests: implements exactly the one
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
# Fake uv for rafiki's repo sync tests -- see writeFakeUVSync.
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

func TestSyncRepoBuildsVenvWithFakeUv(t *testing.T) {
	c := assert.NewCollecting(t)
	uvDir := writeFakeUVSync(t)
	t.Setenv("RAFIKI_PYMODULE_UV", filepath.Join(uvDir, "uv"))
	log := setFakeUVLog(t)
	s := gitSyncServer(t, true)
	repo := gitFixture(t, map[string]string{
		"pyproject.toml":    "[project]\nname = \"ops-tools\"\nversion = \"0.1.0\"\ndependencies = [\"requests\"]\n",
		"scripts/rotate.py": "# rotate\n",
	})

	resp := syncRepo(t, s, "ops-tools", repo, "main")

	if !resp.GetVenvReady() || resp.GetVenvError() != "" {
		t.Fatalf("venv: got {ready: %v, error: %q}, want {ready: true, error: \"\"}",
			resp.GetVenvReady(), resp.GetVenvError())
	}
	checkout := repoCommitDir("ops-tools", resp.GetCommit())
	_, err := os.Stat(filepath.Join(checkout, ".venv", "bin", "python3"))
	c.NoError(err, "the repo's shared venv was not built into the commit's checkout")
	c.Eq(1, fakeUVInvocations(t, log), "uv ran")
	assertNoStaging(t, checkout)
}

// A broken dependency build must not hide the inventory (design §5): the
// discovery still reports, only the venv fails.
func TestSyncRepoReportsDiscoveryEvenOnVenvFailure(t *testing.T) {
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

	resp := syncRepo(t, s, "ops-tools", repo, "main")

	c.Require().False(resp.GetVenvReady(), "a failed uv sync reported VenvReady")
	c.StrContains(resp.GetVenvError(), "simulated sync failure", "venv error")
	assertScripts(t, resp.GetScripts(), map[string]string{"rotate": "rotate"})
	assertPackages(t, resp.GetPackages(), map[string]string{"ops_tools": "operator tooling"})

	checkout := repoCommitDir("ops-tools", resp.GetCommit())
	_, err := os.Stat(filepath.Join(checkout, ".venv"))
	c.True(os.IsNotExist(err), "a failed build published a .venv (err=%v)", err)
	assertNoStaging(t, checkout)
}

// Describe self-reports the option, mirroring how pymodules_sync is reported.
func TestDescribeReportsRepoSync(t *testing.T) {
	c := assert.NewCollecting(t)
	for _, tc := range []struct{ optIn, want bool }{{true, true}, {false, false}} {
		s := NewServer(Options{Root: t.TempDir(), Concurrency: 1, Version: "test", RepoSync: tc.optIn})
		resp, err := s.Describe(context.Background(), connect.NewRequest(&executorpb.DescribeRequest{}))
		c.Require().NoError(err, "Describe")
		got := resp.Msg.GetRepoSync()
		c.Eq(tc.want, got, "optIn=%v: Describe reported repo_sync=%v, want", tc.optIn, got)
	}
}
