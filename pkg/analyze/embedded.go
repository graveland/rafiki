// SPDX-License-Identifier: Apache-2.0

package analyze

import (
	"embed"
	"io/fs"
)

// embeddedDir holds the default analyzer directory shipped in the rafiki
// binary: one profile, "default" (see embedded/profiles.yaml's own header
// comment for why it carries no detector.md/draft.md -- DetectorPromptBase/
// DraftPromptBase, when set, REPLACE the builtin prompt rather than append
// to it, so a placeholder file here would silently gut the real detector).
//
//go:embed embedded/profiles.yaml
var embeddedDir embed.FS

// EmbeddedDefaultDir is the default analyzer directory as an fs.FS, rooted
// so "profiles.yaml" resolves at its top level -- pass it straight to
// LoadAnalyzerDirFS. Callers seed it to a real directory on first use
// (cmd/rafikid/agent_cli.go's resolveProfile) rather than loading it
// in-place on every run, so a user's later edits to the seeded copy are
// never shadowed by the embedded original.
var EmbeddedDefaultDir fs.FS = mustSub(embeddedDir, "embedded")

func mustSub(fsys fs.FS, dir string) fs.FS {
	sub, err := fs.Sub(fsys, dir)
	if err != nil {
		panic("analyze: embedded default dir: " + err.Error())
	}
	return sub
}
