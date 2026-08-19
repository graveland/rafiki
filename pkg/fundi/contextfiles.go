package fundi

import (
	"strings"

	"go.graveland.dev/rafiki/pkg/paths"
	"go.graveland.dev/rafiki/pkg/projectctx"
)

// LoadContextFiles returns the concatenated instruction-file content for cwd:
// the user-global instruction file (paths.InstructionsFile) first, then
// CLAUDE.md and AGENTS.md at the git root, then CLAUDE.md and AGENTS.md at cwd
// itself - deduped when the git root and cwd are the same directory. Files
// that don't exist are skipped silently (most directories have neither); the
// returned sections are joined with a blank line between them.
//
// The global tier is read from this machine; the project tier (git-root walk,
// include expansion, cwd files) is delegated to pkg/projectctx, which the
// executor also imports so a workspace on another machine is answered by the
// machine holding it.
func LoadContextFiles(cwd string) (string, error) {
	var sections []string

	if s := projectctx.LoadInstructionFile(paths.InstructionsFile()); s != "" {
		sections = append(sections, s)
	}

	project, err := projectctx.LoadProjectContext(cwd)
	if err != nil {
		return "", err
	}
	if project != "" {
		sections = append(sections, project)
	}

	return strings.Join(sections, "\n\n"), nil
}
