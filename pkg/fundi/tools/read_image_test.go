package tools

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"testing"

	"github.com/multigres/testkit/assert"
)

// TestReadImageReturnsImageBlock pins the whole point of image reads: a PNG
// comes back as an image content block (which the loop, the persisted message
// row and the event log all carry), not as a numbered listing of base64.
func TestReadImageReturnsImageBlock(t *testing.T) {
	c := assert.NewAborting(t)
	dir := t.TempDir()
	p := filepath.Join(dir, "dot.png")

	var buf bytes.Buffer
	c.Require().NoError(png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 2, 2))))
	c.Require().NoError(os.WriteFile(p, buf.Bytes(), 0o644))

	tool := testReadTool(t, NewFileTracker(), "")
	res, err := tool.Execute(context.Background(), ToolInput(fmt.Sprintf(`{"path":%q}`, p)))
	c.NoError(err, "Execute")

	c.Require().Len(res.Blocks, 2, "want an image block and a text label, got %#v", res.Blocks)
	img, ok := res.Blocks[0].(ImageBlock)
	c.True(ok, "first block should be an ImageBlock, got %T", res.Blocks[0])
	c.Eq("image/png", img.MediaType, "media type")
	c.Eq(len(buf.Bytes()), len(img.Data), "image byte count")
	if _, ok := res.Blocks[1].(TextBlock); !ok {
		t.Errorf("second block should be the text label, got %T", res.Blocks[1])
	}

	// And the loop-facing flattening keeps the image.
	meta := res.toToolmeta()
	c.Require().Len(meta.Images, 1, "toToolmeta dropped the image")
	c.Eq("image/png", meta.Images[0].MediaType, "toolmeta media type")
}

// TestReadImageTooLargeIsRefused: an oversized image is refused, not truncated.
func TestReadImageTooLargeIsRefused(t *testing.T) {
	c := assert.NewAborting(t)
	dir := t.TempDir()
	p := filepath.Join(dir, "big.png")
	// A sparse file: the extension is enough to route it down the image path,
	// and the size check fires before any read.
	c.Require().NoError(os.WriteFile(p, make([]byte, maxImageBytes+1), 0o644))

	tool := testReadTool(t, NewFileTracker(), "")
	_, err := tool.Execute(context.Background(), ToolInput(fmt.Sprintf(`{"path":%q}`, p)))
	c.Error(err, "expected a size refusal")
}
