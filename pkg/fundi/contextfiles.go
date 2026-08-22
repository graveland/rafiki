package fundi

import (
	"fmt"
	"strings"

	"go.graveland.dev/rafiki/pkg/paths"
	"go.graveland.dev/rafiki/pkg/projectctx"
)

// estimatedCharsPerToken converts a token budget to a byte budget for
// truncation purposes. It is a rough, deliberately conservative estimate
// (rounded down from the ~4.24 chars/token measured against a real captured
// vmlx request) — precision doesn't matter here, staying safely under the
// budget does.
const estimatedCharsPerToken = 4

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
	return loadContextFiles(cwd, nil, 0)
}

// loadContextFiles assembles the global and project tiers, then truncates
// the result to budgetTokens (0 = no cap) via truncateContextFiles. When projectOverride
// is non-nil it is the project tier already fetched from the machine holding
// the workspace (possibly empty); a non-nil pointer distinguishes "executor
// answered, with nothing" from "no executor — read the project tier from cwd
// here". nil means load the project tier from cwd, which is correct only when
// the workspace and the agent loop are the same machine.
func loadContextFiles(cwd string, projectOverride *string, budgetTokens int) (string, error) {
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

	return truncateContextFiles(strings.Join(sections, "\n\n"), budgetTokens), nil
}

// truncateContextFiles caps content to approximately budgetTokens tokens,
// cutting at the last newline boundary at or before the byte budget and
// appending a marker naming how much was dropped. Cutting from the end
// preferentially truncates the project tier (joined last in loadContextFiles)
// before ever touching the operator's own global instructions file.
// budgetTokens <= 0 means no cap — content is returned unchanged.
func truncateContextFiles(content string, budgetTokens int) string {
	if budgetTokens <= 0 {
		return content
	}
	byteBudget := budgetTokens * estimatedCharsPerToken
	if len(content) <= byteBudget {
		return content
	}
	cut := strings.LastIndexByte(content[:byteBudget], '\n')
	if cut < 0 {
		cut = byteBudget
	}
	dropped := len(content) - cut
	return fmt.Sprintf("%s\n\n[... %d bytes of context files truncated to fit this model's context budget ...]", content[:cut], dropped)
}
