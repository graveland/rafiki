// SPDX-License-Identifier: Apache-2.0

package skills

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// File is one file of a skill besides SKILL.md. Path is relative to the
// skill's directory, '/'-separated.
type File struct {
	Path    string
	Mode    uint32 // 0o644 or 0o755
	Content []byte
}

const (
	MaxFileBytes  = 1 << 20 // per file
	MaxSkillBytes = 8 << 20 // sum of Content over one skill's files
)

// ValidateFilePath checks one skill file path: relative, '/'-separated, and
// confined to the skill's directory. Each refusal is its own error, and every
// one names the offending path — callers surface these to whoever authored the
// skill, so "invalid path" alone would be useless.
func ValidateFilePath(p string) error {
	if p == "" {
		return fmt.Errorf("skill file path %q is empty", p)
	}
	if strings.HasPrefix(p, "/") {
		return fmt.Errorf("skill file path %q must be relative", p)
	}
	if strings.Contains(p, `\`) {
		return fmt.Errorf("skill file path %q contains a backslash; use %q separators", p, "/")
	}
	if strings.ContainsRune(p, 0) {
		return fmt.Errorf("skill file path %q contains a NUL byte", p)
	}
	for _, seg := range strings.Split(p, "/") {
		switch seg {
		case "":
			return fmt.Errorf("skill file path %q has an empty segment", p)
		case ".":
			return fmt.Errorf("skill file path %q has a %q segment", p, ".")
		case "..":
			return fmt.Errorf("skill file path %q has a %q segment", p, "..")
		}
		if strings.HasPrefix(seg, ".") {
			return fmt.Errorf("skill file path %q has a segment starting with %q", p, ".")
		}
	}
	if p == "SKILL.md" {
		return fmt.Errorf("skill file path %q is SKILL.md, which is not a skill file", p)
	}
	return nil
}

// ValidateFiles checks a whole skill's file set: every path valid and unique,
// every mode one of the two the store allows, and both size caps respected.
// Over-cap is an error, never a truncation — a silently shortened reference
// file is worse than a refused skill.
func ValidateFiles(files []File) error {
	seen := make(map[string]bool, len(files))
	var total int
	for _, f := range files {
		if err := ValidateFilePath(f.Path); err != nil {
			return err
		}
		if seen[f.Path] {
			return fmt.Errorf("duplicate file path %q", f.Path)
		}
		seen[f.Path] = true
		if f.Mode != 0o644 && f.Mode != 0o755 {
			return fmt.Errorf("file %q has mode %#o; the mode must be 0644 or 0755", f.Path, f.Mode)
		}
		total += len(f.Content)
	}
	// The skill budget is checked before the per-file cap on purpose: a set
	// whose files are each too big is also over the total, and the total is
	// the more actionable answer ("this skill is 10 MiB"). A set within the
	// total but with one oversized file still gets the per-file error.
	if total > MaxSkillBytes {
		return fmt.Errorf("skill total is %d bytes; the limit is %d", total, MaxSkillBytes)
	}
	for _, f := range files {
		if len(f.Content) > MaxFileBytes {
			return fmt.Errorf("file %q is %d bytes; the limit is %d", f.Path, len(f.Content), MaxFileBytes)
		}
	}
	return nil
}

// BlobHash is the content address of one file's bytes. It is a distinct
// function from TreeHash so that a store can deduplicate blobs across skills
// without caring which skill (or path) a blob came from.
func BlobHash(content []byte) [32]byte {
	return sha256.Sum256(content)
}

// TreeHash is the content address of a whole skill file set: order-independent
// (files are sorted by path first), and sensitive to every part of every file
// — its path, its reduced mode, and its bytes. Two skills whose file sets
// hash equal are byte-identical, which is what lets a refresh skip a write
// and a push.
func TreeHash(files []File) string {
	sorted := make([]File, len(files))
	copy(sorted, files)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Path < sorted[j].Path })

	h := sha256.New()
	for _, f := range sorted {
		fmt.Fprintf(h, "%s\x00%o\x00%x\n", f.Path, f.Mode, BlobHash(f.Content))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// ReadSkillDir collects a skill's supporting files from its directory. SKILL.md
// itself is the skill (its body lives in the row), so it is skipped; hidden
// entries and __pycache__ are skipped without descending, matching what a
// skill author means by "the files of this skill". A symlink is refused rather
// than followed: a skill's file set must be self-contained and byte-identical
// everywhere it is written, and a link would make the content depend on the
// machine it was read on.
//
// The result is sorted by Path and validated by ValidateFiles.
func ReadSkillDir(dir string) ([]File, error) {
	var files []File
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, relErr := filepath.Rel(dir, path)
		if relErr != nil {
			return relErr
		}
		if rel == "." {
			return nil
		}
		rel = filepath.ToSlash(rel)

		// WalkDir does not follow symlinks, so a link is always reported as
		// itself here; refusing it anywhere (hidden or not) keeps the rule
		// "a symlink is an error" unconditional.
		if d.Type()&fs.ModeSymlink != 0 {
			return fmt.Errorf("symlink %s in skill directory is not allowed", rel)
		}

		name := d.Name()
		if d.IsDir() {
			if name == "__pycache__" || strings.HasPrefix(name, ".") {
				return fs.SkipDir
			}
			return nil
		}
		if strings.HasPrefix(name, ".") {
			return nil
		}
		if !d.Type().IsRegular() {
			return fmt.Errorf("%s is not a regular file", rel)
		}
		if rel == "SKILL.md" {
			return nil
		}

		info, infoErr := d.Info()
		if infoErr != nil {
			return infoErr
		}
		content, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		mode := uint32(0o644)
		if info.Mode().Perm()&0o111 != 0 {
			mode = 0o755
		}
		files = append(files, File{Path: rel, Mode: mode, Content: content})
		return nil
	})
	if err != nil {
		return nil, err
	}

	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	if err := ValidateFiles(files); err != nil {
		return nil, err
	}
	return files, nil
}
