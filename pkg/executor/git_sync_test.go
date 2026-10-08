// SPDX-License-Identifier: Apache-2.0

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

	executorpb "go.graveland.dev/rafiki/pkg/executorpb"

	"github.com/multigres/testkit/assert"
)

// requireGit fails the test when git is not installed. The tests below drive
// the REAL git binary in a temp dir; a missing git is a broken environment,
// not a reason to skip -- a skipped guard is not a guard.
func requireGit(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Fatal("git is required for the executor git-sync tests:", err)
	}
}

// gitServer builds a Server whose git calls run under a pinned environment --
// this process's environment, with the developer's global and system git
// config nulled so neither the fixtures nor the handler inherit a
// commit.gpgsign or an insteadOf rewrite -- and whose scratch directory is a
// fresh temp dir.
func gitServer(t *testing.T) *Server {
	t.Helper()
	requireGit(t)
	t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
	t.Setenv("GIT_CONFIG_SYSTEM", "/dev/null")
	t.Setenv("TMPDIR", t.TempDir())
	return &Server{opts: Options{Env: os.Environ()}}
}

// writeFile writes name under dir, creating parents, and aborts on failure.
func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	c := assert.NewAborting(t)
	path := filepath.Join(dir, name)
	c.NoError(os.MkdirAll(filepath.Dir(path), 0o755))
	c.NoError(os.WriteFile(path, []byte(content), 0o644))
}

// mustRandomHex returns 2n high-entropy hex characters, so a fixture's objects
// do not compress to nothing and bundle sizes are meaningful.
func mustRandomHex(t *testing.T, n int) string {
	t.Helper()
	h, err := randomHex(n)
	assert.NewAborting(t).NoError(err)
	return h
}

// mustBundle runs GitBundle and returns the response message.
func mustBundle(t *testing.T, s *Server, repo, branch string, exclude []string) *executorpb.GitBundleResponse {
	t.Helper()
	resp, err := s.GitBundle(context.Background(), connect.NewRequest(&executorpb.GitBundleRequest{
		Repo:        repo,
		Branch:      branch,
		ExcludeOids: exclude,
	}))
	assert.NewAborting(t).NoError(err, "GitBundle(%s, %s)", repo, branch)
	return resp.Msg
}

// mustFetch runs GitFetchBundle and returns the response message.
func mustFetch(t *testing.T, s *Server, repo, bundlePath, branch string, force bool) *executorpb.GitFetchBundleResponse {
	t.Helper()
	resp, err := s.GitFetchBundle(context.Background(), connect.NewRequest(&executorpb.GitFetchBundleRequest{
		Repo:       repo,
		BundlePath: bundlePath,
		Branch:     branch,
		Force:      force,
	}))
	assert.NewAborting(t).NoError(err, "GitFetchBundle(%s, %s)", repo, branch)
	return resp.Msg
}

func TestGitRefsReportsHeadsAndScratch(t *testing.T) {
	c := assert.NewCollecting(t)
	s := gitServer(t)
	repo := gitFixture(t, map[string]string{"a.txt": "hello\n"})
	gitRun(t, repo, "branch", "side")

	resp, err := s.GitRefs(context.Background(), connect.NewRequest(&executorpb.GitRefsRequest{Repo: repo}))
	c.Require().NoError(err, "GitRefs")
	m := resp.Msg
	c.True(m.GetExists(), "exists")
	c.True(filepath.IsAbs(m.GetScratchDir()), "scratch_dir %q must be absolute", m.GetScratchDir())

	got := map[string]string{}
	for _, h := range m.GetHeads() {
		got[h.GetName()] = h.GetOid()
	}
	c.Eq(2, len(got), "heads = %v", m.GetHeads())
	c.Eq(strings.TrimSpace(gitRun(t, repo, "rev-parse", "refs/heads/main")), got["main"], "main oid")
	c.Eq(strings.TrimSpace(gitRun(t, repo, "rev-parse", "refs/heads/side")), got["side"], "side oid")
}

func TestGitRefsAbsentOrNonRepoReportsNotExists(t *testing.T) {
	c := assert.NewCollecting(t)
	s := gitServer(t)

	missing := filepath.Join(t.TempDir(), "nope")
	resp, err := s.GitRefs(context.Background(), connect.NewRequest(&executorpb.GitRefsRequest{Repo: missing}))
	c.Require().NoError(err, "GitRefs on an absent repo")
	c.False(resp.Msg.GetExists(), "an absent repo reported exists")
	c.NotEq("", resp.Msg.GetScratchDir(), "scratch_dir must be filled even when the repo is absent")

	plain := t.TempDir()
	writeFile(t, plain, "notes.txt", "not a repository\n")
	resp, err = s.GitRefs(context.Background(), connect.NewRequest(&executorpb.GitRefsRequest{Repo: plain}))
	c.Require().NoError(err, "GitRefs on a non-repo directory")
	c.False(resp.Msg.GetExists(), "a non-repo directory reported exists")
}

// A leading dash makes git parse the branch as an option, so the guard must
// hold BEFORE any subprocess is spawned. The runner seam is swapped for a
// counter to prove that: a refusal that still ran `git check-ref-format
// --branch --upload-pack=...` would have handed git the exploit.
func TestGitBundleRefusesLeadingDashBranch(t *testing.T) {
	c := assert.NewCollecting(t)
	s := gitServer(t)
	repo := gitFixture(t, map[string]string{"a.txt": "a\n"})

	orig := gitRunner
	defer func() { gitRunner = orig }()
	var calls int
	gitRunner = func(dir string, env []string, args ...string) ([]byte, error) {
		calls++
		return nil, fmt.Errorf("git must not run for a leading-dash branch: %v", args)
	}

	_, err := s.GitBundle(context.Background(), connect.NewRequest(&executorpb.GitBundleRequest{
		Repo:   repo,
		Branch: "--upload-pack=touch /tmp/pwned",
	}))
	c.Require().Error(err, "accepted a branch that begins with a dash")
	c.Eq(connect.CodeInvalidArgument, connect.CodeOf(err), "code")
	c.Eq(0, calls, "a leading-dash branch reached a git subprocess")
}

func TestGitBundleRefusesBadRefName(t *testing.T) {
	s := gitServer(t)
	repo := gitFixture(t, map[string]string{"a.txt": "a\n"})
	for _, branch := range []string{"a..b", "x y", "HEAD"} {
		t.Run(branch, func(t *testing.T) {
			c := assert.NewCollecting(t)
			_, err := s.GitBundle(context.Background(), connect.NewRequest(&executorpb.GitBundleRequest{
				Repo: repo, Branch: branch,
			}))
			c.Require().Error(err, "accepted branch %q", branch)
			c.Eq(connect.CodeInvalidArgument, connect.CodeOf(err), "code for %q", branch)
		})
	}
}

func TestGitBundleRefusesNonHexExclude(t *testing.T) {
	s := gitServer(t)
	repo := gitFixture(t, map[string]string{"a.txt": "a\n"})
	for _, oid := range []string{"--upload-pack=x", strings.Repeat("A", 40), "abc123"} {
		t.Run(oid, func(t *testing.T) {
			c := assert.NewCollecting(t)
			_, err := s.GitBundle(context.Background(), connect.NewRequest(&executorpb.GitBundleRequest{
				Repo: repo, Branch: "main", ExcludeOids: []string{oid},
			}))
			c.Require().Error(err, "accepted non-hex exclude oid %q", oid)
			c.Eq(connect.CodeInvalidArgument, connect.CodeOf(err), "code for %q", oid)
		})
	}
}

// A prerequisite the source does not have must be dropped: git refuses to
// create a bundle naming an object it cannot resolve, and a caller may offer a
// destination's tips that this source has never seen.
func TestGitBundleIgnoresUnknownExcludeOID(t *testing.T) {
	c := assert.NewCollecting(t)
	s := gitServer(t)
	repo := gitFixture(t, map[string]string{"a.txt": "hello\n"})

	resp, err := s.GitBundle(context.Background(), connect.NewRequest(&executorpb.GitBundleRequest{
		Repo: repo, Branch: "main", ExcludeOids: []string{strings.Repeat("a", 40)},
	}))
	c.Require().NoError(err, "an unknown prerequisite failed the bundle instead of being dropped")
	m := resp.Msg
	c.False(m.GetUpToDate(), "up_to_date")
	c.NotEq("", m.GetBundlePath(), "bundle_path")
	c.True(m.GetBytes() > 0, "bytes = %d", m.GetBytes())
}

func TestGitBundleUpToDateWhenDestHasTip(t *testing.T) {
	c := assert.NewCollecting(t)
	s := gitServer(t)
	repo := gitFixture(t, map[string]string{"a.txt": "hello\n"})
	tip := strings.TrimSpace(gitRun(t, repo, "rev-parse", "refs/heads/main"))

	resp, err := s.GitBundle(context.Background(), connect.NewRequest(&executorpb.GitBundleRequest{
		Repo: repo, Branch: "main", ExcludeOids: []string{tip},
	}))
	c.Require().NoError(err, "GitBundle")
	m := resp.Msg
	c.True(m.GetUpToDate(), "up_to_date")
	c.Eq(tip, m.GetTipOid(), "tip_oid")
	c.Eq("", m.GetBundlePath(), "bundle_path must be empty when up to date")
}

func TestGitBundleIncrementalOnlyCarriesNewObjects(t *testing.T) {
	c := assert.NewCollecting(t)
	s := gitServer(t)
	repo := gitFixture(t, map[string]string{"seed.txt": mustRandomHex(t, 2048)})
	for i := 0; i < 6; i++ {
		writeFile(t, repo, fmt.Sprintf("f%d.txt", i), mustRandomHex(t, 2048))
		gitRun(t, repo, "add", "-A")
		gitCommit(t, repo, fmt.Sprintf("c%d", i))
	}
	tip := strings.TrimSpace(gitRun(t, repo, "rev-parse", "refs/heads/main"))

	first := mustBundle(t, s, repo, "main", nil)
	c.True(first.GetBytes() > 0, "first bundle bytes = %d", first.GetBytes())

	writeFile(t, repo, "new.txt", mustRandomHex(t, 2048))
	gitRun(t, repo, "add", "-A")
	gitCommit(t, repo, "new")

	second := mustBundle(t, s, repo, "main", []string{tip})
	c.False(second.GetUpToDate(), "an incremental bundle reported up_to_date")
	c.True(second.GetBytes() < first.GetBytes(),
		"incremental bundle (%d bytes) is not smaller than the full one (%d bytes)",
		second.GetBytes(), first.GetBytes())
}

func TestGitFetchBundleRefusesPathOutsideScratch(t *testing.T) {
	c := assert.NewCollecting(t)
	s := gitServer(t)
	repo := gitFixture(t, map[string]string{"a.txt": "a\n"})
	outside := filepath.Join(t.TempDir(), "outside.bundle")
	c.Require().NoError(os.WriteFile(outside, []byte("whatever"), 0o600))

	_, err := s.GitFetchBundle(context.Background(), connect.NewRequest(&executorpb.GitFetchBundleRequest{
		Repo: repo, BundlePath: outside, Branch: "main",
	}))
	c.Require().Error(err, "accepted a bundle outside the scratch dir")
	c.Eq(connect.CodeInvalidArgument, connect.CodeOf(err), "code")
	_, serr := os.Stat(outside)
	c.NoError(serr, "a refused bundle outside the scratch dir was removed")
}

func TestGitFetchBundleRemovesBundle(t *testing.T) {
	c := assert.NewCollecting(t)
	s := gitServer(t)
	src := gitFixture(t, map[string]string{"a.txt": "hello\n"})
	b := mustBundle(t, s, src, "main", nil)
	_, serr := os.Stat(b.GetBundlePath())
	c.Require().NoError(serr, "bundle exists before the fetch")

	dest := filepath.Join(t.TempDir(), "dest")
	mustFetch(t, s, dest, b.GetBundlePath(), "main", false)

	_, serr = os.Stat(b.GetBundlePath())
	c.True(os.IsNotExist(serr), "the bundle survived a successful fetch (err=%v)", serr)
}

func TestGitFetchBundleRejectsCorruptBundle(t *testing.T) {
	c := assert.NewCollecting(t)
	s := gitServer(t)
	dest := gitFixture(t, map[string]string{"x.txt": "x\n"})

	scratch, err := scratchDir()
	c.Require().NoError(err, "scratchDir")
	corrupt := filepath.Join(scratch, "corrupt.bundle")
	c.Require().NoError(os.WriteFile(corrupt, []byte("this is not a git bundle\n"), 0o600))

	_, err = s.GitFetchBundle(context.Background(), connect.NewRequest(&executorpb.GitFetchBundleRequest{
		Repo: dest, BundlePath: corrupt, Branch: "main",
	}))
	c.Require().Error(err, "accepted a corrupt bundle")
	c.Eq(connect.CodeInvalidArgument, connect.CodeOf(err), "code")
	_, serr := os.Stat(corrupt)
	c.True(os.IsNotExist(serr), "the corrupt bundle was not removed (err=%v)", serr)
}

func TestGitFetchBundleCreatesRepoWhenAbsent(t *testing.T) {
	c := assert.NewCollecting(t)
	s := gitServer(t)
	src := gitFixture(t, map[string]string{"a.txt": "hello\n"})
	b := mustBundle(t, s, src, "main", nil)

	dest := filepath.Join(t.TempDir(), "created")
	resp := mustFetch(t, s, dest, b.GetBundlePath(), "main", false)
	c.True(resp.GetCreatedRepo(), "created_repo")
	c.Eq("", resp.GetOldOid(), "old_oid")
	c.Eq(b.GetTipOid(), resp.GetNewOid(), "new_oid")

	got, err := os.ReadFile(filepath.Join(dest, "a.txt"))
	c.Require().NoError(err, "the created repo has no checked-out working tree")
	c.Eq("hello\n", string(got), "checked-out content")
}

// divergedFeature builds a destination repo whose branch `feature` is NOT
// checked out and points at an unrelated commit, plus a source repo whose
// branch `feature` diverges from it. The returned bundle carries the source's
// feature tip.
func divergedFeature(t *testing.T, s *Server) (dest string, bundle *executorpb.GitBundleResponse) {
	t.Helper()
	c := assert.NewAborting(t)
	src := gitFixture(t, map[string]string{"a.txt": "base\n"})
	gitRun(t, src, "checkout", "-q", "-b", "feature")
	writeFile(t, src, "feat.txt", "feature\n")
	gitRun(t, src, "add", "-A")
	gitCommit(t, src, "feature")

	dest = t.TempDir()
	gitRun(t, dest, "init", "-q", "-b", "other")
	writeFile(t, dest, "other.txt", "other\n")
	gitRun(t, dest, "add", "-A")
	gitCommit(t, dest, "other")
	gitRun(t, dest, "branch", "feature") // at other's tip, NOT checked out

	c.NotEq("", gitRun(t, dest, "rev-parse", "refs/heads/feature"), "dest feature")
	return dest, mustBundle(t, s, src, "feature", nil)
}

func TestGitFetchBundleRefusesNonFastForward(t *testing.T) {
	c := assert.NewCollecting(t)
	s := gitServer(t)
	dest, b := divergedFeature(t, s)

	_, err := s.GitFetchBundle(context.Background(), connect.NewRequest(&executorpb.GitFetchBundleRequest{
		Repo: dest, BundlePath: b.GetBundlePath(), Branch: "feature",
	}))
	c.Require().Error(err, "a non-fast-forward update was accepted without force")
	c.Eq(connect.CodeFailedPrecondition, connect.CodeOf(err), "code")
	c.StrContains(err.Error(), "non-fast-forward", "the refusal should carry git's own message")
}

func TestGitFetchBundleForceAcceptsRewrite(t *testing.T) {
	c := assert.NewCollecting(t)
	s := gitServer(t)
	dest, b := divergedFeature(t, s)
	oldOID := strings.TrimSpace(gitRun(t, dest, "rev-parse", "refs/heads/feature"))

	resp := mustFetch(t, s, dest, b.GetBundlePath(), "feature", true)
	c.Eq(oldOID, resp.GetOldOid(), "old_oid")
	c.Eq(b.GetTipOid(), resp.GetNewOid(), "new_oid")
	c.False(resp.GetCreatedRepo(), "created_repo")
}

func TestGitFetchBundleRefusesCheckedOutBranch(t *testing.T) {
	c := assert.NewCollecting(t)
	s := gitServer(t)
	src := gitFixture(t, map[string]string{"a.txt": "base\n"})
	gitRun(t, src, "checkout", "-q", "-b", "feature")
	writeFile(t, src, "feat.txt", "feature\n")
	gitRun(t, src, "add", "-A")
	gitCommit(t, src, "feature")
	b := mustBundle(t, s, src, "feature", nil)

	dest := t.TempDir()
	gitRun(t, dest, "init", "-q", "-b", "feature")
	writeFile(t, dest, "dest.txt", "dest\n")
	gitRun(t, dest, "add", "-A")
	gitCommit(t, dest, "dest")

	// force must not override git's refusal to update the checked-out branch:
	// that refusal is what keeps this RPC from touching a working tree.
	_, err := s.GitFetchBundle(context.Background(), connect.NewRequest(&executorpb.GitFetchBundleRequest{
		Repo: dest, BundlePath: b.GetBundlePath(), Branch: "feature", Force: true,
	}))
	c.Require().Error(err, "updated the checked-out branch")
	c.Eq(connect.CodeFailedPrecondition, connect.CodeOf(err), "code")
	c.StrContains(err.Error(), "checked out", "the refusal should carry git's own message")
}

// The executor runs git inside a repository it did not create. Nothing that
// repository's config planted -- a hook, an fsmonitor program, an ssh command
// -- may execute, and none of it may travel to the destination.
func TestGitFetchBundleIgnoresSourceHooksAndConfig(t *testing.T) {
	c := assert.NewCollecting(t)
	s := gitServer(t)
	src := gitFixture(t, map[string]string{"a.txt": "hello\n"})

	// Two markers, so a failure names which config key was honoured: the
	// hooks (a post-checkout would fire if any code path touched a working
	// tree) and core.fsmonitor (which git runs on an index refresh, e.g. a
	// fetch -- the hardening's core.fsmonitor=false is what stops it).
	hookMarker := filepath.Join(t.TempDir(), "hook-marker")
	fsmonMarker := filepath.Join(t.TempDir(), "fsmonitor-marker")
	hookDir := filepath.Join(t.TempDir(), "hooks")
	c.Require().NoError(os.MkdirAll(hookDir, 0o755))
	for _, name := range []string{"post-checkout", "post-merge", "post-index-change"} {
		body := "#!/bin/sh\necho ran >> " + hookMarker + "\n"
		c.Require().NoError(os.WriteFile(filepath.Join(hookDir, name), []byte(body), 0o755))
	}
	fsmon := filepath.Join(t.TempDir(), "fsmonitor.sh")
	c.Require().NoError(os.WriteFile(fsmon, []byte("#!/bin/sh\necho ran >> "+fsmonMarker+"\nexit 1\n"), 0o755))
	gitRun(t, src, "config", "core.hooksPath", hookDir)
	gitRun(t, src, "config", "core.fsmonitor", fsmon)
	gitRun(t, src, "config", "core.sshCommand", fsmon)

	// (1) Bundling the hostile repo's history must not run its hooks or
	// fsmonitor, and a fresh destination must not inherit them.
	b := mustBundle(t, s, src, "main", nil)
	dest := filepath.Join(t.TempDir(), "dest")
	mustFetch(t, s, dest, b.GetBundlePath(), "main", false)

	_, err := os.Stat(fsmonMarker)
	c.True(os.IsNotExist(err), "the source's fsmonitor ran during a bundle round trip (err=%v)", err)
	_, err = os.Stat(hookMarker)
	c.True(os.IsNotExist(err), "the source's hooks ran during a bundle round trip (err=%v)", err)
	cfg, err := os.ReadFile(filepath.Join(dest, ".git", "config"))
	c.Require().NoError(err, "read destination config")
	c.NotStrContains(string(cfg), "hooksPath", "the destination inherited the source's hooksPath")
	c.NotStrContains(string(cfg), "fsmonitor", "the destination inherited the source's fsmonitor")

	// (2) Fetching INTO the hostile repo (an existing repository) must not
	// touch its working tree, so its own post-checkout hook must not fire, and
	// its fsmonitor must not run on the fetch's index refresh.
	other := gitFixture(t, map[string]string{"b.txt": "other\n"})
	gitRun(t, other, "checkout", "-q", "-b", "side")
	writeFile(t, other, "side.txt", "side\n")
	gitRun(t, other, "add", "-A")
	gitCommit(t, other, "side")
	gitRun(t, src, "branch", "side") // present but not checked out in src
	b2 := mustBundle(t, s, other, "side", nil)
	mustFetch(t, s, src, b2.GetBundlePath(), "side", true)

	_, err = os.Stat(fsmonMarker)
	c.True(os.IsNotExist(err), "the destination's fsmonitor ran on an existing repo's fetch (err=%v)", err)
	_, err = os.Stat(hookMarker)
	c.True(os.IsNotExist(err), "the destination's post-checkout hook ran; an existing repo's working tree was touched (err=%v)", err)
}

func TestScratchDirRemovesOldBundles(t *testing.T) {
	c := assert.NewCollecting(t)
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	dir := filepath.Join(tmp, scratchDirName)
	c.Require().NoError(os.MkdirAll(dir, 0o700))

	old := filepath.Join(dir, "old.bundle")
	fresh := filepath.Join(dir, "fresh.bundle")
	other := filepath.Join(dir, "keep.txt")
	oldDir := filepath.Join(dir, "stale.bundle") // the suffix on a directory
	for _, p := range []string{old, fresh, other} {
		c.Require().NoError(os.WriteFile(p, []byte("x"), 0o600))
	}
	c.Require().NoError(os.MkdirAll(oldDir, 0o700))
	past := time.Now().Add(-2 * time.Hour)
	c.Require().NoError(os.Chtimes(old, past, past))
	c.Require().NoError(os.Chtimes(oldDir, past, past))

	got, err := scratchDir()
	c.Require().NoError(err, "scratchDir")
	c.Eq(dir, got, "scratchDir path")

	_, err = os.Stat(old)
	c.True(os.IsNotExist(err), "a .bundle older than an hour survived the sweep (err=%v)", err)
	_, err = os.Stat(fresh)
	c.NoError(err, "a fresh .bundle was removed")
	_, err = os.Stat(other)
	c.NoError(err, "a non-bundle file was removed")
	_, err = os.Stat(oldDir)
	c.NoError(err, "a directory named *.bundle was removed")
}

// TestGitScratchDirRemovesOldBundles is the verify-pattern shim for
// TestScratchDirRemovesOldBundles: the task's verify pattern is `TestGit`, and
// go test -run is an unanchored substring match, so the pinned name alone
// would be silently skipped. The subtest carries the pinned name.
func TestGitScratchDirRemovesOldBundles(t *testing.T) {
	t.Run("TestScratchDirRemovesOldBundles", TestScratchDirRemovesOldBundles)
}
