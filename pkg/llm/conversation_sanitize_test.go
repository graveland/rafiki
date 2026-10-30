// SPDX-License-Identifier: Apache-2.0

package llm

import (
	"encoding/json"
	"testing"

	convpkg "go.graveland.dev/rafiki/pkg/conv"
	"go.graveland.dev/rafiki/pkg/msg"

	"github.com/multigres/testkit/assert"
)

func TestMergeForRequestReplacesEmptyNameToolUses(t *testing.T) {
	c := assert.NewAborting(t)

	// Assistant with a good tool_use and a broken one (empty name), plus text.
	assistant := &convpkg.Node{Msg: msg.Message{Role: msg.Assistant, Content: []msg.Block{
		msg.ToolUse{ID: "id_good", Name: "bash", Input: json.RawMessage(`{"command":"ls"}`)},
		msg.ToolUse{ID: "id_broken", Name: "", Input: json.RawMessage(`{"command":"whoami"}`)},
		msg.Text{Text: "I will run these tools."},
	}}}

	// User message with tool_results for both (is_error for the broken one).
	user := &convpkg.Node{Msg: msg.Message{Role: msg.User, Content: []msg.Block{
		msg.ToolResult{ToolUseID: "id_good", Content: []msg.Block{msg.Text{Text: "output"}}},
		msg.ToolResult{ToolUseID: "id_broken", Content: []msg.Block{msg.Text{Text: "error: empty name"}}, IsError: true},
	}}}

	result, err := mergeForRequest([]*convpkg.Node{assistant, user})
	c.Require().NoError(err)

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

	assistant := &convpkg.Node{Msg: msg.Message{Role: msg.Assistant, Content: []msg.Block{
		msg.ToolUse{ID: "id_1", Name: "bash", Input: json.RawMessage(`{"command":"ls"}`)},
		msg.ToolUse{ID: "id_2", Name: "read", Input: json.RawMessage(`{"path":"/tmp"}`)},
	}}}

	result, err := mergeForRequest([]*convpkg.Node{assistant})
	c.Require().NoError(err)

	c.Eq(1, len(result))
	c.Eq(2, len(result[0].Content))
	c.Eq("bash", result[0].Content[0].OfToolUse.Name)
	c.Eq("read", result[0].Content[1].OfToolUse.Name)
}

func TestMergeForRequestLeavesUserMessagesUntouched(t *testing.T) {
	c := assert.NewAborting(t)

	assistant := &convpkg.Node{Msg: msg.Message{Role: msg.Assistant, Content: []msg.Block{
		msg.Text{Text: "hello"},
	}}}
	user := &convpkg.Node{Msg: msg.Message{Role: msg.User, Content: []msg.Block{
		msg.ToolUse{ID: "id_good", Name: "bash", Input: json.RawMessage(`{"command":"ls"}`)},
		msg.ToolUse{ID: "id_broken", Name: "", Input: json.RawMessage(`{}`)},
	}}}

	result, err := mergeForRequest([]*convpkg.Node{assistant, user})
	c.Require().NoError(err)

	c.Eq(2, len(result))
	c.Eq(2, len(result[1].Content), "user message untouched — replace is assistant-only")
}

func TestMergeForRequestMultipleReplacedToolUses(t *testing.T) {
	c := assert.NewAborting(t)

	assistant := &convpkg.Node{Msg: msg.Message{Role: msg.Assistant, Content: []msg.Block{
		msg.ToolUse{ID: "id_a", Name: "", Input: json.RawMessage(`{}`)},
		msg.ToolUse{ID: "id_b", Name: "read", Input: json.RawMessage(`{"path":"/tmp"}`)},
		msg.ToolUse{ID: "id_c", Name: "", Input: json.RawMessage(`{}`)},
		msg.ToolUse{ID: "id_d", Name: "bash", Input: json.RawMessage(`{"command":"date"}`)},
	}}}

	user := &convpkg.Node{Msg: msg.Message{Role: msg.User, Content: []msg.Block{
		msg.ToolResult{ToolUseID: "id_a", Content: []msg.Block{msg.Text{Text: "err a"}}},
		msg.ToolResult{ToolUseID: "id_b", Content: []msg.Block{msg.Text{Text: "ok b"}}},
		msg.ToolResult{ToolUseID: "id_c", Content: []msg.Block{msg.Text{Text: "err c"}}},
		msg.ToolResult{ToolUseID: "id_d", Content: []msg.Block{msg.Text{Text: "ok d"}}},
	}}}

	result, err := mergeForRequest([]*convpkg.Node{assistant, user})
	c.Require().NoError(err)

	// Assistant: 4 blocks — two kept (read, bash), two replaced with text
	c.Eq(4, len(result[0].Content))
	c.True(result[0].Content[0].OfText != nil, "id_a replaced with text")
	c.Eq("read", result[0].Content[1].OfToolUse.Name)
	c.True(result[0].Content[2].OfText != nil, "id_c replaced with text")
	c.Eq("bash", result[0].Content[3].OfToolUse.Name)

	// User: all four tool_results kept — error feedback preserved
	c.Eq(4, len(result[1].Content))
	c.Eq("id_a", result[1].Content[0].OfToolResult.ToolUseID)
	c.Eq("id_b", result[1].Content[1].OfToolResult.ToolUseID)
	c.Eq("id_c", result[1].Content[2].OfToolResult.ToolUseID)
	c.Eq("id_d", result[1].Content[3].OfToolResult.ToolUseID)
}
