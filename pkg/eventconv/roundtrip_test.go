package eventconv_test

import (
	"testing"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
)

// A zero cache_write must survive as "reported zero", distinct from absent.
// Collapsing the two is the failure mode spec §3.3 constraint 2 guards against.
func TestUsageDistinguishesZeroFromAbsent(t *testing.T) {
	reportedZero := &rafikiv1.Usage{CacheWriteTokens: proto.Int64(0)}
	absent := &rafikiv1.Usage{}

	if reportedZero.CacheWriteTokens == nil {
		t.Fatal("explicit zero was lost")
	}
	if absent.CacheWriteTokens != nil {
		t.Fatal("absent field materialized")
	}

	b, err := protojson.Marshal(reportedZero)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back rafikiv1.Usage
	if err := protojson.Unmarshal(b, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back.CacheWriteTokens == nil {
		t.Fatalf("explicit zero lost through protojson: %s", b)
	}
}

// Tool results must be able to carry an image, not just text.
func TestToolResultCarriesImageBlock(t *testing.T) {
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
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back rafikiv1.ToolResultBlock
	if err := protojson.Unmarshal(b, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	img := back.Content[0].GetImage()
	if img == nil {
		t.Fatal("image block lost")
	}
	if img.MediaType != "image/png" {
		t.Fatalf("media type = %q, want image/png", img.MediaType)
	}
}
