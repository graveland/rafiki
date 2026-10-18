// SPDX-License-Identifier: Apache-2.0

// The sweep that keeps per-commit repo checkouts from accumulating forever.
//
// A checkout is kept for as long as anybody might still be running out of it:
// the sweeper takes the EXCLUSIVE side of the same flock OpenRepoCheckout
// takes SHARED (pkg/repos), so a job that resolved `current` at start keeps
// its commit — and that commit's venv — for its whole life. Only an unheld
// checkout older than repoCommitMaxAge is removed.
package executor

import (
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"go.graveland.dev/rafiki/pkg/repos"
)

// repoCommitMaxAge is how long an UNHELD commit checkout survives. A week is
// long enough that a repo refreshed on a schedule never has a checkout pulled
// from under a job that started before the previous refresh, and short enough
// that a busy repo's history does not fill the disk.
//
// A package variable, not a constant, so a test can drive the age rule to its
// boundary without sleeping for a week.
var repoCommitMaxAge = 7 * 24 * time.Hour

// sweepRepoCommits removes every commit checkout of name other than
// keepCommit's that is both unheld and older than repoCommitMaxAge.
//
// Errors are logged, never returned: a sweep is housekeeping at the end of a
// successful refresh, and failing the refresh over a directory it could not
// delete would trade a disk-space problem for an availability one.
func sweepRepoCommits(name, keepCommit string) {
	commitsDir := repoCommitsDir(name)
	entries, err := os.ReadDir(commitsDir)
	if err != nil {
		if !os.IsNotExist(err) {
			slog.Warn("repo sweep: cannot read commits dir", "source", name, "dir", commitsDir, "error", err)
		}
		return
	}
	for _, e := range entries {
		// Only real directories are checkouts; the staged `.current-*` link
		// lives a level up, and anything else here is not ours to delete.
		if !e.IsDir() || e.Name() == keepCommit {
			continue
		}
		dir := filepath.Join(commitsDir, e.Name())
		hold, busy, err := repos.LockRepoCheckoutExclusive(dir)
		if err != nil {
			slog.Warn("repo sweep: cannot lock checkout", "source", name, "commit", e.Name(), "error", err)
			continue
		}
		if busy {
			continue // something is running out of it
		}
		info, err := os.Stat(dir)
		if err != nil || time.Since(info.ModTime()) < repoCommitMaxAge {
			hold.Close()
			continue
		}
		if out, err := gitOutput(repoGitDir(name), "worktree", "remove", "--force", dir); err != nil {
			slog.Warn("repo sweep: remove checkout failed",
				"source", name, "commit", e.Name(), "error", err, "output", string(out))
			hold.Close()
			continue
		}
		// The lock lives on the descriptor, not the path, so it is safe to
		// close after the directory it named is gone.
		hold.Close()
		if out, err := gitOutput(repoGitDir(name), "worktree", "prune"); err != nil {
			slog.Warn("repo sweep: prune failed",
				"source", name, "commit", e.Name(), "error", err, "output", string(out))
		}
	}
}
