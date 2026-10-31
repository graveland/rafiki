// SPDX-License-Identifier: Apache-2.0

package llm

import (
	"encoding/json"
	"testing"

	convpkg "go.graveland.dev/rafiki/pkg/conv"
	"go.graveland.dev/rafiki/pkg/msg"

	"github.com/multigres/testkit/assert"
)

// toolUseOf asserts b is a ToolUse.
func toolUseOf(t *testing.T, b msg.Block) msg.ToolUse {
	t.Helper()
	tu, ok := b.(msg.ToolUse)
	if !ok {
		t.Fatalf("block %T is not a ToolUse", b)
	}
	return tu
}

// toolResultOf asserts b is a ToolResult.
func toolResultOf(t *testing.T, b msg.Block) msg.ToolResult {
	t.Helper()
	tr, ok := b.(msg.ToolResult)
	if !ok {
		t.Fatalf("block %T is not a ToolResult", b)
	}
	return tr
}

// isText reports whether b is a text block.
func isText(b msg.Block) bool {
	_, ok := b.(msg.Text)
	return ok
}

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
	good := toolUseOf(t, result[0].Content[0])
	c.Eq("bash", good.Name)
	c.Eq("id_good", good.ID)
	// Second block is the replacement text for id_broken
	c.True(isText(result[0].Content[1]), "broken tool_use replaced with text")
	c.True(isText(result[0].Content[2]), "original text block preserved")

	// User: both tool_results kept — model sees the is_error feedback
	c.Eq(2, len(result[1].Content))
	c.Eq("id_good", toolResultOf(t, result[1].Content[0]).ToolUseID)
	c.Eq("id_broken", toolResultOf(t, result[1].Content[1]).ToolUseID, "error feedback preserved")
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
	c.Eq("bash", toolUseOf(t, result[0].Content[0]).Name)
	c.Eq("read", toolUseOf(t, result[0].Content[1]).Name)
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
	c.True(isText(result[0].Content[0]), "id_a replaced with text")
	c.Eq("read", toolUseOf(t, result[0].Content[1]).Name)
	c.True(isText(result[0].Content[2]), "id_c replaced with text")
	c.Eq("bash", toolUseOf(t, result[0].Content[3]).Name)

	// User: all four tool_results kept — error feedback preserved
	c.Eq(4, len(result[1].Content))
	c.Eq("id_a", toolResultOf(t, result[1].Content[0]).ToolUseID)
	c.Eq("id_b", toolResultOf(t, result[1].Content[1]).ToolUseID)
	c.Eq("id_c", toolResultOf(t, result[1].Content[2]).ToolUseID)
	c.Eq("id_d", toolResultOf(t, result[1].Content[3]).ToolUseID)
}
