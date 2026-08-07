// Package version exposes the build's git-derived version string.
// Populated via ldflags at build time; falls back to runtime/debug's VCS
// info for `go run` or non-ldflags builds. Returns "unknown" only when
// neither source is available.
package version

import "runtime/debug"

// Version is set at build time via -ldflags "-X go.graveland.dev/rafiki/pkg/version.Version=<hash>".
// When empty (go run, plain go build), String() falls back to the embedded VCS info.
var Version string

// String returns the build version string. When the package-level Version
// variable is set (via ldflags), it is returned unchanged — the caller owns
// the exact string. Otherwise the embedded VCS info is consulted: short git
// commit hash, with "-dirty" suffix when the working tree had uncommitted
// changes at build time. Returns "unknown" when neither source is available.
func String() string {
	if Version != "" {
		return Version
	}
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
