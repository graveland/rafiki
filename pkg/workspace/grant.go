// Package workspace derives a child's container grant from its worktree
// assignment. The daemon calls Derive and chooses the mount SHAPE — which
// paths are read-only, where they land in the container, what the workdir is —
// from a cwd and a mode, and nothing model-facing composes a mount list.
//
// That is narrower than "nothing model-facing contributes a path", which this
// comment used to claim and which is not true: agent_spawn exposes a `cwd`
// parameter ("Absolute working directory. Omit to use your own"), it reaches
// Derive unchecked through SpawnSpec.Cwd, and Derive turns it into the
// read-write /work mount. A coordinator that names cwd="/" gets the host root
// bind-mounted read-write.
//
// Recorded rather than fixed here because the fix is a containment check
// against the SPAWNER's cwd at admission — the same shape as the executor
// selector's intersection — and belongs with the caller that knows the
// lineage, not in a pure derivation function. Tracked in tasks/todo.md as B4.
package workspace

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
)

// Mode describes how the workspace is constructed.
type Mode string

const (
	ModeEphemeral Mode = "ephemeral"
	ModePinned    Mode = "pinned"
)

// Grant is a child's workspace, expressed as mounts. The daemon derives it;
// nothing model-facing contributes a path.
type Grant struct {
	Mounts  []Mount
	Workdir string
	Network string
}

// Mount is one bind mount.
type Mount struct {
	HostPath      string
	ContainerPath string
	ReadOnly      bool
}

// Derive builds the grant for a child working in cwd.
func Derive(cwd string, mode Mode) (Grant, error) {
	cwd, err := filepath.Abs(cwd)
	if err != nil {
		return Grant{}, fmt.Errorf("derive: cannot resolve cwd: %w", err)
	}
	if resolved, symErr := filepath.EvalSymlinks(cwd); symErr == nil {
		cwd = resolved // best effort; non-fatal when it fails
	}

	repo := RepoRoot(cwd)

	// When cwd is itself the repo (plain checkout), single mount.
	// Use resolved paths to avoid macOS /var vs /private/var drift.
	if repo != "" && samePath(cwd, repo) {
		return Grant{
			Mounts: []Mount{
				{HostPath: cwd, ContainerPath: "/work", ReadOnly: false},
			},
			Workdir: "/work",
			Network: "none",
			// Mode is not exposed on the Grant — the grant is the same
			// regardless; ephemeral/pinned is the executor's concern.
		}, nil
	}

	// Worktree: two mounts. /work (RW, the worktree) and /repo (RO, the main repo).
	mounts := []Mount{
		{HostPath: cwd, ContainerPath: "/work", ReadOnly: false},
	}
	if repo != "" {
		mounts = append(mounts, Mount{HostPath: repo, ContainerPath: "/repo", ReadOnly: true})
	}

	return Grant{
		Mounts:  mounts,
		Workdir: "/work",
		Network: "none",
	}, nil
}

// RepoRoot returns the enclosing git repository's main working tree, or ""
// when cwd is not in a repository.
//
// Uses --git-common-dir, not --show-toplevel, because inside a linked worktree
// --show-toplevel returns the worktree, which makes every worktree its own
// repo and turns the read-only /repo mount into a duplicate of /work —
// silently removing the read-only half of the grant. That distinction is the
// entire reason migration 0013's comment says "repo_root groups worktrees of
// one repo; cwd alone does not."
func RepoRoot(cwd string) string {
	if _, err := exec.LookPath("git"); err != nil {
		return ""
	}

	// --git-common-dir returns the .git directory. For the main working tree
	// it's <repo>/.git; for a linked worktree it's <repo>/.git/worktrees/<name>.
	// In both cases, the parent of that directory is the repo root.
	out, err := exec.Command("git", "-C", cwd,
		"rev-parse", "--path-format=absolute", "--git-common-dir").Output()
	if err != nil {
		return ""
	}

	gitDir := strings.TrimSpace(string(out))
	// --git-common-dir points to the .git directory (or the worktree's gitdir).
	// The repo root is its parent.
	repo := filepath.Dir(gitDir)

	// Inside a linked worktree, --git-common-dir returns
	// <repo>/.git/worktrees/<name>. Go up to the repo root.
	if strings.HasSuffix(filepath.Dir(repo), "worktrees") {
		repo = filepath.Dir(filepath.Dir(repo))
	}

	// Resolve symlinks so macOS /var vs /private/var paths match.
	if resolved, err := filepath.EvalSymlinks(repo); err == nil {
		repo = resolved
	}

	return repo
}

// samePath returns true when a and b refer to the same directory, accounting
// for macOS symlinks (/var → /private/var).
func samePath(a, b string) bool {
	if a == b {
		return true
	}
	ra, errA := filepath.EvalSymlinks(a)
	rb, errB := filepath.EvalSymlinks(b)
	if errA != nil || errB != nil {
		return false
	}
	return ra == rb
}
