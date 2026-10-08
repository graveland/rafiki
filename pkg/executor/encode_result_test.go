package executor

import (
	"testing"

	"go.graveland.dev/rafiki/pkg/toolmeta"

	"github.com/multigres/testkit/assert"
)

// TestEncodeToolResultCarriesImages pins the executor wire's image support: a
// tool result carrying an image must go up as an ImageBlock (not be flattened
// to text), with the text label after it.
func TestEncodeToolResultCarriesImages(t *testing.T) {
	c := assert.NewAborting(t)
	blocks := encodeToolResult(toolmeta.Result{
		Text:   "Read image x.png (image/png, 3 bytes)",
		Images: []toolmeta.Image{{MediaType: "image/png", Data: []byte{1, 2, 3}}},
	})
	c.Require().Len(blocks, 2, "want an image block then a text block")

	img := blocks[0].GetImage()
	c.Require().NotNil(img, "first block should be an image")
	c.Eq("image/png", img.MediaType, "media type")
	c.Eq(string([]byte{1, 2, 3}), string(img.Data), "image bytes")
	c.Eq("Read image x.png (image/png, 3 bytes)", blocks[1].GetText(), "text")
}

// TestEncodeToolResultTextOnly: the common case is one text block.
func TestEncodeToolResultTextOnly(t *testing.T) {
	c := assert.NewAborting(t)
	blocks := encodeToolResult(toolmeta.Result{Text: "hello"})
	c.Require().Len(blocks, 1, "want a single text block")
	c.Eq("hello", blocks[0].GetText(), "text")
}
