// Package picembed bundles static assets embedded into the pic binary.
package picembed

import "embed"

// PicHelpers holds the pic-helpers pi extension source files.
// Walk this filesystem to install: each entry's path is rooted at "pic-helpers".
//
//go:embed all:pic-helpers
var PicHelpers embed.FS
