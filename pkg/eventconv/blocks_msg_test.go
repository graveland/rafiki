package eventconv_test

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"
	"google.golang.org/protobuf/proto"

	"go.graveland.dev/rafiki/pkg/eventconv"
	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/msg"
	msganthropic "go.graveland.dev/rafiki/pkg/msg/anthropic"

	"github.com/multigres/testkit/assert"
)

// TestBlocksBridgeCoversEveryKind fails when either block vocabulary grows a
// kind the bridge does not map: a message-side kind with no table entry, or a
// proto oneof field with no native kind feeding it.
func TestBlocksBridgeCoversEveryKind(t *testing.T) {
	c := assert.NewAborting(t)
	kinds := map[string]string{
		"Text":       "text",
		"Thinking":   "thinking",
		"ToolUse":    "tool_use",
		"ToolResult": "tool_result",
		"Image":      "image",
		"Opaque":     "",
	}

	mapped := make(map[string]bool, len(kinds))
	for _, field := range kinds {
		if field != "" {
			mapped[field] = true
		}
	}

	oneof := (&rafikiv1.ContentBlock{}).ProtoReflect().Descriptor().Oneofs().ByName("block")
	c.NotNil(oneof, "ContentBlock has no block oneof")
	fields := oneof.Fields()
	c.Len(mapped, fields.Len(), "table maps %d fields, the proto oneof has %d", len(mapped), fields.Len())
	for i := range fields.Len() {
		name := string(fields.Get(i).Name())
		c.True(mapped[name], "proto oneof field %q has no msg block in the table", name)
	}

	for _, k := range msg.BlockKinds() {
		name := reflect.TypeOf(k).Name()
		c.HasKey(kinds, name, "msg block kind %q has no proto oneof field in the table", name)
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

// TestBlocksFromMsgMatchesBlocksFromParam pins the native bridge against the
// SDK bridge it will replace: both decode the same stored content and must
// produce the same proto blocks.
func TestBlocksFromMsgMatchesBlocksFromParam(t *testing.T) {
	c := assert.NewAborting(t)

	m, err := msganthropic.Decode("assistant", []byte(bridgeFixture))
	c.NoError(err, "decode fixture into msg.Message")

	var sdkContent []anthropic.ContentBlockParamUnion
	c.NoError(json.Unmarshal([]byte(bridgeFixture), &sdkContent), "decode fixture into SDK content blocks")
	param := anthropic.NewAssistantMessage(sdkContent...)

	want := eventconv.BlocksFromParam(param)
	got := eventconv.BlocksFromMsg(m)

	c.Len(got, len(want), "got %d blocks from msg, want %d from the SDK param", len(got), len(want))
	for i := range want {
		w := proto.Clone(want[i]).(*rafikiv1.ContentBlock)
		g := proto.Clone(got[i]).(*rafikiv1.ContentBlock)
		if wu, gu := w.GetToolUse(), g.GetToolUse(); wu != nil && gu != nil {
			var wv, gv any
			c.NoError(json.Unmarshal([]byte(wu.InputJson), &wv), "block %d: unmarshal BlocksFromParam InputJson", i)
			c.NoError(json.Unmarshal([]byte(gu.InputJson), &gv), "block %d: unmarshal BlocksFromMsg InputJson", i)
			c.EqDeep(wv, gv, "block %d: tool input", i)
			wu.InputJson, gu.InputJson = "", ""
		}
		c.True(proto.Equal(w, g), "block %d differs: BlocksFromParam=%v BlocksFromMsg=%v", i, want[i], got[i])
	}

	c.Len(got[3].GetToolResult().GetContent(), 2, "nested tool result content")
	c.NotNil(got[3].GetToolResult().GetContent()[1].GetImage(), "nested image survives")
}
