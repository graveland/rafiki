// SPDX-License-Identifier: Apache-2.0

// The on-disk layout of an executor's synced repos, and the hold that keeps a
// commit checkout alive while something runs out of it.
//
// This lives in pkg/repos rather than pkg/executor because BOTH sides of the
// executor read it: the sync receiver (pkg/executor) writes the layout, and
// the two consumers that RUN out of it — the executor-hosted script launcher
// and pymodule_run — must resolve `current` and take the same hold. The tool
// layer cannot import pkg/executor (pkg/executor imports pkg/fundi/tools), so
// the one shared implementation has to sit below both.
//
// Layout, under CacheDir()/<name>/:
//
//	git/             bare repo; every fetch lands here
//	commits/<sha>/   detached worktree per commit, its own .venv inside, and a
//	                 .rafiki-checkout marker the executor writes once the tree
//	                 is COMPLETE (`git worktree add` is not atomic, so presence
//	                 alone never proves a checkout — see pkg/executor's
//	                 repoCheckoutComplete)
//	current          symlink -> commits/<sha> (relative)
//
// One checkout per commit is what lets a running job keep its commit while a
// refresh moves `current` on: a job resolves `current` to its real path when
// it starts and holds the flock that keeps the sweeper off that directory.
package repos

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"go.graveland.dev/rafiki/pkg/paths"
)

// CacheDir is the root every synced repo lives under on an executor:
// <cache>/repos, one directory per registered source name.
func CacheDir() string { return filepath.Join(paths.CacheDir(), "repos") }

// repoInUseFile is the flock marker inside a commit checkout. Its presence is
// not the signal — the LOCK is: an unheld marker is what the sweeper deletes
// the whole directory from under, and a held one is what stops it.
const repoInUseFile = ".rafiki-inuse"

// OpenRepoCheckout resolves <CacheDir>/<name>/current to its real commit
// directory and takes a shared flock on <dir>/.rafiki-inuse (created 0644).
// The caller keeps hold open for as long as anything runs from dir.
//
// The hold is what makes per-commit checkouts safe to sweep: a directory
// someone holds cannot be removed, so a job that resolved `current` at start
// keeps its commit (and that commit's .venv) for its whole life, however many
// refreshes land in between. A child process should inherit the descriptor
// (cmd.ExtraFiles) so the lock survives the parent: the lock belongs to
// whoever is still running out of the directory, not to whoever opened it.
func OpenRepoCheckout(name string) (dir string, hold *os.File, err error) {
	current := filepath.Join(CacheDir(), name, "current")
	dir, err = filepath.EvalSymlinks(current)
	if err != nil {
		return "", nil, fmt.Errorf("repo %q has no checked-out commit on this executor (has the repo been refreshed on it yet?): %w", name, err)
	}
	hold, err = lockRepoCheckout(dir)
	if err != nil {
		return "", nil, err
	}
	return dir, hold, nil
}

// LockRepoCheckoutExclusive tries to take the exclusive side of a commit
// checkout's lock without blocking. busy reports that somebody else holds it,
// which is the sweeper's "in use, leave it alone" answer rather than an error.
//
// The caller must Close the returned hold — including after removing the
// directory it names, which is safe: the lock lives on the open descriptor,
// not on the path.
func LockRepoCheckoutExclusive(dir string) (hold *os.File, busy bool, err error) {
	f, err := os.OpenFile(filepath.Join(dir, repoInUseFile), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, false, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if err == syscall.EWOULDBLOCK {
			return nil, true, nil
		}
		return nil, false, err
	}
	return f, false, nil
}

// lockRepoCheckout takes the shared hold on dir and stamps the directory's
// mtime, which is the sweeper's only age signal: a checkout nobody has touched
// for a week is a checkout nobody is using.
func lockRepoCheckout(dir string) (*os.File, error) {
	f, err := os.OpenFile(filepath.Join(dir, repoInUseFile), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open in-use marker in %s: %w", dir, err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_SH); err != nil {
		f.Close()
		return nil, fmt.Errorf("hold checkout %s: %w", dir, err)
	}
	now := time.Now()
	if err := os.Chtimes(dir, now, now); err != nil {
		f.Close()
		return nil, fmt.Errorf("touch checkout %s: %w", dir, err)
	}
	return f, nil
}
