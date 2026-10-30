package eventconv_test

import (
	"encoding/base64"
	"testing"

	"go.graveland.dev/rafiki/pkg/conv"
	"go.graveland.dev/rafiki/pkg/eventconv"
	"go.graveland.dev/rafiki/pkg/msg"

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

// A user message carrying an image renders its bytes into the proto image
// block, so a live viewer sees the same picture history does.
func TestEventsFromNodesCarriesUserImage(t *testing.T) {
	c := assert.NewAborting(t)
	data := png1x1Bytes(t)
	nodes := []*conv.Node{{
		Ref: conv.Ref{Ordinal: 0},
		Msg: msg.Message{Role: msg.User, Content: []msg.Block{
			msg.Image{MediaType: "image/png", Data: data},
			msg.Text{Text: "look"},
		}},
	}}

	evs := eventconv.EventsFromNodes("c_1", nodes)

	c.Require().Len(evs, 1, "events")
	content := evs[0].GetUserMessage().GetContent()
	c.Require().Len(content, 2, "blocks")
	c.EqDeep(data, content[0].GetImage().GetData(), "event image bytes")
	c.Eq("look", content[1].GetText().GetText(), "event text")
}
