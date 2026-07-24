package analyze

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"

	"github.com/timescale/rafiki/insights"
	"github.com/timescale/rafiki/llm"
)

// builtinDetectorPrompt is the default system prompt for Detect, judging a
// single compacted conversation transcript against three axes.
const builtinDetectorPrompt = `You are the detector pass of an agent-conversation analyzer. You are given
one agent conversation transcript (already compacted; some middle turns may
have been elided — you will see a marker turn where that happened). Judge it
against exactly three axes and call report_findings with your verdict.

Axes:

1. skill-gap — Compare the transcript's available_skills (listed in the
   header) against what the agent actually invoked. An unused skill that
   clearly matched the task is a DISCOVERABILITY finding (recommendation
   kind "skill-edit", improving the skill's trigger/description). A gap with
   no matching skill at all — the agent improvised a multi-step procedure
   that would recur — is a NEW-SKILL finding (recommendation kind
   "new-skill").
2. knowledge-to-persist — A fact the agent derived only after real,
   expensive work (multiple tool calls, a long investigation, trial and
   error) that would be cheap to recall next time and durable (not
   specific to this one conversation's transient state). Recommendation
   kind "memory".
3. grind — Wasted effort: error-retry loops, redundant re-reads of the same
   file/resource, prompt-cache collapse (a break in an otherwise stable
   prefix), or many turns producing tiny output. Estimate grind_tokens as
   your best guess at tokens that a smarter approach would have avoided.
   Recommendation kind is usually "none" (grind is diagnostic, not always
   actionable) unless the fix is a clear skill-edit or memory item.

Rules:
- Cite only ordinals you actually saw content for. If the transcript
  contains a compaction-elision marker turn, do not cite any ordinal inside
  the elided range.
- Prefer no finding over a weak one: an axis with nothing worth reporting
  gets verdict "ok" (or "n/a" if the axis genuinely doesn't apply, e.g. no
  skills were available to miss) and no Finding entry.
- topic_key must be a stable, short kebab-case slug that would match across
  conversations hitting the same underlying issue (e.g.
  "missing-vacuum-skill", not "issue-with-vacuum-in-this-chat").
- outcome is one line: what the agent was asked to do and what happened.
- Every finding you report must have at least one evidence citation.

Call report_findings exactly once with your full verdict.`

// detectorOutput mirrors the subset of Analysis that the model fills in via
// report_findings; the caller (Detect) attaches the remaining
// provenance/metrics fields.
type detectorOutput struct {
	Outcome  string            `json:"outcome"`
	Verdicts map[string]string `json:"verdicts"`
	Findings []Finding         `json:"findings"`
}

// Detect runs the schema-forced per-conversation detector pass: it sends t
// (rendered as markdown) to p.DetectorModel under p.EffectiveDetectorPrompt,
// forcing the report_findings tool, and parses the result into an Analysis.
// t is assumed already compacted by the caller — Detect does not call
// Compact itself. owner is the conversation attribution recorded on the
// underlying llm.Conversation. pricer may be nil, in which case CostUSD is 0.
//
// On a malformed or unparseable report_findings call, Detect retries ONCE,
// appending the parse error as a follow-up user turn and re-forcing the
// tool. A second failure returns the error.
func Detect(ctx context.Context, c *llm.Client, t *insights.Transcript, p *Profile, owner string, pricer insights.Pricer) (*Analysis, error) {
	model := p.DetectorModel
	sys := p.EffectiveDetectorPrompt(builtinDetectorPrompt)

	conv, err := c.Conversation(ctx, llm.NewConversation(owner, "analyze"), llm.Model(model), llm.SystemText(sys))
	if err != nil {
		return nil, fmt.Errorf("analyze: detect: %w", err)
	}

	tools := []anthropic.ToolUnionParam{reportFindingsTool()}
	md := renderTranscriptMarkdown(t)

	resp, err := conv.Send(ctx, llm.UserText(md),
		llm.WithTools(tools), llm.WithToolChoice("report_findings"), llm.WithSource("analyze"))
	if err != nil {
		return nil, fmt.Errorf("analyze: detect: %w", err)
	}

	out, perr := parseReportFindings(resp)
	if perr != nil {
		retry := fmt.Sprintf("Your report_findings call could not be used: %v\n\n"+
			"Call report_findings again with input that satisfies the schema.", perr)
		resp, err = conv.Send(ctx, retryContent(resp, retry),
			llm.WithTools(tools), llm.WithToolChoice("report_findings"), llm.WithSource("analyze"))
		if err != nil {
			return nil, fmt.Errorf("analyze: detect retry: %w", err)
		}
		out, perr = parseReportFindings(resp)
		if perr != nil {
			return nil, fmt.Errorf("analyze: detect: %w", perr)
		}
	}

	model = string(resp.Model)
	analysis := &Analysis{
		ConversationID:  t.ConversationID,
		DetectorVersion: DetectorVersion,
		Model:           model,
		PromptHash:      p.PromptHash(),
		Outcome:         out.Outcome,
		Verdicts:        out.Verdicts,
		Findings:        out.Findings,
		InputTokens:     resp.Usage.InputTokens,
		OutputTokens:    resp.Usage.OutputTokens,
		CostUSD:         detectCost(pricer, model, resp.Usage),
	}
	return analysis, nil
}

// detectCost prices resp's usage via pricer, returning 0 when pricer is nil
// or has no entry for model.
func detectCost(pricer insights.Pricer, model string, usage anthropic.Usage) float64 {
	if pricer == nil {
		return 0
	}
	price, ok := pricer(model)
	if !ok {
		return 0
	}
	return float64(usage.InputTokens)*price.PromptUSD +
		float64(usage.OutputTokens)*price.CompletionUSD +
		float64(usage.CacheReadInputTokens)*price.CacheReadUSD +
		float64(usage.CacheCreationInputTokens)*price.CacheWriteUSD
}

// parseReportFindings finds the report_findings tool_use block in resp,
// unmarshals its input, and validates the axis/recommendation-kind enums.
func parseReportFindings(resp *anthropic.Message) (*detectorOutput, error) {
	var raw json.RawMessage
	for _, block := range resp.Content {
		if block.Type != "tool_use" {
			continue
		}
		tu := block.AsToolUse()
		if tu.Name == "report_findings" {
			raw = json.RawMessage(tu.Input)
			break
		}
	}
	if raw == nil {
		return nil, fmt.Errorf("no report_findings tool_use block in response (stop_reason=%s)", resp.StopReason)
	}

	var out detectorOutput
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("parse report_findings input: %w", err)
	}
	if err := validateDetectorOutput(&out); err != nil {
		return nil, err
	}
	return &out, nil
}

// validateDetectorOutput enforces the enums report_findings's schema can't
// express as strictly as Go can (Anthropic tool schemas accept but don't
// reject unknown enum values).
func validateDetectorOutput(out *detectorOutput) error {
	if strings.TrimSpace(out.Outcome) == "" {
		return fmt.Errorf("report_findings input missing outcome")
	}
	for _, f := range out.Findings {
		switch f.Axis {
		case "skill-gap", "knowledge-to-persist", "grind":
		default:
			return fmt.Errorf("finding %q has invalid axis %q", f.Title, f.Axis)
		}
		switch f.Recommendation.Kind {
		case "new-skill", "skill-edit", "memory", "none":
		default:
			return fmt.Errorf("finding %q has invalid recommendation.kind %q", f.Title, f.Recommendation.Kind)
		}
		if len(f.Evidence) == 0 {
			return fmt.Errorf("finding %q has no evidence citations", f.Title)
		}
	}
	return nil
}

// reportFindingsTool mirrors Analysis's outcome/verdicts/findings fields.
func reportFindingsTool() anthropic.ToolUnionParam {
	evidenceItem := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"ordinal": map[string]any{"type": "integer", "description": "turn ordinal this quote is drawn from"},
			"quote":   map[string]any{"type": "string"},
		},
		"required": []string{"ordinal", "quote"},
	}
	recommendation := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"kind":       map[string]any{"type": "string", "enum": []string{"new-skill", "skill-edit", "memory", "none"}},
			"skill_name": map[string]any{"type": "string"},
			"summary":    map[string]any{"type": "string"},
		},
		"required": []string{"kind", "summary"},
	}
	findingItem := map[string]any{
		"type": "object",
		"properties": map[string]any{
			"axis":           map[string]any{"type": "string", "enum": []string{"skill-gap", "knowledge-to-persist", "grind"}},
			"title":          map[string]any{"type": "string"},
			"topic_key":      map[string]any{"type": "string", "description": "stable kebab-case slug for cross-conversation grouping"},
			"evidence":       map[string]any{"type": "array", "items": evidenceItem},
			"recommendation": recommendation,
			"confidence":     map[string]any{"type": "number"},
			"grind_tokens":   map[string]any{"type": "integer", "description": "detector-estimated wasted tokens"},
		},
		"required": []string{"axis", "title", "topic_key", "evidence", "recommendation", "confidence"},
	}
	return anthropic.ToolUnionParam{OfTool: &anthropic.ToolParam{
		Name: "report_findings",
		Description: anthropic.String(
			"Report the detector's verdict for this conversation: one-line outcome, " +
				"per-axis verdicts (skill-gap, knowledge-to-persist, grind), and any findings."),
		InputSchema: anthropic.ToolInputSchemaParam{
			Type: "object",
			Properties: map[string]any{
				"outcome": map[string]any{"type": "string", "description": "one-line what-happened summary"},
				"verdicts": map[string]any{
					"type":                 "object",
					"description":          "axis -> ok|finding|n/a",
					"additionalProperties": map[string]any{"type": "string", "enum": []string{"ok", "finding", "n/a"}},
				},
				"findings": map[string]any{"type": "array", "items": findingItem},
			},
			Required: []string{"outcome", "verdicts", "findings"},
		},
	}}
}

// renderTranscriptMarkdown renders t as compact markdown for the detector
// model: a header (owner/persona/source/available_skills) followed by one
// section per turn (ordinal, role, per-turn metrics/skills, then its content
// blocks rendered verbatim for text, one-line for tool_use/tool_result).
func renderTranscriptMarkdown(t *insights.Transcript) string {
	var b strings.Builder

	fmt.Fprintf(&b, "# conversation %s\n", t.ConversationID)
	fmt.Fprintf(&b, "owner: %s | persona: %s | source: %s\n", t.Owner, t.Persona, t.Source)
	if len(t.AvailableSkills) > 0 {
		fmt.Fprintf(&b, "available_skills: %s\n", strings.Join(t.AvailableSkills, ", "))
	}
	b.WriteString("\n")

	for _, turn := range t.Turns {
		var meta []string
		if turn.Model != "" {
			meta = append(meta, turn.Model)
		}
		if turn.InputTokens != 0 || turn.OutputTokens != 0 {
			meta = append(meta, fmt.Sprintf("in=%d out=%d", turn.InputTokens, turn.OutputTokens))
		}
		if len(turn.Skills) > 0 {
			meta = append(meta, "skills="+strings.Join(turn.Skills, ","))
		}

		fmt.Fprintf(&b, "## [%d] %s", turn.Ordinal, turn.Role)
		if len(meta) > 0 {
			fmt.Fprintf(&b, " (%s)", strings.Join(meta, ", "))
		}
		b.WriteString("\n")
		writeTurnContent(&b, turn.Content)
		b.WriteString("\n")
	}
	return b.String()
}

// writeTurnContent renders one turn's content blocks compactly: text
// verbatim, tool_use as a one-liner "tool: name(input)", tool_result as its
// text (string or joined text sub-blocks). Non-block content (a plain
// string) is written through unchanged.
func writeTurnContent(b *strings.Builder, content json.RawMessage) {
	var blocks []map[string]any
	if err := json.Unmarshal(content, &blocks); err != nil {
		var s string
		if err := json.Unmarshal(content, &s); err == nil && s != "" {
			b.WriteString(s)
			b.WriteString("\n")
		}
		return
	}

	for _, blk := range blocks {
		switch blk["type"] {
		case "text":
			if s, ok := blk["text"].(string); ok {
				b.WriteString(s)
				b.WriteString("\n")
			}
		case "tool_use":
			name, _ := blk["name"].(string)
			input, _ := json.Marshal(blk["input"])
			fmt.Fprintf(b, "tool: %s(%s)\n", name, input)
		case "tool_result":
			fmt.Fprintf(b, "tool_result: %s\n", toolResultText(blk["content"]))
		default:
			if kind, ok := blk["type"].(string); ok {
				fmt.Fprintf(b, "[%s]\n", kind)
			}
		}
	}
}

// toolResultText extracts the text of a tool_result block's content, which
// is either a plain string or an array of content blocks (only the text
// sub-blocks are joined; images are dropped — the markdown feeds a text
// model).
func toolResultText(content any) string {
	switch c := content.(type) {
	case string:
		return c
	case []any:
		var parts []string
		for _, e := range c {
			m, ok := e.(map[string]any)
			if !ok {
				continue
			}
			if s, ok := m["text"].(string); ok {
				parts = append(parts, s)
			}
		}
		return strings.Join(parts, " ")
	default:
		return ""
	}
}
