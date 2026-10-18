// SPDX-License-Identifier: Apache-2.0

package skills

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/multigres/testkit/assert"
)

func TestValidateFilePath(t *testing.T) {
	c := assert.NewAborting(t)

	tests := []struct {
		name    string
		path    string
		wantErr bool
		// wantRule pins WHICH rule refused: without it a row passes when its
		// own guard is deleted but a neighbouring one happens to catch the
		// path (a ".." segment also starts with ".").
		wantRule string
	}{
		{name: "empty", path: "", wantErr: true, wantRule: "is empty"},
		{name: "absolute", path: "/etc/passwd", wantErr: true, wantRule: "must be relative"},
		{name: "backslash", path: `references\api.md`, wantErr: true, wantRule: "contains a backslash"},
		{name: "nul byte", path: "references/a\x00.md", wantErr: true, wantRule: "contains a NUL byte"},
		{name: "empty segment", path: "references//api.md", wantErr: true, wantRule: "has an empty segment"},
		{name: "dot segment", path: "./references/api.md", wantErr: true, wantRule: `has a "." segment`},
		{name: "dotdot segment", path: "../api.md", wantErr: true, wantRule: `has a ".." segment`},
		{name: "hidden segment", path: ".hidden/api.md", wantErr: true, wantRule: `has a segment starting with "."`},
		{name: "skill md itself", path: "SKILL.md", wantErr: true, wantRule: "is SKILL.md"},
		{name: "nested reference", path: "references/api.md"},
		{name: "top-level script", path: "run.sh"},
	}

	// Each refusal must be its own error: a caller (and a skill author) reads
	// the message to know which rule it broke.
	seen := make(map[string]string, len(tests))
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := c.Sub(t)
			err := ValidateFilePath(tc.path)
			if !tc.wantErr {
				c.NoError(err, "ValidateFilePath(%q) must accept", tc.path)
				return
			}
			c.Error(err, "ValidateFilePath(%q) must refuse", tc.path)
			if err == nil {
				return
			}
			msg := err.Error()
			c.True(strings.Contains(msg, fmt.Sprintf("%q", tc.path)),
				"error must name the path %q; got %q", tc.path, msg)
			c.StrContains(msg, tc.wantRule, "error must name the rule that refused %q", tc.path)
			if other, ok := seen[msg]; ok {
				c.Fail("refusals %q and %q produced the same error %q; each rule needs its own",
					other, tc.name, msg)
			}
			seen[msg] = tc.name
		})
	}
}

func TestValidateFiles(t *testing.T) {
	c := assert.NewAborting(t)

	tests := []struct {
		name       string
		files      []File
		wantErr    bool
		wantInErrs string
	}{
		{
			name: "valid",
			files: []File{
				{Path: "references/api.md", Mode: 0o644, Content: []byte("hello")},
				{Path: "run.sh", Mode: 0o755, Content: []byte("#!/bin/sh\n")},
			},
		},
		{
			name: "duplicate path",
			files: []File{
				{Path: "a.txt", Mode: 0o644, Content: []byte("one")},
				{Path: "a.txt", Mode: 0o644, Content: []byte("two")},
			},
			wantErr:    true,
			wantInErrs: `duplicate file path "a.txt"`,
		},
		{
			name: "bad mode",
			files: []File{
				{Path: "a.txt", Mode: 0o600, Content: []byte("one")},
			},
			wantErr:    true,
			wantInErrs: `file "a.txt" has mode 0600`,
		},
		{
			name: "file over cap",
			files: []File{
				{Path: "big.bin", Mode: 0o644, Content: make([]byte, MaxFileBytes+1)},
			},
			wantErr:    true,
			wantInErrs: `file "big.bin" is 1048577 bytes; the limit is 1048576`,
		},
		{
			name: "skill total over cap",
			files: []File{
				{Path: "a.bin", Mode: 0o644, Content: make([]byte, 5<<20)},
				{Path: "b.bin", Mode: 0o644, Content: make([]byte, 5<<20)},
			},
			wantErr:    true,
			wantInErrs: `skill total is 10485760 bytes; the limit is 8388608`,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := c.Sub(t)
			err := ValidateFiles(tc.files)
			if !tc.wantErr {
				c.NoError(err, "ValidateFiles must accept")
				return
			}
			c.Error(err, "ValidateFiles must refuse")
			if err == nil {
				return
			}
			c.StrContains(err.Error(), tc.wantInErrs)
		})
	}
}

func TestTreeHash(t *testing.T) {
	c := assert.NewAborting(t)

	base := []File{
		{Path: "references/api.md", Mode: 0o644, Content: []byte("hello")},
		{Path: "run.sh", Mode: 0o755, Content: []byte("echo hi\n")},
	}
	want := TreeHash(base)
	c.Len(want, 64, "TreeHash must be 64 lowercase hex chars; got %q", want)
	c.Eq(strings.ToLower(want), want, "TreeHash must be lowercase")

	t.Run("order independent", func(t *testing.T) {
		c := c.Sub(t)
		reordered := []File{base[1], base[0]}
		c.Eq(want, TreeHash(reordered), "file order must not change the hash")
	})

	t.Run("one byte changes the hash", func(t *testing.T) {
		c := c.Sub(t)
		changed := []File{
			{Path: "references/api.md", Mode: 0o644, Content: []byte("hellp")},
			{Path: "run.sh", Mode: 0o755, Content: []byte("echo hi\n")},
		}
		c.NotEq(want, TreeHash(changed), "a content change must change the hash")
	})

	t.Run("one mode changes the hash", func(t *testing.T) {
		c := c.Sub(t)
		changed := []File{
			{Path: "references/api.md", Mode: 0o644, Content: []byte("hello")},
			{Path: "run.sh", Mode: 0o644, Content: []byte("echo hi\n")},
		}
		c.NotEq(want, TreeHash(changed), "a mode change must change the hash")
	})

	t.Run("one path changes the hash", func(t *testing.T) {
		c := c.Sub(t)
		changed := []File{
			{Path: "references/api.txt", Mode: 0o644, Content: []byte("hello")},
			{Path: "run.sh", Mode: 0o755, Content: []byte("echo hi\n")},
		}
		c.NotEq(want, TreeHash(changed), "a path change must change the hash")
	})

	t.Run("fixed input has a fixed hash", func(t *testing.T) {
		c := c.Sub(t)
		c.Eq("f1da57f232345cf0f2c7d6c088bb25a828e2d9e9e5002206a799fe5aa0e2d563", want,
			"the tree hash is a content address; changing its format must be a deliberate, "+
				"migration-aware decision")
	})

	t.Run("empty list hashes the empty input", func(t *testing.T) {
		c := c.Sub(t)
		c.Eq("e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855", TreeHash(nil),
			"an empty file set is the sha256 of no bytes")
	})
}

func TestReadSkillDir(t *testing.T) {
	c := assert.NewAborting(t)

	dir := t.TempDir()
	write := func(rel, content string, perm os.FileMode) {
		t.Helper()
		path := filepath.Join(dir, filepath.FromSlash(rel))
		c.NoError(os.MkdirAll(filepath.Dir(path), 0o755))
		c.NoError(os.WriteFile(path, []byte(content), perm))
	}

	write("SKILL.md", "---\nname: demo\ndescription: demo\n---\nbody\n", 0o644)
	write("references/a.md", "reference body", 0o644)
	write("run.sh", "#!/bin/sh\n", 0o755)
	write(".DS_Store", "junk", 0o644)
	write(".git/x", "junk", 0o644)
	write("__pycache__/x.pyc", "junk", 0o644)

	files, err := ReadSkillDir(dir)
	c.NoError(err)
	c.EqDiff([]File{
		{Path: "references/a.md", Mode: 0o644, Content: []byte("reference body")},
		{Path: "run.sh", Mode: 0o755, Content: []byte("#!/bin/sh\n")},
	}, files, "SKILL.md, hidden entries and __pycache__ must be skipped; the rest kept with reduced modes")
}

func TestReadSkillDirRefusesSymlink(t *testing.T) {
	c := assert.NewAborting(t)

	dir := t.TempDir()
	c.NoError(os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("---\nname: demo\n---\n"), 0o644))
	c.NoError(os.MkdirAll(filepath.Join(dir, "references"), 0o755))
	c.NoError(os.Symlink("../SKILL.md", filepath.Join(dir, "references", "link.md")))

	_, err := ReadSkillDir(dir)
	c.Error(err, "a symlink must be refused, never followed")
	if err == nil {
		return
	}
	c.StrContains(err.Error(), "symlink references/link.md in skill directory is not allowed")
}
