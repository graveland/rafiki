// SPDX-License-Identifier: Apache-2.0

// Discovery for git-sourced pymodules: a plain walk of a git checkout with
// no manifest parsing and no pyproject.toml reading (that stays uv's own
// input). Pure filesystem logic -- no store, no network, no subprocess.
package gitpymodules

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// scriptsDirName is the one directory whose *.py files are agent-callable
// scripts. A dedicated directory, not every top-level .py file, keeps a
// repo's noxfile.py/conftest.py/setup.py clutter out of the callable set.
const scriptsDirName = "scripts"

// DiscoveredScript is one callable entry found under <root>/scripts/.
type DiscoveredScript struct {
	Name        string // filename minus ".py"
	Path        string // absolute path to the .py file
	Description string // "" if none found
}

// DiscoveredPackage is one importable entry: a top-level directory of root
// containing an __init__.py.
type DiscoveredPackage struct {
	Name        string // the directory's name
	Path        string // absolute path to the package directory
	Description string
}

// Discover walks root (a git checkout) and returns every scripts/*.py file
// and every top-level directory containing __init__.py. root/scripts itself,
// if present, is never treated as a package even though it could
// theoretically contain an __init__.py -- scripts/ is exclusively for
// DiscoveredScript. Directories starting with "." are skipped (this is where
// .git lives). Both slices are sorted by Name. An error is returned only for
// an unreadable root; an unreadable subdirectory or script merely yields no
// entry (and no description) for that path, never an error, so one stray
// file cannot fail a whole refresh.
func Discover(root string) ([]DiscoveredScript, []DiscoveredPackage, error) {
	top, err := os.ReadDir(root)
	if err != nil {
		return nil, nil, fmt.Errorf("discover %s: %w", root, err)
	}

	var scripts []DiscoveredScript
	var packages []DiscoveredPackage
	for _, e := range top {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
			continue
		}
		dir := filepath.Join(root, e.Name())
		if e.Name() == scriptsDirName {
			scripts = discoverScripts(dir)
			continue
		}
		if _, err := os.Stat(filepath.Join(dir, "__init__.py")); err != nil {
			continue // no __init__.py (or unreadable): not an importable package
		}
		p := DiscoveredPackage{Name: e.Name(), Path: dir}
		p.Description, _ = descriptionFromComment(filepath.Join(dir, "__init__.py"))
		packages = append(packages, p)
	}

	sort.Slice(scripts, func(i, j int) bool { return scripts[i].Name < scripts[j].Name })
	sort.Slice(packages, func(i, j int) bool { return packages[i].Name < packages[j].Name })
	return scripts, packages, nil
}

// discoverScripts lists dir's *.py files as callable entries. An unreadable
// scripts/ dir yields no scripts (Discover errors only for an unreadable
// root), and an unreadable individual script only loses its description.
func discoverScripts(dir string) []DiscoveredScript {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []DiscoveredScript
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".py") {
			continue
		}
		s := DiscoveredScript{
			Name: strings.TrimSuffix(e.Name(), ".py"),
			Path: filepath.Join(dir, e.Name()),
		}
		s.Description, _ = descriptionFromComment(s.Path)
		out = append(out, s)
	}
	return out
}

// descriptionFromComment reads the first `#`-prefixed comment line after an
// optional shebang (`#!...`) from the file at path, with the "#" and at most
// one following space stripped. Returns "" if the file has no such line
// (e.g. the first non-blank line is code, or the file is empty). Blank lines
// between the shebang and the comment are skipped; a blank line AFTER a
// comment line has already started ends the search -- only the single first
// comment line is ever taken, never a multi-line block. A shebang is
// recognized only on the file's first line; anywhere else a "#"-line is an
// ordinary comment.
func descriptionFromComment(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	r := bufio.NewReader(f)
	for lineNo := 0; ; lineNo++ {
		line, err := r.ReadString('\n')
		if len(line) == 0 && err != nil {
			if err == io.EOF {
				return "", nil // clean end of file, nothing found
			}
			return "", err
		}
		line = strings.TrimRight(line, "\r\n")

		if lineNo == 0 && strings.HasPrefix(line, "#!") {
			continue // shebang, not a comment
		}
		if strings.TrimSpace(line) == "" {
			if err == io.EOF {
				return "", nil
			}
			continue // blank lines before the first comment are skipped
		}
		if !strings.HasPrefix(line, "#") {
			return "", nil // code first: no description
		}
		return strings.TrimPrefix(strings.TrimPrefix(line, "#"), " "), nil
	}
}
