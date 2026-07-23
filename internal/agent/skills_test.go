package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeSkill creates <dir>/<name>/SKILL.md with the given frontmatter
// name/description and body.
func writeSkill(t *testing.T, dir, name, fmName, fmDescription, body string) {
	t.Helper()
	skillDir := filepath.Join(dir, name)
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	content := "---\nname: " + fmName + "\ndescription: " + fmDescription + "\n---\n" + body
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestDiscoverSkillsLaterDirOverridesEarlier is the brief's named scenario: a
// project-level skills dir shadows a user-level one carrying the same skill
// name, since callers build the dir list user-level first, project-level
// second.
func TestDiscoverSkillsLaterDirOverridesEarlier(t *testing.T) {
	userDir := t.TempDir()
	projectDir := t.TempDir()

	writeSkill(t, userDir, "reviewer", "reviewer", "USER_LEVEL description", "USER_LEVEL body")
	writeSkill(t, projectDir, "reviewer", "reviewer", "PROJECT_LEVEL description", "PROJECT_LEVEL body")

	skills, err := DiscoverSkills([]string{userDir, projectDir}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(skills) != 1 {
		t.Fatalf("expected exactly one skill after override, got %d: %+v", len(skills), skills)
	}
	got := skills[0]
	if got.Name != "reviewer" {
		t.Fatalf("expected name %q, got %q", "reviewer", got.Name)
	}
	if got.Description != "PROJECT_LEVEL description" {
		t.Fatalf("expected project-level description to win, got %q", got.Description)
	}
	if got.Dir != filepath.Join(projectDir, "reviewer") {
		t.Fatalf("expected project-level dir to win, got %q", got.Dir)
	}
}

// TestDiscoverSkillsFindsMultipleAcrossDirs covers the non-colliding path: two
// distinct skills across two dirs both surface, sorted by name.
func TestDiscoverSkillsFindsMultipleAcrossDirs(t *testing.T) {
	userDir := t.TempDir()
	projectDir := t.TempDir()

	writeSkill(t, userDir, "zeta", "zeta", "zeta description", "zeta body")
	writeSkill(t, projectDir, "alpha", "alpha", "alpha description", "alpha body")

	skills, err := DiscoverSkills([]string{userDir, projectDir}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(skills) != 2 {
		t.Fatalf("expected 2 skills, got %d: %+v", len(skills), skills)
	}
	if skills[0].Name != "alpha" || skills[1].Name != "zeta" {
		t.Fatalf("expected sorted [alpha, zeta], got [%s, %s]", skills[0].Name, skills[1].Name)
	}
}

// TestDiscoverSkillsOnlyFilter asserts the only filter drops unlisted skills.
func TestDiscoverSkillsOnlyFilter(t *testing.T) {
	dir := t.TempDir()
	writeSkill(t, dir, "alpha", "alpha", "alpha description", "alpha body")
	writeSkill(t, dir, "beta", "beta", "beta description", "beta body")
	writeSkill(t, dir, "gamma", "gamma", "gamma description", "gamma body")

	skills, err := DiscoverSkills([]string{dir}, []string{"beta"})
	if err != nil {
		t.Fatal(err)
	}
	if len(skills) != 1 {
		t.Fatalf("expected exactly one skill after only filter, got %d: %+v", len(skills), skills)
	}
	if skills[0].Name != "beta" {
		t.Fatalf("expected beta to survive the only filter, got %q", skills[0].Name)
	}
}

// TestDiscoverSkillsNilOnlyMeansAll asserts that a nil only filter (as
// opposed to an empty-but-non-nil slice) returns everything.
func TestDiscoverSkillsNilOnlyMeansAll(t *testing.T) {
	dir := t.TempDir()
	writeSkill(t, dir, "alpha", "alpha", "alpha description", "alpha body")
	writeSkill(t, dir, "beta", "beta", "beta description", "beta body")

	skills, err := DiscoverSkills([]string{dir}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(skills) != 2 {
		t.Fatalf("expected 2 skills with nil only filter, got %d", len(skills))
	}
}

// TestDiscoverSkillsSkipsMalformedFrontmatterWithoutFailing covers the
// brief's "never fatal" requirement: a skill dir with unparseable
// frontmatter is skipped, but a sibling well-formed skill still surfaces and
// DiscoverSkills returns no error.
func TestDiscoverSkillsSkipsMalformedFrontmatterWithoutFailing(t *testing.T) {
	dir := t.TempDir()
	writeSkill(t, dir, "good", "good", "good description", "good body")

	badDir := filepath.Join(dir, "bad")
	if err := os.MkdirAll(badDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// No closing "---" delimiter - malformed frontmatter.
	if err := os.WriteFile(filepath.Join(badDir, "SKILL.md"), []byte("---\nname: bad\ndescription: [unterminated\nno closing delimiter here\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	skills, err := DiscoverSkills([]string{dir}, nil)
	if err != nil {
		t.Fatalf("expected malformed frontmatter to be skipped, not returned as an error: %v", err)
	}
	if len(skills) != 1 || skills[0].Name != "good" {
		t.Fatalf("expected only the well-formed skill to survive, got %+v", skills)
	}
}

// TestDiscoverSkillsMissingDirIsNotFatal: a dir in the list that doesn't
// exist on disk (the common case for an optional ~/.claude/skills that was
// never created) must not fail discovery.
func TestDiscoverSkillsMissingDirIsNotFatal(t *testing.T) {
	dir := t.TempDir()
	writeSkill(t, dir, "alpha", "alpha", "alpha description", "alpha body")

	missing := filepath.Join(dir, "does-not-exist")
	skills, err := DiscoverSkills([]string{missing, dir}, nil)
	if err != nil {
		t.Fatalf("expected a missing dir to be skipped, not fatal: %v", err)
	}
	if len(skills) != 1 || skills[0].Name != "alpha" {
		t.Fatalf("expected the alpha skill from the present dir, got %+v", skills)
	}
}

// TestSkillsInventoryRendering covers the "- name: description" line format
// consumed by BuildSystemPrompt's SkillsInventory section.
func TestSkillsInventoryRendering(t *testing.T) {
	skills := []SkillMeta{
		{Name: "alpha", Description: "does alpha things"},
		{Name: "beta", Description: "does beta things"},
	}
	got := SkillsInventory(skills)
	want := "- alpha: does alpha things\n- beta: does beta things"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// TestSkillsInventoryEmpty asserts an empty skill list renders as an empty
// string, so BuildSystemPrompt's "omit empty sections" rule has nothing to
// trip over.
func TestSkillsInventoryEmpty(t *testing.T) {
	if got := SkillsInventory(nil); got != "" {
		t.Fatalf("expected empty string for no skills, got %q", got)
	}
}

// TestSkillBodyStripsFrontmatter covers the helper the skill tool uses to
// load a skill's content at invocation time: the YAML frontmatter block must
// not appear in the returned body.
func TestSkillBodyStripsFrontmatter(t *testing.T) {
	dir := t.TempDir()
	writeSkill(t, dir, "alpha", "alpha", "alpha description", "ALPHA_BODY_MARKER\nmore body text\n")

	body, err := SkillBody(filepath.Join(dir, "alpha", "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(body, "---") || strings.Contains(body, "description:") {
		t.Fatalf("expected frontmatter stripped from body, got %q", body)
	}
	if !strings.Contains(body, "ALPHA_BODY_MARKER") {
		t.Fatalf("expected body content preserved, got %q", body)
	}
}
