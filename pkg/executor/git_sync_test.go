// SPDX-License-Identifier: Apache-2.0

package executor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	executorpb "go.graveland.dev/rafiki/pkg/executorpb"
	"golang.org/x/sys/unix"

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

// The executor runs git inside a repository it did not create. Each test below
// plants the hostile configuration in the repository the RPC actually operates
// on, so deleting the matching gitHardening option turns the test red.

// TestGitFetchBundleIgnoresHostileRepoFsmonitor pins
// `-c core.fsmonitor=false`: git runs the repository's fsmonitor on the index
// refresh a fetch performs, so without the option the marker appears.
func TestGitFetchBundleIgnoresHostileRepoFsmonitor(t *testing.T) {
	c := assert.NewCollecting(t)
	s := gitServer(t)
	src := gitFixture(t, map[string]string{"a.txt": "base\n"})
	gitRun(t, src, "checkout", "-q", "-b", "side")
	writeFile(t, src, "side.txt", "side\n")
	gitRun(t, src, "add", "-A")
	gitCommit(t, src, "side")
	b := mustBundle(t, s, src, "side", nil)

	dest := gitFixture(t, map[string]string{"d.txt": "d\n"})
	gitRun(t, dest, "branch", "side") // present, NOT checked out
	marker := filepath.Join(t.TempDir(), "fsmonitor-marker")
	fsmon := filepath.Join(t.TempDir(), "fsmonitor.sh")
	c.Require().NoError(os.WriteFile(fsmon, []byte("#!/bin/sh\necho ran >> "+marker+"\nexit 1\n"), 0o755))
	gitRun(t, dest, "config", "core.fsmonitor", fsmon)

	mustFetch(t, s, dest, b.GetBundlePath(), "side", true)

	_, err := os.Stat(marker)
	c.True(os.IsNotExist(err), "the destination repo's fsmonitor ran on fetch (err=%v)", err)
}

// TestGitFetchBundleRefusesExtInsteadOfRewrite pins
// `-c protocol.ext.allow=never`: a repo-local url.*.insteadOf can rewrite the
// bundle path to an ext:: command, which would otherwise execute here.
func TestGitFetchBundleRefusesExtInsteadOfRewrite(t *testing.T) {
	c := assert.NewCollecting(t)
	s := gitServer(t)
	src := gitFixture(t, map[string]string{"a.txt": "base\n"})
	gitRun(t, src, "checkout", "-q", "-b", "side")
	writeFile(t, src, "side.txt", "side\n")
	gitRun(t, src, "add", "-A")
	gitCommit(t, src, "side")
	b := mustBundle(t, s, src, "side", nil)

	dest := gitFixture(t, map[string]string{"d.txt": "d\n"})
	gitRun(t, dest, "branch", "side")
	marker := filepath.Join(t.TempDir(), "ext-marker")
	gitRun(t, dest, "config", "protocol.ext.allow", "always")
	gitRun(t, dest, "config", "url.ext::touch "+marker+".insteadOf", b.GetBundlePath())

	_, err := s.GitFetchBundle(context.Background(), connect.NewRequest(&executorpb.GitFetchBundleRequest{
		Repo: dest, BundlePath: b.GetBundlePath(), Branch: "side", Force: true,
	}))
	c.Require().Error(err, "a repo-local insteadOf routed the bundle through ext::")
	c.Eq(connect.CodeFailedPrecondition, connect.CodeOf(err), "code")
	_, serr := os.Stat(marker)
	c.True(os.IsNotExist(serr), "the ext:: transport executed (err=%v)", serr)
}

// TestGitFetchBundleIgnoresHostileSSHCommand pins `-c core.sshCommand=false`:
// a repo-local url.*.insteadOf can rewrite the bundle path to an ssh:// URL,
// and the repo's own core.sshCommand would then run.
func TestGitFetchBundleIgnoresHostileSSHCommand(t *testing.T) {
	c := assert.NewCollecting(t)
	s := gitServer(t)
	src := gitFixture(t, map[string]string{"a.txt": "base\n"})
	gitRun(t, src, "checkout", "-q", "-b", "side")
	writeFile(t, src, "side.txt", "side\n")
	gitRun(t, src, "add", "-A")
	gitCommit(t, src, "side")
	b := mustBundle(t, s, src, "side", nil)

	dest := gitFixture(t, map[string]string{"d.txt": "d\n"})
	gitRun(t, dest, "branch", "side")
	marker := filepath.Join(t.TempDir(), "ssh-marker")
	evil := filepath.Join(t.TempDir(), "evil-ssh.sh")
	c.Require().NoError(os.WriteFile(evil, []byte("#!/bin/sh\ntouch "+marker+"\nexit 0\n"), 0o755))
	gitRun(t, dest, "config", "core.sshCommand", evil)
	gitRun(t, dest, "config", "url.ssh://git@nonexistent.invalid/x.insteadOf", b.GetBundlePath())

	_, err := s.GitFetchBundle(context.Background(), connect.NewRequest(&executorpb.GitFetchBundleRequest{
		Repo: dest, BundlePath: b.GetBundlePath(), Branch: "side", Force: true,
	}))
	c.Require().Error(err, "a repo-local insteadOf routed the bundle through ssh://")
	c.Eq(connect.CodeFailedPrecondition, connect.CodeOf(err), "code")
	_, serr := os.Stat(marker)
	c.True(os.IsNotExist(serr), "the config-planted ssh command executed (err=%v)", serr)
}

// TestGitFetchBundleForcesHooksOffOnCheckout pins `-c core.hooksPath=/dev/null`:
// the one git call in this file that runs a hook is the checkout of a freshly
// initialized destination, and a repo created by this call carries no config of
// its own -- the hostile hooksPath arrives through the executor's git config.
func TestGitFetchBundleForcesHooksOffOnCheckout(t *testing.T) {
	c := assert.NewCollecting(t)
	marker := filepath.Join(t.TempDir(), "hook-marker")
	hookDir := filepath.Join(t.TempDir(), "hooks")
	c.Require().NoError(os.MkdirAll(hookDir, 0o755))
	c.Require().NoError(os.WriteFile(filepath.Join(hookDir, "post-checkout"),
		[]byte("#!/bin/sh\ntouch "+marker+"\n"), 0o755))
	cfg := filepath.Join(t.TempDir(), "gitconfig")
	c.Require().NoError(os.WriteFile(cfg, []byte("[core]\n\thooksPath = "+hookDir+"\n"), 0o600))

	// gitFixture nulls the global config; override it AFTER the fixture, then
	// build the server so its pinned env carries the hostile global config.
	src := gitFixture(t, map[string]string{"a.txt": "hello\n"})
	t.Setenv("GIT_CONFIG_GLOBAL", cfg)
	t.Setenv("TMPDIR", t.TempDir())
	s := &Server{opts: Options{Env: os.Environ()}}
	b := mustBundle(t, s, src, "main", nil)

	dest := filepath.Join(t.TempDir(), "created")
	mustFetch(t, s, dest, b.GetBundlePath(), "main", false)

	_, err := os.Stat(marker)
	c.True(os.IsNotExist(err), "a post-checkout hook from the executor's git config ran (err=%v)", err)
}

func TestScratchDirRemovesOldBundles(t *testing.T) {
	c := assert.NewCollecting(t)
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	dir := filepath.Join(tmp, scratchDirName)
	c.Require().NoError(os.MkdirAll(dir, 0o700))

	old := filepath.Join(dir, "old.bundle")
	fresh := filepath.Join(dir, "fresh.bundle")
	oldLock := filepath.Join(dir, "old.bundle.lock")
	freshLock := filepath.Join(dir, "fresh.bundle.lock")
	other := filepath.Join(dir, "keep.txt")
	oldDir := filepath.Join(dir, "stale.bundle") // the suffix on a directory
	for _, p := range []string{old, fresh, oldLock, freshLock, other} {
		c.Require().NoError(os.WriteFile(p, []byte("x"), 0o600))
	}
	c.Require().NoError(os.MkdirAll(oldDir, 0o700))
	past := time.Now().Add(-2 * time.Hour)
	for _, p := range []string{old, oldLock, oldDir} {
		c.Require().NoError(os.Chtimes(p, past, past))
	}

	got, err := scratchDir()
	c.Require().NoError(err, "scratchDir")
	c.Eq(dir, got, "scratchDir path")

	_, err = os.Stat(old)
	c.True(os.IsNotExist(err), "a .bundle older than an hour survived the sweep (err=%v)", err)
	_, err = os.Stat(oldLock)
	c.True(os.IsNotExist(err), "a stale .bundle.lock survived the sweep (err=%v)", err)
	_, err = os.Stat(fresh)
	c.NoError(err, "a fresh .bundle was removed")
	_, err = os.Stat(freshLock)
	c.NoError(err, "a fresh .bundle.lock was removed")
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

// TestGitFetchBundleRefusesBundlePathEscape is the HIGH guard: bundle_path is a
// wire value that becomes a path git opens and this call removes, so it must be
// an absolute, clean regular file whose parent resolves to the scratch
// directory -- and the removal must be registered only after all of that holds.
func TestGitFetchBundleRefusesBundlePathEscape(t *testing.T) {
	c := assert.NewCollecting(t)
	s := gitServer(t)
	repo := gitFixture(t, map[string]string{"a.txt": "a\n"})
	scratch, err := scratchDir()
	c.Require().NoError(err, "scratchDir")

	outside := t.TempDir()
	victim := filepath.Join(outside, "victim")
	c.Require().NoError(os.WriteFile(victim, []byte("victim"), 0o600))
	c.Require().NoError(os.Symlink(outside, filepath.Join(scratch, "link")))

	target := filepath.Join(outside, "target")
	c.Require().NoError(os.WriteFile(target, []byte("target"), 0o600))
	symlinkBundle := filepath.Join(scratch, "evil.bundle")
	c.Require().NoError(os.Symlink(target, symlinkBundle))

	dirEntry := filepath.Join(scratch, "adir")
	c.Require().NoError(os.Mkdir(dirEntry, 0o700))

	// A real regular file inside scratch, reachable only through an unclean
	// path: this is the case ONLY the cleanliness check refuses (its parent
	// resolves to scratch and Lstat sees a regular file), and the file must
	// survive because no removal was registered for a refused path.
	real := filepath.Join(scratch, "real.bundle")
	c.Require().NoError(os.WriteFile(real, []byte("not a bundle"), 0o600))

	// Built by concatenation, not filepath.Join: Join would Clean the path and
	// destroy the very traversal the check must refuse.
	cases := []struct{ name, path string }{
		{"dotdot through symlink", scratch + "/link/../victim"},
		{"symlink final element", symlinkBundle},
		{"relative", "relative.bundle"},
		{"directory", dirEntry},
		{"unclean missing file", scratch + "/./x.bundle"},
		{"unclean existing file", scratch + "/./real.bundle"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cc := assert.NewCollecting(t)
			_, err := s.GitFetchBundle(context.Background(), connect.NewRequest(&executorpb.GitFetchBundleRequest{
				Repo: repo, BundlePath: tc.path, Branch: "main",
			}))
			cc.Require().Error(err, "accepted bundle_path %q", tc.path)
			cc.Eq(connect.CodeInvalidArgument, connect.CodeOf(err), "code for %q", tc.path)
		})
	}
	// A refused bundle_path must not be the file this call removed, and the
	// escape targets outside scratch must all survive.
	for _, p := range []string{victim, target, dirEntry, real} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("a refused bundle_path removed %s (err=%v)", p, err)
		}
	}
}

// TestGitRefsSubdirectoryOfRepoIsNotARepo: repository discovery is fenced at
// the repo's parent, so a subdirectory of a repository is not one.
func TestGitRefsSubdirectoryOfRepoIsNotARepo(t *testing.T) {
	c := assert.NewCollecting(t)
	s := gitServer(t)
	repo := gitFixture(t, map[string]string{"a.txt": "a\n"})
	sub := filepath.Join(repo, "sub")
	c.Require().NoError(os.MkdirAll(sub, 0o755))

	resp, err := s.GitRefs(context.Background(), connect.NewRequest(&executorpb.GitRefsRequest{Repo: sub}))
	c.Require().NoError(err, "GitRefs on a subdirectory of a repository")
	c.False(resp.Msg.GetExists(), "a subdirectory of a repo reported exists=true")
	c.NotEq("", resp.Msg.GetScratchDir(), "scratch_dir")
}

// TestGitFetchBundleRefusesSubdirectoryOfRepo: a non-empty directory that is
// only a subdirectory of a repository must not be adopted, and the ancestor
// repository's refs must be left untouched. The branch is one the parent does
// NOT have, so a fetch that wrongly targeted the ancestor would create it and
// change show-ref -- the test goes red when both the ceiling and the ownership
// check are deleted.
func TestGitFetchBundleRefusesSubdirectoryOfRepo(t *testing.T) {
	c := assert.NewCollecting(t)
	s := gitServer(t)
	src := gitFixture(t, map[string]string{"a.txt": "a\n"})
	gitRun(t, src, "checkout", "-q", "-b", "seed")
	writeFile(t, src, "seed.txt", "seed\n")
	gitRun(t, src, "add", "-A")
	gitCommit(t, src, "seed")
	b := mustBundle(t, s, src, "seed", nil)

	repo := gitFixture(t, map[string]string{"b.txt": "b\n"})
	if out, err := exec.Command("git", "-C", repo, "rev-parse", "--verify", "refs/heads/seed").CombinedOutput(); err == nil {
		t.Fatalf("fixture: the parent repo already has refs/heads/seed: %s", out)
	}
	before := gitRun(t, repo, "show-ref")
	sub := filepath.Join(repo, "sub")
	c.Require().NoError(os.MkdirAll(sub, 0o755))
	c.Require().NoError(os.WriteFile(filepath.Join(sub, "file.txt"), []byte("x"), 0o644))

	_, err := s.GitFetchBundle(context.Background(), connect.NewRequest(&executorpb.GitFetchBundleRequest{
		Repo: sub, BundlePath: b.GetBundlePath(), Branch: "seed",
	}))
	c.Require().Error(err, "adopted a subdirectory of a repository")
	c.Eq(connect.CodeFailedPrecondition, connect.CodeOf(err), "code")
	c.Eq(before, gitRun(t, repo, "show-ref"), "the parent repository's refs changed")
}

func TestGitRepoPathMustBeAbsoluteAndClean(t *testing.T) {
	c := assert.NewCollecting(t)
	s := gitServer(t)
	repo := gitFixture(t, map[string]string{"a.txt": "a\n"})
	for _, bad := range []string{"relative/repo", repo + "/../" + filepath.Base(repo), repo + "/./x"} {
		_, err := s.GitRefs(context.Background(), connect.NewRequest(&executorpb.GitRefsRequest{Repo: bad}))
		c.Require().Error(err, "accepted repo %q", bad)
		c.Eq(connect.CodeInvalidArgument, connect.CodeOf(err), "code for %q", bad)
	}
}

// TestGitRefusesSymlinkedRepoPath: a repo path whose final component is a
// symlink is refused (Lstat, not Stat), so it cannot point anywhere.
func TestGitRefusesSymlinkedRepoPath(t *testing.T) {
	c := assert.NewCollecting(t)
	s := gitServer(t)
	repo := gitFixture(t, map[string]string{"a.txt": "a\n"})
	link := filepath.Join(t.TempDir(), "repo-link")
	c.Require().NoError(os.Symlink(repo, link))

	_, err := s.GitRefs(context.Background(), connect.NewRequest(&executorpb.GitRefsRequest{Repo: link}))
	c.Require().Error(err, "accepted a symlinked repo path")
	c.Eq(connect.CodeInvalidArgument, connect.CodeOf(err), "code")

	_, err = s.GitBundle(context.Background(), connect.NewRequest(&executorpb.GitBundleRequest{Repo: link, Branch: "main"}))
	c.Require().Error(err, "GitBundle accepted a symlinked repo path")
	c.Eq(connect.CodeInvalidArgument, connect.CodeOf(err), "code")
}

func TestGitFetchBundleRefusesNonRepoDirectory(t *testing.T) {
	c := assert.NewCollecting(t)
	s := gitServer(t)
	src := gitFixture(t, map[string]string{"a.txt": "a\n"})
	b := mustBundle(t, s, src, "main", nil)

	dir := t.TempDir()
	c.Require().NoError(os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("x"), 0o644))
	_, err := s.GitFetchBundle(context.Background(), connect.NewRequest(&executorpb.GitFetchBundleRequest{
		Repo: dir, BundlePath: b.GetBundlePath(), Branch: "main",
	}))
	c.Require().Error(err, "adopted a non-empty non-repo directory")
	c.Eq(connect.CodeFailedPrecondition, connect.CodeOf(err), "code")
}

// TestGitBundleRefusesHEADWithSpecificError pins the explicit HEAD refusal: it
// fires before any subprocess, and its message is the one surfaced.
func TestGitBundleRefusesHEADWithSpecificError(t *testing.T) {
	c := assert.NewCollecting(t)
	s := gitServer(t)
	repo := gitFixture(t, map[string]string{"a.txt": "a\n"})

	orig := gitRunner
	defer func() { gitRunner = orig }()
	var calls int
	gitRunner = func(dir string, env []string, args ...string) ([]byte, error) {
		calls++
		return nil, fmt.Errorf("git must not run for HEAD: %v", args)
	}

	_, err := s.GitBundle(context.Background(), connect.NewRequest(&executorpb.GitBundleRequest{Repo: repo, Branch: "HEAD"}))
	c.Require().Error(err, "accepted HEAD")
	c.Eq(connect.CodeInvalidArgument, connect.CodeOf(err), "code")
	c.StrContains(err.Error(), "names the current checkout", "the explicit HEAD refusal must be the one that fired")
	c.Eq(0, calls, "HEAD reached a git subprocess")
}

// TestGitBundleUpToDateWhenBundleWouldBeEmpty pins the empty-bundle branch
// specifically: the branch tip is NOT in exclude_oids, so the tip
// short-circuit cannot fire -- only git's "Refusing to create empty bundle"
// can turn this into up_to_date.
func TestGitBundleUpToDateWhenBundleWouldBeEmpty(t *testing.T) {
	c := assert.NewCollecting(t)
	s := gitServer(t)
	repo := gitFixture(t, map[string]string{"a.txt": "a\n"})
	mainTip := strings.TrimSpace(gitRun(t, repo, "rev-parse", "refs/heads/main"))
	gitRun(t, repo, "checkout", "-q", "-b", "ahead")
	writeFile(t, repo, "b.txt", "b\n")
	gitRun(t, repo, "add", "-A")
	gitCommit(t, repo, "ahead")
	aheadTip := strings.TrimSpace(gitRun(t, repo, "rev-parse", "refs/heads/ahead"))
	c.NotEq(mainTip, aheadTip, "fixture tips")

	resp, err := s.GitBundle(context.Background(), connect.NewRequest(&executorpb.GitBundleRequest{
		Repo: repo, Branch: "main", ExcludeOids: []string{aheadTip},
	}))
	c.Require().NoError(err, "GitBundle")
	c.True(resp.Msg.GetUpToDate(), "up_to_date")
	c.Eq(mainTip, resp.Msg.GetTipOid(), "tip_oid")
	c.Eq("", resp.Msg.GetBundlePath(), "bundle_path")
}

// TestGitScratchDirRefusesSymlink: a symlink at the scratch path must not be
// followed or adopted.
func TestGitScratchDirRefusesSymlink(t *testing.T) {
	c := assert.NewCollecting(t)
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	attacker := filepath.Join(tmp, "attacker")
	c.Require().NoError(os.MkdirAll(attacker, 0o700))
	c.Require().NoError(os.Symlink(attacker, filepath.Join(tmp, scratchDirName)))

	_, err := scratchDir()
	c.Require().Error(err, "adopted a symlinked scratch path")
}

func TestGitScratchDirRefusesNonDirectory(t *testing.T) {
	c := assert.NewCollecting(t)
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	c.Require().NoError(os.WriteFile(filepath.Join(tmp, scratchDirName), []byte("x"), 0o600))

	_, err := scratchDir()
	c.Require().Error(err, "adopted a non-directory scratch path")
}

// TestGitScratchDirRefusesForeignOwner pins the ownership check via the uid seam,
// since the test cannot chown.
func TestGitScratchDirRefusesForeignOwner(t *testing.T) {
	c := assert.NewCollecting(t)
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	c.Require().NoError(os.MkdirAll(filepath.Join(tmp, scratchDirName), 0o700))

	orig := scratchDirEUID
	defer func() { scratchDirEUID = orig }()
	scratchDirEUID = func() int { return os.Geteuid() + 1 }

	_, err := scratchDir()
	c.Require().Error(err, "adopted a scratch dir owned by another uid")
}

// TestGitBundleRefusesReflogSyntax pins the explicit @{ refusal: it fires
// before any subprocess, which check-ref-format would otherwise reach.
func TestGitBundleRefusesReflogSyntax(t *testing.T) {
	c := assert.NewCollecting(t)
	s := gitServer(t)
	repo := gitFixture(t, map[string]string{"a.txt": "a\n"})

	orig := gitRunner
	defer func() { gitRunner = orig }()
	var calls int
	gitRunner = func(dir string, env []string, args ...string) ([]byte, error) {
		calls++
		return nil, fmt.Errorf("git must not run for a reflog-syntax branch: %v", args)
	}

	for _, branch := range []string{"x@{-1}", "main@{1}"} {
		_, err := s.GitBundle(context.Background(), connect.NewRequest(&executorpb.GitBundleRequest{Repo: repo, Branch: branch}))
		c.Require().Error(err, "accepted branch %q", branch)
		c.Eq(connect.CodeInvalidArgument, connect.CodeOf(err), "code for %q", branch)
	}
	c.Eq(0, calls, "a reflog-syntax branch reached a git subprocess")
}

// TestGitFetchBundleDoesNotCreateRepoBeforeVerify: a bundle that fails
// verification must leave no destination behind.
func TestGitFetchBundleDoesNotCreateRepoBeforeVerify(t *testing.T) {
	c := assert.NewCollecting(t)
	s := gitServer(t)
	scratch, err := scratchDir()
	c.Require().NoError(err, "scratchDir")
	corrupt := filepath.Join(scratch, "corrupt.bundle")
	c.Require().NoError(os.WriteFile(corrupt, []byte("this is not a git bundle\n"), 0o600))
	dest := filepath.Join(t.TempDir(), "dest")

	_, err = s.GitFetchBundle(context.Background(), connect.NewRequest(&executorpb.GitFetchBundleRequest{
		Repo: dest, BundlePath: corrupt, Branch: "main",
	}))
	c.Require().Error(err, "accepted a corrupt bundle")
	c.Eq(connect.CodeInvalidArgument, connect.CodeOf(err), "code")
	_, serr := os.Stat(dest)
	c.True(os.IsNotExist(serr), "the destination was created before the bundle verified (err=%v)", serr)
}

// TestGitFetchBundleRemovesRepoCreatedOnFailure: a failure after this call
// created the repository must remove exactly what it created.
func TestGitFetchBundleRemovesRepoCreatedOnFailure(t *testing.T) {
	c := assert.NewCollecting(t)
	s := gitServer(t)
	src := gitFixture(t, map[string]string{"a.txt": "a\n"})

	// The bundle carries refs/heads/main, so asking for refs/heads/other fails
	// the fetch after the call created the repository.
	absentBundle := mustBundle(t, s, src, "main", nil)
	absent := filepath.Join(t.TempDir(), "created")
	_, err := s.GitFetchBundle(context.Background(), connect.NewRequest(&executorpb.GitFetchBundleRequest{
		Repo: absent, BundlePath: absentBundle.GetBundlePath(), Branch: "other",
	}))
	c.Require().Error(err, "fetched a branch the bundle does not contain")
	c.Eq(connect.CodeFailedPrecondition, connect.CodeOf(err), "code")
	_, serr := os.Stat(absent)
	c.True(os.IsNotExist(serr), "a repo directory created by the failed call was left behind (err=%v)", serr)

	// An existing EMPTY directory keeps the directory but loses the .git this
	// call created.
	emptyBundle := mustBundle(t, s, src, "main", nil)
	empty := t.TempDir()
	_, err = s.GitFetchBundle(context.Background(), connect.NewRequest(&executorpb.GitFetchBundleRequest{
		Repo: empty, BundlePath: emptyBundle.GetBundlePath(), Branch: "other",
	}))
	c.Require().Error(err, "fetched a branch the bundle does not contain")
	_, serr = os.Stat(filepath.Join(empty, ".git"))
	c.True(os.IsNotExist(serr), "a .git created by the failed call was left behind (err=%v)", serr)
	entries, rerr := os.ReadDir(empty)
	c.Require().NoError(rerr)
	c.Eq(0, len(entries), "the empty destination is no longer empty")
}

// TestGitEnvForcesCLocale pins LC_ALL=C: it must be present exactly once, and
// the pinned env's own LC_ALL must not shadow it (getenv returns the first
// match).
func TestGitEnvForcesCLocale(t *testing.T) {
	c := assert.NewCollecting(t)
	t.Setenv("TMPDIR", t.TempDir())
	s := &Server{opts: Options{Env: []string{
		"LC_ALL=tr_TR.UTF-8", "GIT_TERMINAL_PROMPT=1", "GIT_CEILING_DIRECTORIES=/nonexistent",
	}}}
	repo := t.TempDir()

	orig := gitRunner
	defer func() { gitRunner = orig }()
	var got []string
	gitRunner = func(dir string, env []string, args ...string) ([]byte, error) {
		if got == nil {
			got = env
		}
		return nil, errors.New("not running git")
	}

	_, err := s.GitRefs(context.Background(), connect.NewRequest(&executorpb.GitRefsRequest{Repo: repo}))
	c.Require().NoError(err, "GitRefs")
	c.Require().NotEmpty(got, "no git invocation recorded")

	counts := map[string]int{}
	for _, kv := range got {
		counts[strings.SplitN(kv, "=", 2)[0]]++
	}
	c.Eq(1, counts["LC_ALL"], "LC_ALL must appear exactly once")
	c.Eq(1, counts["GIT_TERMINAL_PROMPT"], "GIT_TERMINAL_PROMPT must appear exactly once")
	c.Eq(1, counts["GIT_CEILING_DIRECTORIES"], "GIT_CEILING_DIRECTORIES must appear exactly once")
	c.Contains(got, "LC_ALL=C")
	c.Contains(got, "GIT_TERMINAL_PROMPT=0")
}

// hasArgSequence reports whether any recorded invocation contains want as a
// contiguous run of argv elements.
func hasArgSequence(invocations [][]string, want ...string) bool {
	for _, inv := range invocations {
		for i := 0; i+len(want) <= len(inv); i++ {
			match := true
			for j, w := range want {
				if inv[i+j] != w {
					match = false
					break
				}
			}
			if match {
				return true
			}
		}
	}
	return false
}

// TestGitInvocationsUseArgvTerminator pins the `--` separators: every
// positional argument that could be mistaken for an option is preceded by one.
func TestGitInvocationsUseArgvTerminator(t *testing.T) {
	c := assert.NewCollecting(t)
	s := gitServer(t)
	repo := gitFixture(t, map[string]string{"a.txt": "a\n"})

	orig := gitRunner
	defer func() { gitRunner = orig }()
	var invocations [][]string
	gitRunner = func(dir string, env []string, args ...string) ([]byte, error) {
		invocations = append(invocations, append([]string(nil), args...))
		return orig(dir, env, args...)
	}

	mustBundle(t, s, repo, "main", []string{strings.Repeat("a", 40)})
	_, err := s.GitRefs(context.Background(), connect.NewRequest(&executorpb.GitRefsRequest{Repo: repo}))
	c.Require().NoError(err, "GitRefs")

	c.True(hasArgSequence(invocations, "bundle", "create", "--"), "no `git bundle create --` invocation")
	c.True(hasArgSequence(invocations, "cat-file", "-e", "--"), "no `git cat-file -e --` invocation")
	c.True(hasArgSequence(invocations, "for-each-ref", "--format=%(refname:short)%09%(objectname)", "--", "refs/heads"),
		"no `git for-each-ref -- refs/heads` invocation")
}

// TestGitFetchBundleReturnsNewOIDResolutionError pins the new_oid error return:
// a resolution failure after a successful fetch must be returned, not silently
// turned into an empty oid.
func TestGitFetchBundleReturnsNewOIDResolutionError(t *testing.T) {
	c := assert.NewCollecting(t)
	s := gitServer(t)
	src := gitFixture(t, map[string]string{"a.txt": "base\n"})
	gitRun(t, src, "checkout", "-q", "-b", "side")
	writeFile(t, src, "side.txt", "side\n")
	gitRun(t, src, "add", "-A")
	gitCommit(t, src, "side")
	b := mustBundle(t, s, src, "side", nil)

	dest := gitFixture(t, map[string]string{"d.txt": "d\n"})
	gitRun(t, dest, "branch", "side")

	orig := gitRunner
	defer func() { gitRunner = orig }()
	var resolves int
	gitRunner = func(dir string, env []string, args ...string) ([]byte, error) {
		if hasArgSequence([][]string{args}, "rev-parse", "--verify") {
			resolves++
			if resolves == 2 { // old_oid resolves first, new_oid second
				return []byte("simulated failure"), errors.New("exit status 1")
			}
		}
		return orig(dir, env, args...)
	}

	_, err := s.GitFetchBundle(context.Background(), connect.NewRequest(&executorpb.GitFetchBundleRequest{
		Repo: dest, BundlePath: b.GetBundlePath(), Branch: "side", Force: true,
	}))
	c.Require().Error(err, "a failed new_oid resolution must be returned, not silently emptied")
	c.Eq(connect.CodeFailedPrecondition, connect.CodeOf(err), "code")
}

// linkedWorktree creates a fresh repo and a linked worktree of it on branch,
// returning the worktree path.
func linkedWorktree(t *testing.T, branch string) string {
	t.Helper()
	main := gitFixture(t, map[string]string{"a.txt": "base\n"})
	wt := filepath.Join(t.TempDir(), "wt")
	gitRun(t, main, "worktree", "add", "-q", wt, "-b", branch)
	return wt
}

// TestGitRefsLinkedWorktreeIsARepo: the project's own sandboxes use linked
// worktrees, whose git dir lives in the main checkout -- that must still count
// as a repository at the worktree path.
func TestGitRefsLinkedWorktreeIsARepo(t *testing.T) {
	c := assert.NewCollecting(t)
	s := gitServer(t)
	wt := linkedWorktree(t, "wtbranch")

	resp, err := s.GitRefs(context.Background(), connect.NewRequest(&executorpb.GitRefsRequest{Repo: wt}))
	c.Require().NoError(err, "GitRefs on a linked worktree")
	c.True(resp.Msg.GetExists(), "a linked worktree reported exists=false")
	names := map[string]bool{}
	for _, h := range resp.Msg.GetHeads() {
		names[h.GetName()] = true
	}
	c.True(names["wtbranch"], "heads = %v", resp.Msg.GetHeads())
}

// TestGitBundleFromLinkedWorktree: a linked worktree is a valid bundle source.
func TestGitBundleFromLinkedWorktree(t *testing.T) {
	c := assert.NewCollecting(t)
	s := gitServer(t)
	wt := linkedWorktree(t, "wtbranch")

	b := mustBundle(t, s, wt, "wtbranch", nil)
	c.NotEq("", b.GetBundlePath(), "bundle_path")
	c.True(b.GetBytes() > 0, "bytes = %d", b.GetBytes())
	c.Eq(strings.TrimSpace(gitRun(t, wt, "rev-parse", "refs/heads/wtbranch")), b.GetTipOid(), "tip_oid")
}

// TestGitFetchBundleIntoLinkedWorktree: a linked worktree is a valid fetch
// destination, and the second, INCREMENTAL bundle only verifies because the
// worktree's COMMON object store was found (its own git dir has no objects/).
func TestGitFetchBundleIntoLinkedWorktree(t *testing.T) {
	c := assert.NewCollecting(t)
	s := gitServer(t)
	wt := linkedWorktree(t, "wtbranch")

	src := gitFixture(t, map[string]string{"s.txt": "one\n"})
	gitRun(t, src, "checkout", "-q", "-b", "seed")
	writeFile(t, src, "seed.txt", "one\n")
	gitRun(t, src, "add", "-A")
	gitCommit(t, src, "seed one")

	full := mustBundle(t, s, src, "seed", nil)
	first := mustFetch(t, s, wt, full.GetBundlePath(), "seed", false)
	c.False(first.GetCreatedRepo(), "created_repo")
	c.Eq(full.GetTipOid(), first.GetNewOid(), "new_oid")

	writeFile(t, src, "seed.txt", "two\n")
	gitRun(t, src, "add", "-A")
	gitCommit(t, src, "seed two")
	incr := mustBundle(t, s, src, "seed", []string{full.GetTipOid()})
	c.False(incr.GetUpToDate(), "up_to_date")

	second := mustFetch(t, s, wt, incr.GetBundlePath(), "seed", false)
	c.Eq(incr.GetTipOid(), second.GetNewOid(), "new_oid")
}

// TestGitRefsSeparateGitDirIsARepo: a submodule-style checkout keeps its git
// dir outside the work tree; it is still a repository at that path.
func TestGitRefsSeparateGitDirIsARepo(t *testing.T) {
	c := assert.NewCollecting(t)
	s := gitServer(t)
	repo := filepath.Join(t.TempDir(), "repo")
	gitdir := filepath.Join(t.TempDir(), "gitdir")
	gitRun(t, filepath.Dir(repo), "init", "-q", "-b", "main", "--separate-git-dir="+gitdir, repo)
	writeFile(t, repo, "a.txt", "a\n")
	gitRun(t, repo, "add", "-A")
	gitCommit(t, repo, "a")

	resp, err := s.GitRefs(context.Background(), connect.NewRequest(&executorpb.GitRefsRequest{Repo: repo}))
	c.Require().NoError(err, "GitRefs on a separate-git-dir checkout")
	c.True(resp.Msg.GetExists(), "a separate-git-dir checkout reported exists=false")

	b := mustBundle(t, s, repo, "main", nil)
	c.NotEq("", b.GetBundlePath(), "bundle_path")
}

// TestGitRefsSymlinkedAncestorSubdirectoryIsNotARepo: reached through a
// symlinked ancestor, a subdirectory of a repository must still be refused by
// the ownership check, not by the ceiling alone.
func TestGitRefsSymlinkedAncestorSubdirectoryIsNotARepo(t *testing.T) {
	c := assert.NewCollecting(t)
	s := gitServer(t)
	repo := gitFixture(t, map[string]string{"a.txt": "a\n"})
	link := filepath.Join(t.TempDir(), "repo-link")
	c.Require().NoError(os.Symlink(repo, link))
	sub := filepath.Join(link, "sub")
	c.Require().NoError(os.MkdirAll(sub, 0o755))

	resp, err := s.GitRefs(context.Background(), connect.NewRequest(&executorpb.GitRefsRequest{Repo: sub}))
	c.Require().NoError(err, "GitRefs")
	c.False(resp.Msg.GetExists(), "a subdirectory reached through a symlinked ancestor reported exists=true")
}

// TestGitScratchDirForcesMode0700: a pre-created scratch directory is chmodded to
// 0700, whatever mode it arrived with.
func TestGitScratchDirForcesMode0700(t *testing.T) {
	c := assert.NewCollecting(t)
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	dir := filepath.Join(tmp, scratchDirName)
	c.Require().NoError(os.Mkdir(dir, 0o755))
	c.Require().NoError(os.Chmod(dir, 0o755))

	got, err := scratchDir()
	c.Require().NoError(err, "scratchDir")
	c.Eq(dir, got, "scratchDir path")
	fi, err := os.Lstat(dir)
	c.Require().NoError(err)
	c.Eq(os.FileMode(0o700), fi.Mode().Perm(), "scratch dir mode")
}

// TestGitFetchBundleIgnoresRepoReferenceTransactionHook pins
// `-c core.hooksPath=/dev/null` through the hook a FETCH itself runs: a
// repo-local reference-transaction hook must not execute.
func TestGitFetchBundleIgnoresRepoReferenceTransactionHook(t *testing.T) {
	c := assert.NewCollecting(t)
	s := gitServer(t)
	src := gitFixture(t, map[string]string{"a.txt": "base\n"})
	gitRun(t, src, "checkout", "-q", "-b", "side")
	writeFile(t, src, "side.txt", "side\n")
	gitRun(t, src, "add", "-A")
	gitCommit(t, src, "side")
	b := mustBundle(t, s, src, "side", nil)

	dest := gitFixture(t, map[string]string{"d.txt": "d\n"})
	gitRun(t, dest, "branch", "side")
	marker := filepath.Join(t.TempDir(), "reference-transaction-marker")
	hooks := filepath.Join(t.TempDir(), "hooks")
	c.Require().NoError(os.MkdirAll(hooks, 0o755))
	c.Require().NoError(os.WriteFile(filepath.Join(hooks, "reference-transaction"),
		[]byte("#!/bin/sh\necho ran >> "+marker+"\n"), 0o755))
	gitRun(t, dest, "config", "core.hooksPath", hooks)

	mustFetch(t, s, dest, b.GetBundlePath(), "side", true)

	_, err := os.Stat(marker)
	c.True(os.IsNotExist(err), "the destination repo's reference-transaction hook ran on fetch (err=%v)", err)
}

// TestGitScratchDirRemovesStaleVerifyDirs: the throwaway verify directories are
// swept after an hour, but only real directories -- a symlink of that name is
// never followed and its target survives.
func TestGitScratchDirRemovesStaleVerifyDirs(t *testing.T) {
	c := assert.NewCollecting(t)
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	dir := filepath.Join(tmp, scratchDirName)
	c.Require().NoError(os.MkdirAll(dir, 0o700))

	oldVerify := filepath.Join(dir, ".verify-old")
	freshVerify := filepath.Join(dir, ".verify-fresh")
	c.Require().NoError(os.MkdirAll(oldVerify, 0o700))
	c.Require().NoError(os.MkdirAll(freshVerify, 0o700))

	outside := t.TempDir()
	target := filepath.Join(outside, "target")
	c.Require().NoError(os.WriteFile(target, []byte("x"), 0o600))
	link := filepath.Join(dir, ".verify-link")
	c.Require().NoError(os.Symlink(outside, link))

	oldFile := filepath.Join(dir, ".verify-file")
	c.Require().NoError(os.WriteFile(oldFile, []byte("x"), 0o600))

	past := time.Now().Add(-2 * time.Hour)
	ts := []unix.Timespec{unix.NsecToTimespec(past.UnixNano()), unix.NsecToTimespec(past.UnixNano())}
	c.Require().NoError(unix.UtimesNanoAt(unix.AT_FDCWD, oldVerify, ts, 0))
	c.Require().NoError(unix.UtimesNanoAt(unix.AT_FDCWD, oldFile, ts, 0))
	// AT_SYMLINK_NOFOLLOW: give the SYMLINK itself an old mtime, so the sweep
	// would remove it if it followed the name.
	c.Require().NoError(unix.UtimesNanoAt(unix.AT_FDCWD, link, ts, unix.AT_SYMLINK_NOFOLLOW))

	_, err := scratchDir()
	c.Require().NoError(err, "scratchDir")

	_, err = os.Stat(oldVerify)
	c.True(os.IsNotExist(err), "a stale .verify-* directory survived the sweep (err=%v)", err)
	_, err = os.Stat(freshVerify)
	c.NoError(err, "a fresh .verify-* directory was removed")
	_, err = os.Stat(link)
	c.NoError(err, "a symlink named .verify-* was removed")
	_, err = os.Stat(target)
	c.NoError(err, "the sweep followed a symlink out of the scratch dir")
	_, err = os.Stat(oldFile)
	c.NoError(err, "a file named .verify-* was removed")
}

// TestGitFetchBundleLeavesPreexistingEmptyDirOnFailure: a destination directory
// that already existed was not created by this call, so a later failure must
// leave it in place.
func TestGitFetchBundleLeavesPreexistingEmptyDirOnFailure(t *testing.T) {
	c := assert.NewCollecting(t)
	s := gitServer(t)
	src := gitFixture(t, map[string]string{"a.txt": "a\n"})
	b := mustBundle(t, s, src, "main", nil)

	empty := t.TempDir()
	_, err := s.GitFetchBundle(context.Background(), connect.NewRequest(&executorpb.GitFetchBundleRequest{
		Repo: empty, BundlePath: b.GetBundlePath(), Branch: "other",
	}))
	c.Require().Error(err, "fetched a branch the bundle does not contain")
	_, serr := os.Stat(empty)
	c.NoError(serr, "a pre-existing empty destination was removed")
	entries, rerr := os.ReadDir(empty)
	c.Require().NoError(rerr)
	c.Eq(0, len(entries), "a pre-existing empty destination is no longer empty")
}

// TestGitFetchBundleLeavesCreatedParentsOnFailure: only the leaf this call
// created is removed on failure; parents it created with MkdirAll stay.
func TestGitFetchBundleLeavesCreatedParentsOnFailure(t *testing.T) {
	c := assert.NewCollecting(t)
	s := gitServer(t)
	src := gitFixture(t, map[string]string{"a.txt": "a\n"})
	b := mustBundle(t, s, src, "main", nil)

	parent := filepath.Join(t.TempDir(), "parent")
	leaf := filepath.Join(parent, "leaf")
	_, err := s.GitFetchBundle(context.Background(), connect.NewRequest(&executorpb.GitFetchBundleRequest{
		Repo: leaf, BundlePath: b.GetBundlePath(), Branch: "other",
	}))
	c.Require().Error(err, "fetched a branch the bundle does not contain")
	_, serr := os.Stat(parent)
	c.NoError(serr, "a parent directory created by MkdirAll was removed")
	_, serr = os.Stat(leaf)
	c.True(os.IsNotExist(serr), "the leaf directory created by this call survived a failure (err=%v)", serr)
}

// TestGitFetchBundleLeavesDirCreatedByRacerOnFailure pins createdLeaf: if the
// leaf already exists when the create runs, this call did not create it and
// must not remove it on a later failure.
func TestGitFetchBundleLeavesDirCreatedByRacerOnFailure(t *testing.T) {
	c := assert.NewCollecting(t)
	s := gitServer(t)
	src := gitFixture(t, map[string]string{"a.txt": "a\n"})
	b := mustBundle(t, s, src, "main", nil)

	orig := repoMkdir
	defer func() { repoMkdir = orig }()
	leaf := filepath.Join(t.TempDir(), "leaf")
	repoMkdir = func(path string, perm os.FileMode) error {
		if err := os.Mkdir(path, perm); err != nil {
			return err
		}
		// The directory now exists, but this call did not create it.
		return os.ErrExist
	}

	_, err := s.GitFetchBundle(context.Background(), connect.NewRequest(&executorpb.GitFetchBundleRequest{
		Repo: leaf, BundlePath: b.GetBundlePath(), Branch: "other",
	}))
	c.Require().Error(err, "fetched a branch the bundle does not contain")
	_, serr := os.Stat(leaf)
	c.NoError(serr, "a destination directory this call did not create was removed")
}

// TestGitRepoGitDirRejectsSymlinkedPathInsideRepo pins repoGitDir's ownership
// check ALONE. The path is a symlink into a repository's SUBDIRECTORY, so the
// ceiling (the symlink's parent) is not on the physical ancestor chain and is
// ineffective: git discovers the enclosing repository. Only "the repository
// must be repo itself" refuses it, so deleting that check turns this red while
// the ceiling stays in place.
func TestGitRepoGitDirRejectsSymlinkedPathInsideRepo(t *testing.T) {
	c := assert.NewCollecting(t)
	s := gitServer(t)
	repo := gitFixture(t, map[string]string{"a.txt": "a\n"})
	inner := filepath.Join(repo, "sub", "inner")
	c.Require().NoError(os.MkdirAll(inner, 0o755))
	link := filepath.Join(t.TempDir(), "link")
	c.Require().NoError(os.Symlink(inner, link))

	if _, ok := s.repoGitDir(link); ok {
		t.Errorf("repoGitDir accepted %q: a symlink into a repository's subdirectory is not a repository", link)
	}
}

// TestGitFetchBundleIgnoresHostileAlternateRefsCommand pins
// `-c core.alternateRefsCommand=true`. A fetch's connectivity check enumerates
// the refs of an alternate object store by running this repository-local
// command, so a destination repository whose objects/info/alternates points
// anywhere could otherwise run a program of the source's choosing on this
// executor. `true` is a harmless no-op; an empty value is deliberately NOT
// used because git treats it as unset and would fall back to the repository's
// own command.
func TestGitFetchBundleIgnoresHostileAlternateRefsCommand(t *testing.T) {
	c := assert.NewCollecting(t)
	s := gitServer(t)
	src := gitFixture(t, map[string]string{"a.txt": "base\n"})
	gitRun(t, src, "checkout", "-q", "-b", "side")
	writeFile(t, src, "side.txt", "side\n")
	gitRun(t, src, "add", "-A")
	gitCommit(t, src, "side")
	b := mustBundle(t, s, src, "side", nil)

	dest := gitFixture(t, map[string]string{"d.txt": "d\n"})
	gitRun(t, dest, "branch", "side") // present, NOT checked out

	// An alternate object store is what makes the fetch enumerate alternate
	// refs, and therefore what makes it run core.alternateRefsCommand.
	alt := t.TempDir()
	gitRun(t, alt, "init", "-q", "--bare")
	infoDir := filepath.Join(dest, ".git", "objects", "info")
	c.Require().NoError(os.MkdirAll(infoDir, 0o755))
	c.Require().NoError(os.WriteFile(filepath.Join(infoDir, "alternates"),
		[]byte(filepath.Join(alt, "objects")+"\n"), 0o600))

	marker := filepath.Join(t.TempDir(), "alternate-refs-marker")
	prog := filepath.Join(t.TempDir(), "alternate-refs.sh")
	c.Require().NoError(os.WriteFile(prog, []byte("#!/bin/sh\ntouch "+marker+"\nexit 0\n"), 0o755))
	gitRun(t, dest, "config", "core.alternateRefsCommand", prog)

	mustFetch(t, s, dest, b.GetBundlePath(), "side", true)

	_, err := os.Stat(marker)
	c.True(os.IsNotExist(err), "the destination repo's core.alternateRefsCommand ran on fetch (err=%v)", err)
}

// markerScript writes an executable that touches marker and returns its path.
func markerScript(t *testing.T, marker string) string {
	t.Helper()
	prog := filepath.Join(t.TempDir(), "prog.sh")
	assert.NewAborting(t).NoError(os.WriteFile(prog,
		[]byte("#!/bin/sh\ntouch "+marker+"\nexit 0\n"), 0o755))
	return prog
}

// hostileConfigProbe is one repository-local config key (or the config that
// reaches it) that can name a program. plant receives the repository and the
// path of a marker-writing script and must configure the repository so the key
// names that script.
type hostileConfigProbe struct {
	name  string
	plant func(t *testing.T, repo, script string)
}

// includeProbe returns a plant that smuggles a hostile core.alternateRefsCommand
// and core.fsmonitor through a config include (key is "include.path" or
// "includeIf.gitdir:**.path"), together with the alternate object store that
// would make the smuggled command fire.
func includeProbe(key string) func(t *testing.T, repo, script string) {
	return func(t *testing.T, repo, script string) {
		t.Helper()
		c := assert.NewAborting(t)
		included := filepath.Join(t.TempDir(), "included.cfg")
		c.NoError(os.WriteFile(included,
			[]byte("[core]\n\talternateRefsCommand = "+script+"\n\tfsmonitor = "+script+"\n"), 0o600))
		alt := t.TempDir()
		gitRun(t, alt, "init", "-q", "--bare")
		infoDir := filepath.Join(repo, ".git", "objects", "info")
		c.NoError(os.MkdirAll(infoDir, 0o755))
		c.NoError(os.WriteFile(filepath.Join(infoDir, "alternates"),
			[]byte(filepath.Join(alt, "objects")+"\n"), 0o600))
		gitRun(t, repo, "config", key, included)
	}
}

// TestGitHardeningHostileRepoConfigAudit is the PROBE-based audit behind the
// comment on gitHardening: every other repository-local config key that can
// name a program is planted in the repository an RPC operates on, pointing at
// a marker-writing script, and the marker must not exist after every git RPC
// in this file has run against it. The keys whose program would otherwise run
// (core.fsmonitor, core.hooksPath, core.sshCommand, core.alternateRefsCommand)
// are also covered by their named pin tests; this test is the evidence that
// the rest are inert for the calls this file makes. The url.*.insteadOf
// rewrite to ext::/ssh:// is covered by
// TestGitFetchBundleRefusesExtInsteadOfRewrite and
// TestGitFetchBundleIgnoresHostileSSHCommand.
func TestGitHardeningHostileRepoConfigAudit(t *testing.T) {
	probes := []hostileConfigProbe{
		{"core.pager", func(t *testing.T, repo, script string) {
			gitRun(t, repo, "config", "core.pager", script)
		}},
		{"core.editor", func(t *testing.T, repo, script string) {
			gitRun(t, repo, "config", "core.editor", script)
		}},
		{"core.askPass", func(t *testing.T, repo, script string) {
			gitRun(t, repo, "config", "core.askPass", script)
		}},
		{"credential.helper", func(t *testing.T, repo, script string) {
			gitRun(t, repo, "config", "credential.helper", script)
		}},
		{"core.fsmonitor", func(t *testing.T, repo, script string) {
			gitRun(t, repo, "config", "core.fsmonitor", script)
		}},
		{"core.sshCommand", func(t *testing.T, repo, script string) {
			gitRun(t, repo, "config", "core.sshCommand", script)
		}},
		{"core.hooksPath", func(t *testing.T, repo, script string) {
			hooks := t.TempDir()
			c := assert.NewAborting(t)
			c.NoError(os.Symlink(script, filepath.Join(hooks, "reference-transaction")))
			c.NoError(os.Symlink(script, filepath.Join(hooks, "post-checkout")))
			gitRun(t, repo, "config", "core.hooksPath", hooks)
		}},
		{"gc.auto with pre-auto-gc", func(t *testing.T, repo, script string) {
			hooks := t.TempDir()
			assert.NewAborting(t).NoError(os.Symlink(script, filepath.Join(hooks, "pre-auto-gc")))
			gitRun(t, repo, "config", "gc.auto", "1")
			gitRun(t, repo, "config", "gc.autoDetach", "false")
			gitRun(t, repo, "config", "core.hooksPath", hooks)
		}},
		{"uploadpack.packObjectsHook", func(t *testing.T, repo, script string) {
			gitRun(t, repo, "config", "uploadpack.packObjectsHook", script)
		}},
		{"filter driver", func(t *testing.T, repo, script string) {
			writeFile(t, repo, ".gitattributes", "* filter=evil\n")
			gitRun(t, repo, "config", "filter.evil.clean", script)
			gitRun(t, repo, "config", "filter.evil.smudge", script)
		}},
		{"diff driver", func(t *testing.T, repo, script string) {
			writeFile(t, repo, ".gitattributes", "* diff=evil\n")
			gitRun(t, repo, "config", "diff.evil.command", script)
		}},
		{"merge driver", func(t *testing.T, repo, script string) {
			writeFile(t, repo, ".gitattributes", "* merge=evil\n")
			gitRun(t, repo, "config", "merge.evil.driver", script)
		}},
		{"submodule.recurse", func(t *testing.T, repo, script string) {
			gitRun(t, repo, "config", "submodule.recurse", "true")
			gitRun(t, repo, "config", "fetch.recurseSubmodules", "yes")
		}},
		{"include.path", includeProbe("include.path")},
		{"includeIf", includeProbe("includeIf.gitdir:**.path")},
	}
	for _, p := range probes {
		t.Run(p.name, func(t *testing.T) {
			c := assert.NewCollecting(t)
			s := gitServer(t)
			marker := filepath.Join(t.TempDir(), "marker")
			script := markerScript(t, marker)

			src := gitFixture(t, map[string]string{"a.txt": "base\n"})
			gitRun(t, src, "checkout", "-q", "-b", "side")
			writeFile(t, src, "side.txt", "side\n")
			gitRun(t, src, "add", "-A")
			gitCommit(t, src, "side")

			dest := gitFixture(t, map[string]string{"d.txt": "d\n"})
			gitRun(t, dest, "branch", "side")

			p.plant(t, src, script)
			p.plant(t, dest, script)

			b := mustBundle(t, s, src, "side", nil)
			_, err := s.GitRefs(context.Background(), connect.NewRequest(&executorpb.GitRefsRequest{Repo: src}))
			c.Require().NoError(err, "GitRefs")
			mustFetch(t, s, dest, b.GetBundlePath(), "side", true)

			_, serr := os.Stat(marker)
			c.True(os.IsNotExist(serr), "repo-local config %q executed a program (err=%v)", p.name, serr)
		})
	}
}

// TestGitFetchBundleDoesNotRecurseSubmodules pins the audit decision that
// `fetch.recurseSubmodules=false`/`submodule.recurse=false` are NOT added: a
// fetch from a bundle file has no remote to recurse into, so even a
// destination configured to recurse fetches nothing for the gitlink a bundle
// carries -- no .git/modules appears and no submodule working tree is written.
func TestGitFetchBundleDoesNotRecurseSubmodules(t *testing.T) {
	c := assert.NewCollecting(t)
	s := gitServer(t)

	sub := gitFixture(t, map[string]string{"s.txt": "s\n"})
	super := gitFixture(t, map[string]string{"x.txt": "x\n"})
	gitRun(t, super, "config", "protocol.file.allow", "always")
	gitRun(t, super, "checkout", "-q", "-b", "side")
	gitRun(t, super, "-c", "protocol.file.allow=always", "submodule", "add", "-q", sub, "sub")
	gitRun(t, super, "add", "-A")
	gitCommit(t, super, "super")
	b := mustBundle(t, s, super, "side", nil)

	dest := gitFixture(t, map[string]string{"d.txt": "d\n"})
	gitRun(t, dest, "branch", "side")
	gitRun(t, dest, "config", "fetch.recurseSubmodules", "yes")
	gitRun(t, dest, "config", "submodule.recurse", "true")

	mustFetch(t, s, dest, b.GetBundlePath(), "side", true)

	_, err := os.Stat(filepath.Join(dest, ".git", "modules"))
	c.True(os.IsNotExist(err), "the fetch recursed into a submodule (err=%v)", err)
	_, err = os.Stat(filepath.Join(dest, "sub"))
	c.True(os.IsNotExist(err), "the fetch wrote a submodule working tree (err=%v)", err)
}
