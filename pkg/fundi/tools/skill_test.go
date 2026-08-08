package tools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	skillspkg "go.graveland.dev/rafiki/pkg/skills"
)

// writeSkill creates <dir>/<name>/SKILL.md with the given frontmatter and
// body, mirroring the layout DiscoverSkills expects.
func writeSkill(t *testing.T, dir, name, description, body string) skillspkg.SkillMeta {
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
	return skillspkg.SkillMeta{Name: name, Description: description, Dir: skillDir, Path: path}
}

// TestSkillToolReturnsBodyAndBaseDir covers the tool's success shape: the
// base-dir line, then the SKILL.md body with frontmatter stripped.
func TestSkillToolReturnsBodyAndBaseDir(t *testing.T) {
	dir := t.TempDir()
	meta := writeSkill(t, dir, "reviewer", "reviews code", "REVIEWER_BODY_MARKER\nstep one\n")

	r := NewRegistry()
	skillT, _ := (&SkillBlueprint{}).Materialize(ToolOpts{Skills: []skillspkg.SkillMeta{meta}})
	r.Register(skillT)

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
	skillT, _ := (&SkillBlueprint{}).Materialize(ToolOpts{Skills: []skillspkg.SkillMeta{m1, m2}})
	r.Register(skillT)

	out, err := r.Execute(context.Background(), "skill", json.RawMessage(`{"skill":"nonexistent"}`))
	if err == nil {
		t.Fatalf("expected an error for an unknown skill, got output %q", out)
	}
	if !strings.Contains(err.Error(), "alpha") || !strings.Contains(err.Error(), "beta") {
		t.Fatalf("expected error to list available skill names, got %v", err)
	}
}

// TestSkillToolRegistersUnderName asserts the tool is registered as "skill"
// and appears in Definitions(). It must materialize with a non-empty skill
// set: Materialize declines (returns a nil Tool) for an empty one, which
// TestSkillBlueprintDeclinesWithoutSkills covers.
func TestSkillToolRegistersUnderName(t *testing.T) {
	r := NewRegistry()
	meta := writeSkill(t, t.TempDir(), "reviewer", "reviews code", "body\n")
	skillT, _ := (&SkillBlueprint{}).Materialize(ToolOpts{Skills: []skillspkg.SkillMeta{meta}})
	r.Register(skillT)

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

// TestSkillBlueprintDeclinesWithoutSkills pins the contract that keeps a
// useless skill tool out of the model's tools[]: with no skills discovered
// (also how --no-skills arrives here), Materialize returns a nil Tool and a
// nil error rather than a tool whose every call answers "unknown skill".
func TestSkillBlueprintDeclinesWithoutSkills(t *testing.T) {
	for _, tc := range []struct {
		name   string
		skills []skillspkg.SkillMeta
	}{
		{"nil", nil},
		{"empty", []skillspkg.SkillMeta{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := (&SkillBlueprint{}).Materialize(ToolOpts{Skills: tc.skills})
			if err != nil {
				t.Fatalf("Materialize returned an error: %v", err)
			}
			if got != nil {
				t.Fatalf("Materialize returned a tool %q, want nil (declined)", got.Name())
			}
		})
	}
}
