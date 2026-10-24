package tui

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// completePath extends a directory path as far as it is unambiguous: a single
// match completes with a trailing separator, several complete to their common
// prefix, none leaves the input alone. It reads the client's filesystem, so it
// is a convenience for the common local executor, not an authority.
func completePath(in string) string {
	if in == "" {
		return in
	}
	expanded := in
	if in == "~" || strings.HasPrefix(in, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return in
		}
		expanded = home + in[1:]
	}
	dir, base := filepath.Split(expanded)
	if dir == "" {
		dir = "."
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return in
	}
	var names []string
	for _, e := range entries {
		if !strings.HasPrefix(e.Name(), base) {
			continue
		}
		if base == "" && strings.HasPrefix(e.Name(), ".") {
			continue
		}
		if isDir(filepath.Join(dir, e.Name()), e) {
			names = append(names, e.Name())
		}
	}
	if len(names) == 0 {
		return in
	}
	sort.Strings(names)
	prefix := names[0]
	for _, n := range names[1:] {
		for !strings.HasPrefix(n, prefix) {
			prefix = prefix[:len(prefix)-1]
		}
	}
	// Re-attach whatever the user typed before the final element, so a "~" or
	// a relative path keeps the form they wrote.
	head := in[:len(in)-len(base)]
	out := head + prefix
	if len(names) == 1 {
		out += string(filepath.Separator)
	}
	return out
}

func isDir(path string, e os.DirEntry) bool {
	if e.IsDir() {
		return true
	}
	if e.Type()&os.ModeSymlink == 0 {
		return false
	}
	st, err := os.Stat(path)
	return err == nil && st.IsDir()
}
