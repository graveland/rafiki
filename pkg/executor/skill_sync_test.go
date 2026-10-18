// SPDX-License-Identifier: Apache-2.0

package executor

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"connectrpc.com/connect"

	executorpb "go.graveland.dev/rafiki/pkg/executorpb"

	"github.com/multigres/testkit/assert"
)

// syncServer builds a Server whose skills reconcile targets a temp dir, by
// pointing CLAUDE_CONFIG_DIR at it. That indirection is the real mechanism, not
// a test seam: the executor resolves the same variable the child will.
func syncServer(t *testing.T) (*Server, string) {
	t.Helper()
	cfg := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", cfg)
	s := &Server{opts: Options{SkillsSync: true}}
	return s, filepath.Join(cfg, "skills")
}

func syncOnce(t *testing.T, s *Server, req *executorpb.SyncSkillsRequest) *executorpb.SyncSkillsResponse {
	t.Helper()
	resp, err := s.SyncSkills(context.Background(), connect.NewRequest(req))
	assert.NewAborting(t).NoError(err, "SyncSkills")
	return resp.Msg
}

func TestSyncWritesASkillsDirPlugin(t *testing.T) {
	c := assert.NewCollecting(t)
	s, skillsDir := syncServer(t)

	syncOnce(t, s, &executorpb.SyncSkillsRequest{
		Namespaces: []*executorpb.SkillNamespace{{
			Name: "rafiki", Version: "1.2.3",
			Skills: []*executorpb.SyncSkill{
				{Name: "coordinating", Description: "how to coordinate", Body: "the body\n"},
			},
		}},
	})

	// The plugin manifest is what makes a nested directory legal: without it
	// Claude Code's discovery is flat and would ignore the whole tree.
	manifest := filepath.Join(skillsDir, "rafiki", ".claude-plugin", "plugin.json")
	if _, err := os.Stat(manifest); err != nil {
		t.Fatalf("plugin.json missing: %v", err)
	}

	body, err := os.ReadFile(filepath.Join(skillsDir, "rafiki", "skills", "coordinating", "SKILL.md"))
	c.Require().NoError(err, "SKILL.md missing")
	if _, err := os.Stat(filepath.Join(skillsDir, "rafiki", "coordinating")); !os.IsNotExist(err) {
		t.Errorf("bare skill dir at the plugin root; Claude Code would not discover it (err=%v)", err)
	}
	got := string(body)
	if !strings.HasPrefix(got, "---\n") {
		t.Errorf("SKILL.md has no frontmatter block:\n%s", got)
	}
	c.StrContains(got, "name: coordinating", "frontmatter missing name:\n")
	c.StrContains(got, "the body", "body missing:\n")
}

// A namespace that leaves the corpus takes its whole directory with it.
func TestSyncPrunesANamespaceThatLeftTheCorpus(t *testing.T) {
	s, skillsDir := syncServer(t)

	syncOnce(t, s, &executorpb.SyncSkillsRequest{
		Namespaces: []*executorpb.SkillNamespace{
			{Name: "rafiki", Skills: []*executorpb.SyncSkill{{Name: "a", Body: "x"}}},
			{Name: "pg", Skills: []*executorpb.SyncSkill{{Name: "b", Body: "y"}}},
		},
	})
	syncOnce(t, s, &executorpb.SyncSkillsRequest{
		Namespaces: []*executorpb.SkillNamespace{
			{Name: "rafiki", Skills: []*executorpb.SyncSkill{{Name: "a", Body: "x"}}},
		},
	})

	if _, err := os.Stat(filepath.Join(skillsDir, "pg")); !os.IsNotExist(err) {
		t.Errorf("pg namespace survived a sync that omitted it (err=%v)", err)
	}
	_, err := os.Stat(filepath.Join(skillsDir, "rafiki", "skills", "a", "SKILL.md"))
	assert.NewCollecting(t).NoError(err, "rafiki namespace was collaterally damaged")
}

// THE safety test. A directory rafiki never wrote is the operator's, and no
// sync may touch it — not to replace it, not to prune it.
func TestSyncNeverTouchesAnUnmanagedDirectory(t *testing.T) {
	c := assert.NewAborting(t)
	s, skillsDir := syncServer(t)

	operator := filepath.Join(skillsDir, "my-own-skill")
	c.NoError(os.MkdirAll(operator, 0o755))
	c.NoError(os.WriteFile(filepath.Join(operator, "SKILL.md"), []byte("mine"), 0o644))

	syncOnce(t, s, &executorpb.SyncSkillsRequest{
		Namespaces: []*executorpb.SkillNamespace{
			{Name: "rafiki", Skills: []*executorpb.SyncSkill{{Name: "a", Body: "x"}}},
		},
	})

	got, err := os.ReadFile(filepath.Join(operator, "SKILL.md"))
	c.False(err != nil || string(got) != "mine", "the operator's own skill was damaged: content=%q err=%v", got, err)
}

// A name is a directory name. Anything that could escape the skills dir must be
// refused before a single byte is written.
func TestSyncRejectsPathEscapingNames(t *testing.T) {
	for _, tc := range []struct {
		label string
		req   *executorpb.SyncSkillsRequest
	}{
		{"namespace traversal", &executorpb.SyncSkillsRequest{
			Namespaces: []*executorpb.SkillNamespace{{Name: "../evil",
				Skills: []*executorpb.SyncSkill{{Name: "a", Body: "x"}}}}}},
		{"namespace separator", &executorpb.SyncSkillsRequest{
			Namespaces: []*executorpb.SkillNamespace{{Name: "a/b",
				Skills: []*executorpb.SyncSkill{{Name: "a", Body: "x"}}}}}},
		{"skill traversal", &executorpb.SyncSkillsRequest{
			Namespaces: []*executorpb.SkillNamespace{{Name: "rafiki",
				Skills: []*executorpb.SyncSkill{{Name: "../../a", Body: "x"}}}}}},
		{"dot namespace", &executorpb.SyncSkillsRequest{
			Namespaces: []*executorpb.SkillNamespace{{Name: ".",
				Skills: []*executorpb.SyncSkill{{Name: "a", Body: "x"}}}}}},
	} {
		t.Run(tc.label, func(t *testing.T) {
			c := assert.NewCollecting(t)
			s, _ := syncServer(t)
			_, err := s.SyncSkills(context.Background(), connect.NewRequest(tc.req))
			c.Require().Error(err, "accepted a name that can escape the skills directory")
			c.Eq(connect.CodeInvalidArgument, connect.CodeOf(err), "got code")
		})
	}
}

// Byte-stability: re-syncing identical content must not rewrite files, or a
// converged fleet churns Claude Code's file watching forever.
func TestResyncingIdenticalContentDoesNotRewrite(t *testing.T) {
	c := assert.NewCollecting(t)
	s, skillsDir := syncServer(t)
	req := &executorpb.SyncSkillsRequest{
		Namespaces: []*executorpb.SkillNamespace{{
			Name: "rafiki", Version: "1.0.0",
			Skills: []*executorpb.SyncSkill{{Name: "a", Description: "d", Body: "b"}},
		}},
	}

	syncOnce(t, s, req)
	p := filepath.Join(skillsDir, "rafiki", "skills", "a", "SKILL.md")
	first, err := os.Stat(p)
	c.Require().NoError(err)

	resp := syncOnce(t, s, req)
	second, err := os.Stat(p)
	c.Require().NoError(err)
	c.True(first.ModTime().Equal(second.ModTime()), "identical corpus rewrote SKILL.md; the sync is not byte-stable")
	c.Eq(0, resp.GetWritten(), "written")
}

// An empty namespace list is a full removal, which is a legitimate but
// destructive instruction — it must still only remove rafiki-managed trees.
func TestEmptyNamespaceListRemovesOnlyManagedTrees(t *testing.T) {
	c := assert.NewCollecting(t)
	s, skillsDir := syncServer(t)
	syncOnce(t, s, &executorpb.SyncSkillsRequest{
		Namespaces: []*executorpb.SkillNamespace{
			{Name: "rafiki", Skills: []*executorpb.SyncSkill{{Name: "a", Body: "x"}}},
		},
	})
	operator := filepath.Join(skillsDir, "hand-written")
	c.Require().NoError(os.MkdirAll(operator, 0o755))

	syncOnce(t, s, &executorpb.SyncSkillsRequest{})

	if _, err := os.Stat(filepath.Join(skillsDir, "rafiki")); !os.IsNotExist(err) {
		t.Error("managed tree survived an empty sync")
	}
	_, err := os.Stat(operator)
	c.NoError(err, "unmanaged dir removed by an empty sync")
}

func TestSyncRefusedWhenNotEnabled(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	s := &Server{opts: Options{}}
	_, err := s.SyncSkills(context.Background(), connect.NewRequest(&executorpb.SyncSkillsRequest{}))
	assert.NewCollecting(t).Eq(connect.CodePermissionDenied, connect.CodeOf(err), "got")
}

// syncWithFiles drives one sync of a single "rafiki" namespace holding one
// skill, and returns the response so a test can read the change counters.
func syncWithFiles(t *testing.T, s *Server, files []*executorpb.SkillFile) *executorpb.SyncSkillsResponse {
	t.Helper()
	return syncOnce(t, s, &executorpb.SyncSkillsRequest{
		Namespaces: []*executorpb.SkillNamespace{{
			Name: "rafiki",
			Skills: []*executorpb.SyncSkill{{
				Name: "supporting", Description: "has files", Body: "the body\n", Files: files,
			}},
		}},
	})
}

func skillDirOf(t *testing.T, skillsDir string) string {
	t.Helper()
	return filepath.Join(skillsDir, "rafiki", "skills", "supporting")
}

// A skill's supporting files land beside SKILL.md with their modes intact: a
// reference document is readable and a script is executable, and Claude Code
// runs what it finds there.
func TestSyncSkillsWritesFiles(t *testing.T) {
	c := assert.NewAborting(t)
	s, skillsDir := syncServer(t)

	syncWithFiles(t, s, []*executorpb.SkillFile{
		{Path: "references/api.md", Mode: 0o644, Content: []byte("the api\n")},
		{Path: "run.sh", Mode: 0o755, Content: []byte("#!/bin/sh\n")},
	})

	dir := skillDirOf(t, skillsDir)
	doc, err := os.ReadFile(filepath.Join(dir, "references", "api.md"))
	c.NoError(err, "nested reference file")
	c.Eq("the api\n", string(doc), "reference content")

	script := filepath.Join(dir, "run.sh")
	info, err := os.Stat(script)
	c.NoError(err, "script file")
	c.Eq(os.FileMode(0o755), info.Mode().Perm(), "script mode")

	// SKILL.md itself must still be there: writing the file set must not have
	// replaced the skill's own document.
	_, err = os.Stat(filepath.Join(dir, "SKILL.md"))
	c.NoError(err, "SKILL.md")
}

// A traversal path refuses the WHOLE sync before anything is written: the set
// is otherwise valid, so only the path rule can be what refuses it.
func TestSyncSkillsRefusesTraversal(t *testing.T) {
	c := assert.NewAborting(t)
	s, skillsDir := syncServer(t)

	// The skills dir exists BEFORE the call, so "nothing was written under it"
	// is an observation about this sync rather than about a directory the sync
	// never got as far as creating.
	c.NoError(os.MkdirAll(skillsDir, 0o755), "create the skills dir")

	_, err := s.SyncSkills(context.Background(), connect.NewRequest(&executorpb.SyncSkillsRequest{
		Namespaces: []*executorpb.SkillNamespace{{
			Name: "rafiki",
			Skills: []*executorpb.SyncSkill{{
				Name: "supporting", Description: "has files", Body: "x",
				Files: []*executorpb.SkillFile{
					{Path: "references/api.md", Mode: 0o644, Content: []byte("fine")},
					{Path: "../x", Mode: 0o644, Content: []byte("escape")},
				},
			}},
		}},
	}))
	c.Eq(connect.CodeInvalidArgument, connect.CodeOf(err), "code")
	c.StrContains(err.Error(), "rafiki:supporting", "error must name namespace:skill")
	c.StrContains(err.Error(), `has a ".." segment`, "must be refused by the path rule")

	// Nothing under the skills dir: not the namespace tree, not a staging dir,
	// not the traversal target one level up.
	entries, err := os.ReadDir(skillsDir)
	c.NoError(err, "read skills dir")
	c.Eq(0, len(entries), "the refused sync wrote under the skills dir: %v", entries)
	_, statErr := os.Stat(filepath.Join(filepath.Dir(skillsDir), "x"))
	c.True(os.IsNotExist(statErr), "the traversal target was written (err=%v)", statErr)
}

// A mode-only change is a change: the mode is part of the skill's identity
// (skills.TreeHash covers it) and decides whether Claude Code will execute a
// file, so a converged sync that ignored it would leave the wrong mode on disk
// forever.
func TestSyncSkillsModeChangeIsChange(t *testing.T) {
	c := assert.NewAborting(t)
	s, skillsDir := syncServer(t)

	before := []*executorpb.SkillFile{
		{Path: "run.sh", Mode: 0o644, Content: []byte("#!/bin/sh\n")},
	}
	c.Eq(int32(1), syncWithFiles(t, s, before).GetWritten(), "first sync writes")
	c.Eq(int32(0), syncWithFiles(t, s, before).GetWritten(), "an identical set writes nothing")

	after := []*executorpb.SkillFile{
		{Path: "run.sh", Mode: 0o755, Content: []byte("#!/bin/sh\n")},
	}
	c.Eq(int32(1), syncWithFiles(t, s, after).GetWritten(), "a mode-only change is a change")

	info, err := os.Stat(filepath.Join(skillDirOf(t, skillsDir), "run.sh"))
	c.NoError(err, "stat run.sh")
	c.Eq(os.FileMode(0o755), info.Mode().Perm(), "the file was rewritten with the new mode")
}

// A file added, removed or edited is a change: the convergence check compares
// the whole tree, not just SKILL.md.
func TestSyncSkillsFileChangeIsChange(t *testing.T) {
	c := assert.NewAborting(t)
	s, skillsDir := syncServer(t)

	base := []*executorpb.SkillFile{
		{Path: "references/api.md", Mode: 0o644, Content: []byte("one\n")},
	}
	c.Eq(int32(1), syncWithFiles(t, s, base).GetWritten(), "first sync writes")
	c.Eq(int32(0), syncWithFiles(t, s, base).GetWritten(), "identical file set writes nothing")

	added := append(append([]*executorpb.SkillFile{}, base...),
		&executorpb.SkillFile{Path: "run.sh", Mode: 0o755, Content: []byte("#!/bin/sh\n")})
	c.Eq(int32(1), syncWithFiles(t, s, added).GetWritten(), "an added file is a change")

	edited := []*executorpb.SkillFile{
		{Path: "references/api.md", Mode: 0o644, Content: []byte("two\n")},
		{Path: "run.sh", Mode: 0o755, Content: []byte("#!/bin/sh\n")},
	}
	c.Eq(int32(1), syncWithFiles(t, s, edited).GetWritten(), "an edited file is a change")

	removed := []*executorpb.SkillFile{
		{Path: "references/api.md", Mode: 0o644, Content: []byte("two\n")},
	}
	c.Eq(int32(1), syncWithFiles(t, s, removed).GetWritten(), "a removed file is a change")

	// The removal is real, not just counted.
	_, err := os.Stat(filepath.Join(skillDirOf(t, skillsDir), "run.sh"))
	c.True(os.IsNotExist(err), "run.sh survived its removal (err=%v)", err)
}
