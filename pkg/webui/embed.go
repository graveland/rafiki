// SPDX-License-Identifier: Apache-2.0

// Package webui serves the rafiki web UI from `rafiki web`: a localhost-only
// reverse proxy in front of the daemon's Connect control plane, with a
// single-use launch-code handshake that mints a per-tab session secret. It is
// a pure socket client — nothing here opens a database or speaks OIDC — so
// cmd/rafiki can import it without breaking the two-binary split.
package webui

import (
	"embed"
	"io/fs"
)

//go:embed all:dist
var distFS embed.FS

// Assets returns the built web UI, rooted at dist/.
func Assets() fs.FS {
	sub, err := fs.Sub(distFS, "dist")
	if err != nil {
		// Unreachable: the go:embed directive above fails the build if the
		// literal path dist is absent, so fs.Sub cannot fail.
		panic(err)
	}
	return sub
}

// HasUI reports whether the embedded assets contain index.html.
func HasUI() bool { return hasUI(Assets()) }

// hasUI reports whether fsys holds index.html. Split out so a test can pin the
// rule against an in-memory tree: the real embed ships only .gitkeep.
func hasUI(fsys fs.FS) bool {
	_, err := fs.Stat(fsys, "index.html")
	return err == nil
}
