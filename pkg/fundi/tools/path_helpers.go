package tools

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// resolveToolPath picks the non-empty path from either path or file_path,
// expands ~ to the user's home directory, and when cwd is set resolves a
// relative path against it. The returned string is always absolute.
func resolveToolPath(path, filePath, cwd string) (string, error) {
	p := path
	if p == "" {
		p = filePath
	}
	if p == "" {
		return "", fmt.Errorf("path is required")
	}
	if strings.HasPrefix(p, "~") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("expand ~: %w", err)
		}
		p = filepath.Join(home, p[1:])
	}
	if !filepath.IsAbs(p) && cwd != "" {
		p = filepath.Join(cwd, p)
	}
	if !filepath.IsAbs(p) {
		return "", fmt.Errorf("path must be absolute, got %q (no working directory available)", p)
	}
	return p, nil
}
