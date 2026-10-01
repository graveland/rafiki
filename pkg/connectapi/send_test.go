// SPDX-License-Identifier: Apache-2.0

package connectapi_test

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/png"
	"testing"

	"connectrpc.com/connect"

	"go.graveland.dev/rafiki/pkg/connectapi"
	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/inbox"

	"github.com/multigres/testkit/assert"
)

// fakeAccepter is the narrow inbox seam the Server holds: it records what Send
// submitted and hands back an id. Delivery is the Queue's job and is not what
// these tests are about. err exercises the accept-failure path (send_test.go's
// redaction pin uses it).
type fakeAccepter struct {
	got inbox.Inbound
	err error
}

func (f *fakeAccepter) Accept(_ context.Context, in inbox.Inbound) (string, error) {
	f.got = in
	if f.err != nil {
		return "", f.err
	}
	return "m_1", nil
}

func textBlocks(s string) []*rafikiv1.ContentBlock {
	return []*rafikiv1.ContentBlock{{
		Index: 0,
		Block: &rafikiv1.ContentBlock_Text{Text: &rafikiv1.TextBlock{Text: s}},
	}}
}

// pngOf encodes a w×h NRGBA image as PNG, so the send tests exercise image
// bytes a decoder actually accepts.
func pngOf(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.SetNRGBA(x, y, color.NRGBA{R: uint8(x % 256), G: uint8(y % 256), B: 0x80, A: 0xff})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("png.Encode(%dx%d): %v", w, h, err)
	}
	return buf.Bytes()
}

func TestSendRoutesPromptThroughInbox(t *testing.T) {
	c := assert.NewCollecting(t)
	acc := &fakeAccepter{}
	s := connectapi.NewServer(nil)
	s.SetInbox(acc)

	resp, err := s.Send(context.Background(), connect.NewRequest(&rafikiv1.SendRequest{
		ChildId: "c_1",
		Mode:    rafikiv1.SendMode_SEND_MODE_PROMPT,
		Blocks:  textBlocks("hello"),
	}))
	c.Require().NoError(err, "Send")
	c.NotEq("", resp.Msg.GetMessageId(), "SendResponse.MessageId is empty")
	if acc.got.ChildID != "c_1" || acc.got.Mode != inbox.ModePrompt || acc.got.Text != "hello" {
		t.Errorf("inbound = %+v, want child=c_1 mode=prompt text=hello", acc.got)
	}
}

func TestSendMapsSteerAndAbort(t *testing.T) {
	c := assert.NewCollecting(t)
	cases := []struct {
		wire rafikiv1.SendMode
		want inbox.Mode
	}{
		{rafikiv1.SendMode_SEND_MODE_STEER, inbox.ModeSteer},
		{rafikiv1.SendMode_SEND_MODE_ABORT, inbox.ModeAbort},
	}
	for _, tc := range cases {
		acc := &fakeAccepter{}
		s := connectapi.NewServer(nil)
		s.SetInbox(acc)
		_, err := s.Send(context.Background(), connect.NewRequest(&rafikiv1.SendRequest{
			ChildId: "c_1", Mode: tc.wire, Blocks: textBlocks("x"),
		}))
		c.Require().NoError(err, "Send(%v)", tc.wire)
		c.Eq(tc.want, acc.got.Mode, "mode for %v = %v, want", tc.wire, acc.got.Mode)
	}
}

func TestSendWithoutInboxFailsClosed(t *testing.T) {
	s := connectapi.NewServer(nil)
	_, err := s.Send(context.Background(), connect.NewRequest(&rafikiv1.SendRequest{
		ChildId: "c_1", Mode: rafikiv1.SendMode_SEND_MODE_PROMPT, Blocks: textBlocks("x"),
	}))
	assert.NewCollecting(t).Eq(connect.CodeUnavailable, connect.CodeOf(err), "code")
}

func TestSendRejectsEmptyChildID(t *testing.T) {
	s := connectapi.NewServer(nil)
	s.SetInbox(&fakeAccepter{})
	_, err := s.Send(context.Background(), connect.NewRequest(&rafikiv1.SendRequest{
		Mode: rafikiv1.SendMode_SEND_MODE_PROMPT, Blocks: textBlocks("x"),
	}))
	assert.NewCollecting(t).Eq(connect.CodeInvalidArgument, connect.CodeOf(err), "code")
}

func TestSendRejectsUnspecifiedMode(t *testing.T) {
	s := connectapi.NewServer(nil)
	s.SetInbox(&fakeAccepter{})
	_, err := s.Send(context.Background(), connect.NewRequest(&rafikiv1.SendRequest{
		ChildId: "c_1", Blocks: textBlocks("x"),
	}))
	assert.NewCollecting(t).Eq(connect.CodeInvalidArgument, connect.CodeOf(err), "code")
}

// TestSendRejectsNonTextBlocks proves an image is refused rather than silently
// dropped: Engine.HandlePrompt takes a string, so there is nowhere for image
// bytes to go until that changes.
// Send CARRIES images now — this used to assert they were refused. What has
// not changed is that a block type with nowhere to go is refused rather than
// skipped: silently dropping a payload looks to the sender like delivering it.
func TestSendCarriesAnImageBlock(t *testing.T) {
	c := assert.NewCollecting(t)
	s := connectapi.NewServer(nil)
	acc := &fakeAccepter{}
	s.SetInbox(acc)
	png := pngOf(t, 2, 2)
	_, err := s.Send(context.Background(), connect.NewRequest(&rafikiv1.SendRequest{
		ChildId: "c_1",
		Mode:    rafikiv1.SendMode_SEND_MODE_PROMPT,
		Blocks: []*rafikiv1.ContentBlock{
			{Block: &rafikiv1.ContentBlock_Image{Image: &rafikiv1.ImageBlock{
				MediaType: "image/png", Data: png,
			}}},
			{Block: &rafikiv1.ContentBlock_Text{Text: &rafikiv1.TextBlock{Text: "what is this?"}}},
		},
	}))
	c.Require().NoError(err, "Send with an image")
	c.Eq("what is this?", acc.got.Text, "text")
	c.Require().Eq(1, len(acc.got.Attachments), "got")
	c.Eq("image/png", acc.got.Attachments[0].MediaType, "media type =")
	c.EqDeep(png, acc.got.Attachments[0].Data, "image bytes did not survive: %d bytes", len(acc.got.Attachments[0].Data))
}

// An image larger than ingestImageBox is downscaled before it reaches the
// inbox: what is stored, echoed and sent to the model is the resized image.
func TestSendResizesAnOversizedImage(t *testing.T) {
	c := assert.NewCollecting(t)
	s := connectapi.NewServer(nil)
	acc := &fakeAccepter{}
	s.SetInbox(acc)
	_, err := s.Send(context.Background(), connect.NewRequest(&rafikiv1.SendRequest{
		ChildId: "c_1",
		Mode:    rafikiv1.SendMode_SEND_MODE_PROMPT,
		Blocks: []*rafikiv1.ContentBlock{
			{Block: &rafikiv1.ContentBlock_Image{Image: &rafikiv1.ImageBlock{
				MediaType: "image/png", Data: pngOf(t, 2000, 1000),
			}}},
			{Block: &rafikiv1.ContentBlock_Text{Text: &rafikiv1.TextBlock{Text: "what is this?"}}},
		},
	}))
	c.Require().NoError(err, "Send with an oversized image")
	c.Require().Eq(1, len(acc.got.Attachments), "got")
	att := acc.got.Attachments[0]
	c.Eq("image/png", att.MediaType, "media type =")
	cfg, _, err := image.DecodeConfig(bytes.NewReader(att.Data))
	c.Require().NoError(err, "DecodeConfig of the accepted attachment")
	c.LessOrEqual(1568, cfg.Width, "width")
	c.LessOrEqual(1568, cfg.Height, "height")
	c.LessOrEqual(1_150_000, cfg.Width*cfg.Height, "pixels")
	c.GreaterOrEqual(1500, cfg.Width, "width")
}

// Bytes that claim PNG/JPEG but do not decode are refused at ingest rather
// than left for the provider to reject, and nothing reaches the inbox.
func TestSendRefusesAnUndecodableImage(t *testing.T) {
	c := assert.NewCollecting(t)
	s := connectapi.NewServer(nil)
	acc := &fakeAccepter{}
	s.SetInbox(acc)
	_, err := s.Send(context.Background(), connect.NewRequest(&rafikiv1.SendRequest{
		ChildId: "c_1",
		Mode:    rafikiv1.SendMode_SEND_MODE_PROMPT,
		Blocks: []*rafikiv1.ContentBlock{
			{Block: &rafikiv1.ContentBlock_Image{Image: &rafikiv1.ImageBlock{
				MediaType: "image/png", Data: []byte("\x89PNGfake"),
			}}},
			{Block: &rafikiv1.ContentBlock_Text{Text: &rafikiv1.TextBlock{Text: "what is this?"}}},
		},
	}))
	c.Eq(connect.CodeInvalidArgument, connect.CodeOf(err), "code")
	c.EqDeep(inbox.Inbound{}, acc.got, "the inbox accepted nothing")
}

// An image block with no bytes is a caller error, not something to pass on as
// an empty attachment.
func TestSendRejectsAnEmptyImage(t *testing.T) {
	s := connectapi.NewServer(nil)
	s.SetInbox(&fakeAccepter{})
	_, err := s.Send(context.Background(), connect.NewRequest(&rafikiv1.SendRequest{
		ChildId: "c_1",
		Mode:    rafikiv1.SendMode_SEND_MODE_PROMPT,
		Blocks: []*rafikiv1.ContentBlock{{
			Block: &rafikiv1.ContentBlock_Image{Image: &rafikiv1.ImageBlock{MediaType: "image/png"}},
		}},
	}))
	assert.NewCollecting(t).Eq(connect.CodeInvalidArgument, connect.CodeOf(err), "code")
}

// A block type Send cannot carry is still refused rather than skipped.
func TestSendRefusesABlockItCannotCarry(t *testing.T) {
	s := connectapi.NewServer(nil)
	s.SetInbox(&fakeAccepter{})
	_, err := s.Send(context.Background(), connect.NewRequest(&rafikiv1.SendRequest{
		ChildId: "c_1",
		Mode:    rafikiv1.SendMode_SEND_MODE_PROMPT,
		Blocks: []*rafikiv1.ContentBlock{{
			Block: &rafikiv1.ContentBlock_ToolUse{ToolUse: &rafikiv1.ToolUseBlock{Id: "tu_1"}},
		}},
	}))
	assert.NewCollecting(t).Eq(connect.CodeUnimplemented, connect.CodeOf(err), "code")
}

// TestSendAbortNeedsNoBlocks: an abort carries no content.
func TestSendAbortNeedsNoBlocks(t *testing.T) {
	s := connectapi.NewServer(nil)
	s.SetInbox(&fakeAccepter{})
	_, err := s.Send(context.Background(), connect.NewRequest(&rafikiv1.SendRequest{
		ChildId: "c_1", Mode: rafikiv1.SendMode_SEND_MODE_ABORT,
	}))
	assert.NewCollecting(t).NoError(err, "Send(ABORT) with no blocks")
}
