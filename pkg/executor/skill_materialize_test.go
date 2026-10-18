// SPDX-License-Identifier: Apache-2.0

package executor

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"connectrpc.com/connect"

	executorpb "go.graveland.dev/rafiki/pkg/executorpb"
	"go.graveland.dev/rafiki/pkg/skills"

	"github.com/multigres/testkit/assert"
)

// materializeServer builds a Server whose skill cache lands under a temp
// directory, by pointing XDG_CACHE_HOME at it. That indirection is the real
// mechanism, not a test seam: paths.CacheDir() resolves the same variable in
// production.
func materializeServer(t *testing.T) *Server {
	t.Helper()
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	return &Server{}
}

func pbSkillFiles(files []skills.File) []*executorpb.SkillFile {
	out := make([]*executorpb.SkillFile, 0, len(files))
	for _, f := range files {
		out = append(out, &executorpb.SkillFile{Path: f.Path, Mode: f.Mode, Content: f.Content})
	}
	return out
}

func materializeOnce(t *testing.T, s *Server, hash string, files []skills.File) *executorpb.MaterializeSkillResponse {
	t.Helper()
	resp, err := s.MaterializeSkill(context.Background(), connect.NewRequest(
		&executorpb.MaterializeSkillRequest{TreeHash: hash, Files: pbSkillFiles(files)}))
	assert.NewAborting(t).NoError(err, "MaterializeSkill")
	return resp.Msg
}

func sampleSkillFiles() []skills.File {
	return []skills.File{
		{Path: "references/api.md", Mode: 0o644, Content: []byte("the api\n")},
		{Path: "run.sh", Mode: 0o755, Content: []byte("#!/bin/sh\necho hi\n")},
	}
}

func TestMaterializeSkillWritesTree(t *testing.T) {
	c := assert.NewAborting(t)
	s := materializeServer(t)
	files := sampleSkillFiles()
	hash := skills.TreeHash(files)

	resp := materializeOnce(t, s, hash, files)

	c.Eq(filepath.Join(skillCacheRoot(), hash), resp.GetDir(), "dir")
	c.Eq(hash, filepath.Base(resp.GetDir()), "the directory is named by its content address")

	doc, err := os.ReadFile(filepath.Join(resp.GetDir(), "references", "api.md"))
	c.NoError(err, "nested reference file")
	c.Eq("the api\n", string(doc), "reference content")

	info, err := os.Stat(filepath.Join(resp.GetDir(), "run.sh"))
	c.NoError(err, "script file")
	c.Eq(os.FileMode(0o755), info.Mode().Perm(), "script mode")

	// Nothing may be left behind: a staging directory that survived a
	// successful call would be swept a week later, not never.
	entries, err := os.ReadDir(skillCacheRoot())
	c.NoError(err, "read skill cache")
	for _, e := range entries {
		c.True(e.Name() == hash || e.Name() == managedMarker,
			"unexpected entry %q left in the skill cache", e.Name())
	}
}

// A second call for the same tree returns the same directory and does not
// rewrite it -- but does bump its mtime, which is what keeps the sweep from
// evicting a tree the daemon is still using.
func TestMaterializeSkillIdempotent(t *testing.T) {
	c := assert.NewAborting(t)
	s := materializeServer(t)
	files := sampleSkillFiles()
	hash := skills.TreeHash(files)

	first := materializeOnce(t, s, hash, files)

	old := time.Now().Add(-time.Hour).Truncate(time.Second)
	c.NoError(os.Chtimes(first.GetDir(), old, old), "age the materialized tree")

	second := materializeOnce(t, s, hash, files)

	c.Eq(first.GetDir(), second.GetDir(), "a second call returns the same dir")
	info, err := os.Stat(second.GetDir())
	c.NoError(err, "stat")
	c.True(info.ModTime().After(old), "mtime was not advanced (still %s)", info.ModTime())

	// The content is still intact: a bump, not a rewrite of the wrong bytes.
	doc, err := os.ReadFile(filepath.Join(second.GetDir(), "references", "api.md"))
	c.NoError(err, "read reference file")
	c.Eq("the api\n", string(doc), "reference content")
}

func TestMaterializeSkillHashMismatch(t *testing.T) {
	c := assert.NewAborting(t)
	s := materializeServer(t)
	files := sampleSkillFiles()

	// A well-formed hash of DIFFERENT bytes: the executor must recompute, not
	// trust the caller.
	other := skills.TreeHash([]skills.File{
		{Path: "references/api.md", Mode: 0o644, Content: []byte("something else\n")},
	})

	_, err := s.MaterializeSkill(context.Background(), connect.NewRequest(
		&executorpb.MaterializeSkillRequest{TreeHash: other, Files: pbSkillFiles(files)}))
	c.Eq(connect.CodeInvalidArgument, connect.CodeOf(err), "code")
	c.StrContains(err.Error(), other, "error must name the hash the caller sent")

	if _, statErr := os.Stat(filepath.Join(skillCacheRoot(), other)); !os.IsNotExist(statErr) {
		t.Errorf("a refused materialization wrote a tree (err=%v)", statErr)
	}
}

// tree_hash becomes a path segment, so anything that is not exactly a
// lowercase hex sha256 is refused before it reaches the filesystem.
func TestMaterializeSkillRefusesBadHash(t *testing.T) {
	c := assert.NewAborting(t)
	s := materializeServer(t)
	files := sampleSkillFiles()

	for _, hash := range []string{
		"../../etc",
		"",
		"../../../../../../../../../../tmp/pwned",
		"0123456789ABCDEF0123456789abcdef0123456789abcdef0123456789abcdef", // uppercase
		"0123456789abcdef", // too short
	} {
		_, err := s.MaterializeSkill(context.Background(), connect.NewRequest(
			&executorpb.MaterializeSkillRequest{TreeHash: hash, Files: pbSkillFiles(files)}))
		c.Eq(connect.CodeInvalidArgument, connect.CodeOf(err), "code for tree_hash %q", hash)
	}

	// Nothing escaped: the cache root was never even created.
	if _, err := os.Stat(skillCacheRoot()); !os.IsNotExist(err) {
		t.Errorf("a refused tree_hash created the cache root (err=%v)", err)
	}
}

// The sweep removes stale trees and keeps fresh ones -- including the tree the
// call just materialized, which is never a candidate.
func TestMaterializeSkillSweepsOld(t *testing.T) {
	c := assert.NewAborting(t)
	s := materializeServer(t)
	files := sampleSkillFiles()
	hash := skills.TreeHash(files)

	// Establish the root through a real materialization, so the marker and the
	// kept tree are exactly what production produces.
	kept := materializeOnce(t, s, hash, files)

	root := skillCacheRoot()
	oldHash := "1111111111111111111111111111111111111111111111111111111111111111"
	freshHash := "2222222222222222222222222222222222222222222222222222222222222222"
	oldDir := filepath.Join(root, oldHash)
	freshDir := filepath.Join(root, freshHash)
	c.NoError(os.MkdirAll(oldDir, 0o755), "mkdir old")
	c.NoError(os.MkdirAll(freshDir, 0o755), "mkdir fresh")

	stale := time.Now().Add(-skillCacheMaxAge - time.Hour)
	c.NoError(os.Chtimes(oldDir, stale, stale), "age the stale tree")
	c.NoError(os.Chtimes(freshDir, time.Now(), time.Now()), "date the fresh tree")

	staged := filepath.Join(root, skillStagingPrefix+"abandoned")
	c.NoError(os.MkdirAll(staged, 0o755), "mkdir abandoned staging dir")
	c.NoError(os.Chtimes(staged, stale, stale), "age the abandoned staging dir")

	// A second call sweeps.
	materializeOnce(t, s, hash, files)

	if _, err := os.Stat(oldDir); !os.IsNotExist(err) {
		t.Errorf("stale tree survived the sweep (err=%v)", err)
	}
	if _, err := os.Stat(staged); !os.IsNotExist(err) {
		t.Errorf("abandoned staging dir survived the sweep (err=%v)", err)
	}
	if _, err := os.Stat(freshDir); err != nil {
		t.Errorf("fresh tree was swept: %v", err)
	}
	if _, err := os.Stat(kept.GetDir()); err != nil {
		t.Errorf("the tree just materialized was swept: %v", err)
	}
}

// An existing cache root rafiki did not mark is the operator's: refuse it
// rather than materialize into it.
func TestMaterializeSkillRefusesUnmanagedRoot(t *testing.T) {
	c := assert.NewAborting(t)
	s := materializeServer(t)
	files := sampleSkillFiles()
	hash := skills.TreeHash(files)

	root := skillCacheRoot()
	c.NoError(os.MkdirAll(root, 0o755), "create an unmarked root")
	operatorFile := filepath.Join(root, "keep-me.txt")
	c.NoError(os.WriteFile(operatorFile, []byte("operator's\n"), 0o644), "seed the unmarked root")

	_, err := s.MaterializeSkill(context.Background(), connect.NewRequest(
		&executorpb.MaterializeSkillRequest{TreeHash: hash, Files: pbSkillFiles(files)}))
	c.Eq(connect.CodeFailedPrecondition, connect.CodeOf(err), "code")
	c.StrContains(err.Error(), "not rafiki-managed", "error must say why")

	if _, statErr := os.Stat(operatorFile); statErr != nil {
		t.Errorf("the operator's file was touched: %v", statErr)
	}
	if _, statErr := os.Stat(filepath.Join(root, hash)); !os.IsNotExist(statErr) {
		t.Errorf("a refused materialization wrote a tree (err=%v)", statErr)
	}
}

// A bad file path is refused by VALIDATION even when tree_hash is exactly the
// hash of the bad set: a self-consistent content address says nothing about
// whether the paths can be written, so the hash check must not be what a
// traversal attempt trips over.
func TestMaterializeSkillRefusesBadFilePath(t *testing.T) {
	c := assert.NewAborting(t)
	s := materializeServer(t)

	files := []skills.File{
		{Path: "references/api.md", Mode: 0o644, Content: []byte("fine\n")},
		{Path: "../escape.md", Mode: 0o644, Content: []byte("out\n")},
	}
	// Self-consistent: the caller hashed the bad set it is sending.
	hash := skills.TreeHash(files)

	_, err := s.MaterializeSkill(context.Background(), connect.NewRequest(
		&executorpb.MaterializeSkillRequest{TreeHash: hash, Files: pbSkillFiles(files)}))
	c.Eq(connect.CodeInvalidArgument, connect.CodeOf(err), "code")
	c.StrContains(err.Error(), `has a ".." segment`, "must be refused by the path rule, not the hash check")
	c.StrContains(err.Error(), "../escape.md", "error must name the offending path")

	if _, statErr := os.Stat(skillCacheRoot()); !os.IsNotExist(statErr) {
		t.Errorf("a refused file set created the cache root (err=%v)", statErr)
	}
}
