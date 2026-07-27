// Package helpersembed bundles static assets embedded into the fundi binary.
package helpersembed

import "embed"

// Dir is the directory the extension is bundled under, the root of every path
// inside Helpers, and the leaf it installs to under pi's extensions directory.
// Named once because it has to agree in all three places.
const Dir = "fundi-helpers"

// Helpers holds the fundi-helpers pi extension source files.
// Walk this filesystem to install: each entry's path is rooted at Dir.
//
//go:embed all:fundi-helpers
var Helpers embed.FS
