package tools

import (
	"fmt"
	"path/filepath"
	"strings"
)

// resolveLSPPosition resolves a symbol position from a file + optional
// line:col or symbol name. Returns absolute path, 0-based line, 0-based col,
// and any error.
func resolveLSPPosition(cwd, path, symbol string, line, col int) (string, int, int, error) {
	absPath, err := resolveToolPath(path, "", cwd)
	if err != nil {
		return "", 0, 0, err
	}
	return absPath, line, col, nil
}

// formatLSPLocations formats a list of locations for tool output.
func formatLSPLocations(locs []LSPLocation, label string) string {
	if len(locs) == 0 {
		return label + ": none"
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "%s (%d):\n", label, len(locs))
	for _, l := range locs {
		path := l.URI
		if strings.HasPrefix(path, "file://") {
			path = path[7:]
		}
		fmt.Fprintf(&sb, "  %s:%d:%d\n", path, l.Line+1, l.Col+1)
	}
	return strings.TrimRight(sb.String(), "\n")
}

// formatLSPCallHierarchy formats call hierarchy items for tool output.
func formatLSPCallHierarchy(items []LSPCallHierarchyItem, label string) string {
	if len(items) == 0 {
		return label + ": none"
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "%s (%d):\n", label, len(items))
	for _, it := range items {
		path := it.URI
		if strings.HasPrefix(path, "file://") {
			path = path[7:]
		}
		fmt.Fprintf(&sb, "  %s — %s:%d:%d\n", it.Name, path, it.Line+1, it.Col+1)
	}
	return strings.TrimRight(sb.String(), "\n")
}

// uriToPath strips the file:// prefix from a URI.
func uriToPath(uri string) string {
	return strings.TrimPrefix(uri, "file://")
}

// relativePath returns a path relative to cwd if possible, otherwise absolute.
func relativePath(cwd, abs string) string {
	rel, err := filepath.Rel(cwd, abs)
	if err != nil {
		return abs
	}
	return rel
}
