// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/multigres/testkit/assert"
)

// writeSwapFile writes content to path, failing the test on error.
func writeSwapFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// writeSwapPair stages "staged-<name>" holding "new-<name>" and, when live is
// true, a live "<name>" holding "old-<name>".
func writeSwapPair(t *testing.T, dir, name string, live bool) swapPair {
	t.Helper()
	livePath := filepath.Join(dir, name)
	staged := filepath.Join(dir, "staged-"+name)
	if live {
		writeSwapFile(t, livePath, "old-"+name)
	}
	writeSwapFile(t, staged, "new-"+name)
	return swapPair{Staged: staged, Live: livePath}
}

func readSwapFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

func swapDirNames(t *testing.T, dir string) []string {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir %s: %v", dir, err)
	}
	names := make([]string, 0, len(ents))
	for _, e := range ents {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	return names
}

func swapBackups(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	for _, n := range swapDirNames(t, dir) {
		if strings.Contains(n, ".old.") {
			out = append(out, n)
		}
	}
	return out
}

func swapFileExists(t *testing.T, path string) bool {
	t.Helper()
	_, err := os.Stat(path)
	if err == nil {
		return true
	}
	if os.IsNotExist(err) {
		return false
	}
	t.Fatalf("stat %s: %v", path, err)
	return false
}

// injectSwapRename fails renameFile with err when its source path matches
// failOn, and restores the real seam in t.Cleanup.
func injectSwapRename(t *testing.T, failOn func(src string) bool, err error) {
	t.Helper()
	orig := renameFile
	renameFile = func(old, new string) error {
		if failOn(old) {
			return err
		}
		return orig(old, new)
	}
	t.Cleanup(func() { renameFile = orig })
}

func TestSwapReplacesBothInOrder(t *testing.T) {
	c := assert.NewCollecting(t)
	dir := t.TempDir()
	a := writeSwapPair(t, dir, "rafikid", true)
	b := writeSwapPair(t, dir, "rafiki", true)

	var order []string
	orig := renameFile
	renameFile = func(old, new string) error {
		order = append(order, old)
		return orig(old, new)
	}
	t.Cleanup(func() { renameFile = orig })

	c.Require().NoError(swapBinaries([]swapPair{a, b}), "swapBinaries")
	c.Eq("new-rafikid", readSwapFile(t, a.Live), "rafikid content")
	c.Eq("new-rafiki", readSwapFile(t, b.Live), "rafiki content")
	c.EqDeep([]string{a.Staged, b.Staged}, order, "rename order")
}

func TestSwapLeavesNoBackupsOrTempFiles(t *testing.T) {
	c := assert.NewCollecting(t)
	dir := t.TempDir()
	a := writeSwapPair(t, dir, "rafikid", true)
	b := writeSwapPair(t, dir, "rafiki", true)

	c.Require().NoError(swapBinaries([]swapPair{a, b}), "swapBinaries")
	c.EqDeep([]string{"rafiki", "rafikid"}, swapDirNames(t, dir), "directory after swap")
}

func TestSwapSecondRenameFailureRestoresFirst(t *testing.T) {
	c := assert.NewCollecting(t)
	dir := t.TempDir()
	a := writeSwapPair(t, dir, "rafikid", true)
	b := writeSwapPair(t, dir, "rafiki", true)
	injectSwapRename(t, func(src string) bool { return src == b.Staged }, errors.New("boom-second"))

	err := swapBinaries([]swapPair{a, b})
	c.Error(err, "second rename must fail")
	c.Eq("old-rafikid", readSwapFile(t, a.Live), "first live restored")
	c.Eq(0, len(swapBackups(t, dir)), "no .old. backups remain")
	c.True(swapFileExists(t, a.Staged), "first staged file survives")
	c.True(swapFileExists(t, b.Staged), "second staged file survives")
	c.ErrorContains(err, b.Live, "error names the failing live path")
}

func TestSwapFirstRenameFailureLeavesEverythingIntact(t *testing.T) {
	c := assert.NewCollecting(t)
	dir := t.TempDir()
	a := writeSwapPair(t, dir, "rafikid", true)
	b := writeSwapPair(t, dir, "rafiki", true)
	injectSwapRename(t, func(src string) bool { return src == a.Staged }, errors.New("boom-first"))

	c.Error(swapBinaries([]swapPair{a, b}), "first rename must fail")
	c.Eq("old-rafikid", readSwapFile(t, a.Live), "rafikid untouched")
	c.Eq("old-rafiki", readSwapFile(t, b.Live), "rafiki untouched")
	c.Eq(0, len(swapBackups(t, dir)), "no .old. backups remain")
}

func TestSwapFreshInstallNeedsNoBackup(t *testing.T) {
	c := assert.NewCollecting(t)

	dir := t.TempDir()
	a := writeSwapPair(t, dir, "rafikid", false)
	b := writeSwapPair(t, dir, "rafiki", false)
	c.Require().NoError(swapBinaries([]swapPair{a, b}), "fresh install swap")
	c.Eq("new-rafikid", readSwapFile(t, a.Live), "rafikid created")
	c.Eq("new-rafiki", readSwapFile(t, b.Live), "rafiki created")

	dir2 := t.TempDir()
	a2 := writeSwapPair(t, dir2, "rafikid", false)
	b2 := writeSwapPair(t, dir2, "rafiki", false)
	injectSwapRename(t, func(src string) bool { return src == b2.Staged }, errors.New("boom"))
	c.Error(swapBinaries([]swapPair{a2, b2}), "second rename must fail")
	c.False(swapFileExists(t, a2.Live), "first new live removed")
	c.False(swapFileExists(t, b2.Live), "second live never created")
	c.EqDeep([]string{"staged-rafiki", "staged-rafikid"}, swapDirNames(t, dir2), "only staged files remain")
}

func TestSwapFallsBackToCopyWhenLinkFails(t *testing.T) {
	c := assert.NewCollecting(t)
	origLink := linkFile
	linkFile = func(old, new string) error { return errors.New("no hardlinks") }
	t.Cleanup(func() { linkFile = origLink })

	dir := t.TempDir()
	a := writeSwapPair(t, dir, "rafikid", true)
	b := writeSwapPair(t, dir, "rafiki", true)
	c.Require().NoError(swapBinaries([]swapPair{a, b}), "copy fallback swap")
	c.Eq("new-rafikid", readSwapFile(t, a.Live), "rafikid content")
	c.Eq("new-rafiki", readSwapFile(t, b.Live), "rafiki content")

	dir2 := t.TempDir()
	a2 := writeSwapPair(t, dir2, "rafikid", true)
	b2 := writeSwapPair(t, dir2, "rafiki", true)
	injectSwapRename(t, func(src string) bool { return src == b2.Staged }, errors.New("boom"))
	c.Error(swapBinaries([]swapPair{a2, b2}), "second rename must fail")
	c.Eq("old-rafikid", readSwapFile(t, a2.Live), "restored from copied backup")
}

func TestSwapRejectsPairsInDifferentDirectories(t *testing.T) {
	c := assert.NewCollecting(t)
	dir1 := t.TempDir()
	dir2 := t.TempDir()
	a := writeSwapPair(t, dir1, "rafikid", true)
	other := filepath.Join(dir2, "staged-rafiki")
	writeSwapFile(t, other, "new-rafiki")

	c.Error(swapBinaries([]swapPair{{Staged: other, Live: a.Live}}), "different directories rejected")
	c.Eq("old-rafikid", readSwapFile(t, a.Live), "live untouched")
	c.True(swapFileExists(t, a.Staged), "staged untouched")
	c.EqDeep([]string{"staged-rafiki"}, swapDirNames(t, dir2), "other directory untouched")
}

func TestSwapRejectsRelativePaths(t *testing.T) {
	c := assert.NewCollecting(t)
	dir := t.TempDir()
	a := writeSwapPair(t, dir, "rafikid", true)

	c.Error(swapBinaries([]swapPair{{Staged: "staged-rafikid", Live: "rafikid"}}), "relative paths rejected")
	c.Eq("old-rafikid", readSwapFile(t, a.Live), "live untouched")
	c.True(swapFileExists(t, a.Staged), "staged untouched")
}

func TestSwapKeepsBackupWhenRestoreFails(t *testing.T) {
	c := assert.NewCollecting(t)
	dir := t.TempDir()
	a := writeSwapPair(t, dir, "rafikid", true)
	b := writeSwapPair(t, dir, "rafiki", true)
	injectSwapRename(t, func(src string) bool {
		return src == b.Staged || strings.Contains(src, ".old.")
	}, errors.New("boom"))

	err := swapBinaries([]swapPair{a, b})
	c.Error(err, "swap must fail")
	backups := swapBackups(t, dir)
	c.Eq(1, len(backups), "the backup whose restore failed survives")
	c.ErrorContains(err, "boom", "error carries the restore failure")
	c.ErrorContains(err, b.Live, "error names the failing rename")
}
