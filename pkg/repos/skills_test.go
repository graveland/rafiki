// SPDX-License-Identifier: Apache-2.0

package repos

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/multigres/testkit/assert"
)

// skillMD is a minimal well-formed SKILL.md: the frontmatter Claude Code
// requires, then the body.
func skillMD(name, description, body string) string {
	return "---\nname: " + name + "\ndescription: " + description + "\n---\n" + body
}

// distMetadata is a wheel's METADATA header block.
func distMetadata(name string) string {
	return "Metadata-Version: 2.1\nName: " + name + "\nVersion: 1.0\n\n"
}

// fakeVenv writes files into a fake virtualenv's site-packages and returns
// that directory. Nothing in these tests runs Python: METADATA, RECORD and
// direct_url.json are hand-written, which is all the discoverer reads.
func fakeVenv(t *testing.T, venv string, files map[string]string) string {
	t.Helper()
	c := assert.NewAborting(t)
	site := filepath.Join(venv, "lib", "python3.12", "site-packages")
	for rel, content := range files {
		p := filepath.Join(site, rel)
		c.NoError(os.MkdirAll(filepath.Dir(p), 0o755), "mkdir %s", filepath.Dir(p))
		c.NoError(os.WriteFile(p, []byte(content), 0o644), "write %s", p)
	}
	return site
}

// writePyproject writes a checkout's pyproject.toml declaring deps and
// skill-sources, in the shape a real project uses.
func writePyproject(t *testing.T, checkout string, deps, sources []string) {
	t.Helper()
	quote := func(vals []string) string {
		quoted := make([]string, len(vals))
		for i, v := range vals {
			quoted[i] = fmt.Sprintf("%q", v)
		}
		return "[" + strings.Join(quoted, ", ") + "]"
	}
	body := "[project]\ndependencies = " + quote(deps) +
		"\n\n[tool.rafiki]\nskill-sources = " + quote(sources) + "\n"
	c := assert.NewAborting(t)
	c.NoError(os.WriteFile(filepath.Join(checkout, "pyproject.toml"), []byte(body), 0o644))
}

// writeRecord joins RECORD rows, each a path with empty hash and size columns.
func writeRecord(paths ...string) string {
	rows := make([]string, len(paths))
	for i, p := range paths {
		rows[i] = p + ",,"
	}
	return strings.Join(rows, "\n") + "\n"
}

// All three checkout locations are searched, the repo name is the namespace
// when the checkout declares no plugin, and a directory without a SKILL.md is
// not a skill.
func TestDiscoverSkillsCheckoutLocations(t *testing.T) {
	c := assert.NewCollecting(t)
	checkout := writeTree(t, map[string]string{
		"skills/alpha/SKILL.md":         skillMD("alpha", "Alpha skill", "# Alpha\n"),
		"skills/alpha/reference.md":     "alpha reference\n",
		".claude/skills/beta/SKILL.md":  skillMD("beta", "Beta skill", "# Beta\n"),
		".rafiki/skills/gamma/SKILL.md": skillMD("gamma", "Gamma skill", "# Gamma\n"),
		"skills/not-a-skill/README.md":  "no SKILL.md here\n",
		"skills/loose-file.md":          "not a directory\n",
	})

	report := DiscoverSkills(checkout, "corpus", "")
	c.Require().Empty(report.Errors, "errors")
	c.Require().Len(report.Skills, 3, "got %d skills %+v, want 3", len(report.Skills), report.Skills)

	want := []struct{ namespace, name, from string }{
		{"corpus", "alpha", "skills/alpha/SKILL.md"},
		{"corpus", "beta", ".claude/skills/beta/SKILL.md"},
		{"corpus", "gamma", ".rafiki/skills/gamma/SKILL.md"},
	}
	for i, w := range want {
		c.Eq(w.namespace, report.Skills[i].Namespace, "skill %d namespace", i)
		c.Eq(w.name, report.Skills[i].Name, "skill %d name", i)
		c.Eq(w.from, report.Skills[i].From, "skill %d from", i)
	}
	c.Eq("Alpha skill", report.Skills[0].Description, "alpha description")
	c.Eq("# Alpha\n", report.Skills[0].Body, "alpha body")
	c.Require().Len(report.Skills[0].Files, 1, "alpha files")
	c.Eq("reference.md", report.Skills[0].Files[0].Path, "alpha file path")
	c.Eq("alpha reference\n", string(report.Skills[0].Files[0].Content), "alpha file content")
	c.Eq(uint32(0o644), report.Skills[0].Files[0].Mode, "alpha file mode")
}

// A checkout that declares a plugin is namespaced by it, not by the repo name.
func TestDiscoverSkillsPluginNamespace(t *testing.T) {
	c := assert.NewCollecting(t)
	checkout := writeTree(t, map[string]string{
		".claude-plugin/plugin.json": `{"name":"upstream"}`,
		"skills/alpha/SKILL.md":      skillMD("alpha", "Alpha", "# Alpha\n"),
	})

	report := DiscoverSkills(checkout, "corpus", "")
	c.Require().Empty(report.Errors, "errors")
	c.Require().Len(report.Skills, 1, "got %d skills, want 1", len(report.Skills))
	c.Eq("upstream", report.Skills[0].Namespace, "namespace")
}

// A SKILL.md whose frontmatter name disagrees with its directory is refused,
// naming both spellings.
func TestDiscoverSkillsNameMismatch(t *testing.T) {
	c := assert.NewCollecting(t)
	checkout := writeTree(t, map[string]string{
		"skills/alpha/SKILL.md": skillMD("wrong", "Alpha", "# Alpha\n"),
	})

	report := DiscoverSkills(checkout, "corpus", "")
	c.Require().Len(report.Errors, 1, "got errors %q, want exactly 1", report.Errors)
	c.Eq(`skills/alpha/SKILL.md: frontmatter name "wrong" does not match directory name "alpha"`,
		report.Errors[0], "error")
	c.Empty(report.Skills, "skills")
}

// A skill-sources entry that is not a dependency is refused before the venv
// is consulted at all.
func TestDiscoverSkillsSourceNotDependency(t *testing.T) {
	c := assert.NewCollecting(t)
	checkout := writeTree(t, map[string]string{})
	writePyproject(t, checkout, []string{"other"}, []string{"review-swarm"})

	report := DiscoverSkills(checkout, "corpus", "")
	c.Require().Len(report.Errors, 1, "got errors %q, want exactly 1", report.Errors)
	c.Eq(`skill-sources entry "review-swarm" is not in [project].dependencies`, report.Errors[0], "error")
	c.Empty(report.Skills, "skills")
}

// A declared source that the venv never installed is an error, not silence.
func TestDiscoverSkillsSourceNotInstalled(t *testing.T) {
	c := assert.NewCollecting(t)
	checkout := writeTree(t, map[string]string{})
	writePyproject(t, checkout, []string{"review-swarm"}, []string{"review-swarm"})

	report := DiscoverSkills(checkout, "corpus", t.TempDir())
	c.Require().Len(report.Errors, 1, "got errors %q, want exactly 1", report.Errors)
	c.Eq(`skill-sources entry "review-swarm" is not installed`, report.Errors[0], "error")
	c.Empty(report.Skills, "skills")
}

// No venv is its own error: the source cannot be resolved without one.
func TestDiscoverSkillsNoVenv(t *testing.T) {
	c := assert.NewCollecting(t)
	checkout := writeTree(t, map[string]string{})
	writePyproject(t, checkout, []string{"review-swarm"}, []string{"review-swarm"})

	report := DiscoverSkills(checkout, "corpus", "")
	c.Require().Len(report.Errors, 1, "got errors %q, want exactly 1", report.Errors)
	c.Eq(`skill-sources entry "review-swarm": the repo's venv did not build`, report.Errors[0], "error")
	c.Empty(report.Skills, "skills")
}

// A wheel's skills are found through its RECORD, read out of site-packages,
// and namespaced by the entry as written.
func TestDiscoverSkillsWheel(t *testing.T) {
	c := assert.NewCollecting(t)
	checkout := writeTree(t, map[string]string{})
	writePyproject(t, checkout, []string{"review-swarm"}, []string{"review-swarm"})
	venv := t.TempDir()
	fakeVenv(t, venv, map[string]string{
		"review_swarm-1.0.dist-info/METADATA": distMetadata("review-swarm"),
		"review_swarm-1.0.dist-info/RECORD": writeRecord(
			"review_swarm/__init__.py",
			"review_swarm/skills/one/SKILL.md",
			"review_swarm/skills/one/checklist.md",
			"review_swarm/skills/two/SKILL.md",
			"review_swarm-1.0.dist-info/METADATA",
			"review_swarm-1.0.dist-info/RECORD",
		),
		"review_swarm/__init__.py":             "",
		"review_swarm/skills/one/SKILL.md":     skillMD("one", "First", "# One\n"),
		"review_swarm/skills/one/checklist.md": "- item\n",
		"review_swarm/skills/two/SKILL.md":     skillMD("two", "Second", "# Two\n"),
	})

	report := DiscoverSkills(checkout, "corpus", venv)
	c.Require().Empty(report.Errors, "errors")
	c.Require().Len(report.Skills, 2, "got %d skills %+v, want 2", len(report.Skills), report.Skills)

	c.Eq("review-swarm", report.Skills[0].Namespace, "first namespace")
	c.Eq("one", report.Skills[0].Name, "first name")
	c.Eq("First", report.Skills[0].Description, "first description")
	c.Eq("# One\n", report.Skills[0].Body, "first body")
	c.Eq("review-swarm:review_swarm/skills/one/SKILL.md", report.Skills[0].From, "first from")
	c.Require().Len(report.Skills[0].Files, 1, "first files")
	c.Eq("checklist.md", report.Skills[0].Files[0].Path, "first file path")
	c.Eq("- item\n", string(report.Skills[0].Files[0].Content), "first file content")

	c.Eq("two", report.Skills[1].Name, "second name")
	c.Eq("review-swarm:review_swarm/skills/two/SKILL.md", report.Skills[1].From, "second from")
}

// A source installed from a local path is searched like a checkout, and takes
// the namespace its plugin manifest declares.
func TestDiscoverSkillsLocalPath(t *testing.T) {
	c := assert.NewCollecting(t)
	src := writeTree(t, map[string]string{
		".claude-plugin/plugin.json": `{"name":"localplug"}`,
		"skills/one/SKILL.md":        skillMD("one", "First", "# One\n"),
	})
	checkout := writeTree(t, map[string]string{})
	writePyproject(t, checkout, []string{"review-swarm"}, []string{"review-swarm"})
	directURL, err := json.Marshal(map[string]string{"url": "file://" + filepath.ToSlash(src)})
	c.Require().NoError(err, "marshal direct_url.json")

	venv := t.TempDir()
	fakeVenv(t, venv, map[string]string{
		"review_swarm-1.0.dist-info/METADATA":        distMetadata("review-swarm"),
		"review_swarm-1.0.dist-info/direct_url.json": string(directURL),
	})

	report := DiscoverSkills(checkout, "corpus", venv)
	c.Require().Empty(report.Errors, "errors")
	c.Require().Len(report.Skills, 1, "got %d skills %+v, want 1", len(report.Skills), report.Skills)
	c.Eq("localplug", report.Skills[0].Namespace, "namespace")
	c.Eq("one", report.Skills[0].Name, "name")
	c.Eq("review-swarm:skills/one/SKILL.md", report.Skills[0].From, "from")
}

// A source directory that exists but ships no skills is legitimate — a pinned
// engine version may ship none — so it stays silent rather than looking like a
// broken declaration.
func TestDiscoverSkillsLocalPathWithoutSkills(t *testing.T) {
	c := assert.NewCollecting(t)
	src := writeTree(t, map[string]string{
		"README.md": "no skills in this one\n",
	})
	checkout := writeTree(t, map[string]string{})
	writePyproject(t, checkout, []string{"review-swarm"}, []string{"review-swarm"})
	directURL, err := json.Marshal(map[string]string{"url": "file://" + filepath.ToSlash(src)})
	c.Require().NoError(err, "marshal direct_url.json")

	venv := t.TempDir()
	fakeVenv(t, venv, map[string]string{
		"review_swarm-1.0.dist-info/METADATA":        distMetadata("review-swarm"),
		"review_swarm-1.0.dist-info/direct_url.json": string(directURL),
	})

	report := DiscoverSkills(checkout, "corpus", venv)
	c.Empty(report.Errors, "errors")
	c.Empty(report.Skills, "skills")
}

// A source directory that no longer exists cannot be read at all, so it is an
// error naming both the entry and the path: silence would let the user believe
// skills they do not have are reaching their agents.
func TestDiscoverSkillsLocalPathMissingDirectory(t *testing.T) {
	c := assert.NewCollecting(t)
	gone := filepath.Join(t.TempDir(), "deleted")
	c.Require().NoError(os.MkdirAll(gone, 0o755), "mkdir the source directory")
	checkout := writeTree(t, map[string]string{})
	writePyproject(t, checkout, []string{"review-swarm"}, []string{"review-swarm"})
	directURL, err := json.Marshal(map[string]string{"url": "file://" + filepath.ToSlash(gone)})
	c.Require().NoError(err, "marshal direct_url.json")

	venv := t.TempDir()
	fakeVenv(t, venv, map[string]string{
		"review_swarm-1.0.dist-info/METADATA":        distMetadata("review-swarm"),
		"review_swarm-1.0.dist-info/direct_url.json": string(directURL),
	})
	c.Require().NoError(os.RemoveAll(gone), "remove the source directory")

	report := DiscoverSkills(checkout, "corpus", venv)
	c.Require().Len(report.Errors, 1, "got errors %q, want exactly 1", report.Errors)
	c.Eq(`skill-sources entry "review-swarm": source directory `+gone+` does not exist`,
		report.Errors[0], "error")
	c.Empty(report.Skills, "skills")
}

// One qualified name from two places is ambiguous, so both are dropped and the
// collision names both origins.
func TestDiscoverSkillsDuplicate(t *testing.T) {
	c := assert.NewCollecting(t)
	checkout := writeTree(t, map[string]string{
		"skills/alpha/SKILL.md": skillMD("alpha", "Checkout alpha", "# Alpha\n"),
	})
	writePyproject(t, checkout, []string{"corpus"}, []string{"corpus"})
	venv := t.TempDir()
	fakeVenv(t, venv, map[string]string{
		"corpus-1.0.dist-info/METADATA":        distMetadata("corpus"),
		"corpus-1.0.dist-info/RECORD":          writeRecord("corpus/skills/alpha/SKILL.md"),
		"corpus/skills/alpha/SKILL.md":         skillMD("alpha", "Wheel alpha", "# Alpha\n"),
		"corpus/skills/alpha/another-file.txt": "x\n",
	})

	report := DiscoverSkills(checkout, "corpus", venv)
	c.Require().Len(report.Errors, 1, "got errors %q, want exactly 1", report.Errors)
	c.Eq("corpus:alpha found twice: skills/alpha/SKILL.md and corpus:corpus/skills/alpha/SKILL.md",
		report.Errors[0], "error")
	c.Empty(report.Skills, "skills")
}

// A symlink inside one skill refuses that skill only: its error names the
// skill's SKILL.md, and a sibling skill is still reported.
func TestDiscoverSkillsBadFileIsSkillError(t *testing.T) {
	c := assert.NewAborting(t)
	checkout := writeTree(t, map[string]string{
		"skills/bad/SKILL.md":  skillMD("bad", "Bad", "# Bad\n"),
		"skills/good/SKILL.md": skillMD("good", "Good", "# Good\n"),
	})
	c.NoError(os.Symlink(
		filepath.Join(checkout, "skills", "good", "SKILL.md"),
		filepath.Join(checkout, "skills", "bad", "link.md"),
	), "symlink")

	report := DiscoverSkills(checkout, "corpus", "")
	c.Require().Len(report.Errors, 1, "got errors %q, want exactly 1", report.Errors)
	c.StrContains(report.Errors[0], "skills/bad/SKILL.md: symlink link.md", "error names the skill and the link")
	c.Require().Len(report.Skills, 1, "got %d skills, want the sibling only", len(report.Skills))
	c.Eq("good", report.Skills[0].Name, "surviving skill")
}

// A skill-sources entry matches its dependency and its installed distribution
// across the spelling differences PEP 503 normalisation covers.
func TestDiscoverSkillsNormalisesNames(t *testing.T) {
	c := assert.NewCollecting(t)
	checkout := writeTree(t, map[string]string{})
	writePyproject(t, checkout, []string{"review-swarm>=1.0"}, []string{"Review_Swarm"})
	venv := t.TempDir()
	fakeVenv(t, venv, map[string]string{
		"review_swarm-1.0.dist-info/METADATA": distMetadata("review_swarm"),
		"review_swarm-1.0.dist-info/RECORD":   writeRecord("review_swarm/skills/one/SKILL.md"),
		"review_swarm/skills/one/SKILL.md":    skillMD("one", "First", "# One\n"),
	})

	report := DiscoverSkills(checkout, "corpus", venv)
	c.Require().Empty(report.Errors, "errors")
	c.Require().Len(report.Skills, 1, "got %d skills %+v, want 1", len(report.Skills), report.Skills)
	c.Eq("Review_Swarm", report.Skills[0].Namespace, "namespace is the entry as written")
	c.Eq("Review_Swarm:review_swarm/skills/one/SKILL.md", report.Skills[0].From, "from")
}
