package main

import (
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"graveland.dev/pi-controller/cmd/pic/picembed"
)

func TestPicHelpers_EmbedHasExpectedFiles(t *testing.T) {
	var seen []string
	if err := fs.WalkDir(picembed.PicHelpers, "pic-helpers", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		seen = append(seen, path)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{
		"pic-helpers/package.json",
		"pic-helpers/index.ts",
		"pic-helpers/README.md",
	} {
		found := false
		for _, s := range seen {
			if s == expected {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("missing embedded file %q; got: %v", expected, seen)
		}
	}
}

func TestInstallFromEmbed_CopiesFiles(t *testing.T) {
	destDir := t.TempDir()
	if err := installFromEmbed(destDir); err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"package.json", "index.ts", "README.md"} {
		path := filepath.Join(destDir, expected)
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("missing %s: %v", path, err)
		}
		if info.Size() == 0 {
			t.Fatalf("file %s is empty", path)
		}
	}
}
