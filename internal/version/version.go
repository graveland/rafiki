// Package version exposes the build's git-derived version string.
// Populated automatically by `go build` from VCS info; returns "unknown"
// for `go run` or non-git builds.
package version

import "runtime/debug"

// String returns the short git commit hash, with "-dirty" suffix if the
// working tree had uncommitted changes at build time. Returns "unknown"
// when no VCS info is embedded.
func String() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "unknown"
	}
	var rev string
	var dirty bool
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			dirty = s.Value == "true"
		}
	}
	if rev == "" {
		return "unknown"
	}
	if len(rev) > 7 {
		rev = rev[:7]
	}
	if dirty {
		rev += "-dirty"
	}
	return rev
}
