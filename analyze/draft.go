// SPDX-License-Identifier: Apache-2.0

package analyze

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"

	"git.graveland.dev/brent/rafiki/insights"
	"git.graveland.dev/brent/rafiki/llm"
)

// builtinDraftPrompt is the default system prompt for Draft, proposing a
// skill-file edit (or new skill) for a single ranked finding.
const builtinDraftPrompt = `You are the drafting pass of an agent-conversation analyzer. You are given
one ranked finding (with its recommendation and evidence) and, if a matching
skill already exists, its current file(s). Call propose_skill_edit with the
full proposed file contents.

Skill file format (SKILL.md):
- YAML frontmatter with at least "name" (kebab-case, matching the skill's
  directory) and "description" (one line, trigger-oriented: when should an
  agent reach for this skill).
- A markdown body giving a concrete, actionable procedure — not vague
  advice. Prefer numbered steps, example commands, and gotchas over prose.

Rules:
- If current files are provided, edit them minimally: preserve unrelated
  content, sections, and structure, and change only what the finding
  justifies. Always output the FULL resulting file contents, not a diff.
- If no current files are provided, this is a new skill: create it at
  skills/<skill-name>/SKILL.md, where <skill-name> is a kebab-case slug
  derived from the finding (prefer the recommendation's skill_name if set).
- rationale is one or two sentences explaining what changed and why, tied
  back to the finding's evidence.

Call propose_skill_edit exactly once with your full proposal.`

// draftOutput mirrors the propose_skill_edit tool input.
type draftOutput struct {
	Files     []SkillFile `json:"files"`
	Rationale string      `json:"rationale"`
}

// Draft runs the schema-forced drafting pass for a single ranked finding: it
// sends the finding (and any current skill file contents) to p.DraftModel
// under p.EffectiveDraftPrompt, forcing the propose_skill_edit tool, and
// parses the result into a SkillEdit. current is empty for a new-skill
// recommendation and non-empty when editing an existing skill file. owner is
// the conversation attribution recorded on the underlying llm.Conversation.
//
// On a malformed, unparseable, or schema-invalid propose_skill_edit call,
// Draft retries ONCE, appending the parse error as a follow-up user turn and
// re-forcing the tool. A second failure returns the error.
//
// pricer prices the returned SkillEdit's InputTokens/OutputTokens/CostUSD
// against the response's actually-served model (resp.Model, which a
// catalog-mediated failover can differ from p.DraftModel) — mirroring
// Detect's own pricer parameter and detectCost helper exactly. nil is safe
// (CostUSD stays 0).
func Draft(ctx context.Context, c *llm.Client, f RankedFinding, current []SkillFile, p *Profile, owner string, pricer insights.Pricer) (*SkillEdit, error) {
	model := p.DraftModel
	sys := p.EffectiveDraftPrompt(builtinDraftPrompt)

	conv, err := c.Conversation(ctx, llm.NewConversation(owner, "analyze"), llm.Model(model), llm.SystemText(sys))
	if err != nil {
		return nil, fmt.Errorf("analyze: draft: %w", err)
	}

	tools := []anthropic.ToolUnionParam{proposeSkillEditTool()}
	msg := renderDraftRequest(f, current)

	resp, err := conv.Send(ctx, llm.UserText(msg),
		llm.WithMaxTokens(int64(p.MaxOutputTokens)), llm.WithTools(tools), llm.WithToolChoice("propose_skill_edit"), llm.WithSource("analyze"))
	if err != nil {
		return nil, fmt.Errorf("analyze: draft: %w", err)
	}

	out, perr := parseProposeSkillEdit(resp)
	if perr != nil {
		retry := fmt.Sprintf("Your propose_skill_edit call could not be used: %v\n\n"+
			"Call propose_skill_edit again with input that satisfies the schema.", perr)
		resp, err = conv.Send(ctx, retryContent(resp, retry),
			llm.WithMaxTokens(int64(p.MaxOutputTokens)), llm.WithTools(tools), llm.WithToolChoice("propose_skill_edit"), llm.WithSource("analyze"))
		if err != nil {
			return nil, fmt.Errorf("analyze: draft retry: %w", err)
		}
		out, perr = parseProposeSkillEdit(resp)
		if perr != nil {
			return nil, fmt.Errorf("analyze: draft: %w", perr)
		}
	}

	servedModel := string(resp.Model)
	return &SkillEdit{
		FindingTitle: f.Title,
		Files:        out.Files,
		Rationale:    out.Rationale,
		InputTokens:  resp.Usage.InputTokens,
		OutputTokens: resp.Usage.OutputTokens,
		CostUSD:      detectCost(pricer, servedModel, resp.Usage),
	}, nil
}

// renderDraftRequest renders the user turn for Draft: the finding
// (title/axis/recommendation/evidence) followed by each current skill file
// as a fenced block headed by its path, and the drafting rules the model
// must follow.
func renderDraftRequest(f RankedFinding, current []SkillFile) string {
	var b strings.Builder

	fmt.Fprintf(&b, "# Finding: %s\n", f.Title)
	fmt.Fprintf(&b, "axis: %s\n", f.Axis)
	fmt.Fprintf(&b, "recommendation: kind=%s", f.Recommendation.Kind)
	if f.Recommendation.SkillName != "" {
		fmt.Fprintf(&b, " skill_name=%s", f.Recommendation.SkillName)
	}
	b.WriteString("\n")
	if f.Recommendation.Summary != "" {
		fmt.Fprintf(&b, "recommendation summary: %s\n", f.Recommendation.Summary)
	}
	if len(f.Conversations) > 0 {
		fmt.Fprintf(&b, "occurrences: %d across conversations %s\n", f.Occurrences, strings.Join(f.Conversations, ", "))
	}

	if len(f.Evidence) > 0 {
		b.WriteString("\nevidence:\n")
		for _, e := range f.Evidence {
			fmt.Fprintf(&b, "- [turn %d] %q\n", e.Ordinal, e.Quote)
		}
	}

	if len(current) == 0 {
		b.WriteString("\nNo existing skill file matches this finding: this is a new skill. " +
			"Create it at skills/<skill-name>/SKILL.md.\n")
	} else {
		b.WriteString("\nCurrent skill file(s) to edit minimally:\n")
		for _, sf := range current {
			fmt.Fprintf(&b, "\n## %s\n```\n%s\n```\n", sf.Path, sf.Content)
		}
	}

	b.WriteString("\nFollow the drafting rules in your system prompt. Call propose_skill_edit with the full proposed file contents.\n")
	return b.String()
}

// parseProposeSkillEdit finds the propose_skill_edit tool_use block in resp,
// unmarshals its input, and validates the file paths/rationale.
func parseProposeSkillEdit(resp *anthropic.Message) (*draftOutput, error) {
	var raw json.RawMessage
	for _, block := range resp.Content {
		if block.Type != "tool_use" {
			continue
		}
		tu := block.AsToolUse()
		if tu.Name == "propose_skill_edit" {
			raw = json.RawMessage(tu.Input)
			break
		}
	}
	if raw == nil {
		return nil, fmt.Errorf("no propose_skill_edit tool_use block in response (stop_reason=%s)", resp.StopReason)
	}

	var out draftOutput
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("parse propose_skill_edit input: %w", err)
	}
	if err := validateDraftOutput(&out); err != nil {
		return nil, err
	}
	return &out, nil
}

// validateDraftOutput enforces the constraints propose_skill_edit's schema
// can't express: at least one file, non-empty relative paths (no leading
// "/" and no ".." path segments), and non-empty content/rationale.
func validateDraftOutput(out *draftOutput) error {
	if len(out.Files) == 0 {
		return fmt.Errorf("propose_skill_edit input has no files")
	}
	if strings.TrimSpace(out.Rationale) == "" {
		return fmt.Errorf("propose_skill_edit input missing rationale")
	}
	for _, f := range out.Files {
		if strings.TrimSpace(f.Path) == "" {
			return fmt.Errorf("propose_skill_edit input has a file with an empty path")
		}
		if err := validateRelativePath(f.Path); err != nil {
			return fmt.Errorf("file %q: %w", f.Path, err)
		}
		if strings.TrimSpace(f.Content) == "" {
			return fmt.Errorf("file %q has empty content", f.Path)
		}
	}
	return nil
}

// validateRelativePath rejects absolute paths, backslash path separators
// (Windows-style traversal), and any path containing a ".." segment.
func validateRelativePath(p string) error {
	if path.IsAbs(p) {
		return fmt.Errorf("path must be relative, got absolute path")
	}
	if strings.Contains(p, "\\") {
		return fmt.Errorf("path must not contain \"\\\" separators")
	}
	// Reject every ".." segment outright, even ones that would be benign
	// after normalization (e.g. "a/../b") — proposed skill paths have no
	// legitimate reason to reference a parent directory at all, so this is
	// the sole traversal defense (no separate path.Clean check needed).
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." {
			return fmt.Errorf("path must not contain \"..\" segments")
		}
	}
	return nil
}

// proposeSkillEditTool describes the propose_skill_edit tool: the full
// proposed file contents plus a short rationale.
func proposeSkillEditTool() anthropic.ToolUnionParam {
	fileItem := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"path":    map[string]any{"type": "string", "description": "repo-relative path, e.g. skills/foo/SKILL.md"},
			"content": map[string]any{"type": "string", "description": "full file contents"},
		},
		"required": []string{"path", "content"},
	}
	return anthropic.ToolUnionParam{OfTool: &anthropic.ToolParam{
		Name: "propose_skill_edit",
		Description: anthropic.String(
			"Propose a skill-file edit (or new skill) addressing the given finding: full " +
				"file contents for each file to write, plus a short rationale."),
		InputSchema: anthropic.ToolInputSchemaParam{
			Type: "object",
			Properties: map[string]any{
				"files":     map[string]any{"type": "array", "items": fileItem},
				"rationale": map[string]any{"type": "string"},
			},
			Required: []string{"files", "rationale"},
		},
	}}
}
