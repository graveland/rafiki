package agentloop

import (
	"testing"

	"go.graveland.dev/rafiki/pkg/msg"
	"go.graveland.dev/rafiki/pkg/toolmeta"

	"github.com/multigres/testkit/assert"
)

// TestToolResultBlockCarriesImages pins the one place a tool's images become
// message content: images first, then the text. Everything downstream — the
// live request, the persisted message row, the event log — inherits this shape.
func TestToolResultBlockCarriesImages(t *testing.T) {
	c := assert.NewAborting(t)
	block := toolResultBlock("tu_1", toolmeta.Result{
		Text:   "label",
		Images: []toolmeta.Image{{MediaType: "image/png", Data: []byte{1, 2, 3}}},
	}, false)

	tr, ok := block.(msg.ToolResult)
	c.Require().True(ok, "want a tool_result block, got %T", block)
	c.Eq("tu_1", tr.ToolUseID, "tool use id")
	c.Require().Len(tr.Content, 2, "want an image block then a text block")
	img, ok := tr.Content[0].(msg.Image)
	c.Require().True(ok, "first content should be an image, got %T", tr.Content[0])
	c.Eq("image/png", img.MediaType, "media type")
	c.EqDeep([]byte{1, 2, 3}, img.Data, "image data")
	text, ok := tr.Content[1].(msg.Text)
	c.Require().True(ok, "second content should be text, got %T", tr.Content[1])
	c.Eq("label", text.Text, "text")

	// A text-only result still yields exactly one text content block.
	plainBlock := toolResultBlock("tu_2", toolmeta.Result{Text: "hi"}, true)
	plain, ok := plainBlock.(msg.ToolResult)
	c.Require().True(ok, "want a tool_result block, got %T", plainBlock)
	c.Require().Len(plain.Content, 1, "want one text block")
	plainText, ok := plain.Content[0].(msg.Text)
	c.Require().True(ok, "want a text block, got %T", plain.Content[0])
	c.Eq("hi", plainText.Text, "text")
	c.True(plain.IsError, "is_error should be set")
}
