package main

import (
	"os"
	"path/filepath"
	"strings"
)

func activeFilePath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".pi", "run", "active")
}

// setActive writes childID to the active file atomically (best-effort).
// Callers should ignore the returned error — this is a UX convenience,
// not a correctness requirement.
func setActive(childID string) error {
	path := activeFilePath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(childID+"\n"), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// getActive reads the active childID from the marker file.
// Returns "" if the file is absent, unreadable, or empty.
func getActive() string {
	b, err := os.ReadFile(activeFilePath())
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}
