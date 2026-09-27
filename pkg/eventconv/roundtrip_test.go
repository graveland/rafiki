package eventconv_test

import (
	"testing"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"

	"github.com/multigres/testkit/assert"
)

// A zero cache_write must survive as "reported zero", distinct from absent.
// Collapsing the two is the failure mode spec §3.3 constraint 2 guards against.
func TestUsageDistinguishesZeroFromAbsent(t *testing.T) {
	c := assert.NewAborting(t)
	reportedZero := &rafikiv1.Usage{CacheWriteTokens: proto.Int64(0)}
	absent := &rafikiv1.Usage{}

	c.NotNil(reportedZero.CacheWriteTokens, "explicit zero was lost")
	c.Nil(absent.CacheWriteTokens, "absent field materialized")

	b, err := protojson.Marshal(reportedZero)
	c.NoError(err, "marshal")
	var back rafikiv1.Usage
	c.NoError(protojson.Unmarshal(b, &back), "unmarshal")
	c.NotNil(back.CacheWriteTokens, "explicit zero lost through protojson: %s", b)
}

// Tool results must be able to carry an image, not just text.
func TestToolResultCarriesImageBlock(t *testing.T) {
	c := assert.NewAborting(t)
	tr := &rafikiv1.ToolResultBlock{
		ToolUseId: "tu_1",
		Content: []*rafikiv1.ContentBlock{{
			Index: 0,
			Block: &rafikiv1.ContentBlock_Image{Image: &rafikiv1.ImageBlock{
				MediaType: "image/png",
				Data:      []byte{0x89, 0x50, 0x4e, 0x47},
			}},
		}},
	}
	b, err := protojson.Marshal(tr)
	c.NoError(err, "marshal")
	var back rafikiv1.ToolResultBlock
	c.NoError(protojson.Unmarshal(b, &back), "unmarshal")
	img := back.Content[0].GetImage()
	c.NotNil(img, "image block lost")
	c.Eq("image/png", img.MediaType, "media type")
}
