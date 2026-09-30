package fundi

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/llm"

	"github.com/multigres/testkit/assert"
)

// png1x1Base64 is a real 1×1 PNG: the wire tests assert the exact bytes round
// trip, so the fixture should be an image a decoder would accept, not a
// hand-waved byte string.
const png1x1Base64 = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNkYPhfDwAChwGA60e6kgAAAABJRU5ErkJggg=="

// contentBlock mirrors the shape the wire must carry for one content block:
// text, or an image whose Source holds the base64 bytes.
type contentBlock struct {
	Type   string `json:"type"`
	Text   string `json:"text"`
	Source *struct {
		MediaType string `json:"media_type"`
		Data      string `json:"data"`
	} `json:"source"`
}

// lastUserBlocks decodes the content blocks of the final user-role message in
// the captured request.
func lastUserBlocks(t *testing.T, params anthropic.MessageNewParams) []contentBlock {
	t.Helper()
	c := assert.NewAborting(t)
	body, err := json.Marshal(params.Messages)
	c.NoError(err, "marshal request messages")
	var msgs []struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	}
	c.NoError(json.Unmarshal(body, &msgs), "decode request messages")
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role != "user" {
			continue
		}
		var blocks []contentBlock
		c.NoError(json.Unmarshal(msgs[i].Content, &blocks), "decode content blocks")
		return blocks
	}
	t.Fatal("no user message in the captured request")
	return nil
}

func assertImageThenText(t *testing.T, blocks []contentBlock) {
	t.Helper()
	c := assert.NewAborting(t)
	c.Require().True(len(blocks) >= 2, "got %d content blocks, want image then text", len(blocks))
	c.Eq("image", blocks[0].Type, "first block type (images go first, llm.UserContent)")
	c.Require().NotNil(blocks[0].Source, "image block carries no source")
	c.Eq("image/png", blocks[0].Source.MediaType, "image media type")
	c.Eq(png1x1Base64, blocks[0].Source.Data, "image bytes did not survive to the wire")
	c.Eq("text", blocks[1].Type, "second block type")
	c.Eq("look", blocks[1].Text, "text block")
}

// TestPromptWithAttachmentsReachesTheWire pins the engine half of image
// delivery: a queued attachment prompt must reach the sender as an image block
// FIRST in the user message, bytes intact, text after.
func TestPromptWithAttachmentsReachesTheWire(t *testing.T) {
	c := assert.NewAborting(t)
	cs := newCapturingSender(t, sampleEndTurn)
	eng, _ := newTestEngineWithSender(t, fakeToolSet{}, cs)
	png, err := base64.StdEncoding.DecodeString(png1x1Base64)
	c.NoError(err, "decode fixture")
	eng.HandlePromptWithAttachments("F1", "look", []llm.UserImage{{MediaType: "image/png", Data: png}})
	eng.Wait()
	assertImageThenText(t, lastUserBlocks(t, cs.lastParams(t)))
}

// TestAttachmentFrameThroughTheRealFrontendReachesTheWire pins the composition
// the engine test cannot: the daemon's injection frame — attachments beside the
// text, base64 on the wire — through a real Frontend dispatch into a real
// Engine, and out to the sender. Every half of this path had passing tests
// while the whole still needed one.
func TestAttachmentFrameThroughTheRealFrontendReachesTheWire(t *testing.T) {
	c := assert.NewAborting(t)
	cs := newCapturingSender(t, sampleEndTurn)
	eng, _ := newTestEngineWithSender(t, fakeToolSet{}, cs)
	frame := `{"type":"prompt","id":"F1","message":"look",` +
		`"attachments":[{"media_type":"image/png","data":"` + png1x1Base64 + `"}]}` + "\n"
	fe := NewFrontend(strings.NewReader(frame), io.Discard, eng)
	done := make(chan error, 1)
	go func() { done <- fe.Run() }()
	c.NoError(<-done, "frontend run")
	eng.Wait()
	assertImageThenText(t, lastUserBlocks(t, cs.lastParams(t)))
}

// attachmentRecorder is a Handler that also implements AttachmentHandler and
// records what the dispatch handed it.
type attachmentRecorder struct {
	fakeHandler
	id    string
	text  string
	imgs  []llm.UserImage
	calls int
	// steerCalls counts steer dispatches separately so a steer frame that
	// fell through to the prompt path is caught rather than miscounted.
	steerCalls int
}

func (h *attachmentRecorder) HandlePromptWithAttachments(id, text string, images []llm.UserImage) {
	h.calls++
	h.id, h.text, h.imgs = id, text, images
}

func (h *attachmentRecorder) HandleSteerWithAttachments(id, text string, images []llm.UserImage) {
	h.steerCalls++
	h.id, h.text, h.imgs = id, text, images
}

// TestAttachmentFrameDispatchesToTheAttachmentHandler pins the frame-parse half
// in isolation: the media_type/data JSON keys must decode through the real
// Frontend reader, not just match by luck between two structs nobody compared.
func TestAttachmentFrameDispatchesToTheAttachmentHandler(t *testing.T) {
	c := assert.NewAborting(t)
	rec := &attachmentRecorder{}
	in := strings.NewReader(`{"type":"prompt","id":"F1","message":"look",` +
		`"attachments":[{"media_type":"image/png","data":"aGVsbG8="}]}` + "\n")
	c.NoError(NewFrontend(in, io.Discard, rec).Run(), "frontend run")
	c.Eq(1, rec.calls, "attachment dispatch count")
	c.Eq("F1", rec.id, "frame id")
	c.Eq("look", rec.text, "text")
	c.Require().Len(rec.imgs, 1, "images")
	c.Eq("image/png", rec.imgs[0].MediaType, "media type")
	c.Eq("hello", string(rec.imgs[0].Data), "decoded bytes")
}

// TestPromptWithAttachmentsEchoesTheImage pins the ECHO half of image
// delivery: the native user_message the engine publishes for the cockpit
// carries the image too, in the same order the wire gets it.
func TestPromptWithAttachmentsEchoesTheImage(t *testing.T) {
	c := assert.NewAborting(t)
	sink := &recordingSink{}
	eng, _ := newTestEngineWithConfig(t, fakeToolSet{}, newCapturingSender(t, sampleEndTurn), func(cfg *EngineConfig) {
		cfg.NativeSink = sink
	})
	png, err := base64.StdEncoding.DecodeString(png1x1Base64)
	c.NoError(err, "decode fixture")
	eng.HandlePromptWithAttachments("F1", "look", []llm.UserImage{{MediaType: "image/png", Data: png}})
	eng.Wait()
	var um *rafikiv1.UserMessage
	for _, ev := range sink.events {
		if u := ev.GetUserMessage(); u != nil {
			um = u
			break
		}
	}
	c.Require().NotNil(um, "no native user_message published")
	c.Require().Len(um.Content, 2, "got %d content blocks, want image then text", len(um.Content))
	img := um.Content[0].GetImage()
	c.Require().NotNil(img, "content[0] is not an image (runTurn passed nil instead of images)")
	c.Eq("image/png", img.MediaType, "media type")
	c.EqDiff(png, img.Data, "image bytes did not survive to the echo")
	c.Eq("look", um.Content[1].GetText().GetText(), "content[1] text")
}

// TestSteerFrameWithAttachmentsDispatchesToTheAttachmentHandler pins the steer
// half of the frame parse: a steer is not a prompt, and used to drop its images.
func TestSteerFrameWithAttachmentsDispatchesToTheAttachmentHandler(t *testing.T) {
	c := assert.NewAborting(t)
	rec := &attachmentRecorder{}
	in := strings.NewReader(`{"type":"steer","id":"F2","message":"and this",` +
		`"attachments":[{"media_type":"image/png","data":"aGVsbG8="}]}` + "\n")
	c.NoError(NewFrontend(in, io.Discard, rec).Run(), "frontend run")
	c.Eq(1, rec.steerCalls, "steer dispatch count")
	c.Eq(0, rec.calls, "a steer must not take the prompt path")
	c.Eq("F2", rec.id, "frame id")
	c.Eq("and this", rec.text, "text")
	c.Require().Len(rec.imgs, 1, "images")
	c.Eq("hello", string(rec.imgs[0].Data), "decoded bytes")
}

// TestSteerWithAttachmentsMidTurnReachesTheWire pins the engine half: a steer
// buffered while a tool runs is drained into the turn with its image, images
// first, and the echo carries the image as well.
func TestSteerWithAttachmentsMidTurnReachesTheWire(t *testing.T) {
	c := assert.NewAborting(t)
	started := make(chan struct{})
	release := make(chan struct{})
	ts := fakeToolSet{"bash": func(ctx context.Context, in json.RawMessage) (string, error) {
		close(started)
		<-release
		return "file.txt", nil
	}}
	cs := newCapturingSender(t, sampleResp, sampleEndTurn)
	sink := &recordingSink{}
	eng, _ := newTestEngineWithConfig(t, ts, cs, func(cfg *EngineConfig) { cfg.NativeSink = sink })
	png, err := base64.StdEncoding.DecodeString(png1x1Base64)
	c.NoError(err, "decode fixture")

	eng.HandlePromptID("F1", "go")
	<-started
	eng.HandleSteerWithAttachments("F2", "look", []llm.UserImage{{MediaType: "image/png", Data: png}})
	close(release)
	eng.Wait()

	// The steer rides the user message that answers the tool call, after the
	// tool_result: image then text is asserted on what follows it.
	blocks := lastUserBlocks(t, cs.lastParams(t))
	c.Require().Len(blocks, 3, "tool_result, image, text")
	c.Eq("tool_result", blocks[0].Type, "tool_result leads")
	assertImageThenText(t, blocks[1:])
	var steerEcho *rafikiv1.UserMessage
	for _, ev := range sink.events {
		if u := ev.GetUserMessage(); u != nil && len(u.Content) == 2 {
			steerEcho = u
		}
	}
	c.Require().NotNil(steerEcho, "no user_message echo carried the steer's image")
	c.EqDiff(png, steerEcho.Content[0].GetImage().GetData(), "echoed image bytes")
}
