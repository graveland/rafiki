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

// bridgeOpaqueFixture is stored content whose first block the proto vocabulary
// cannot name. Both bridges skip it and keep numbering by source position, so
// the emitted blocks carry indices 1 and 2 rather than 0 and 1.
const bridgeOpaqueFixture = `[
	{"type":"redacted_thinking","data":"opaque_blob"},
	{"type":"text","text":"after the opaque block"},
	{"type":"tool_use","id":"tu_2","name":"grep","input":{"pattern":"x"}}
]`

// TestBlocksFromMsgMatchesBlocksFromParam pins the native bridge against the
// SDK bridge it will replace: both decode the same stored content and must
// produce the same proto blocks, indices included.
func TestBlocksFromMsgMatchesBlocksFromParam(t *testing.T) {
	t.Run("every kind", func(t *testing.T) {
		c := assert.NewAborting(t)
		got := bridgesAgree(t, bridgeFixture)

		c.Len(got, 5, "blocks from the full fixture")
		c.Len(got[3].GetToolResult().GetContent(), 2, "nested tool result content")
		c.NotNil(got[3].GetToolResult().GetContent()[1].GetImage(), "nested image survives")
	})

	t.Run("past an opaque block", func(t *testing.T) {
		c := assert.NewAborting(t)
		got := bridgesAgree(t, bridgeOpaqueFixture)

		c.Len(got, 2, "blocks emitted past the opaque one")
		c.Eq(int32(1), got[0].Index, "first emitted block keeps its source index")
		c.Eq(int32(2), got[1].Index, "second emitted block keeps its source index")
	})
}

// bridgesAgree decodes content through both routes, asserts the two bridges
// produced identical blocks, and returns the native bridge's result.
func bridgesAgree(t *testing.T, content string) []*rafikiv1.ContentBlock {
	t.Helper()
	c := assert.NewAborting(t)

	m, err := msganthropic.Decode("assistant", []byte(content))
	c.NoError(err, "decode content into msg.Message")

	var sdkContent []anthropic.ContentBlockParamUnion
	c.NoError(json.Unmarshal([]byte(content), &sdkContent), "decode content into SDK content blocks")
	want := eventconv.BlocksFromParam(anthropic.NewAssistantMessage(sdkContent...))
	got := eventconv.BlocksFromMsg(m)
	c.Len(got, len(want), "got %d blocks from msg, want %d from the SDK param", len(got), len(want))

	wantAll := proto.Clone(&rafikiv1.AssistantMessage{Content: want}).(*rafikiv1.AssistantMessage)
	gotAll := proto.Clone(&rafikiv1.AssistantMessage{Content: got}).(*rafikiv1.AssistantMessage)
	for i := range want {
		if want[i].GetToolUse() == nil || got[i].GetToolUse() == nil {
			continue
		}
		var wv, gv any
		c.NoError(json.Unmarshal([]byte(want[i].GetToolUse().InputJson), &wv), "block %d: unmarshal BlocksFromParam InputJson", i)
		c.NoError(json.Unmarshal([]byte(got[i].GetToolUse().InputJson), &gv), "block %d: unmarshal BlocksFromMsg InputJson", i)
		c.EqDeep(wv, gv, "block %d: tool input", i)
		wantAll.Content[i].GetToolUse().InputJson = ""
		gotAll.Content[i].GetToolUse().InputJson = ""
	}
	c.True(proto.Equal(wantAll, gotAll), "bridges disagree: BlocksFromParam=%v BlocksFromMsg=%v", want, got)

	return got
}
