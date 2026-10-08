package executorclient_test

import (
	"testing"

	"go.graveland.dev/rafiki/pkg/executorclient"
	"go.graveland.dev/rafiki/pkg/executorpb"

	"github.com/multigres/testkit/assert"
)

// TestDecodeResultCarriesImages: an image read ON the executor must arrive here
// as a toolmeta.Image, not be dropped as a non-text block.
func TestDecodeResultCarriesImages(t *testing.T) {
	c := assert.NewAborting(t)
	got := executorclient.DecodeResult([]*executorpb.ContentBlock{
		{Block: &executorpb.ContentBlock_Image{Image: &executorpb.ImageBlock{MediaType: "image/png", Data: []byte{9, 8}}}},
		{Block: &executorpb.ContentBlock_Text{Text: "label"}},
	})
	c.Eq("label", got.Text, "text")
	c.Require().Len(got.Images, 1, "image dropped")
	c.Eq("image/png", got.Images[0].MediaType, "media type")
	c.Eq(string([]byte{9, 8}), string(got.Images[0].Data), "image bytes")

	// A text-only result has no images.
	plain := executorclient.DecodeResult([]*executorpb.ContentBlock{
		{Block: &executorpb.ContentBlock_Text{Text: "hi"}},
	})
	c.Eq("hi", plain.Text, "text")
	c.Eq(0, len(plain.Images), "want no images")
}
