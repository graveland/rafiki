package agentloop

import (
	"testing"

	"go.graveland.dev/rafiki/pkg/toolmeta"

	"github.com/multigres/testkit/assert"
)

// TestToolResultBlockCarriesImages pins the one place a tool's images become
// SDK content: images first, then the text. Everything downstream — the live
// request, the persisted message row, the event log — inherits this shape.
func TestToolResultBlockCarriesImages(t *testing.T) {
	c := assert.NewAborting(t)
	block := toolResultBlock("tu_1", toolmeta.Result{
		Text:   "label",
		Images: []toolmeta.Image{{MediaType: "image/png", Data: []byte{1, 2, 3}}},
	}, false)

	tr := block.OfToolResult
	c.Require().NotNil(tr, "want a tool_result block")
	c.Eq("tu_1", tr.ToolUseID, "tool use id")
	c.Require().Len(tr.Content, 2, "want an image block then a text block")
	img := tr.Content[0].OfImage
	c.Require().NotNil(img, "first content should be an image")
	c.Eq("image/png", string(img.Source.OfBase64.MediaType), "media type")
	c.Eq("label", tr.Content[1].OfText.Text, "text")

	// A text-only result still yields exactly one text content block.
	plain := toolResultBlock("tu_2", toolmeta.Result{Text: "hi"}, true)
	c.Require().Len(plain.OfToolResult.Content, 1, "want one text block")
	c.Eq("hi", plain.OfToolResult.Content[0].OfText.Text, "text")
	c.True(plain.OfToolResult.IsError.Or(false), "is_error should be set")
}
