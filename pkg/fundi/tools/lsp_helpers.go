package tools

import (
	"fmt"
	"path/filepath"
	"strings"
)

// formatLSPLocations formats a list of locations for tool output.
func formatLSPLocations(locs []LSPLocation, label string) string {
	if len(locs) == 0 {
		return label + ": none"
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "%s (%d):\n", label, len(locs))
	for _, l := range locs {
		path := strings.TrimPrefix(l.URI, "file://")
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
		path := strings.TrimPrefix(it.URI, "file://")
		fmt.Fprintf(&sb, "  %s — %s:%d:%d\n", it.Name, path, it.Line+1, it.Col+1)
	}
	return strings.TrimRight(sb.String(), "\n")
}

// relativePath returns a path relative to cwd if possible, otherwise absolute.
func relativePath(cwd, abs string) string {
	rel, err := filepath.Rel(cwd, abs)
	if err != nil {
		return abs
	}
	return rel
}
