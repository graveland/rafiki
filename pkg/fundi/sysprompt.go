package fundi

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

// WorkspaceInfo describes where a child actually landed. Every field is
// resolved by the daemon at spawn; none of it is asked of the model.
type WorkspaceInfo struct {
	ExecutorName  string
	Isolation     string // "none" | "container"
	WorkspaceMode string // "pinned" | "ephemeral"
	Roots         []string
	ReadOnlyRoots []string
	Network       string // "none" | "bridge"
}

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
	// Workspace, when non-nil and Isolation != "none", appends a per-child
	// machine block to the environment section. It varies BETWEEN children, so
	// it belongs at the end — after everything cacheable across children.
	Workspace *WorkspaceInfo
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
	if c.Workspace != nil && c.Workspace.Isolation != "none" {
		sections = append(sections, workspaceBlock(c.Workspace))
	}

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

// workspaceBlock renders the per-child machine section. It is only appended
// for sandboxed children (Isolation != "none"): a native pinned agent is in
// the situation every agent was in before this phase, and telling it so costs
// tokens to convey nothing.
//
// The wording is operationally useful, not decorative — and the last sentence
// is the whole point of the phase: a sandboxed worker that does not know it is
// sandboxed misreads its first denial as a broken repository. Keep it.
func workspaceBlock(w *WorkspaceInfo) string {
	var sb strings.Builder
	sb.WriteString("## Your machine\n\n")

	noun := "machine"
	if w.Isolation == "container" {
		noun = "a container"
	}
	fmt.Fprintf(&sb, "You are running on %s, inside %s. ", w.ExecutorName, noun)

	switch w.WorkspaceMode {
	case "ephemeral":
		sb.WriteString("Your workspace is ephemeral: it was built for you and will be discarded. Anything you do not commit is lost when this agent ends.")
	case "pinned":
		sb.WriteString("Your workspace is pinned to this machine: it will not be moved, and your uncommitted changes live on the host's filesystem.")
	}
	sb.WriteString("\n")

	if len(w.Roots) > 0 {
		sb.WriteString("\n")
		readOnly := map[string]bool{}
		for _, r := range w.ReadOnlyRoots {
			readOnly[r] = true
		}
		for _, root := range w.Roots {
			mode := "read-write"
			label := ""
			if readOnly[root] {
				mode = "read-only"
				label = "the repository this was cloned from"
			} else {
				label = "your checkout"
			}
			fmt.Fprintf(&sb, "  %-8s %-11s %s\n", root, mode, label)
		}
		sb.WriteString("\nNothing outside those paths exists for you — not the host's home directory,\nnot other checkouts. A \"no such file or directory\" outside them is the sandbox\nworking, not a broken repository.")
	}

	if w.Network == "none" {
		sb.WriteString(" There is no network access.")
	}
	return strings.TrimRight(sb.String(), "\n")
}
