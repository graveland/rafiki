package skills

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// SkillMeta is one discovered skill: its identity (Name/Description, from
// SKILL.md frontmatter) and where it lives on disk (Dir is the skill's own
// directory, Path is the SKILL.md file itself).
type SkillMeta struct {
	Name, Description, Dir, Path string
}

// skillFrontmatter is the YAML shape of a SKILL.md's frontmatter block.
type skillFrontmatter struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description"`
}

// DiscoverSkills scans each directory in dirs for the Claude Code skills
// layout - <dir>/<skill-name>/SKILL.md, with a YAML frontmatter block naming
// the skill - and returns the merged, filtered result sorted by name.
//
// Later entries in dirs override earlier ones on name collision: callers
// build dirs as [paths.SkillsDirs()..., <cwd>/.claude/skills,
// <cwd>/.rafiki/skills, ...--skills-dir extras] (see
// cmd/rafikid/agent.go:assembleSkillDirs), so a project-level skill shadows a
// user-level one of the same name, and .rafiki/skills shadows .claude/skills
// of the same name. Note the project-level dirs are keyed off the child's
// cwd, not its git root - so a skill dir living at the repo root is only
// found when the child's cwd IS the repo root.
// only, when non-nil, restricts the result to those names
// (SpawnRequest.Skills); nil means "all discovered skills".
//
// A directory that doesn't exist is skipped, not an error - most of the
// default dirs won't exist on a given machine. A SKILL.md with unparseable
// frontmatter is logged and skipped, never fatal: one malformed skill must
// not take down the whole inventory.
//
// The result is sorted by name. This is load-bearing, not cosmetic: the
// inventory feeds SkillsInventory, which sits in the system prompt under
// rafiki's prompt-cache breakpoint - unstable ordering (e.g. from map or
// filesystem iteration) would bust the cache prefix on every turn.
func DiscoverSkills(dirs []string, only []string) ([]SkillMeta, error) {
	var filter map[string]bool
	if only != nil {
		filter = make(map[string]bool, len(only))
		for _, name := range only {
			filter[name] = true
		}
	}

	found := make(map[string]SkillMeta)
	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, fmt.Errorf("agent: read skills dir %s: %w", dir, err)
		}

		var names []string
		for _, e := range entries {
			// os.Stat follows symlinks; e.IsDir does not (a DirEntry
			// reports the link itself). A skills dir assembled from
			// symlinks - e.g. ~/.config/rafiki/skills entries pointing
			// into ~/.claude/skills or a plugin cache, matching how
			// Claude Code traverses its own skills dir - must discover
			// the target, not skip the link. A broken symlink fails
			// Stat and is skipped like any other non-skill entry.
			info, err := os.Stat(filepath.Join(dir, e.Name()))
			if err != nil || !info.IsDir() {
				continue
			}
			names = append(names, e.Name())
		}
		sort.Strings(names)

		for _, name := range names {
			skillDir := filepath.Join(dir, name)
			skillPath := filepath.Join(skillDir, "SKILL.md")
			if _, err := os.Stat(skillPath); err != nil {
				if !os.IsNotExist(err) {
					slog.Warn("agent: skipping skill dir, stat failed", "path", skillPath, "error", err)
				}
				continue // not a skill directory (or, if err != nil above, unreadable)
			}

			fm, err := readSkillFrontmatter(skillPath)
			if err != nil {
				slog.Warn("agent: skipping skill with unparseable frontmatter", "path", skillPath, "error", err)
				continue
			}

			// fm.Name is guaranteed non-empty here: readSkillFrontmatter
			// itself errors (and is skipped above) when the frontmatter has
			// no "name" field.
			found[fm.Name] = SkillMeta{
				Name:        fm.Name,
				Description: fm.Description,
				Dir:         skillDir,
				Path:        skillPath,
			}
		}
	}

	names := make([]string, 0, len(found))
	for name := range found {
		if filter != nil && !filter[name] {
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)

	out := make([]SkillMeta, 0, len(names))
	for _, name := range names {
		out = append(out, found[name])
	}
	return out, nil
}

// SkillsInventory renders skills as "- name: description" lines, one per
// skill, for BuildSystemPrompt's SkillsInventory section. Returns "" for an
// empty list, so BuildSystemPrompt's empty-section omission has nothing to
// trip over.
func SkillsInventory(skills []SkillMeta) string {
	if len(skills) == 0 {
		return ""
	}
	lines := make([]string, 0, len(skills))
	for _, s := range skills {
		lines = append(lines, fmt.Sprintf("- %s: %s", s.Name, s.Description))
	}
	return strings.Join(lines, "\n")
}

// SkillBody reads a SKILL.md file and returns its content with the
// frontmatter block stripped - what the skill tool injects into the
// conversation when the skill is invoked.
func SkillBody(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("agent: read skill file %s: %w", path, err)
	}
	_, body, err := splitFrontmatter(string(data))
	if err != nil {
		return "", fmt.Errorf("agent: %s: %w", path, err)
	}
	return body, nil
}

// readSkillFrontmatter reads path and parses its frontmatter block into a
// skillFrontmatter. It returns an error for anything short of a valid
// "---\n<yaml with a name field>\n---\n" block - the caller decides whether
// that's fatal (it isn't, for DiscoverSkills).
func readSkillFrontmatter(path string) (skillFrontmatter, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return skillFrontmatter{}, err
	}
	fm, _, err := splitFrontmatter(string(data))
	if err != nil {
		return skillFrontmatter{}, err
	}
	var meta skillFrontmatter
	if err := yaml.Unmarshal([]byte(fm), &meta); err != nil {
		return skillFrontmatter{}, fmt.Errorf("parse frontmatter: %w", err)
	}
	if meta.Name == "" {
		return skillFrontmatter{}, fmt.Errorf("frontmatter missing required \"name\" field")
	}
	return meta, nil
}

// frontmatterDelim is the line that opens and closes a SKILL.md frontmatter
// block, per the Claude Code skills convention this package must stay
// compatible with.
const frontmatterDelim = "---"

// splitFrontmatter splits a SKILL.md's raw content into its frontmatter
// (the YAML between the opening and closing "---" delimiter lines) and its
// body (everything after the closing delimiter). Returns an error if content
// doesn't open with the delimiter or never closes it.
func splitFrontmatter(content string) (frontmatter, body string, err error) {
	if !strings.HasPrefix(content, frontmatterDelim) {
		return "", "", fmt.Errorf("missing opening frontmatter delimiter %q", frontmatterDelim)
	}
	rest := trimLeadingNewline(content[len(frontmatterDelim):])

	closeMarker := "\n" + frontmatterDelim
	idx := strings.Index(rest, closeMarker)
	if idx == -1 {
		return "", "", fmt.Errorf("missing closing frontmatter delimiter %q", frontmatterDelim)
	}

	frontmatter = rest[:idx]
	body = trimLeadingNewline(rest[idx+len(closeMarker):])
	return frontmatter, body, nil
}

// trimLeadingNewline strips a single leading line break, "\r\n" or "\n",
// from s. A bare TrimPrefix(s, "\n") leaves the "\r" of a CRLF-authored
// SKILL.md in place, which surfaces as a stray leading blank line in the
// split-out body - so the CRLF form must be checked first.
func trimLeadingNewline(s string) string {
	if rest, ok := strings.CutPrefix(s, "\r\n"); ok {
		return rest
	}
	return strings.TrimPrefix(s, "\n")
}
