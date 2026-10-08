// SPDX-License-Identifier: Apache-2.0

package integration_test

// End-to-end proof of the path-sync plane: the real `rafiki` CLI against a real
// daemon with TWO real native executors enrolled.
//
// This is the only test that pins the POSITIVE wiring of main.go's
// wirePathSync: the unit tests around the pathSyncer exercise it directly and
// the connectapi tests install it on a hand-built Server, so a dropped
// `srv.SetPathSyncer(ps)` (or a wirePathSync call removed entirely) leaves every
// one of them green. Only driving the CLI through the daemon's Connect face
// fails here — SyncPath/SyncRepo answer Unavailable the moment the server has no
// syncer.
//
// Both executors are native (isolation "none"), which is also what pins the
// overwrite gate reading the destination executor's ROW: a native destination
// must refuse --overwrite even though nothing about the request itself is
// container-shaped.
//
// Everything a subprocess is handed goes through the harness: daemonBinary()
// via bootGrantDaemon, cliBinary() for `executor serve`, cliCmd for the CLI.
// The executor roots and every scratch path are t.TempDir(), and each executor
// gets its own TMPDIR so the two local processes do not share the git bundle
// scratch directory os.TempDir() would otherwise resolve to one machine-wide
// path.

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.graveland.dev/rafiki/pkg/executors"

	"github.com/multigres/testkit/assert"
)

// pathSyncRefA/pathSyncRefB are the executor refs the CLI arguments address.
// resolve (cmd/rafikid/path_sync.go) matches a ref against an executor row's
// `machine` label, so each enrollment below stamps one.
const (
	pathSyncRefA = "A"
	pathSyncRefB = "B"
)

// pathSyncEnv is one booted daemon plus the two native executors enrolled on it
// and the roots they were started with.
type pathSyncEnv struct {
	g     *grantDaemon
	rootA string
	rootB string
}

// bootPathSyncEnv boots the executor-bearing daemon and enrolls two native
// executors, one rooted at rootA and one at rootB, both live before returning.
func bootPathSyncEnv(t *testing.T) *pathSyncEnv {
	t.Helper()
	dsn := requireExecutorDB(t)
	g := bootGrantDaemon(t, dsn)

	rootA := t.TempDir()
	rootB := t.TempDir()
	enrollNativeExecutorAt(t, g, rootA, pathSyncRefA)
	enrollNativeExecutorAt(t, g, rootB, pathSyncRefB)
	// Wait for BOTH to be live in the pool, not merely enrolled: enrollment's
	// TouchSeen precedes the pool's live-map insert, and resolve refuses an
	// executor that is not connected.
	g.waitForLiveExecutors(t, 2)
	return &pathSyncEnv{g: g, rootA: rootA, rootB: rootB}
}

// enrollNativeExecutorAt is grantDaemon.enrollExecutor with a caller-supplied
// root and machine label, so the test can put files where the executor will
// read them. The enrollment is native (isolation "none") — the overwrite gate
// under test reads exactly that.
func enrollNativeExecutorAt(t *testing.T, g *grantDaemon, root, machine string) string {
	t.Helper()
	c := assert.NewAborting(t)

	// A label unique to THIS enrollment, so the row is identified by the
	// enrollment that created it rather than by a label shared with stale rows
	// from earlier runs (the shared test database accumulates executor rows).
	marker := "t" + uniqueSuffix()
	token, err := g.store.MintToken(context.Background(), executors.NewToken{
		Labels: map[string]string{
			"machine":  machine,
			"test-run": marker,
		},
		Isolation:     "none",
		WorkspaceMode: "pinned",
		ExpiresAt:     time.Now().Add(time.Hour),
	})
	c.NoError(err, "mint token")

	credFile := filepath.Join(t.TempDir(), "cred")
	// Each executor gets its own TMPDIR: scratchDir() is
	// os.TempDir()/rafiki-sync, and two local executor processes would
	// otherwise share it, making a git bundle relay read and write the same
	// file. TMPDIR must exist before the executor's scratch Mkdir runs.
	tmp := filepath.Join(t.TempDir(), "tmp")
	c.NoError(os.MkdirAll(tmp, 0o700), "mkdir the executor's TMPDIR")

	cmd := exec.Command(cliBinary(), "executor", "serve",
		"--connect", g.listenAddr,
		"--enroll-token", token,
		"--credential-file", credFile,
		"--pin-cert", g.fingerprint,
		"--root", root,
	)
	// The executor's own XDG dirs are isolated too, so nothing it resolves
	// leaks into the developer's machine.
	cmd.Env = append(os.Environ(),
		"TMPDIR="+tmp,
		"XDG_CACHE_HOME="+filepath.Join(tmp, "cache"),
		"XDG_CONFIG_HOME="+filepath.Join(tmp, "config"),
		"XDG_STATE_HOME="+filepath.Join(tmp, "state"),
		"XDG_DATA_HOME="+filepath.Join(tmp, "data"),
	)
	c.NoError(cmd.Start(), "start native executor")
	t.Cleanup(func() {
		_ = cmd.Process.Signal(os.Kill)
		_ = cmd.Wait()
	})

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		execs, _ := g.store.List(context.Background())
		for _, e := range execs {
			if e.Labels["test-run"] == marker && !e.LastSeenAt.IsZero() {
				return e.ID
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("native executor %q never enrolled and became live", machine)
	return ""
}

// runSyncCLI execs the real CLI against d and returns its stdout, stderr and
// exit error. d is only used for the profile's socket path, so a caller may
// pass a daemon that is not listening to prove a verb never dialled.
func runSyncCLI(t *testing.T, d *daemon, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	cmd := cliCmd(t, d, args...)
	var out, errOut bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errOut
	err = cmd.Run()
	return out.String(), errOut.String(), err
}

// ─── helpers for the filesystem and git fixtures ──────────────────────────────

// writeTreeFile writes content to path, creating parents.
func writeTreeFile(t *testing.T, path, content string) {
	t.Helper()
	c := assert.NewAborting(t)
	c.NoError(os.MkdirAll(filepath.Dir(path), 0o755), "mkdir %s", filepath.Dir(path))
	c.NoError(os.WriteFile(path, []byte(content), 0o644), "write %s", path)
}

// readTreeFile reads path and returns its content.
func readTreeFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	assert.NewAborting(t).NoError(err, "read %s", path)
	return string(b)
}

// gitRun runs one git invocation in dir and returns its stdout, failing the
// test on a nonzero exit. A commit identity is supplied through the environment
// so a repository the EXECUTOR created (which carries no local user config) can
// be committed to, and commit signing is forced off so a developer's global
// `commit.gpgsign = true` cannot break the fixture.
func gitRun(t *testing.T, dir string, args ...string) string {
	t.Helper()
	full := append([]string{"-c", "commit.gpgsign=false"}, args...)
	cmd := exec.Command("git", full...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=pathsync-it",
		"GIT_AUTHOR_EMAIL=pathsync-it@example.com",
		"GIT_COMMITTER_NAME=pathsync-it",
		"GIT_COMMITTER_EMAIL=pathsync-it@example.com",
		"GIT_CONFIG_NOSYSTEM=1",
	)
	var out, errOut bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errOut
	assert.NewAborting(t).NoError(cmd.Run(), "git %s in %s: %s",
		strings.Join(args, " "), dir, errOut.String())
	return out.String()
}

// gitRev is gitRun's trimmed stdout for a rev-parse-shaped call.
func gitRev(t *testing.T, dir string, args ...string) string {
	t.Helper()
	return strings.TrimSpace(gitRun(t, dir, args...))
}

// gitInitRepo creates dir as a git repository with branch `feat` checked out
// and an identity configured locally.
func gitInitRepo(t *testing.T, dir string) {
	t.Helper()
	c := assert.NewAborting(t)
	c.NoError(os.MkdirAll(dir, 0o755), "mkdir %s", dir)
	gitRun(t, dir, "init", "-q", "-b", "feat")
	gitRun(t, dir, "config", "user.email", "pathsync-it@example.com")
	gitRun(t, dir, "config", "user.name", "pathsync-it")
}

// gitCommit stages the whole working tree and commits it.
func gitCommit(t *testing.T, dir, message string) {
	t.Helper()
	gitRun(t, dir, "add", "-A")
	gitRun(t, dir, "commit", "-q", "-m", message)
}

// ─── sandbox sync ─────────────────────────────────────────────────────────────

// TestPathSyncCopiesDirectoryAcrossExecutors pins the whole tree-sync path end
// to end: a directory with a nested file, an executable script and a symlink
// travels from executor A's root to executor B's root, and content, mode and
// link target all survive.
func TestPathSyncCopiesDirectoryAcrossExecutors(t *testing.T) {
	t.Parallel()
	c := assert.NewAborting(t)
	env := bootPathSyncEnv(t)

	src := filepath.Join(env.rootA, "src")
	dst := filepath.Join(env.rootB, "dst")
	writeTreeFile(t, filepath.Join(src, "hello.txt"), "hello\n")
	writeTreeFile(t, filepath.Join(src, "sub", "nested.txt"), "nested\n")
	c.NoError(os.MkdirAll(src, 0o755), "mkdir src")
	c.NoError(os.WriteFile(filepath.Join(src, "run.sh"), []byte("#!/bin/sh\necho hi\n"), 0o755), "write run.sh")
	c.NoError(os.Symlink("hello.txt", filepath.Join(src, "link")), "symlink link")

	out, errOut, err := runSyncCLI(t, env.g.daemon, "sandbox", "sync",
		pathSyncRefA+":"+src, pathSyncRefB+":"+dst)
	c.Require().NoError(err, "sandbox sync A->B failed: stderr=%s stdout=%s", errOut, out)
	c.StrContains(out, "copied", "sync table output")

	// Content.
	c.Eq("hello\n", readTreeFile(t, filepath.Join(dst, "hello.txt")), "hello.txt content")
	c.Eq("nested\n", readTreeFile(t, filepath.Join(dst, "sub", "nested.txt")), "sub/nested.txt content")

	// Executable bit, not just the bytes.
	fi, err := os.Lstat(filepath.Join(dst, "run.sh"))
	c.Require().NoError(err, "lstat dst/run.sh")
	c.True(fi.Mode().Perm()&0o111 != 0, "run.sh lost its executable bit: mode %v", fi.Mode())
	c.StrContains(readTreeFile(t, filepath.Join(dst, "run.sh")), "echo hi", "run.sh content")

	// Symlink, verbatim.
	li, err := os.Lstat(filepath.Join(dst, "link"))
	c.Require().NoError(err, "lstat dst/link")
	c.True(li.Mode()&os.ModeSymlink != 0, "dst/link is not a symlink: mode %v", li.Mode())
	target, err := os.Readlink(filepath.Join(dst, "link"))
	c.NoError(err, "readlink dst/link")
	c.Eq("hello.txt", target, "symlink target")
}

// TestPathSyncOverwriteRefusedOnNativeExecutor pins the overwrite gate reading
// the DESTINATION executor's row: both executors are native, so --overwrite is
// refused by the daemon before the destination is touched.
func TestPathSyncOverwriteRefusedOnNativeExecutor(t *testing.T) {
	t.Parallel()
	c := assert.NewAborting(t)
	env := bootPathSyncEnv(t)

	src := filepath.Join(env.rootA, "src")
	dst := filepath.Join(env.rootB, "dst")
	writeTreeFile(t, filepath.Join(src, "hello.txt"), "hello\n")

	// Seed the destination with a plain sync.
	_, errOut, err := runSyncCLI(t, env.g.daemon, "sandbox", "sync",
		pathSyncRefA+":"+src, pathSyncRefB+":"+dst)
	c.Require().NoError(err, "the seeding sync failed: %s", errOut)

	// Change the source and try to replace the destination.
	writeTreeFile(t, filepath.Join(src, "hello.txt"), "changed\n")
	_, errOut, err = runSyncCLI(t, env.g.daemon, "sandbox", "sync",
		pathSyncRefA+":"+src, pathSyncRefB+":"+dst, "--overwrite")
	c.Require().Error(err, "overwrite on a native destination must fail")
	c.StrContains(errOut, "overwrite is only allowed on container executors", "the refusal must name the container-only rule")
	c.Eq("hello\n", readTreeFile(t, filepath.Join(dst, "hello.txt")), "the destination must be unchanged")
}

// TestPathSyncRefusesNonEmptyDestination pins the executor's finalize guard as
// seen from the CLI: a second sync without --overwrite refuses a non-empty
// destination and leaves it untouched.
func TestPathSyncRefusesNonEmptyDestination(t *testing.T) {
	t.Parallel()
	c := assert.NewAborting(t)
	env := bootPathSyncEnv(t)

	src := filepath.Join(env.rootA, "src")
	dst := filepath.Join(env.rootB, "dst")
	writeTreeFile(t, filepath.Join(src, "hello.txt"), "hello\n")

	_, errOut, err := runSyncCLI(t, env.g.daemon, "sandbox", "sync",
		pathSyncRefA+":"+src, pathSyncRefB+":"+dst)
	c.Require().NoError(err, "the seeding sync failed: %s", errOut)

	writeTreeFile(t, filepath.Join(src, "hello.txt"), "changed\n")
	_, errOut, err = runSyncCLI(t, env.g.daemon, "sandbox", "sync",
		pathSyncRefA+":"+src, pathSyncRefB+":"+dst)
	c.Require().Error(err, "a sync onto a non-empty destination without --overwrite must fail")
	c.StrContains(errOut, "destination exists", "the refusal must name the existing destination")
	c.Eq("hello\n", readTreeFile(t, filepath.Join(dst, "hello.txt")), "the destination must be unchanged")
}

// ─── sandbox sync-repo ────────────────────────────────────────────────────────

// TestRepoSyncRoundTripAcrossExecutors drives a branch both ways between two
// executors: seeding a fresh repository, a fast-forward back, a refused
// non-fast-forward and the forced rewrite that accepts it. It also pins that a
// sync never touches the destination's working tree.
func TestRepoSyncRoundTripAcrossExecutors(t *testing.T) {
	t.Parallel()
	c := assert.NewAborting(t)
	env := bootPathSyncEnv(t)

	repoA := filepath.Join(env.rootA, "repo")
	repoB := filepath.Join(env.rootB, "repo")
	gitInitRepo(t, repoA)
	writeTreeFile(t, filepath.Join(repoA, "a.txt"), "a\n")
	gitCommit(t, repoA, "add a")
	writeTreeFile(t, filepath.Join(repoA, "b.txt"), "b\n")
	gitCommit(t, repoA, "add b")
	tipA := gitRev(t, repoA, "rev-parse", "refs/heads/feat")

	// A keeps a DIFFERENT branch checked out, so the reverse sync's fetch is
	// not blocked by the checked-out-branch rule and the working tree can be
	// shown unchanged afterwards.
	gitRun(t, repoA, "checkout", "-q", "-b", "other")
	otherTip := gitRev(t, repoA, "rev-parse", "refs/heads/other")

	// 1. Seed B: the branch is created and checked out there.
	out, errOut, err := runSyncCLI(t, env.g.daemon, "sandbox", "sync-repo",
		pathSyncRefA+":"+repoA, pathSyncRefB+":"+repoB, "feat")
	c.Require().NoError(err, "sync-repo A->B failed: stderr=%s stdout=%s", errOut, out)
	c.StrContains(out, "created repo", "the seeding sync must report a created repository")
	c.Eq("feat", gitRev(t, repoB, "rev-parse", "--abbrev-ref", "HEAD"), "B's checked-out branch")
	c.Eq(tipA, gitRev(t, repoB, "rev-parse", "refs/heads/feat"), "B's feat tip after seeding")
	c.Eq("a\n", readTreeFile(t, filepath.Join(repoB, "a.txt")), "B's working tree was not seeded")

	// 2. Commit in B, then fast-forward A's feat.
	writeTreeFile(t, filepath.Join(repoB, "c.txt"), "c\n")
	gitCommit(t, repoB, "add c")
	tipB := gitRev(t, repoB, "rev-parse", "refs/heads/feat")

	_, errOut, err = runSyncCLI(t, env.g.daemon, "sandbox", "sync-repo",
		pathSyncRefB+":"+repoB, pathSyncRefA+":"+repoA, "feat")
	c.Require().NoError(err, "sync-repo B->A failed: %s", errOut)
	c.Eq(tipB, gitRev(t, repoA, "rev-parse", "refs/heads/feat"), "A's feat was not fast-forwarded")
	// A's working tree is untouched: still on `other`, at its old tip, clean.
	c.Eq("other", gitRev(t, repoA, "rev-parse", "--abbrev-ref", "HEAD"), "A's checked-out branch")
	c.Eq(otherTip, gitRev(t, repoA, "rev-parse", "refs/heads/other"), "A's other branch moved")
	c.Eq("", strings.TrimSpace(gitRun(t, repoA, "status", "--porcelain")), "A's working tree changed")

	// 3. Diverge A's feat with a commit on top of the shared tip. Built with
	// plumbing so `other` stays checked out — a checked-out feat would be
	// refused for the wrong reason.
	tree := gitRev(t, repoA, "rev-parse", "feat^{tree}")
	diverged := gitRev(t, repoA, "commit-tree", tree, "-p", tipB, "-m", "diverged")
	gitRun(t, repoA, "update-ref", "refs/heads/feat", diverged)

	_, errOut, err = runSyncCLI(t, env.g.daemon, "sandbox", "sync-repo",
		pathSyncRefB+":"+repoB, pathSyncRefA+":"+repoA, "feat")
	c.Require().Error(err, "a non-fast-forward update must be refused")
	c.StrContains(errOut, "non-fast-forward", "the refusal must name the non-fast-forward update")
	c.Eq(diverged, gitRev(t, repoA, "rev-parse", "refs/heads/feat"), "A's feat must be untouched by the refused sync")

	// 4. --force accepts the rewrite.
	_, errOut, err = runSyncCLI(t, env.g.daemon, "sandbox", "sync-repo",
		pathSyncRefB+":"+repoB, pathSyncRefA+":"+repoA, "feat", "--force")
	c.Require().NoError(err, "the forced sync failed: %s", errOut)
	c.Eq(tipB, gitRev(t, repoA, "rev-parse", "refs/heads/feat"), "force must reset A's feat to B's tip")
}

// TestRepoSyncRefusesCheckedOutBranch pins git's own refusal to fetch into the
// destination's checked-out branch, surfaced through the daemon and the CLI.
func TestRepoSyncRefusesCheckedOutBranch(t *testing.T) {
	t.Parallel()
	c := assert.NewAborting(t)
	env := bootPathSyncEnv(t)

	repoA := filepath.Join(env.rootA, "repo")
	repoB := filepath.Join(env.rootB, "repo")
	gitInitRepo(t, repoA)
	writeTreeFile(t, filepath.Join(repoA, "a.txt"), "a\n")
	gitCommit(t, repoA, "add a")

	// Seed B, which checks `feat` out there.
	_, errOut, err := runSyncCLI(t, env.g.daemon, "sandbox", "sync-repo",
		pathSyncRefA+":"+repoA, pathSyncRefB+":"+repoB, "feat")
	c.Require().NoError(err, "the seeding sync failed: %s", errOut)
	c.Eq("feat", gitRev(t, repoB, "rev-parse", "--abbrev-ref", "HEAD"), "B's checked-out branch")
	seededTip := gitRev(t, repoB, "rev-parse", "refs/heads/feat")

	// Advance A so the next sync has something to fetch.
	writeTreeFile(t, filepath.Join(repoA, "b.txt"), "b\n")
	gitCommit(t, repoA, "add b")

	_, errOut, err = runSyncCLI(t, env.g.daemon, "sandbox", "sync-repo",
		pathSyncRefA+":"+repoA, pathSyncRefB+":"+repoB, "feat")
	c.Require().Error(err, "a sync into a checked-out branch must fail")
	c.StrContains(errOut, "checked out", "the refusal must name the checked-out branch")
	c.Eq(seededTip, gitRev(t, repoB, "rev-parse", "refs/heads/feat"), "B's feat must be untouched")
}

// TestSyncVerbsRejectMalformedEndpoints pins the client-side endpoint parser:
// a malformed <executor>:<path> is refused before any round trip. The profile
// names a socket nothing is listening on, so a verb that dialled first would
// fail with a connection error instead of the parse refusal.
func TestSyncVerbsRejectMalformedEndpoints(t *testing.T) {
	t.Parallel()

	dead := &daemon{socketPath: filepath.Join(t.TempDir(), "not-a-daemon.sock")}
	cases := []struct {
		name string
		args []string
	}{
		{"sync source without a colon", []string{"sandbox", "sync", "noColon", "B:/x"}},
		{"sync destination without a colon", []string{"sandbox", "sync", "A:/x", "noColon"}},
		{"sync source with a relative path", []string{"sandbox", "sync", "host:relative", "B:/x"}},
		{"sync-repo source without a colon", []string{"sandbox", "sync-repo", "noColon", "B:/x", "feat"}},
		{"sync-repo destination with a relative path", []string{"sandbox", "sync-repo", "A:/x", "host:relative", "feat"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := assert.NewCollecting(t)
			_, errOut, err := runSyncCLI(t, dead, tc.args...)
			c.Require().Error(err, "%v exited 0, want a refusal", tc.args)
			c.StrContains(errOut, "want <executor>:<absolute path>", "the refusal must name the wanted shape")
		})
	}
}
