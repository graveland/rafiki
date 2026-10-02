// SPDX-License-Identifier: Apache-2.0

package llm

import (
	"testing"

	"github.com/anthropics/anthropic-sdk-go"

	"go.graveland.dev/rafiki/pkg/store"

	"github.com/multigres/testkit/assert"
)

func TestMergeForRequestReplacesEmptyNameToolUses(t *testing.T) {
	c := assert.NewAborting(t)

	// Assistant with a good tool_use and a broken one (empty name)
	assistantContent := []anthropic.ContentBlockParamUnion{
		{
			OfToolUse: &anthropic.ToolUseBlockParam{
				ID:    "id_good",
				Name:  "bash",
				Input: map[string]any{"command": "ls"},
			},
		},
		{
			OfToolUse: &anthropic.ToolUseBlockParam{
				ID:    "id_broken",
				Name:  "",
				Input: map[string]any{"command": "whoami"},
			},
		},
		{OfText: &anthropic.TextBlockParam{Text: "I will run these tools."}},
	}
	assistant := anthropic.MessageParam{
		Role:    anthropic.MessageParamRoleAssistant,
		Content: assistantContent,
	}

	// User message with tool_results for both (is_error for the broken one)
	user := anthropic.MessageParam{
		Role: anthropic.MessageParamRoleUser,
		Content: []anthropic.ContentBlockParamUnion{
			{OfToolResult: &anthropic.ToolResultBlockParam{
				ToolUseID: "id_good",
				Content:   []anthropic.ToolResultBlockParamContentUnion{{OfText: &anthropic.TextBlockParam{Text: "output"}}},
			}},
			{OfToolResult: &anthropic.ToolResultBlockParam{
				ToolUseID: "id_broken",
				Content:   []anthropic.ToolResultBlockParamContentUnion{{OfText: &anthropic.TextBlockParam{Text: "error: empty name"}}},
			}},
		},
	}

	history := []store.Message{
		{Ordinal: 0, Param: assistant},
		{Ordinal: 1, Param: user},
	}

	result := mergeForRequest(history)

	c.Eq(2, len(result), "two messages")

	// Assistant: id_broken replaced with text, id_good and original text kept
	c.Eq(3, len(result[0].Content))
	c.Eq("bash", result[0].Content[0].OfToolUse.Name)
	c.Eq("id_good", result[0].Content[0].OfToolUse.ID)
	// Second block is the replacement text for id_broken
	c.True(result[0].Content[1].OfText != nil, "broken tool_use replaced with text")
	c.True(result[0].Content[2].OfText != nil, "original text block preserved")

	// User: both tool_results kept — model sees the is_error feedback
	c.Eq(2, len(result[1].Content))
	c.Eq("id_good", result[1].Content[0].OfToolResult.ToolUseID)
	c.Eq("id_broken", result[1].Content[1].OfToolResult.ToolUseID, "error feedback preserved")
}

func TestMergeForRequestReplacesOnlyEmptyNames(t *testing.T) {
	c := assert.NewAborting(t)

	assistantContent := []anthropic.ContentBlockParamUnion{
		{OfToolUse: &anthropic.ToolUseBlockParam{ID: "id_1", Name: "bash", Input: map[string]any{"command": "ls"}}},
		{OfToolUse: &anthropic.ToolUseBlockParam{ID: "id_2", Name: "read", Input: map[string]any{"path": "/tmp"}}},
	}
	assistant := anthropic.MessageParam{
		Role:    anthropic.MessageParamRoleAssistant,
		Content: assistantContent,
	}

	history := []store.Message{{Ordinal: 0, Param: assistant}}

	result := mergeForRequest(history)

	c.Eq(1, len(result))
	c.Eq(2, len(result[0].Content))
	c.Eq("bash", result[0].Content[0].OfToolUse.Name)
	c.Eq("read", result[0].Content[1].OfToolUse.Name)
}

func TestMergeForRequestLeavesUserMessagesUntouched(t *testing.T) {
	c := assert.NewAborting(t)

	assistant := anthropic.MessageParam{
		Role:    anthropic.MessageParamRoleAssistant,
		Content: []anthropic.ContentBlockParamUnion{{OfText: &anthropic.TextBlockParam{Text: "hello"}}},
	}
	user := anthropic.MessageParam{
		Role: anthropic.MessageParamRoleUser,
		Content: []anthropic.ContentBlockParamUnion{
			{OfToolUse: &anthropic.ToolUseBlockParam{ID: "id_good", Name: "bash", Input: map[string]any{"command": "ls"}}},
			{OfToolUse: &anthropic.ToolUseBlockParam{ID: "id_broken", Name: "", Input: map[string]any{}}},
		},
	}

	history := []store.Message{
		{Ordinal: 0, Param: assistant},
		{Ordinal: 1, Param: user},
	}

	result := mergeForRequest(history)

	c.Eq(2, len(result))
	c.Eq(2, len(result[1].Content), "user message untouched — replace is assistant-only")
}

func TestMergeForRequestMultipleReplacedToolUses(t *testing.T) {
	c := assert.NewAborting(t)

	assistantContent := []anthropic.ContentBlockParamUnion{
		{OfToolUse: &anthropic.ToolUseBlockParam{ID: "id_a", Name: "", Input: map[string]any{}}},
		{OfToolUse: &anthropic.ToolUseBlockParam{ID: "id_b", Name: "read", Input: map[string]any{"path": "/tmp"}}},
		{OfToolUse: &anthropic.ToolUseBlockParam{ID: "id_c", Name: "", Input: map[string]any{}}},
		{OfToolUse: &anthropic.ToolUseBlockParam{ID: "id_d", Name: "bash", Input: map[string]any{"command": "date"}}},
	}
	assistant := anthropic.MessageParam{
		Role:    anthropic.MessageParamRoleAssistant,
		Content: assistantContent,
	}

	user := anthropic.MessageParam{
		Role: anthropic.MessageParamRoleUser,
		Content: []anthropic.ContentBlockParamUnion{
			{OfToolResult: &anthropic.ToolResultBlockParam{
				ToolUseID: "id_a",
				Content:   []anthropic.ToolResultBlockParamContentUnion{{OfText: &anthropic.TextBlockParam{Text: "err a"}}},
			}},
			{OfToolResult: &anthropic.ToolResultBlockParam{
				ToolUseID: "id_b",
				Content:   []anthropic.ToolResultBlockParamContentUnion{{OfText: &anthropic.TextBlockParam{Text: "ok b"}}},
			}},
			{OfToolResult: &anthropic.ToolResultBlockParam{
				ToolUseID: "id_c",
				Content:   []anthropic.ToolResultBlockParamContentUnion{{OfText: &anthropic.TextBlockParam{Text: "err c"}}},
			}},
			{OfToolResult: &anthropic.ToolResultBlockParam{
				ToolUseID: "id_d",
				Content:   []anthropic.ToolResultBlockParamContentUnion{{OfText: &anthropic.TextBlockParam{Text: "ok d"}}},
			}},
		},
	}

	history := []store.Message{
		{Ordinal: 0, Param: assistant},
		{Ordinal: 1, Param: user},
	}

	result := mergeForRequest(history)

	// Assistant: 4 blocks — two kept (read, bash), two replaced with text
	c.Eq(4, len(result[0].Content))
	// Block 0: replaced (was id_a, now text)
	c.True(result[0].Content[0].OfText != nil, "id_a replaced with text")
	// Block 1: id_b kept
	c.Eq("read", result[0].Content[1].OfToolUse.Name)
	// Block 2: replaced (was id_c, now text)
	c.True(result[0].Content[2].OfText != nil, "id_c replaced with text")
	// Block 3: id_d kept
	c.Eq("bash", result[0].Content[3].OfToolUse.Name)

	// User: all four tool_results kept — error feedback preserved
	c.Eq(4, len(result[1].Content))
	c.Eq("id_a", result[1].Content[0].OfToolResult.ToolUseID)
	c.Eq("id_b", result[1].Content[1].OfToolResult.ToolUseID)
	c.Eq("id_c", result[1].Content[2].OfToolResult.ToolUseID)
	c.Eq("id_d", result[1].Content[3].OfToolResult.ToolUseID)
}
