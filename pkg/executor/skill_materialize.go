// SPDX-License-Identifier: Apache-2.0

package executor

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"connectrpc.com/connect"

	executorpb "go.graveland.dev/rafiki/pkg/executorpb"
	"go.graveland.dev/rafiki/pkg/paths"
	"go.graveland.dev/rafiki/pkg/skills"
)

// skillCacheRoot is where materialized skill trees live:
// <cache>/skill-cache, a sibling cache root to the pymodule blob and repo
// caches. Cache and not Data: every tree here is reconstructible from the
// daemon's store, so losing the directory costs a re-materialization rather
// than a fact.
func skillCacheRoot() string {
	return filepath.Join(paths.CacheDir(), "skill-cache")
}

// skillCacheMaxAge is how long an unused skill tree survives before a
// materialization sweeps it. A package var so a test can shrink it rather than
// fabricate week-old mtimes for every case.
var skillCacheMaxAge = 7 * 24 * time.Hour

// skillStagingPrefix marks a tree being written. Same idiom as the namespace
// sync: a tree is staged under a temp name and renamed into place, so a
// concurrent reader never observes a half-written directory.
const skillStagingPrefix = ".rafiki-staging-"

// treeHashPattern is what a tree_hash must look like before it becomes a path
// segment. Anchored, lowercase-only: skills.TreeHash emits hex.EncodeToString,
// and anything that is not exactly that shape is refused rather than cleaned,
// because a value this close to a filesystem path is not a place to be clever.
var treeHashPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// ensureSkillCacheRoot establishes the skill cache root the same way
// ensureGitReposRoot establishes the repo cache root: only ever write into a
// directory rafiki marked, or one that does not exist yet. Checked BEFORE
// MkdirAll, because creating the directory first would make the absent case
// unrecognisable -- an empty directory is not a managed one. A root that
// exists without the marker (or is not a directory at all) is refused, never
// adopted: materialize and the sweep both RemoveAll beneath this root.
func ensureSkillCacheRoot(root string) error {
	if err := assertManagedOrAbsent(root); err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(root, managedMarker)); !os.IsNotExist(err) {
		return nil // exists and is already marked ours
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return fmt.Errorf("create skill-cache dir: %w", err)
	}
	if err := os.WriteFile(filepath.Join(root, managedMarker), []byte("skill-cache\n"), 0o644); err != nil {
		return fmt.Errorf("mark skill-cache dir: %w", err)
	}
	return nil
}

// MaterializeSkill writes one skill's supporting files into a
// content-addressed cache directory on this executor and returns its path.
//
// Addressed by tree_hash so that the executor never has to be trusted about
// WHICH bytes it wrote: it recomputes the hash from the bytes it was sent and
// refuses a mismatch, which makes the returned directory name a proof of its
// own contents.
//
// No opt-in flag: unlike SyncSkills, this writes only under the executor's own
// disposable cache, never into anything an operator owns.
func (s *Server) MaterializeSkill(
	_ context.Context,
	req *connect.Request[executorpb.MaterializeSkillRequest],
) (*connect.Response[executorpb.MaterializeSkillResponse], error) {
	hash := req.Msg.GetTreeHash()
	if !treeHashPattern.MatchString(hash) {
		return nil, connect.NewError(connect.CodeInvalidArgument,
			fmt.Errorf("tree_hash %q is not 64 lowercase hex characters", hash))
	}
	files := skillFiles(req.Msg.GetFiles())
	if err := skills.ValidateFiles(files); err != nil {
		return nil, connect.NewError(connect.CodeInvalidArgument, err)
	}
	if got := skills.TreeHash(files); got != hash {
		return nil, connect.NewError(connect.CodeInvalidArgument,
			fmt.Errorf("tree_hash %q does not match the %d file(s) sent, which hash to %q", hash, len(files), got))
	}

	root := skillCacheRoot()
	if err := ensureSkillCacheRoot(root); err != nil {
		return nil, connect.NewError(connect.CodeFailedPrecondition, err)
	}

	dir := filepath.Join(root, hash)
	now := time.Now()
	_, statErr := os.Stat(dir)
	switch {
	case statErr == nil:
		// Already materialized. Bump the mtime rather than rewrite: the tree
		// is content-addressed, so it is either the tree asked for or a
		// corrupt one, and rewriting on every call would make a warm cache as
		// expensive as a cold one.
		if err := os.Chtimes(dir, now, now); err != nil {
			slog.Warn("executor: could not refresh materialized skill mtime", "dir", dir, "error", err)
		}
	case !errors.Is(statErr, os.ErrNotExist):
		return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("stat %s: %w", dir, statErr))
	default:
		if err := materializeTree(root, dir, req.Msg.GetFiles()); err != nil {
			return nil, connect.NewError(connect.CodeInternal, err)
		}
	}

	// Sweeping AFTER the write, so the tree just asked for is never a
	// candidate; sweep failures are logged and never fail the call, because
	// an eviction that cannot happen costs disk, not correctness.
	sweepSkillCache(root, dir, now)

	return connect.NewResponse(&executorpb.MaterializeSkillResponse{Dir: dir}), nil
}

// materializeTree stages the tree under a temp name in root and renames it
// into place, so a concurrent reader never sees a half-written directory. A
// rename that loses to a concurrent materialization of the same hash is
// success: the target is content-addressed, so whatever landed is
// byte-identical to what this call would have written.
func materializeTree(root, dir string, files []*executorpb.SkillFile) error {
	staged, err := os.MkdirTemp(root, skillStagingPrefix+"*")
	if err != nil {
		return fmt.Errorf("stage skill tree: %w", err)
	}
	defer os.RemoveAll(staged) // no-op once the rename has moved it

	if err := writeSkillFiles(staged, files); err != nil {
		return err
	}
	if err := os.Rename(staged, dir); err != nil {
		if _, statErr := os.Stat(dir); statErr == nil {
			return nil
		}
		return fmt.Errorf("publish skill tree: %w", err)
	}
	return nil
}

// sweepSkillCache removes trees and abandoned staging directories older than
// skillCacheMaxAge, keeping keep. Errors are logged, never returned: a sweep
// is housekeeping, and a call that successfully materialized a tree must not
// fail because some unrelated directory could not be removed.
func sweepSkillCache(root, keep string, now time.Time) {
	entries, err := os.ReadDir(root)
	if err != nil {
		slog.Warn("executor: could not read the skill cache to sweep it", "dir", root, "error", err)
		return
	}
	for _, e := range entries {
		name := e.Name()
		path := filepath.Join(root, name)
		switch {
		case e.IsDir() && treeHashPattern.MatchString(name):
			if path == keep {
				continue
			}
		case strings.HasPrefix(name, skillStagingPrefix):
			// A staging dir this old belongs to a call that died mid-write.
		default:
			continue
		}
		info, err := e.Info()
		if err != nil {
			slog.Warn("executor: could not stat a skill cache entry", "path", path, "error", err)
			continue
		}
		if now.Sub(info.ModTime()) < skillCacheMaxAge {
			continue
		}
		if err := os.RemoveAll(path); err != nil {
			slog.Warn("executor: could not remove a stale skill cache entry", "path", path, "error", err)
		}
	}
}
