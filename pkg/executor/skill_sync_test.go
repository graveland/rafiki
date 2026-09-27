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
