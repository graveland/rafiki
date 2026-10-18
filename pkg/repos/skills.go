// SPDX-License-Identifier: Apache-2.0

// Skills discovery for a repo: the skill directories in a git checkout, plus
// the skills shipped inside the python distributions that checkout declares
// as [tool.rafiki] skill-sources. Pure filesystem and manifest logic -- no
// store, no network, no subprocess, and no Python: an installed wheel is read
// out of site-packages with its own RECORD as the index.
package repos

import (
	"bufio"
	"bytes"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"net/textproto"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"

	"go.graveland.dev/rafiki/pkg/skills"
)

// DiscoveredSkill is one skill found in a checkout or in an installed
// skill-source distribution.
type DiscoveredSkill struct {
	Namespace, Name, Description, Body string

	// From is where the skill was found: a path relative to the checkout, or
	// "<dist>:<path>" for a wheel. A skill read from a file:// skill-source
	// is "<dist>:<path relative to that source's directory>".
	From string

	Files []skills.File
}

// SkillsReport is the skills half of a repo refresh. Errors are per-skill and
// per-source: a bad skill is reported and omitted, never fatal to the rest of
// the report.
type SkillsReport struct {
	Skills []DiscoveredSkill // sorted by Namespace, Name
	Errors []string
}

// skillLocations are the directories, relative to a checkout or to a
// skill-source's own directory, that may hold skills: the generic one and the
// two agent-specific ones.
var skillLocations = []string{
	"skills",
	filepath.Join(".claude", "skills"),
	filepath.Join(".rafiki", "skills"),
}

// DiscoverSkills runs the skills discoverer over checkout. repoName is the
// fallback namespace; venv is the checkout's venv dir ("" if none was built).
//
// Namespace is the checkout's plugin name when it declares one, else
// repoName; a skill-source installed as a wheel is namespaced by its
// [tool.rafiki] skill-sources entry, and a skill-source installed from a local
// path by that directory's plugin name (falling back to the entry).
func DiscoverSkills(checkout, repoName, venv string) SkillsReport {
	var report SkillsReport
	var found []DiscoveredSkill

	if checkout != "" {
		namespace := pluginName(checkout)
		if namespace == "" {
			namespace = repoName
		}
		found = append(found, scanSkillsRoot(checkout, namespace, "", &report.Errors)...)
	}
	found = append(found, discoverSkillSources(checkout, venv, &report.Errors)...)

	report.Skills = dedupeSkills(found, &report.Errors)
	return report
}

// scanSkillsRoot returns every skill under root's three skill locations.
// namespace is theirs; fromPrefix is prepended to each From ("" for the
// checkout itself, "<dist>:" for a skill-source directory, whose skills are
// not relative to the checkout).
func scanSkillsRoot(root, namespace, fromPrefix string, errs *[]string) []DiscoveredSkill {
	var out []DiscoveredSkill
	for _, loc := range skillLocations {
		dir := filepath.Join(root, loc)
		entries, err := os.ReadDir(dir)
		if err != nil {
			if !os.IsNotExist(err) {
				*errs = append(*errs, fmt.Sprintf("%s: %v", dir, err))
			}
			continue
		}
		// os.ReadDir sorts by name, so discovery order is stable and a
		// duplicate's two Froms are reported in a predictable order. A
		// symlinked skill directory is skipped rather than followed, the same
		// rule ReadSkillDir applies inside one.
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			skillDir := filepath.Join(dir, e.Name())
			skillFile := filepath.Join(skillDir, "SKILL.md")
			rel, err := filepath.Rel(root, skillFile)
			if err != nil {
				*errs = append(*errs, fmt.Sprintf("%s: %v", skillFile, err))
				continue
			}
			from := fromPrefix + filepath.ToSlash(rel)
			if _, err := os.Stat(skillFile); err != nil {
				if !os.IsNotExist(err) {
					*errs = append(*errs, fmt.Sprintf("%s: %v", from, err))
				}
				continue // a directory without a SKILL.md is not a skill
			}
			if s, ok := readSkill(skillDir, skillFile, e.Name(), namespace, from, errs); ok {
				out = append(out, s)
			}
		}
	}
	return out
}

// readSkill reads one skill directory: its SKILL.md frontmatter (whose name
// must equal the directory's) and its supporting files. Any error becomes an
// entry in errs and the skill is omitted -- one bad skill never fails the rest
// of the report.
func readSkill(skillDir, skillFile, dirName, namespace, from string, errs *[]string) (DiscoveredSkill, bool) {
	data, err := os.ReadFile(skillFile)
	if err != nil {
		*errs = append(*errs, fmt.Sprintf("%s: %v", from, err))
		return DiscoveredSkill{}, false
	}
	name, description, body, err := skills.ParseSkillFile(string(data))
	if err != nil {
		*errs = append(*errs, fmt.Sprintf("%s: %v", from, err))
		return DiscoveredSkill{}, false
	}
	// A skill is addressed by its directory name, so a frontmatter name that
	// disagrees would make the inventory and the file tree two different
	// answers to "what is this skill called".
	if name != dirName {
		*errs = append(*errs, fmt.Sprintf("%s: frontmatter name %q does not match directory name %q", from, name, dirName))
		return DiscoveredSkill{}, false
	}
	files, err := skills.ReadSkillDir(skillDir)
	if err != nil {
		*errs = append(*errs, fmt.Sprintf("%s: %v", from, err))
		return DiscoveredSkill{}, false
	}
	return DiscoveredSkill{
		Namespace:   namespace,
		Name:        name,
		Description: description,
		Body:        body,
		From:        from,
		Files:       files,
	}, true
}

// pyproject is the part of a checkout's pyproject.toml this package reads:
// the requirements a skill-source must be one of, and the skill-sources
// themselves.
type pyproject struct {
	Project struct {
		Dependencies []string `toml:"dependencies"`
	} `toml:"project"`
	Tool struct {
		Rafiki struct {
			SkillSources []string `toml:"skill-sources"`
		} `toml:"rafiki"`
	} `toml:"tool"`
}

// discoverSkillSources resolves each [tool.rafiki] skill-sources entry against
// the checkout's venv. A source that is not a dependency is refused before the
// venv is consulted: the declaration has to be self-consistent first.
func discoverSkillSources(checkout, venv string, errs *[]string) []DiscoveredSkill {
	if checkout == "" {
		return nil
	}
	pyprojectPath := filepath.Join(checkout, "pyproject.toml")
	data, err := os.ReadFile(pyprojectPath)
	if err != nil {
		if !os.IsNotExist(err) {
			*errs = append(*errs, fmt.Sprintf("%s: %v", pyprojectPath, err))
		}
		return nil
	}
	var decl pyproject
	if err := toml.Unmarshal(data, &decl); err != nil {
		*errs = append(*errs, fmt.Sprintf("%s: %v", pyprojectPath, err))
		return nil
	}
	if len(decl.Tool.Rafiki.SkillSources) == 0 {
		return nil
	}

	deps := make(map[string]bool, len(decl.Project.Dependencies))
	for _, dep := range decl.Project.Dependencies {
		if name := normaliseName(dependencyName(dep)); name != "" {
			deps[name] = true
		}
	}

	var out []DiscoveredSkill
	for _, entry := range decl.Tool.Rafiki.SkillSources {
		if !deps[normaliseName(entry)] {
			*errs = append(*errs, fmt.Sprintf("skill-sources entry %q is not in [project].dependencies", entry))
			continue
		}
		out = append(out, resolveSkillSource(entry, venv, errs)...)
	}
	return out
}

// dependencyNameRe is the leading distribution name of a PEP 508 requirement:
// "review-swarm[extra]>=1.0" names the distribution "review-swarm", which is
// all such an entry has in common with a skill-sources entry.
var dependencyNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*`)

// dependencyName extracts the distribution name a requirement names.
func dependencyName(requirement string) string {
	return dependencyNameRe.FindString(requirement)
}

// nameSeparatorRe matches the runs PEP 503 normalisation collapses.
var nameSeparatorRe = regexp.MustCompile(`[-_.]+`)

// normaliseName is PEP 503's distribution-name normalisation: lowercase, with
// runs of "-", "_" and "." collapsed to a single "-". "Review_Swarm",
// "review.swarm" and "review-swarm" are therefore the same distribution.
func normaliseName(name string) string {
	return nameSeparatorRe.ReplaceAllString(strings.ToLower(name), "-")
}

// resolveSkillSource resolves one validated skill-sources entry against venv
// and returns its skills. The namespace is the entry as written for a wheel
// and the source directory's plugin name for a local install.
func resolveSkillSource(entry, venv string, errs *[]string) []DiscoveredSkill {
	if venv == "" {
		*errs = append(*errs, fmt.Sprintf("skill-sources entry %q: the repo's venv did not build", entry))
		return nil
	}
	sitePackages, distInfo, ok := findDistInfo(venv, normaliseName(entry))
	if !ok {
		*errs = append(*errs, fmt.Sprintf("skill-sources entry %q is not installed", entry))
		return nil
	}

	if dir, ok := localSourceDir(distInfo); ok {
		// An entry that resolves to a source tree which is not there cannot be
		// read at all. Silence would let the user believe skills they do not
		// have are reaching their agents; a directory that IS there and holds
		// no skills stays silent, because a pinned version may ship none.
		if info, err := os.Stat(dir); err != nil || !info.IsDir() {
			*errs = append(*errs, fmt.Sprintf("skill-sources entry %q: source directory %s does not exist", entry, dir))
			return nil
		}
		namespace, origin := pluginName(dir), dir
		if namespace == "" {
			namespace, origin = entry, entry
		}
		if validNamespace(namespace) == "" {
			*errs = append(*errs, fmt.Sprintf("namespace %q (from %s) is not a valid plugin name", namespace, origin))
			return nil
		}
		return scanSkillsRoot(dir, namespace, entry+":", errs)
	}

	if validNamespace(entry) == "" {
		*errs = append(*errs, fmt.Sprintf("namespace %q (from %s) is not a valid plugin name", entry, entry))
		return nil
	}
	return scanWheel(sitePackages, distInfo, entry, errs)
}

// findDistInfo locates the installed distribution whose METADATA name
// normalises to entry, returning its site-packages directory and its
// .dist-info directory. Entries are searched in sorted order so two
// installations of the same distribution in one venv resolve predictably.
func findDistInfo(venv, entry string) (sitePackages, distInfo string, ok bool) {
	sites, err := filepath.Glob(filepath.Join(venv, "lib", "python*", "site-packages"))
	if err != nil {
		return "", "", false
	}
	for _, site := range sites {
		dists, err := filepath.Glob(filepath.Join(site, "*.dist-info"))
		if err != nil {
			continue
		}
		for _, dist := range dists {
			if normaliseName(distName(dist)) == entry {
				return site, dist, true
			}
		}
	}
	return "", "", false
}

// distName reads the Name header of a .dist-info directory's METADATA. A
// missing or unreadable METADATA answers "", which matches no entry.
func distName(distInfo string) string {
	data, err := os.ReadFile(filepath.Join(distInfo, "METADATA"))
	if err != nil {
		return ""
	}
	// The header block may end at EOF without the blank line that normally
	// terminates it; whatever was read before the error is still usable, and
	// Get on a nil map answers "".
	headers, _ := textproto.NewReader(bufio.NewReader(bytes.NewReader(data))).ReadMIMEHeader()
	return headers.Get("Name")
}

// localSourceDir reports the directory a direct_url.json points at, when it
// names a file:// URL -- an editable or `uv pip install <path>` install, whose
// skills live in the source tree rather than in site-packages.
func localSourceDir(distInfo string) (string, bool) {
	data, err := os.ReadFile(filepath.Join(distInfo, "direct_url.json"))
	if err != nil {
		return "", false
	}
	var decl struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal(data, &decl); err != nil {
		return "", false
	}
	u, err := url.Parse(decl.URL)
	if err != nil || u.Scheme != "file" || u.Path == "" {
		return "", false
	}
	return filepath.FromSlash(u.Path), true
}

// scanWheel reads the skills a wheel installed into sitePackages. A skill is a
// `<pkg>/skills/<name>` directory whose SKILL.md the RECORD lists; its files
// are read from site-packages. RECORD is the index of what the distribution
// actually installed, which is why a wheel is searched through it rather than
// by walking a tree that may hold anything else.
func scanWheel(sitePackages, distInfo, entry string, errs *[]string) []DiscoveredSkill {
	recordPath := filepath.Join(distInfo, "RECORD")
	f, err := os.Open(recordPath)
	if err != nil {
		*errs = append(*errs, fmt.Sprintf("%s: read RECORD: %v", entry, err))
		return nil
	}
	defer f.Close()

	records, err := csv.NewReader(f).ReadAll()
	if err != nil {
		*errs = append(*errs, fmt.Sprintf("%s: read RECORD: %v", entry, err))
		return nil
	}

	dirs := make(map[string]string, len(records)) // skill directory -> skill name
	for _, rec := range records {
		if len(rec) == 0 {
			continue
		}
		dir, name, ok := skillRecordDir(filepath.ToSlash(rec[0]))
		if !ok {
			continue
		}
		if _, dup := dirs[dir]; !dup {
			dirs[dir] = name
		}
	}
	sorted := make([]string, 0, len(dirs))
	for dir := range dirs {
		sorted = append(sorted, dir)
	}
	sort.Strings(sorted)

	var out []DiscoveredSkill
	for _, dir := range sorted {
		skillDir := filepath.Join(sitePackages, filepath.FromSlash(dir))
		from := entry + ":" + dir + "/SKILL.md"
		if s, ok := readSkill(skillDir, filepath.Join(skillDir, "SKILL.md"), dirs[dir], entry, from, errs); ok {
			out = append(out, s)
		}
	}
	return out
}

// skillRecordDir turns a RECORD path into its `<pkg>/skills/<name>` directory
// and the skill's name, when the path is that directory's SKILL.md. Anything
// else -- a module, a nested file, a script installed outside site-packages --
// is not a skill. A path that escapes site-packages is refused rather than
// read: a RECORD is a file rafiki did not write.
func skillRecordDir(recordPath string) (dir, name string, ok bool) {
	if !strings.HasSuffix(recordPath, "/SKILL.md") {
		return "", "", false
	}
	dir = strings.TrimSuffix(recordPath, "/SKILL.md")
	if strings.HasPrefix(dir, "/") {
		return "", "", false
	}
	for _, seg := range strings.Split(dir, "/") {
		if seg == ".." {
			return "", "", false
		}
	}
	i := strings.LastIndex(dir, "/skills/")
	if i <= 0 {
		return "", "", false
	}
	name = dir[i+len("/skills/"):]
	if name == "" || strings.Contains(name, "/") {
		return "", "", false
	}
	return dir, name, true
}

// dedupeSkills collapses skills on their qualified name and sorts the rest.
// A name that arrives from two places is ambiguous -- a layer would have to
// pick one arbitrarily -- so both are dropped and the collision reported.
func dedupeSkills(found []DiscoveredSkill, errs *[]string) []DiscoveredSkill {
	groups := make(map[string][]DiscoveredSkill, len(found))
	var order []string
	for _, s := range found {
		key := s.Namespace + ":" + s.Name
		if _, ok := groups[key]; !ok {
			order = append(order, key)
		}
		groups[key] = append(groups[key], s)
	}

	var out []DiscoveredSkill
	for _, key := range order {
		group := groups[key]
		if len(group) > 1 {
			*errs = append(*errs, fmt.Sprintf("%s found twice: %s and %s", key, group[0].From, group[1].From))
			continue
		}
		out = append(out, group[0])
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Namespace != out[j].Namespace {
			return out[i].Namespace < out[j].Namespace
		}
		return out[i].Name < out[j].Name
	})
	return out
}
