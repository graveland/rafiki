// SPDX-License-Identifier: Apache-2.0

package analyze

import (
	"github.com/anthropics/anthropic-sdk-go"

	"go.graveland.dev/rafiki/llm"
)

// retryContent builds the follow-up user turn for a failed schema-forced
// tool call (Detect's report_findings, Draft's propose_skill_edit). If resp
// contains any tool_use block, the assistant turn ended with (at least) one
// tool_use unanswered — the Anthropic API rejects a plain user-text turn
// immediately after it ("tool_use ids were found without tool_result blocks
// immediately after"), so the retry MUST answer EVERY tool_use block in
// resp with a tool_result block (marked as an error) referencing that
// block's ID. With parallel tool use the model can emit more than one
// tool_use block in a single response (e.g. two report_findings calls);
// leaving any of them dangling still 400s, so every ID gets a tool_result,
// not just the first one. Only when resp has no tool_use at all (the model
// replied with text and never called the tool) is a plain user-text turn
// legal.
func retryContent(resp *anthropic.Message, errText string) []anthropic.ContentBlockParamUnion {
	ids := toolUseIDs(resp)
	if len(ids) == 0 {
		return llm.UserText(errText)
	}
	blocks := make([]anthropic.ContentBlockParamUnion, len(ids))
	for i, id := range ids {
		blocks[i] = anthropic.NewToolResultBlock(id, errText, true)
	}
	return blocks
}

// toolUseIDs returns the IDs of every tool_use block in resp's content, in
// order.
func toolUseIDs(resp *anthropic.Message) []string {
	var ids []string
	for _, block := range resp.Content {
		if block.Type != "tool_use" {
			continue
		}
		ids = append(ids, block.AsToolUse().ID)
	}
	return ids
}
