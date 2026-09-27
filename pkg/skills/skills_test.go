package skills

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/multigres/testkit/assert"
)

// writeSkill creates <dir>/<name>/SKILL.md with the given frontmatter
// name/description and body.
func writeSkill(t *testing.T, dir, name, fmName, fmDescription, body string) {
	t.Helper()
	c := assert.NewAborting(t)
	skillDir := filepath.Join(dir, name)
	c.NoError(os.MkdirAll(skillDir, 0o755))
	content := "---\nname: " + fmName + "\ndescription: " + fmDescription + "\n---\n" + body
	c.NoError(os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(content), 0o644))
}

// TestDiscoverSkillsLaterDirOverridesEarlier is the brief's named scenario: a
// project-level skills dir shadows a user-level one carrying the same skill
// name, since callers build the dir list user-level first, project-level
// second.
func TestDiscoverSkillsLaterDirOverridesEarlier(t *testing.T) {
	c := assert.NewAborting(t)
	userDir := t.TempDir()
	projectDir := t.TempDir()

	writeSkill(t, userDir, "reviewer", "reviewer", "USER_LEVEL description", "USER_LEVEL body")
	writeSkill(t, projectDir, "reviewer", "reviewer", "PROJECT_LEVEL description", "PROJECT_LEVEL body")

	skills, err := DiscoverSkills([]string{userDir, projectDir}, nil)
	c.NoError(err)
	c.Len(skills, 1, "expected exactly one skill after override, got %d", len(skills))
	got := skills[0]
	c.Eq("reviewer", got.Name, "expected name")
	c.Eq("PROJECT_LEVEL description", got.Description, "expected project-level description to win, got")
	c.Eq(filepath.Join(projectDir, "reviewer"), got.Dir, "expected project-level dir to win, got")
}

// TestDiscoverSkillsFindsMultipleAcrossDirs covers the non-colliding path: two
// distinct skills across two dirs both surface, sorted by name.
func TestDiscoverSkillsFindsMultipleAcrossDirs(t *testing.T) {
	c := assert.NewAborting(t)
	userDir := t.TempDir()
	projectDir := t.TempDir()

	writeSkill(t, userDir, "zeta", "zeta", "zeta description", "zeta body")
	writeSkill(t, projectDir, "alpha", "alpha", "alpha description", "alpha body")

	skills, err := DiscoverSkills([]string{userDir, projectDir}, nil)
	c.NoError(err)
	c.Len(skills, 2, "expected 2 skills, got %d", len(skills))
	if skills[0].Name != "alpha" || skills[1].Name != "zeta" {
		t.Fatalf("expected sorted [alpha, zeta], got [%s, %s]", skills[0].Name, skills[1].Name)
	}
}

// TestDiscoverSkillsOnlyFilter asserts the only filter drops unlisted skills.
func TestDiscoverSkillsOnlyFilter(t *testing.T) {
	c := assert.NewAborting(t)
	dir := t.TempDir()
	writeSkill(t, dir, "alpha", "alpha", "alpha description", "alpha body")
	writeSkill(t, dir, "beta", "beta", "beta description", "beta body")
	writeSkill(t, dir, "gamma", "gamma", "gamma description", "gamma body")

	skills, err := DiscoverSkills([]string{dir}, []string{"beta"})
	c.NoError(err)
	c.Len(skills, 1, "expected exactly one skill after only filter, got %d", len(skills))
	c.Eq("beta", skills[0].Name, "expected beta to survive the only filter, got")
}

// TestDiscoverSkillsNilOnlyMeansAll asserts that a nil only filter (as
// opposed to an empty-but-non-nil slice) returns everything.
func TestDiscoverSkillsNilOnlyMeansAll(t *testing.T) {
	c := assert.NewAborting(t)
	dir := t.TempDir()
	writeSkill(t, dir, "alpha", "alpha", "alpha description", "alpha body")
	writeSkill(t, dir, "beta", "beta", "beta description", "beta body")

	skills, err := DiscoverSkills([]string{dir}, nil)
	c.NoError(err)
	c.Len(skills, 2, "expected 2 skills with nil only filter, got %d", len(skills))
}

// TestDiscoverSkillsSkipsMalformedFrontmatterWithoutFailing covers the
// brief's "never fatal" requirement: a skill dir with unparseable
// frontmatter is skipped, but a sibling well-formed skill still surfaces and
// DiscoverSkills returns no error.
func TestDiscoverSkillsSkipsMalformedFrontmatterWithoutFailing(t *testing.T) {
	c := assert.NewAborting(t)
	dir := t.TempDir()
	writeSkill(t, dir, "good", "good", "good description", "good body")

	badDir := filepath.Join(dir, "bad")
	c.NoError(os.MkdirAll(badDir, 0o755))
	// No closing "---" delimiter - malformed frontmatter.
	c.NoError(os.WriteFile(filepath.Join(badDir, "SKILL.md"), []byte("---\nname: bad\ndescription: [unterminated\nno closing delimiter here\n"), 0o644))

	skills, err := DiscoverSkills([]string{dir}, nil)
	c.NoError(err, "expected malformed frontmatter to be skipped, not returned as an error")
	c.False(len(skills) != 1 || skills[0].Name != "good", "expected only the well-formed skill to survive, got %+v", skills)
}

// TestDiscoverSkillsMissingDirIsNotFatal: a dir in the list that doesn't
// exist on disk (the common case for an optional ~/.claude/skills that was
// never created) must not fail discovery.
func TestDiscoverSkillsMissingDirIsNotFatal(t *testing.T) {
	c := assert.NewAborting(t)
	dir := t.TempDir()
	writeSkill(t, dir, "alpha", "alpha", "alpha description", "alpha body")

	missing := filepath.Join(dir, "does-not-exist")
	skills, err := DiscoverSkills([]string{missing, dir}, nil)
	c.NoError(err, "expected a missing dir to be skipped, not fatal")
	c.False(len(skills) != 1 || skills[0].Name != "alpha", "expected the alpha skill from the present dir, got %+v", skills)
}

// TestDiscoverSkillsFollowsSymlinks pins the symlink-traversal behaviour: a
// skills dir assembled from symlinks (e.g. ~/.config/rafiki/skills entries
// pointing into ~/.claude/skills, the layout ~/.claude-work/skills itself
// uses) must discover the linked skill - DirEntry.IsDir reports the link
// itself, not the target, so a symlink to a directory is skipped unless the
// entry is stat'ed. A broken symlink must be skipped, not fatal.
func TestDiscoverSkillsFollowsSymlinks(t *testing.T) {
	c := assert.NewAborting(t)
	real := t.TempDir()
	writeSkill(t, real, "linked", "linked", "linked description", "linked body")

	dir := t.TempDir()
	c.NoError(os.Symlink(filepath.Join(real, "linked"), filepath.Join(dir, "linked")))
	c.NoError(os.Symlink(filepath.Join(real, "gone"), filepath.Join(dir, "broken")))

	skills, err := DiscoverSkills([]string{dir}, nil)
	c.NoError(err, "expected the broken symlink to be skipped, not fatal")
	c.False(len(skills) != 1 || skills[0].Name != "linked", "expected the symlinked skill to be discovered, got %+v", skills)
	// Dir/Path point at the symlink inside the scanned dir, not the resolved
	// target: reads through them work either way, and the path the skill tool
	// reports as the skill's base directory is the one the user configured.
	c.Eq(filepath.Join(dir, "linked"), skills[0].Dir, "expected Dir to be the symlink path, got")
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
	assert.NewAborting(t).Eq(want, got, "got")
}

// TestSkillsInventoryEmpty asserts an empty skill list renders as an empty
// string, so BuildSystemPrompt's "omit empty sections" rule has nothing to
// trip over.
func TestSkillsInventoryEmpty(t *testing.T) {
	assert.NewAborting(t).Eq("", SkillsInventory(nil), "expected empty string for no skills, got")
}

// TestSkillBodyStripsFrontmatter covers the helper the skill tool uses to
// load a skill's content at invocation time: the YAML frontmatter block must
// not appear in the returned body.
func TestSkillBodyStripsFrontmatter(t *testing.T) {
	c := assert.NewAborting(t)
	dir := t.TempDir()
	writeSkill(t, dir, "alpha", "alpha", "alpha description", "ALPHA_BODY_MARKER\nmore body text\n")

	body, err := SkillBody(filepath.Join(dir, "alpha", "SKILL.md"))
	c.NoError(err)
	c.False(strings.Contains(body, "---") || strings.Contains(body, "description:"), "expected frontmatter stripped from body, got %q", body)
	c.StrContains(body, "ALPHA_BODY_MARKER", "expected body content preserved, got")
}

// TestSplitFrontmatterCRLF is the regression test for the CRLF body-split
// bug: a Windows-authored SKILL.md (all line endings "\r\n") must not leave
// a stray leading blank line in the split-out body. Without the
// trimLeadingNewline fix, TrimPrefix(rest, "\n") fails to match the "\r\n"
// left after the closing delimiter, and body comes back as
// "\r\nBody text\r\nmore\r\n".
func TestSplitFrontmatterCRLF(t *testing.T) {
	c := assert.NewAborting(t)
	content := "---\r\nname: x\r\ndescription: y\r\n---\r\nBody text\r\nmore\r\n"

	_, body, err := splitFrontmatter(content)
	c.NoError(err)
	c.False(strings.HasPrefix(body, "\r\n") || strings.HasPrefix(body, "\n"), "expected no leading blank line in CRLF body, got %q", body)
	if !strings.HasPrefix(body, "Body text") {
		t.Fatalf("expected body to start with %q, got %q", "Body text", body)
	}
}

// TestSplitFrontmatterNoFrontmatter covers content that never opens with the
// "---" delimiter at all - the documented error path, not a crash or a
// silently-wrong split.
func TestSplitFrontmatterNoFrontmatter(t *testing.T) {
	_, _, err := splitFrontmatter("Just a body, no frontmatter block here.\n")
	assert.NewAborting(t).Error(err, "expected an error for content with no opening frontmatter delimiter")
}

// TestSplitFrontmatterHorizontalRuleInBody asserts that a "---" appearing
// later in the body (e.g. a markdown horizontal rule) is preserved verbatim
// and does not get mistaken for a second closing delimiter - splitFrontmatter
// uses strings.Index, which finds the *first* "\n---" only.
func TestSplitFrontmatterHorizontalRuleInBody(t *testing.T) {
	c := assert.NewAborting(t)
	content := "---\nname: x\ndescription: y\n---\nIntro text.\n\n---\n\nMore text after the rule.\n"

	_, body, err := splitFrontmatter(content)
	c.NoError(err)
	want := "Intro text.\n\n---\n\nMore text after the rule.\n"
	c.Eq(want, body, "expected horizontal rule preserved in body, got")
}

func TestQualifiedNameUsesNamespaceWhenPresent(t *testing.T) {
	c := assert.NewCollecting(t)
	bare := SkillMeta{Name: "deploy"}
	c.Eq("deploy", bare.QualifiedName(), "bare skill: got")
	ns := SkillMeta{Namespace: "rafiki", Name: "deploy"}
	c.Eq("rafiki:deploy", ns.QualifiedName(), "namespaced skill: got")
}

func TestSkillsInventoryRendersQualifiedNames(t *testing.T) {
	got := SkillsInventory([]SkillMeta{
		{Namespace: "rafiki", Name: "coordinating", Description: "how to run subagents"},
		{Name: "local-only", Description: "from a directory"},
	})
	want := "- rafiki:coordinating: how to run subagents\n- local-only: from a directory"
	assert.NewCollecting(t).Eq(want, got, "got:\n")
}
