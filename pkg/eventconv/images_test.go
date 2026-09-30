package eventconv_test

import (
	"encoding/base64"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"

	"go.graveland.dev/rafiki/pkg/eventconv"
	"go.graveland.dev/rafiki/pkg/store"

	"github.com/multigres/testkit/assert"
)

const png1x1 = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg=="

func png1x1Bytes(t *testing.T) []byte {
	t.Helper()
	data, err := base64.StdEncoding.DecodeString(png1x1)
	if err != nil {
		t.Fatalf("decode png1x1: %v", err)
	}
	return data
}

func TestBlocksFromParamCarriesImage(t *testing.T) {
	c := assert.NewAborting(t)
	p := anthropic.NewUserMessage(
		anthropic.NewImageBlockBase64("image/png", png1x1),
		anthropic.NewTextBlock("look"),
	)

	blocks := eventconv.BlocksFromParam(p)

	c.Len(blocks, 2, "got %d blocks, want 2", len(blocks))
	img := blocks[0].GetImage()
	c.NotNil(img, "block 0 is %T, want an image", blocks[0].Block)
	c.Eq("image/png", img.MediaType, "media type")
	c.EqDeep(png1x1Bytes(t), img.Data, "decoded image bytes")
	txt := blocks[1].GetText()
	c.NotNil(txt, "block 1 is %T, want text", blocks[1].Block)
	c.Eq("look", txt.Text, "block 1 text")
	c.Eq(int32(0), blocks[0].Index, "block 0 index")
	c.Eq(int32(1), blocks[1].Index, "block 1 index")
}

func TestBlocksFromParamURLImageKeepsTheBlock(t *testing.T) {
	c := assert.NewAborting(t)
	p := anthropic.NewUserMessage(anthropic.ContentBlockParamUnion{
		OfImage: &anthropic.ImageBlockParam{
			Source: anthropic.ImageBlockParamSourceUnion{
				OfURL: &anthropic.URLImageSourceParam{URL: "https://example.com/x.png"},
			},
		},
	})

	blocks := eventconv.BlocksFromParam(p)

	c.Len(blocks, 1, "got %d blocks, want 1", len(blocks))
	img := blocks[0].GetImage()
	c.NotNil(img, "block is %T, want an image", blocks[0].Block)
	c.Len(img.Data, 0, "a URL source carries no bytes")
}

func TestBlocksFromParamUndecodableImageKeepsTheBlock(t *testing.T) {
	c := assert.NewAborting(t)
	p := anthropic.NewUserMessage(anthropic.NewImageBlockBase64("image/png", "!!!not base64"))

	blocks := eventconv.BlocksFromParam(p)

	c.Len(blocks, 1, "got %d blocks, want 1", len(blocks))
	img := blocks[0].GetImage()
	c.NotNil(img, "block is %T, want an image", blocks[0].Block)
	c.Eq("image/png", img.MediaType, "media type")
	c.Len(img.Data, 0, "undecodable base64 must not carry bytes")
}

func TestToolResultImageSurvivesBlocksFromParam(t *testing.T) {
	c := assert.NewAborting(t)
	p := anthropic.NewUserMessage(anthropic.ContentBlockParamUnion{
		OfToolResult: &anthropic.ToolResultBlockParam{
			ToolUseID: "tu_1",
			Content: []anthropic.ToolResultBlockParamContentUnion{
				{OfText: &anthropic.TextBlockParam{Text: "shot"}},
				{OfImage: &anthropic.ImageBlockParam{
					Source: anthropic.ImageBlockParamSourceUnion{
						OfBase64: &anthropic.Base64ImageSourceParam{Data: png1x1, MediaType: "image/png"},
					},
				}},
			},
		},
	})

	blocks := eventconv.BlocksFromParam(p)

	c.Len(blocks, 1, "got %d blocks, want 1", len(blocks))
	tr := blocks[0].GetToolResult()
	c.NotNil(tr, "block is %T, want a tool result", blocks[0].Block)
	c.Len(tr.Content, 2, "got %d content blocks, want 2", len(tr.Content))
	txt := tr.Content[0].GetText()
	c.NotNil(txt, "content 0 is %T, want text", tr.Content[0].Block)
	c.Eq("shot", txt.Text, "content 0 text")
	img := tr.Content[1].GetImage()
	c.NotNil(img, "content 1 is %T, want an image", tr.Content[1].Block)
	c.EqDeep(png1x1Bytes(t), img.Data, "decoded image bytes")
}

func TestEventsFromMessagesCarriesUserImage(t *testing.T) {
	c := assert.NewAborting(t)
	msgs := []store.Message{
		{Ordinal: 0, Param: anthropic.NewUserMessage(
			anthropic.NewImageBlockBase64("image/png", png1x1),
			anthropic.NewTextBlock("look"),
		)},
	}

	evs := eventconv.EventsFromMessages("c_1", msgs)

	c.Len(evs, 1, "got %d events, want 1", len(evs))
	content := evs[0].GetUserMessage().GetContent()
	c.Len(content, 2, "got %d blocks, want 2", len(content))
	c.EqDeep(png1x1Bytes(t), content[0].GetImage().GetData(), "event image bytes")
}
