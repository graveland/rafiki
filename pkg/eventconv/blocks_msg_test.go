package eventconv_test

import (
	"encoding/json"
	"reflect"
	"testing"

	"google.golang.org/protobuf/reflect/protoreflect"

	"go.graveland.dev/rafiki/pkg/eventconv"
	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/msg"
	msganthropic "go.graveland.dev/rafiki/pkg/msg/anthropic"

	"github.com/multigres/testkit/assert"
)

// TestBlocksBridgeCoversEveryKind drives the real conversion switch with one
// block per msg.BlockKinds() entry and asserts the ContentBlock it produces sets
// the oneof field the table names. A kind added to msg without a mapping fails
// here — through protoBlock, not by comparing one table with another — and so
// does a proto field no native kind feeds.
func TestBlocksBridgeCoversEveryKind(t *testing.T) {
	c := assert.NewAborting(t)

	// One filled block per native kind, with the proto oneof field the bridge must
	// render it as. "" means the proto vocabulary cannot name that block, so the
	// bridge skips it and leaves the gap its source index leaves.
	samples := map[string]struct {
		block msg.Block
		field string
	}{
		"Text":       {msg.Text{Text: "hello"}, "text"},
		"Thinking":   {msg.Thinking{Text: "weighing", Sig: msg.Signature{Dialect: msg.DialectAnthropic, Value: "sig"}}, "thinking"},
		"ToolUse":    {msg.ToolUse{ID: "tu_1", Name: "bash", Input: json.RawMessage(`{"command":"ls"}`)}, "tool_use"},
		"ToolResult": {msg.ToolResult{ToolUseID: "tu_1", Content: []msg.Block{msg.Text{Text: "ok"}}}, "tool_result"},
		"Image":      {msg.Image{MediaType: "image/png", Data: png1x1Bytes(t)}, "image"},
		"Opaque":     {msg.Opaque{Provider: msg.DialectAnthropic, Raw: json.RawMessage(`{"type":"redacted_thinking","data":"opaque_blob"}`)}, ""},
	}

	oneof := (&rafikiv1.ContentBlock{}).ProtoReflect().Descriptor().Oneofs().ByName("block")
	c.NotNil(oneof, "ContentBlock has no block oneof")

	rendered := make(map[string]bool, len(samples))
	for _, kind := range msg.BlockKinds() {
		name := reflect.TypeOf(kind).Name()
		sample, ok := samples[name]
		c.Require().True(ok, "msg block kind %q has no sample: a new kind must be mapped by the bridge and listed here", name)
		if !ok {
			continue
		}
		blocks := eventconv.BlocksFromMsg(msg.Message{Role: msg.Assistant, Content: []msg.Block{sample.block}})
		if sample.field == "" {
			c.Len(blocks, 0, "%s has no proto field, so the bridge skips it", name)
			continue
		}
		c.Require().Len(blocks, 1, "%s renders one block", name)
		field := blocks[0].ProtoReflect().WhichOneof(oneof)
		c.Require().NotNil(field, "%s rendered no proto block", name)
		c.Eq(sample.field, string(field.Name()), "%s renders as the field the table names", name)
		rendered[sample.field] = true
	}

	fields := oneof.Fields()
	for i := range fields.Len() {
		name := string(fields.Get(i).Name())
		c.True(rendered[name], "proto oneof field %q is fed by no msg block kind", name)
	}

	// An Opaque block is converted by its declared type when the proto can name
	// it, so a block kept verbatim for a field this model does not carry does not
	// vanish from a transcript.
	named := eventconv.BlocksFromMsg(msg.Message{Role: msg.Assistant, Content: []msg.Block{
		msg.Opaque{Provider: msg.DialectAnthropic, Raw: json.RawMessage(
			`{"type":"tool_use","id":"tu_2","name":"grep","input":{"pattern":"x"}}`)},
	}})
	c.Require().Len(named, 1, "an opaque block of a nameable type renders")
	field := named[0].ProtoReflect().WhichOneof(oneof)
	c.Require().NotNil(field, "the opaque block rendered no proto block")
	c.Eq("tool_use", string(field.Name()), "an opaque tool_use renders as tool_use")

	// A message payload renders only through the block list the bridge emits, so
	// every payload carrying message content must be in this table — a new one
	// lands here rather than shipping unrenderable.
	messages := map[string]string{
		"user_message":      "UserMessage",
		"assistant_message": "AssistantMessage",
		"system_message":    "SystemMessage",
	}
	payload := (&rafikiv1.Event{}).ProtoReflect().Descriptor().Oneofs().ByName("payload")
	c.NotNil(payload, "Event has no payload oneof")
	for field, message := range messages {
		fd := payload.Fields().ByName(protoreflect.Name(field))
		c.NotNil(fd, "Event.payload has no %q field", field)
		if fd == nil {
			continue
		}
		c.Eq(protoreflect.Name(message), fd.Message().Name(), "Event.payload.%s message type", field)
		content := fd.Message().Fields().ByName("content")
		c.NotNil(content, "%s has no content field", message)
		if content == nil {
			continue
		}
		c.True(content.IsList(), "%s.content is not repeated", message)
		c.Eq(protoreflect.Name("ContentBlock"), content.Message().Name(), "%s.content block type", message)
	}
}

// bridgeFixture is stored message content carrying every typed kind, with a
// tool result that nests text and an image.
const bridgeFixture = `[
	{"type":"text","text":"hello"},
	{"type":"thinking","thinking":"weighing the options","signature":"sig_abc"},
	{"type":"tool_use","id":"tu_1","name":"bash","input":{"command":"ls -la","paths":["/tmp"]}},
	{"type":"tool_result","tool_use_id":"tu_1","is_error":true,"content":[
		{"type":"text","text":"boom"},
		{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + png1x1 + `"}}
	]},
	{"type":"image","source":{"type":"base64","media_type":"image/jpeg","data":"` + png1x1 + `"}}
]`

// bridgeOpaqueFixture is stored content whose first block the proto vocabulary
// cannot name. Both bridges skip it and keep numbering by source position, so
// the emitted blocks carry indices 1 and 2 rather than 0 and 1.
const bridgeOpaqueFixture = `[
	{"type":"redacted_thinking","data":"opaque_blob"},
	{"type":"text","text":"after the opaque block"},
	{"type":"tool_use","id":"tu_2","name":"grep","input":{"pattern":"x"}}
]`

// TestBlocksFromMsgRendersEveryKind pins the native bridge over a fixture
// carrying every typed kind, with a tool result that nests text and an image.
func TestBlocksFromMsgRendersEveryKind(t *testing.T) {
	c := assert.NewAborting(t)
	m, err := msganthropic.Decode("assistant", []byte(bridgeFixture))
	c.NoError(err, "decode content into msg.Message")

	blocks := eventconv.BlocksFromMsg(m)

	c.Require().Len(blocks, 5, "blocks from the full fixture")
	for i, b := range blocks {
		c.Eq(int32(i), b.Index, "block %d keeps its source index", i)
	}
	c.Eq("hello", blocks[0].GetText().GetText(), "text block")
	c.Eq("weighing the options", blocks[1].GetThinking().GetThinking(), "thinking text")
	c.Eq("sig_abc", blocks[1].GetThinking().GetSignature(), "thinking signature")
	tu := blocks[2].GetToolUse()
	c.Require().NotNil(tu, "block 2 is %T, want a tool_use", blocks[2].Block)
	c.Eq("tu_1", tu.GetId(), "tool_use id")
	c.Eq("bash", tu.GetName(), "tool_use name")
	var input map[string]any
	c.NoError(json.Unmarshal([]byte(tu.GetInputJson()), &input), "input_json is JSON")
	c.EqDeep(map[string]any{"command": "ls -la", "paths": []any{"/tmp"}}, input, "tool_use input")
	tr := blocks[3].GetToolResult()
	c.Require().NotNil(tr, "block 3 is %T, want a tool_result", blocks[3].Block)
	c.Eq("tu_1", tr.GetToolUseId(), "tool_result id")
	c.True(tr.GetIsError(), "tool_result is_error")
	c.Len(tr.GetContent(), 2, "nested tool result content")
	c.Eq("boom", tr.GetContent()[0].GetText().GetText(), "nested text")
	c.EqDeep(png1x1Bytes(t), tr.GetContent()[1].GetImage().GetData(), "nested image bytes")
	img := blocks[4].GetImage()
	c.Require().NotNil(img, "block 4 is %T, want an image", blocks[4].Block)
	c.Eq("image/jpeg", img.GetMediaType(), "image media type")
	c.EqDeep(png1x1Bytes(t), img.GetData(), "image bytes")
}

// TestBlocksFromMsgSkipsPastAnOpaqueBlock pins that a block the proto cannot
// name is skipped and leaves a gap, so the emitted blocks keep their source
// indices.
func TestBlocksFromMsgSkipsPastAnOpaqueBlock(t *testing.T) {
	c := assert.NewAborting(t)
	m, err := msganthropic.Decode("assistant", []byte(bridgeOpaqueFixture))
	c.NoError(err, "decode content into msg.Message")

	got := eventconv.BlocksFromMsg(m)

	c.Require().Len(got, 2, "blocks emitted past the opaque one")
	c.Eq(int32(1), got[0].Index, "first emitted block keeps its source index")
	c.Eq(int32(2), got[1].Index, "second emitted block keeps its source index")
}

// A block kept verbatim is still real content: when the proto vocabulary can
// name its type it renders exactly as the typed block of that kind does. Only a
// type the proto cannot name is skipped.
func TestBlocksFromMsgRendersOpaqueByType(t *testing.T) {
	c := assert.NewAborting(t)
	m := msg.Message{Role: msg.Assistant, Content: []msg.Block{
		msg.Opaque{Provider: msg.DialectAnthropic, Raw: json.RawMessage(
			`{"type":"text","text":"kept","citations":[{"url":"https://example.test"}]}`)},
		msg.Opaque{Provider: msg.DialectAnthropic, Raw: json.RawMessage(
			`{"type":"tool_use","id":"tu_9","name":"read","input":{"path":"/tmp/x"},"caller":{"type":"direct"}}`)},
		msg.Opaque{Provider: msg.DialectAnthropic, Raw: json.RawMessage(
			`{"type":"tool_result","tool_use_id":"tu_9","is_error":true,"content":[{"type":"text","text":"nested"}]}`)},
		msg.Opaque{Provider: msg.DialectAnthropic, Raw: json.RawMessage(
			`{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + png1x1 + `"}}`)},
		msg.Opaque{Provider: msg.DialectAnthropic, Raw: json.RawMessage(
			`{"type":"tool_result","tool_use_id":"tu_9","content":"shorthand"}`)},
		msg.Opaque{Provider: msg.DialectAnthropic, Raw: json.RawMessage(
			`{"type":"redacted_thinking","data":"opaque_blob"}`)},
	}}

	blocks := eventconv.BlocksFromMsg(m)

	c.Len(blocks, 5, "named opaque blocks render; the unknown type is skipped")

	c.Eq(int32(0), blocks[0].Index, "text keeps its source index")
	c.Eq("kept", blocks[0].GetText().GetText(), "opaque text")

	c.Eq(int32(1), blocks[1].Index, "tool_use keeps its source index")
	tu := blocks[1].GetToolUse()
	c.NotNil(tu, "block 1 is %T, want a tool_use", blocks[1].Block)
	c.Eq("tu_9", tu.GetId(), "tool_use id")
	c.Eq("read", tu.GetName(), "tool_use name")
	var input map[string]any
	c.NoError(json.Unmarshal([]byte(tu.GetInputJson()), &input), "input_json is JSON")
	c.EqDeep(map[string]any{"path": "/tmp/x"}, input, "tool_use input")

	c.Eq(int32(2), blocks[2].Index, "tool_result keeps its source index")
	tr := blocks[2].GetToolResult()
	c.NotNil(tr, "block 2 is %T, want a tool_result", blocks[2].Block)
	c.Eq("tu_9", tr.GetToolUseId(), "tool_result id")
	c.True(tr.GetIsError(), "tool_result is_error")
	c.Len(tr.GetContent(), 1, "nested tool result content")
	c.Eq("nested", tr.GetContent()[0].GetText().GetText(), "nested tool result text")

	c.Eq(int32(3), blocks[3].Index, "image keeps its source index")
	img := blocks[3].GetImage()
	c.NotNil(img, "block 3 is %T, want an image", blocks[3].Block)
	c.Eq("image/png", img.GetMediaType(), "image media type")
	c.EqDeep(png1x1Bytes(t), img.GetData(), "decoded image bytes")

	c.Eq(int32(4), blocks[4].Index, "string-content tool_result keeps its source index")
	shorthand := blocks[4].GetToolResult()
	c.NotNil(shorthand, "block 4 is %T, want a tool_result", blocks[4].Block)
	c.Len(shorthand.GetContent(), 1, "string content is one text block")
	c.Eq("shorthand", shorthand.GetContent()[0].GetText().GetText(), "string tool result text")
}
