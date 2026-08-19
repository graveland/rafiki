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
	return loadContextFiles(cwd, nil)
}

// loadContextFiles assembles the global and project tiers. When projectOverride
// is non-nil it is the project tier already fetched from the machine holding
// the workspace (possibly empty); a non-nil pointer distinguishes "executor
// answered, with nothing" from "no executor — read the project tier from cwd
// here". nil means load the project tier from cwd, which is correct only when
// the workspace and the agent loop are the same machine.
func loadContextFiles(cwd string, projectOverride *string) (string, error) {
	var sections []string

	if s := projectctx.LoadInstructionFile(paths.InstructionsFile()); s != "" {
		sections = append(sections, s)
	}

	var project string
	if projectOverride != nil {
		project = *projectOverride
	} else {
		var err error
		project, err = projectctx.LoadProjectContext(cwd)
		if err != nil {
			return "", err
		}
	}
	if project != "" {
		sections = append(sections, project)
	}

	return strings.Join(sections, "\n\n"), nil
}
