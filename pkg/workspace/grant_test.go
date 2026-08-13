package workspace

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// helpers: create a git repo and optionally a linked worktree.

func gitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	run(t, dir, "git", "init", "-b", "main")
	run(t, dir, "git", "config", "user.email", "test@test")
	run(t, dir, "git", "config", "user.name", "Test")
	// Need at least one commit for worktree operations.
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("# test"), 0o644); err != nil {
		t.Fatal(err)
	}
	run(t, dir, "git", "add", "README.md")
	run(t, dir, "git", "commit", "-m", "init")
	return dir
}

func gitRepoWithWorktree(t *testing.T) (repo, worktree string) {
	t.Helper()
	repo = gitRepo(t)
	wt := filepath.Join(t.TempDir(), "wt")
	run(t, repo, "git", "worktree", "add", wt)
	return repo, wt
}

func run(t *testing.T, dir string, argv ...string) {
	t.Helper()
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%v: %s", argv, out)
	}
}

func resolveHost(path string) string {
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		return resolved
	}
	return path
}

// The shape the design specifies: the worktree read-write at /work, the repo
// read-only at /repo. Two mounts, both derived, neither authored.
func TestDeriveMountsAWorktreeRWAndItsRepoRO(t *testing.T) {
	repo, worktree := gitRepoWithWorktree(t)
	g, err := Derive(worktree, ModeEphemeral)
	if err != nil {
		t.Fatal(err)
	}
	if g.Workdir != "/work" {
		t.Errorf("workdir = %q, want /work", g.Workdir)
	}
	byPath := map[string]Mount{}
	for _, m := range g.Mounts {
		byPath[m.ContainerPath] = m
	}
	if m := byPath["/work"]; resolveHost(m.HostPath) != resolveHost(worktree) || m.ReadOnly {
		t.Errorf("/work must be the worktree, read-write: %+v", m)
	}
	if m := byPath["/repo"]; resolveHost(m.HostPath) != resolveHost(repo) || !m.ReadOnly {
		t.Errorf("/repo must be the repo, READ-ONLY: %+v", m)
	}
}

// A plain directory in a repo (not a worktree) mounts once. Mounting the same
// path twice at different container paths gives the child two names for one
// file and a read-only alias it can trivially bypass through the other.
func TestDeriveDoesNotDoubleMountTheSamePath(t *testing.T) {
	repo := gitRepo(t)
	g, err := Derive(repo, ModeEphemeral)
	if err != nil {
		t.Fatal(err)
	}
	if len(g.Mounts) != 1 {
		t.Fatalf("want 1 mount for a plain repo checkout, got %+v", g.Mounts)
	}
	if g.Mounts[0].ReadOnly {
		t.Error("the child's own checkout must be writable")
	}
}

// Outside a repository there is no repo mount to derive — just the directory.
func TestDeriveOutsideAGitRepo(t *testing.T) {
	dir := t.TempDir()
	g, err := Derive(dir, ModeEphemeral)
	if err != nil {
		t.Fatal(err)
	}
	if len(g.Mounts) != 1 || resolveHost(g.Mounts[0].HostPath) != resolveHost(dir) {
		t.Fatalf("got %+v", g.Mounts)
	}
}

// Network defaults to none. An unattended worker that does not need egress
// should not have it; a build that needs a package registry is a deliberate
// choice, not an accident of the default.
func TestDeriveDefaultsToNoNetwork(t *testing.T) {
	g, _ := Derive(t.TempDir(), ModeEphemeral)
	if g.Network != "none" {
		t.Fatalf("network = %q, want none", g.Network)
	}
}

// Nothing model-facing reaches this function. Assert the signature carries no
// path from a request — this is a compile-time property, so the test is a
// reminder in the only place someone would break it.
func TestDeriveTakesNoCallerSuppliedMounts(t *testing.T) {
	// Derive(cwd string, mode Mode) — if a third parameter ever appears
	// carrying paths from a SpawnRequest, this comment is where to argue
	// about it. A coordinator composing path allowlists makes subtle mistakes
	// a coordinator choosing labels and a workspace mode cannot.
}

// RepoRoot must use --git-common-dir, never --show-toplevel. Inside a linked
// worktree --show-toplevel returns the worktree, which makes every worktree
// its own repo and turns the read-only /repo mount into a duplicate of /work.
func TestRepoRootReturnsMainWorktree(t *testing.T) {
	repo, worktree := gitRepoWithWorktree(t)
	// RepoRoot returns the path git sees, which on macOS may be /private/var/…
	// while TempDir returns /var/…. Resolve both.
	expected := repo
	if resolved, err := filepath.EvalSymlinks(repo); err == nil {
		expected = resolved
	}
	got := RepoRoot(worktree)
	if got != expected {
		t.Errorf("RepoRoot(%q) = %q, want %q (the main repo, not the worktree)", worktree, got, expected)
	}
}
