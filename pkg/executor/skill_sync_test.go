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
	if err != nil {
		t.Fatalf("SyncSkills: %v", err)
	}
	return resp.Msg
}

func TestSyncWritesASkillsDirPlugin(t *testing.T) {
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
	if err != nil {
		t.Fatalf("SKILL.md missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(skillsDir, "rafiki", "coordinating")); !os.IsNotExist(err) {
		t.Errorf("bare skill dir at the plugin root; Claude Code would not discover it (err=%v)", err)
	}
	got := string(body)
	if !strings.HasPrefix(got, "---\n") {
		t.Errorf("SKILL.md has no frontmatter block:\n%s", got)
	}
	if !strings.Contains(got, "name: coordinating") {
		t.Errorf("frontmatter missing name:\n%s", got)
	}
	if !strings.Contains(got, "the body") {
		t.Errorf("body missing:\n%s", got)
	}
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
	if _, err := os.Stat(filepath.Join(skillsDir, "rafiki", "skills", "a", "SKILL.md")); err != nil {
		t.Errorf("rafiki namespace was collaterally damaged: %v", err)
	}
}

// THE safety test. A directory rafiki never wrote is the operator's, and no
// sync may touch it — not to replace it, not to prune it.
func TestSyncNeverTouchesAnUnmanagedDirectory(t *testing.T) {
	s, skillsDir := syncServer(t)

	operator := filepath.Join(skillsDir, "my-own-skill")
	if err := os.MkdirAll(operator, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(operator, "SKILL.md"), []byte("mine"), 0o644); err != nil {
		t.Fatal(err)
	}

	syncOnce(t, s, &executorpb.SyncSkillsRequest{
		Namespaces: []*executorpb.SkillNamespace{
			{Name: "rafiki", Skills: []*executorpb.SyncSkill{{Name: "a", Body: "x"}}},
		},
	})

	got, err := os.ReadFile(filepath.Join(operator, "SKILL.md"))
	if err != nil || string(got) != "mine" {
		t.Fatalf("the operator's own skill was damaged: content=%q err=%v", got, err)
	}
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
			s, _ := syncServer(t)
			_, err := s.SyncSkills(context.Background(), connect.NewRequest(tc.req))
			if err == nil {
				t.Fatal("accepted a name that can escape the skills directory")
			}
			if connect.CodeOf(err) != connect.CodeInvalidArgument {
				t.Errorf("got code %v, want InvalidArgument", connect.CodeOf(err))
			}
		})
	}
}

// Byte-stability: re-syncing identical content must not rewrite files, or a
// converged fleet churns Claude Code's file watching forever.
func TestResyncingIdenticalContentDoesNotRewrite(t *testing.T) {
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
	if err != nil {
		t.Fatal(err)
	}

	resp := syncOnce(t, s, req)
	second, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if !first.ModTime().Equal(second.ModTime()) {
		t.Error("identical corpus rewrote SKILL.md; the sync is not byte-stable")
	}
	if resp.GetWritten() != 0 {
		t.Errorf("written=%d, want 0 for an unchanged corpus", resp.GetWritten())
	}
}

// An empty namespace list is a full removal, which is a legitimate but
// destructive instruction — it must still only remove rafiki-managed trees.
func TestEmptyNamespaceListRemovesOnlyManagedTrees(t *testing.T) {
	s, skillsDir := syncServer(t)
	syncOnce(t, s, &executorpb.SyncSkillsRequest{
		Namespaces: []*executorpb.SkillNamespace{
			{Name: "rafiki", Skills: []*executorpb.SyncSkill{{Name: "a", Body: "x"}}},
		},
	})
	operator := filepath.Join(skillsDir, "hand-written")
	if err := os.MkdirAll(operator, 0o755); err != nil {
		t.Fatal(err)
	}

	syncOnce(t, s, &executorpb.SyncSkillsRequest{})

	if _, err := os.Stat(filepath.Join(skillsDir, "rafiki")); !os.IsNotExist(err) {
		t.Error("managed tree survived an empty sync")
	}
	if _, err := os.Stat(operator); err != nil {
		t.Errorf("unmanaged dir removed by an empty sync: %v", err)
	}
}

func TestSyncRefusedWhenNotEnabled(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", t.TempDir())
	s := &Server{opts: Options{}}
	_, err := s.SyncSkills(context.Background(), connect.NewRequest(&executorpb.SyncSkillsRequest{}))
	if connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Errorf("got %v, want PermissionDenied", connect.CodeOf(err))
	}
}
