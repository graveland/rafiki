// SPDX-License-Identifier: Apache-2.0

package executor

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"go.graveland.dev/rafiki/pkg/repos"

	"github.com/multigres/testkit/assert"
)

// syncFirstCommit syncs a fixture repo's first commit and returns the source
// and that commit; the caller adds a second commit and syncs it.
func syncFirstCommit(t *testing.T, s *Server) (repo, commit string) {
	t.Helper()
	repo = gitFixture(t, map[string]string{"scripts/a.py": "# first\n"})
	commit = resolveRepo(t, s, "ops-tools", repo, "main")
	syncCommit(t, s, "ops-tools", repo, "main", commit)
	return repo, commit
}

// A held checkout survives a sweep that would otherwise remove it: that is the
// whole point of per-commit checkouts, so a job that resolved `current` at
// start keeps its commit — and its venv — while refreshes move on.
func TestRepoHoldKeepsOldCommit(t *testing.T) {
	c := assert.NewAborting(t)
	s := gitSyncServer(t, true)
	repo, held := syncFirstCommit(t, s)

	dir, hold, err := repos.OpenRepoCheckout("ops-tools")
	c.NoError(err, "OpenRepoCheckout")
	// EvalSymlinks on the expectation too: on a platform whose temp dir is
	// itself reached through a symlink, the hold reports the REAL path.
	want, err := filepath.EvalSymlinks(repoCommitDir("ops-tools", held))
	c.NoError(err, "EvalSymlinks")
	c.Eq(want, dir, "the hold resolved current to the wrong directory")
	c.Cleanup(func() { hold.Close() })

	second := commitFixture(t, repo, map[string]string{"scripts/b.py": "# second\n"})
	// Zero max age: every unheld checkout is fair game.
	old := repoCommitMaxAge
	repoCommitMaxAge = 0
	t.Cleanup(func() { repoCommitMaxAge = old })

	resp := syncCommit(t, s, "ops-tools", repo, "main", second)

	c.Eq(second, resp.GetCommit(), "response commit")
	c.Eq(second, currentCommit("ops-tools"), "current after the refresh")
	_, err = os.Stat(filepath.Join(dir, "scripts", "a.py"))
	c.NoError(err, "the HELD checkout was swept; a running job would have lost its commit")
}

// The other half of the same rule: an unheld checkout past the max age is
// removed, and git forgets the worktree too.
func TestRepoSweepRemovesUnheldOld(t *testing.T) {
	c := assert.NewAborting(t)
	s := gitSyncServer(t, true)
	repo, old := syncFirstCommit(t, s)

	second := commitFixture(t, repo, map[string]string{"scripts/b.py": "# second\n"})
	age := repoCommitMaxAge
	repoCommitMaxAge = 0
	t.Cleanup(func() { repoCommitMaxAge = age })

	syncCommit(t, s, "ops-tools", repo, "main", second)

	_, err := os.Stat(repoCommitDir("ops-tools", old))
	c.True(os.IsNotExist(err), "an unheld, past-age checkout survived the sweep (err=%v)", err)
	// The surviving checkout is still a live worktree of the bare repo.
	list := gitRun(t, repoGitDir("ops-tools"), "worktree", "list", "--porcelain")
	c.NotStrContains(list, repoCommitDir("ops-tools", old), "git still tracks the swept worktree")
	c.StrContains(list, repoCommitDir("ops-tools", second), "git lost the current worktree")
}

// A checkout newer than the max age is kept even when unheld: the age rule is
// what keeps a repo refreshed on a schedule from yanking a checkout out from
// under a job that started before the previous refresh.
func TestRepoSweepKeepsFreshUnheld(t *testing.T) {
	c := assert.NewAborting(t)
	s := gitSyncServer(t, true)
	repo, first := syncFirstCommit(t, s)

	second := commitFixture(t, repo, map[string]string{"scripts/b.py": "# second\n"})
	syncCommit(t, s, "ops-tools", repo, "main", second)

	_, err := os.Stat(repoCommitDir("ops-tools", first))
	c.NoError(err, "a fresh, unheld checkout was swept before its max age")
}

// OpenRepoCheckout stamps the directory it holds, which is the sweeper's only
// age signal: a checkout somebody is using is never "old".
func TestOpenRepoCheckoutTouchesDirectory(t *testing.T) {
	c := assert.NewAborting(t)
	s := gitSyncServer(t, true)
	_, commit := syncFirstCommit(t, s)
	dir := repoCommitDir("ops-tools", commit)

	past := time.Now().Add(-30 * 24 * time.Hour)
	c.NoError(os.Chtimes(dir, past, past), "age the checkout")

	_, hold, err := repos.OpenRepoCheckout("ops-tools")
	c.NoError(err, "OpenRepoCheckout")
	defer hold.Close()

	info, err := os.Stat(dir)
	c.NoError(err, "stat checkout")
	c.True(info.ModTime().After(past.Add(24*time.Hour)), "the hold did not touch the checkout's mtime (still %v)", info.ModTime())
}

// A name nothing was ever synced under is a refusal, not an empty path.
func TestOpenRepoCheckoutRefusesUnsyncedRepo(t *testing.T) {
	gitSyncServer(t, true)
	_, hold, err := repos.OpenRepoCheckout("never-synced")
	assert.NewAborting(t).Error(err, "OpenRepoCheckout on an unsynced repo")
	assert.NewCollecting(t).True(hold == nil, "an unsynced repo returned a hold")
}
