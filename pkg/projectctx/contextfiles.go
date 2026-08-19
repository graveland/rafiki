// Package projectctx loads the instruction files that belong to a WORKSPACE
// rather than to whoever runs the agent loop.
//
// The split exists because the workspace is frequently on a different machine.
// CLAUDE.md at a git root is a fact about the checkout; the user's global
// instructions file is a fact about the operator. Reading both on the daemon
// was correct only while they were the same host, and produced silence rather
// than an error once they were not — the path simply did not exist.
//
// A top-level package rather than one under pkg/fundi because pkg/executor
// imports it and must link no database driver, while pkg/fundi links eleven
// pgx packages.
package projectctx

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// includeLine matches a line that is nothing but an @-include reference, per
// the brief: ^@(\S+)$.
var includeLine = regexp.MustCompile(`^@(\S+)$`)

// maxIncludeDepth caps @-include recursion. It is a backstop independent of
// cycle detection: a long, non-cyclic include chain must still terminate.
const maxIncludeDepth = 5

// missingIncludeMarker is what the loader substitutes for an @-include
// line whose target is absent, cyclic, or beyond the depth cap. Deliberately
// a plain string (not an error): a missing include must never fail context
// assembly.
func missingIncludeMarker(ref string) string {
	return fmt.Sprintf("[missing include: %s]", ref)
}

// LoadProjectContext returns the concatenated project-tier instruction-file
// content for cwd: CLAUDE.md and AGENTS.md at the git root, then the same at
// cwd itself — deduped when the git root and cwd are the same directory.
// Files that don't exist are skipped silently (most directories have neither);
// the returned sections are joined with a blank line between them.
//
// The user's global instructions file is deliberately NOT part of this: it
// belongs to whoever runs the agent loop, which is a different machine from
// the workspace.
func LoadProjectContext(cwd string) (string, error) {
	absCwd, err := filepath.Abs(cwd)
	if err != nil {
		return "", fmt.Errorf("projectctx: resolve cwd %q: %w", cwd, err)
	}

	root := findGitRoot(absCwd)

	var dirs []string
	if root != "" {
		dirs = append(dirs, root)
	}
	if root == "" || !sameDir(root, absCwd) {
		dirs = append(dirs, absCwd)
	}

	var sections []string
	for _, dir := range dirs {
		for _, name := range []string{"CLAUDE.md", "AGENTS.md"} {
			if s := LoadInstructionFile(filepath.Join(dir, name)); s != "" {
				sections = append(sections, s)
			}
		}
	}

	return strings.Join(sections, "\n\n"), nil
}

// findGitRoot walks upward from cwd (which must already be absolute) looking
// for a directory containing a .git entry - a directory for a normal repo, a
// file for a worktree or submodule, so a plain existence check covers both.
// Returns "" when no repo is found; LoadProjectContext then falls back to
// loading only cwd's own instruction files. It cannot fail: os.Stat errors
// (missing entry, permission denied) both just mean "keep walking up".
func findGitRoot(absCwd string) string {
	dir := absCwd
	for {
		if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

// sameDir reports whether a and b (both absolute) name the same directory,
// resolving symlinks first - macOS aliases /tmp to /private/tmp, so a naive
// string comparison would fail to dedup a git root and cwd that are actually
// identical.
func sameDir(a, b string) bool {
	ra, errA := filepath.EvalSymlinks(a)
	rb, errB := filepath.EvalSymlinks(b)
	if errA != nil || errB != nil {
		return a == b
	}
	return ra == rb
}

// LoadInstructionFile reads path and expands its @-includes, returning "" if
// path does not exist (the normal case: most directories have no
// CLAUDE.md/AGENTS.md) or if it could not be read for any other reason (that
// case is logged, not silently dropped).
//
// Exported because the user's global instructions file (a fact about the
// daemon's host, not the workspace) needs the same include expansion, and
// duplicating the include machinery across two packages is the cross-boundary
// drift this split exists to prevent.
func LoadInstructionFile(path string) string {
	if _, err := os.Stat(path); err != nil {
		if !os.IsNotExist(err) {
			slog.Warn("failed to stat instruction file", "path", path, "error", err)
		}
		return ""
	}
	content, err := expandIncludes(path, 0, map[string]bool{})
	if err != nil {
		slog.Warn("failed to load instruction file", "path", path, "error", err)
		return ""
	}
	return content
}

// expandIncludes reads path and inlines every @-include line it contains,
// recursively. depth is the nesting level of path itself (0 for a top-level
// instruction file); ancestors holds the absolute paths currently being
// expanded on this call stack, so a true cycle (a -> b -> a) is caught
// immediately rather than only via the depth cap - and so a diamond (two
// branches independently including the same leaf) is still allowed, since
// ancestors is popped on return from each branch.
func expandIncludes(path string, depth int, ancestors map[string]bool) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", path, err)
	}

	baseDir := filepath.Dir(path)
	lines := strings.Split(string(data), "\n")
	for i, line := range lines {
		m := includeLine.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		lines[i] = resolveInclude(m[1], baseDir, depth, ancestors)
	}
	return strings.Join(lines, "\n"), nil
}

// resolveInclude resolves one @-include reference found in a file at nesting
// depth (that file's own depth), relative to that file's directory baseDir.
// It never returns an error: any failure - missing target, depth cap,
// cycle - becomes the literal missing-include marker so one bad reference
// can't fail the whole load.
func resolveInclude(ref, baseDir string, depth int, ancestors map[string]bool) string {
	target := ref
	if !filepath.IsAbs(target) {
		target = filepath.Join(baseDir, target)
	}
	absTarget, err := filepath.Abs(target)
	if err != nil {
		slog.Warn("could not resolve include path", "include", ref, "error", err)
		return missingIncludeMarker(ref)
	}

	if depth >= maxIncludeDepth {
		slog.Warn("include depth cap reached, not expanding further", "include", ref, "depth", depth)
		return missingIncludeMarker(ref)
	}
	if ancestors[absTarget] {
		slog.Warn("include cycle detected", "include", ref)
		return missingIncludeMarker(ref)
	}
	if _, err := os.Stat(absTarget); err != nil {
		if !os.IsNotExist(err) {
			slog.Warn("failed to stat include target", "include", ref, "path", absTarget, "error", err)
		}
		return missingIncludeMarker(ref)
	}

	ancestors[absTarget] = true
	content, err := expandIncludes(absTarget, depth+1, ancestors)
	delete(ancestors, absTarget)
	if err != nil {
		slog.Warn("failed to load include", "include", ref, "error", err)
		return missingIncludeMarker(ref)
	}
	return content
}
