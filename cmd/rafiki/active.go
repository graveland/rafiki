package main

import (
	"os"
	"path/filepath"
	"strings"

	"go.graveland.dev/rafiki/pkg/profile"
)

func activeFilePath(profileName string) string { return profile.ActiveFile(profileName) }

// setActive writes childID to the profile's active-file atomically
// (best-effort). Callers should ignore the returned error — this is a UX
// convenience, not a correctness requirement.
func setActive(profileName, childID string) error {
	path := activeFilePath(profileName)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(childID+"\n"), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// getActive reads the profile's active childID.
// Returns "" if the file is absent, unreadable, or empty.
func getActive(profileName string) string {
	b, err := os.ReadFile(activeFilePath(profileName))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}
