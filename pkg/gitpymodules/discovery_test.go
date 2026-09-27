// SPDX-License-Identifier: Apache-2.0

package gitpymodules

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/multigres/testkit/assert"
)

// writeTree materializes a relative-path -> content map under a fresh temp
// dir, creating intermediate directories, and returns the root's absolute
// path. All discovery tests are pure filesystem: no daemon, no uv, no git.
func writeTree(t *testing.T, files map[string]string) string {
	t.Helper()
	c := assert.NewAborting(t)
	root := t.TempDir()
	for rel, content := range files {
		p := filepath.Join(root, rel)
		c.NoError(os.MkdirAll(filepath.Dir(p), 0o755), "mkdir %s", filepath.Dir(p))
		c.NoError(os.WriteFile(p, []byte(content), 0o644), "write %s", p)
	}
	return root
}

// Discover must report exactly the scripts under scripts/ and exactly the
// top-level __init__.py-bearing directories, leaving root-level .py clutter
// (noxfile.py, conftest.py) and an __init__.py-less tests/ dir unreported.
func TestDiscoverFindsScriptsAndPackages(t *testing.T) {
	c := assert.NewCollecting(t)
	root := writeTree(t, map[string]string{
		"scripts/rotate.py":     "#!/usr/bin/env python3\n# Rotates the API keys\nimport os\n",
		"scripts/check.py":      "import sys\n",
		"ops_tools/__init__.py": "# Operational helpers for the ops_tools package.\n",
		"ops_tools/keys.py":     "x = 1\n",
		"noxfile.py":            "import nox\n",
		"conftest.py":           "import pytest\n",
		"tests/test_rotate.py":  "def test_rotate():\n    pass\n",
	})

	scripts, packages, err := Discover(root)
	c.Require().NoError(err, "discover")

	c.Require().Len(scripts, 2, "got %d scripts %+v, want exactly 2", len(scripts), scripts)
	if scripts[0].Name != "check" || scripts[1].Name != "rotate" {
		t.Fatalf("scripts = %q, %q; want check, rotate (sorted by name)", scripts[0].Name, scripts[1].Name)
	}
	for _, s := range scripts {
		c.Eq(filepath.Join(root, "scripts", s.Name+".py"), s.Path, "script %s path = %q, want", s.Name, s.Path)
	}
	c.Eq("Rotates the API keys", scripts[1].Description, "rotate description")
	c.Eq("", scripts[0].Description, "check description")

	c.Require().Len(packages, 1, "got %d packages %+v, want exactly 1", len(packages), packages)
	c.Require().Eq("ops_tools", packages[0].Name, "package")
	c.Eq(filepath.Join(root, "ops_tools"), packages[0].Path, "package path")
	c.Eq("Operational helpers for the ops_tools package.", packages[0].Description, "package description")
}

// A scripts/ dir holding an __init__.py is still exclusively a script home:
// it must never be reported as a package, and an __init__.py under scripts/
// is not a script either (only *.py there are, and __init__.py is one --
// which is why scripts/ is reserved for scripts, never a package).
func TestDiscoverNeverTreatsScriptsDirAsPackage(t *testing.T) {
	c := assert.NewAborting(t)
	root := writeTree(t, map[string]string{
		"scripts/__init__.py": "# scripts is not a package\n",
		"scripts/tool.py":     "# A tool\n",
	})

	scripts, packages, err := Discover(root)
	c.NoError(err, "discover")
	c.Empty(packages, "packages")
	c.False(len(scripts) != 2 || scripts[0].Name != "__init__" || scripts[1].Name != "tool", "scripts = %+v, want __init__ and tool", scripts)
}

// Dot directories (where .git lives) are invisible to discovery even when
// they carry an __init__.py-bearing subtree. The dot rule is about
// directories only: a dot-prefixed *.py FILE under scripts/ is still a
// scripts/*.py file per the walk's contract, so the fixture keeps one and
// expects it reported.
func TestDiscoverIgnoresDotDirectories(t *testing.T) {
	c := assert.NewAborting(t)
	root := writeTree(t, map[string]string{
		".git/hooks/__init__.py": "# a stray package inside .git\n",
		".git/config":            "[core]\n",
		".hidden/__init__.py":    "# also hidden\n",
		"real_pkg/__init__.py":   "# visible\n",
		"scripts/only_script.py": "# visible script\n",
		"scripts/.stowaway.py":   "# a dot file under scripts/\n",
	})

	scripts, packages, err := Discover(root)
	c.NoError(err, "discover")
	c.False(len(packages) != 1 || packages[0].Name != "real_pkg", "packages = %+v, want exactly real_pkg: nothing under a dot directory may leak", packages)
	c.False(len(scripts) != 2 || scripts[0].Name != ".stowaway" || scripts[1].Name != "only_script", "scripts = %+v, want exactly .stowaway and only_script", scripts)
}

func TestDescriptionFromCommentSkipsShebang(t *testing.T) {
	c := assert.NewAborting(t)
	p := filepath.Join(t.TempDir(), "rotate.py")
	content := "#!/usr/bin/env python3\n# Rotates the API keys\nimport os\n"
	c.NoError(os.WriteFile(p, []byte(content), 0o644), "write")
	got, err := descriptionFromComment(p)
	c.NoError(err, "descriptionFromComment")
	c.Eq("Rotates the API keys", got, "got")
}

func TestDescriptionFromCommentNoShebang(t *testing.T) {
	c := assert.NewAborting(t)
	p := filepath.Join(t.TempDir(), "check.py")
	content := "# Checks replica lag\nimport sys\n"
	c.NoError(os.WriteFile(p, []byte(content), 0o644), "write")
	got, err := descriptionFromComment(p)
	c.NoError(err, "descriptionFromComment")
	c.Eq("Checks replica lag", got, "got")
}

// The first non-blank line being code means no description, no matter what
// comments follow it.
func TestDescriptionFromCommentEmptyWhenCodeFirst(t *testing.T) {
	c := assert.NewCollecting(t)
	for name, content := range map[string]string{
		"code_first.py":   "import os\n# not a description\n",
		"empty.py":        "",
		"blank_only.py":   "\n\n\n",
		"blank_then_code": "\n\nimport os\n# not a description\n",
	} {
		p := filepath.Join(t.TempDir(), name)
		c.Require().NoError(os.WriteFile(p, []byte(content), 0o644), "write")
		got, err := descriptionFromComment(p)
		c.Require().NoError(err, "%s: descriptionFromComment", name)
		c.Eq("", got, "%s: got %q, want empty", name, got)
	}
}

// Only the single first comment line is ever taken: a consecutive comment
// line is not appended, and a blank line after a comment has started ends
// the search.
func TestDescriptionFromCommentStopsAtFirstLine(t *testing.T) {
	c := assert.NewCollecting(t)
	for name, content := range map[string]string{
		"consecutive.py":   "# First\n# Second\n",
		"blank_between.py": "# First\n\n# Second\n",
	} {
		p := filepath.Join(t.TempDir(), name)
		c.Require().NoError(os.WriteFile(p, []byte(content), 0o644), "write")
		got, err := descriptionFromComment(p)
		c.Require().NoError(err, "%s: descriptionFromComment", name)
		c.Eq("First", got, "%s: got %q, want %q (never a multi-line block)", name, got, "First")
	}
}

// Blank lines between the shebang and the comment are skipped, and the "#"
// plus at most ONE following space is stripped -- extra spaces survive.
func TestDescriptionFromCommentSkipsBlanksAndKeepsSpacing(t *testing.T) {
	c := assert.NewAborting(t)
	p := filepath.Join(t.TempDir(), "spacing.py")
	content := "#!/usr/bin/env python3\n\n   \n#  Two spaces survive\nimport os\n"
	c.NoError(os.WriteFile(p, []byte(content), 0o644), "write")
	got, err := descriptionFromComment(p)
	c.NoError(err, "descriptionFromComment")
	c.Eq(" Two spaces survive", got, "got")
}

// A shebang-looking line that is not the file's first line is an ordinary
// comment (its text is the description).
func TestDescriptionFromCommentShebangOnlyOnFirstLine(t *testing.T) {
	c := assert.NewAborting(t)
	p := filepath.Join(t.TempDir(), "late_shebang.py")
	content := "\n#!/usr/bin/env python3\nimport os\n"
	c.NoError(os.WriteFile(p, []byte(content), 0o644), "write")
	got, err := descriptionFromComment(p)
	c.NoError(err, "descriptionFromComment")
	c.Eq("!/usr/bin/env python3", got, "got")
}
