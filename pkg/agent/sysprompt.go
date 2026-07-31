package agent

import (
	"fmt"
	"runtime"
	"strings"
	"time"
)

// defaultBasePrompt is the default base system prompt for fundi's native
// agent runtime, used when a spawn request supplies no override. It is
// intentionally short: identity, tool conventions, and the reminder that
// this isn't a human terminal session - its output streams to a controller
// (pi's TUI or another agent), so verbosity has a cost.
const defaultBasePrompt = `You are fundi, a coding agent working directly against a real project checkout via file and shell tools. Make the requested change, verify it, and stop - don't narrate every intermediate step. Prefer the provided tools over asking the user to do something manually. Your output streams to a controller, not a human terminal: be concise.`

// SysPromptConfig is the input to BuildSystemPrompt. Base is the runtime's
// default prompt; Override, when non-empty, is SpawnRequest.SystemPrompt and
// REPLACES Base rather than supplementing it. ContextFiles and
// SkillsInventory are pre-rendered sections (produced by LoadContextFiles and
// Task 12's skills loader respectively) and are omitted entirely when empty.
type SysPromptConfig struct {
	Base, Override, Append string
	ContextFiles           string
	SkillsInventory        string
	Cwd, ModelID           string
}

// BuildSystemPrompt assembles the sections of the system prompt in the exact
// order cache stability requires - rafiki places its prompt-cache breakpoint
// over the tools+system prefix, so static content (base, append, context
// files, skills) MUST precede anything that changes turn to turn, and the
// per-turn environment block (which includes today's date) goes last:
//
//	base (Override if set, else Base) -> Append -> ContextFiles ->
//	SkillsInventory -> environment block
//
// Empty optional sections are omitted entirely - no stray blank sections or
// doubled separators.
func BuildSystemPrompt(c SysPromptConfig) string {
	base := c.Base
	if c.Override != "" {
		base = c.Override
	}

	var sections []string
	for _, s := range []string{base, c.Append, c.ContextFiles, c.SkillsInventory} {
		if strings.TrimSpace(s) != "" {
			sections = append(sections, s)
		}
	}
	sections = append(sections, environmentBlock(c.Cwd, c.ModelID))

	return strings.Join(sections, "\n\n")
}

// environmentBlock renders the per-turn environment section: cwd, platform
// (darwin/linux), the model in use, and today's date. This is the one
// section BuildSystemPrompt never omits.
func environmentBlock(cwd, modelID string) string {
	return fmt.Sprintf(
		"# Environment\ncwd: %s\nplatform: %s\nmodel: %s\ndate: %s",
		cwd, runtime.GOOS, modelID, time.Now().Format("2006-01-02"))
}
