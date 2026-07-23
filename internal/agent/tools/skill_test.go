package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"git.graveland.dev/brent/fundi/internal/agent"
)

// writeSkill creates <dir>/<name>/SKILL.md with the given frontmatter and
// body, mirroring the layout DiscoverSkills expects.
func writeSkill(t *testing.T, dir, name, description, body string) agent.SkillMeta {
	t.Helper()
	skillDir := filepath.Join(dir, name)
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(skillDir, "SKILL.md")
	content := "---\nname: " + name + "\ndescription: " + description + "\n---\n" + body
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return agent.SkillMeta{Name: name, Description: description, Dir: skillDir, Path: path}
}

// TestSkillToolReturnsBodyAndBaseDir covers the tool's success shape: the
// base-dir line, then the SKILL.md body with frontmatter stripped.
func TestSkillToolReturnsBodyAndBaseDir(t *testing.T) {
	dir := t.TempDir()
	meta := writeSkill(t, dir, "reviewer", "reviews code", "REVIEWER_BODY_MARKER\nstep one\n")

	r := NewRegistry()
	RegisterSkillTool(r, []agent.SkillMeta{meta})

	out, err := r.Execute(context.Background(), "skill", json.RawMessage(`{"skill":"reviewer"}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	wantPrefix := "Base directory for this skill: " + meta.Dir + "\n\n"
	if !strings.HasPrefix(out, wantPrefix) {
		t.Fatalf("expected output to start with %q, got %q", wantPrefix, out)
	}
	if !strings.Contains(out, "REVIEWER_BODY_MARKER") {
		t.Fatalf("expected body content in output, got %q", out)
	}
	if strings.Contains(out, "description: reviews code") {
		t.Fatalf("expected frontmatter stripped from output, got %q", out)
	}
}

// TestSkillToolUnknownNameListsAvailable covers the required-recovery path:
// an unknown skill name is a tool error (is_error result) whose message
// lists the available names so the model can self-correct.
func TestSkillToolUnknownNameListsAvailable(t *testing.T) {
	dir := t.TempDir()
	m1 := writeSkill(t, dir, "alpha", "alpha desc", "alpha body")
	m2 := writeSkill(t, dir, "beta", "beta desc", "beta body")

	r := NewRegistry()
	RegisterSkillTool(r, []agent.SkillMeta{m1, m2})

	out, err := r.Execute(context.Background(), "skill", json.RawMessage(`{"skill":"nonexistent"}`))
	if err == nil {
		t.Fatalf("expected an error for an unknown skill, got output %q", out)
	}
	if !strings.Contains(err.Error(), "alpha") || !strings.Contains(err.Error(), "beta") {
		t.Fatalf("expected error to list available skill names, got %v", err)
	}
}

// TestSkillToolRegistersUnderName asserts the tool is registered as "skill"
// and appears in Definitions().
func TestSkillToolRegistersUnderName(t *testing.T) {
	r := NewRegistry()
	RegisterSkillTool(r, nil)

	found := false
	for _, def := range r.Definitions() {
		if def.OfTool != nil && def.OfTool.Name == "skill" {
			found = true
		}
	}
	if !found {
		t.Fatal("expected a \"skill\" tool to be registered")
	}
}
