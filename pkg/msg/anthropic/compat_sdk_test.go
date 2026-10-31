package anthropic_test

import (
	"encoding/json"
	"testing"

	sdk "github.com/anthropics/anthropic-sdk-go"

	"go.graveland.dev/rafiki/pkg/msg"
	"go.graveland.dev/rafiki/pkg/msg/anthropic"

	"github.com/multigres/testkit/assert"
)

// jsonEqual compares two JSON documents by value.
func jsonEqual(t *testing.T, want, got, msg string) {
	t.Helper()
	c := assert.NewAborting(t)
	var w, g any
	c.NoError(json.Unmarshal([]byte(want), &w), "%s: want is not JSON", msg)
	c.NoError(json.Unmarshal([]byte(got), &g), "%s: got is not JSON", msg)
	c.EqDeep(w, g, "%s: %s", msg, got)
}

// TestRequestFromSDKCarriesTheRequest pins the SDK → native conversion this
// wave runs at the sender call: every field the client still sets on SDK params
// reaches the native request, and the params' extra fields come back separately
// (they ride EncodeOptions.Extra).
func TestRequestFromSDKCarriesTheRequest(t *testing.T) {
	c := assert.NewAborting(t)
	temperature := 0.2
	topK := int64(7)
	params := sdk.MessageNewParams{
		Model:         "claude-haiku-4-5",
		MaxTokens:     512,
		System:        []sdk.TextBlockParam{{Text: "be terse"}, {Text: "be kind"}},
		Messages:      []sdk.MessageParam{sdk.NewUserMessage(sdk.NewTextBlock("hi"))},
		Temperature:   sdk.Float(temperature),
		TopK:          sdk.Int(topK),
		Thinking:      sdk.ThinkingConfigParamOfEnabled(2048),
		ToolChoice:    sdk.ToolChoiceUnionParam{OfTool: &sdk.ToolChoiceToolParam{Name: "read_file"}},
		Tools:         []sdk.ToolUnionParam{sdk.ToolUnionParamOfTool(sdk.ToolInputSchemaParam{Properties: map[string]any{"path": map[string]any{"type": "string"}}}, "read_file")},
		StopSequences: []string{"STOP"},
	}
	params.Tools[0].OfTool.Description = sdk.String("read a file")
	params.SetExtraFields(map[string]any{"provider": map[string]any{"only": []string{"fireworks"}}})

	req, extra, err := anthropic.RequestFromSDK(params)
	c.NoError(err, "RequestFromSDK")

	c.Eq("claude-haiku-4-5", req.Model, "model")
	c.Eq(int64(512), req.MaxTokens, "max tokens")
	c.Eq(2, len(req.System), "system blocks")
	c.Eq("be kind", req.System[1].Text, "system text")
	c.Eq(1, len(req.Messages), "messages")
	c.Require().Len(req.Tools, 1, "tools")
	c.Eq("read_file", req.Tools[0].Name, "tool name")
	c.Eq("read a file", req.Tools[0].Description, "tool description")
	jsonEqual(t, `{"type":"object","properties":{"path":{"type":"string"}}}`, string(req.Tools[0].InputSchema), "tool schema")
	c.Eq("read_file", req.ForceTool, "forced tool")
	c.Require().NotNil(req.Temperature, "temperature")
	c.Eq(temperature, *req.Temperature, "temperature value")
	c.Require().NotNil(req.TopK, "top_k")
	c.Eq(topK, *req.TopK, "top_k value")
	c.Eq(int64(2048), req.ThinkingBudget, "thinking budget")
	c.EqDeep(map[string]any{"provider": map[string]any{"only": []string{"fireworks"}}}, extra, "extra fields")
}

// TestRequestFromSDKDerivesCachePolicy pins the breakpoint rule this wave
// needs: the system TTL comes from the last system block's marker, and the
// message TTL and count from the message markers, clamped to [1, 3].
func TestRequestFromSDKDerivesCachePolicy(t *testing.T) {
	c := assert.NewAborting(t)
	mk := func(systemMarker, msgMarker sdk.CacheControlEphemeralParam, blocks int) sdk.MessageNewParams {
		content := make([]sdk.ContentBlockParamUnion, 0, blocks)
		for i := 0; i < blocks; i++ {
			block := sdk.TextBlockParam{Text: "x"}
			if i == blocks-1 {
				block.CacheControl = msgMarker
			}
			content = append(content, sdk.ContentBlockParamUnion{OfText: &block})
		}
		return sdk.MessageNewParams{
			Model:     "m",
			MaxTokens: 16,
			System:    []sdk.TextBlockParam{{Text: "sys", CacheControl: systemMarker}},
			Messages:  []sdk.MessageParam{{Role: "user", Content: content}},
		}
	}
	none := sdk.CacheControlEphemeralParam{}
	five := sdk.NewCacheControlEphemeralParam()
	hour := sdk.NewCacheControlEphemeralParam()
	hour.TTL = sdk.CacheControlEphemeralTTLTTL1h

	req, _, err := anthropic.RequestFromSDK(mk(five, five, 1))
	c.NoError(err, "RequestFromSDK")
	c.Eq(msg.Cache5m, req.Cache.SystemTTL, "system TTL")
	c.Eq(msg.Cache5m, req.Cache.MessagesTTL, "messages TTL")
	c.Eq(1, req.Cache.Breakpoints, "breakpoints")

	req, _, err = anthropic.RequestFromSDK(mk(hour, hour, 1))
	c.NoError(err, "RequestFromSDK (1h)")
	c.Eq(msg.Cache1h, req.Cache.SystemTTL, "system TTL (1h)")
	c.Eq(msg.Cache1h, req.Cache.MessagesTTL, "messages TTL (1h)")

	// No markers at all: caching is off, and the request encodes without a
	// marker.
	req, _, err = anthropic.RequestFromSDK(mk(none, none, 1))
	c.NoError(err, "RequestFromSDK (none)")
	c.Eq(msg.CacheOff, req.Cache.SystemTTL, "system TTL (off)")
	c.Eq(msg.CacheOff, req.Cache.MessagesTTL, "messages TTL (off)")
	c.Eq(0, req.Cache.Breakpoints, "breakpoints (off)")
	body, err := anthropic.Encode(req, msg.EncodeOptions{})
	c.NoError(err, "encode")
	c.NotStrContains(string(body), "cache_control", "a marker was invented")
}

// TestRequestFromSDKClampsBreakpoints pins the clamp: the API accepts at most
// three message breakpoints, so a params value carrying more derives three.
func TestRequestFromSDKClampsBreakpoints(t *testing.T) {
	c := assert.NewAborting(t)
	marker := sdk.NewCacheControlEphemeralParam()
	content := make([]sdk.ContentBlockParamUnion, 0, 5)
	for i := 0; i < 5; i++ {
		block := sdk.TextBlockParam{Text: "x", CacheControl: marker}
		content = append(content, sdk.ContentBlockParamUnion{OfText: &block})
	}
	req, _, err := anthropic.RequestFromSDK(sdk.MessageNewParams{
		Model: "m", MaxTokens: 16,
		Messages: []sdk.MessageParam{{Role: "user", Content: content}},
	})
	c.NoError(err, "RequestFromSDK")
	c.Eq(3, req.Cache.Breakpoints, "breakpoints clamped")
}

// TestToSDKMessageKeepsProviderAndBlocks pins the native → SDK direction: the
// content blocks keep their own JSON (which is what ToParam and the
// accumulators read), the usage counters arrive, and OpenRouter's served-by
// field rides the top-level "provider" key — the only place ProviderOf looks.
func TestToSDKMessageKeepsProviderAndBlocks(t *testing.T) {
	c := assert.NewAborting(t)
	resp := &msg.Response{
		ID:         "msg_1",
		Model:      "claude-x",
		Provider:   "Together",
		StopReason: msg.StopToolUse,
		Usage: msg.Usage{
			Input:      msg.Ptr(11),
			Output:     msg.Ptr(3),
			CacheRead:  msg.Ptr(5),
			CacheWrite: msg.Ptr(2),
		},
		Content: []msg.Block{
			msg.Text{Text: "calling"},
			msg.ToolUse{ID: "toolu_1", Name: "read_file", Input: json.RawMessage(`{"path":"a"}`)},
		},
	}

	got, err := anthropic.ToSDKMessage(resp)
	c.NoError(err, "ToSDKMessage")
	c.Eq("msg_1", got.ID, "id")
	c.Eq("claude-x", string(got.Model), "model")
	c.Eq("tool_use", string(got.StopReason), "stop reason")
	c.Eq(int64(11), got.Usage.InputTokens, "input tokens")
	c.Eq(int64(3), got.Usage.OutputTokens, "output tokens")
	c.Eq(int64(5), got.Usage.CacheReadInputTokens, "cache read")
	c.Eq(int64(2), got.Usage.CacheCreationInputTokens, "cache write")
	c.Require().Len(got.Content, 2, "content blocks")
	c.Eq("text", got.Content[0].Type, "block 0 type")
	c.Eq("calling", got.Content[0].Text, "block 0 text")
	c.Eq("tool_use", got.Content[1].Type, "block 1 type")
	c.Eq("toolu_1", got.Content[1].ID, "block 1 id")
	jsonEqual(t, `{"path":"a"}`, string(got.Content[1].Input), "block 1 input")

	// Provider survives as an extra field, which is where ProviderOf reads it.
	raw, ok := got.JSON.ExtraFields["provider"]
	c.True(ok, "the provider field is missing from the SDK message")
	c.Eq(`"Together"`, raw.Raw(), "provider extra")
}

// TestSDKEventAdapterReconcilesFinalBlocks pins the adapter's whole job: the
// events it emits drive the SDK accumulator to the same message the native
// stream describes — including the parts only the finalized block carries (a
// citation list, a server-tool block's completed input) and the provider on
// message_start.
func TestSDKEventAdapterReconcilesFinalBlocks(t *testing.T) {
	c := assert.NewAborting(t)
	adapter := &anthropic.SDKEventAdapter{}

	var events []sdk.MessageStreamEventUnion
	add := func(ev msg.StreamEvent) {
		out, err := adapter.Add(ev)
		c.NoError(err, "Add(%T)", ev)
		events = append(events, out...)
	}

	add(msg.MessageStart{ID: "msg_1", Model: "m", Provider: "Together", Usage: msg.Usage{Input: msg.Ptr(9)}})
	// A text block whose citations only exist in the finalized form.
	add(msg.BlockStart{Index: 0, Block: msg.Text{}})
	add(msg.BlockDelta{Index: 0, Kind: msg.DeltaText, Text: "hi"})
	add(msg.BlockStop{Index: 0, Block: msg.Text{Text: "hi", Ext: msg.Ext{Dialect: msg.DialectAnthropic, Raw: `{"citations":[{"type":"web_search_result_location","url":"https://example.com"}]}`}}})
	// An opaque server-tool block: its arguments never arrive as deltas.
	add(msg.BlockStart{Index: 1, Block: msg.Opaque{Provider: msg.DialectAnthropic, Raw: json.RawMessage(`{"type":"server_tool_use","id":"srv_1","name":"web_search","input":{}}`)}})
	add(msg.BlockStop{Index: 1, Block: msg.Opaque{Provider: msg.DialectAnthropic, Raw: json.RawMessage(`{"type":"server_tool_use","id":"srv_1","name":"web_search","input":{"query":"rafiki"}}`)}})
	add(msg.MessageDelta{StopReason: msg.StopEndTurn, Usage: msg.Usage{Output: msg.Ptr(4)}})

	acc := sdk.Message{}
	for _, ev := range events {
		c.NoError(acc.Accumulate(ev), "Accumulate(%s)", ev.Type)
	}

	c.Eq("msg_1", acc.ID, "id")
	c.Eq("end_turn", string(acc.StopReason), "stop reason")
	c.Eq(int64(9), acc.Usage.InputTokens, "input tokens")
	c.Eq(int64(4), acc.Usage.OutputTokens, "output tokens")
	raw, ok := acc.JSON.ExtraFields["provider"]
	c.True(ok, "the provider did not ride message_start")
	c.Eq(`"Together"`, raw.Raw(), "provider")

	c.Require().Len(acc.Content, 2, "content blocks")
	c.Eq("hi", acc.Content[0].Text, "text block")
	c.Require().Len(acc.Content[0].Citations, 1, "the finalized block's citation was dropped")
	c.Eq("https://example.com", acc.Content[0].Citations[0].URL, "citation url")
	c.Eq("server_tool_use", acc.Content[1].Type, "opaque block type")
	jsonEqual(t, `{"query":"rafiki"}`, string(acc.Content[1].Input), "the server-tool block's completed input was dropped")
}

// TestSDKEventAdapterPassesCoreDeltasThrough pins that the live core deltas are
// not deferred to the final block: a text, thinking and signature delta each
// arrive as its own SDK event, and the finalized replacement adds nothing the
// stream already delivered.
func TestSDKEventAdapterPassesCoreDeltasThrough(t *testing.T) {
	c := assert.NewAborting(t)
	adapter := &anthropic.SDKEventAdapter{}

	out, err := adapter.Add(msg.BlockStart{Index: 0, Block: msg.Thinking{}})
	c.NoError(err, "start")
	c.Require().Len(out, 1, "start events")

	for _, ev := range []msg.StreamEvent{
		msg.BlockDelta{Index: 0, Kind: msg.DeltaThinking, Text: "think"},
		msg.BlockDelta{Index: 0, Kind: msg.DeltaSignature, Text: "sig"},
	} {
		out, err = adapter.Add(ev)
		c.NoError(err, "delta")
		c.Require().Len(out, 1, "one event per delta")
	}

	out, err = adapter.Add(msg.BlockStop{Index: 0, Block: msg.Thinking{
		Text: "think",
		Sig:  msg.Signature{Dialect: msg.DialectAnthropic, Value: "sig"},
	}})
	c.NoError(err, "stop")
	c.Require().Len(out, 1, "nothing was already delivered twice")
	c.Eq("content_block_stop", out[0].Type, "only the stop is emitted")
}

// TestSDKEventAdapterEmitsMessageStop pins the terminal sequence: a native
// message delta is followed by message_stop, which is what re-marshals the
// accumulated message's own JSON.
func TestSDKEventAdapterEmitsMessageStop(t *testing.T) {
	c := assert.NewAborting(t)
	adapter := &anthropic.SDKEventAdapter{}
	out, err := adapter.Add(msg.MessageDelta{StopReason: msg.StopMaxTokens})
	c.NoError(err, "message delta")
	c.Require().Len(out, 2, "message_delta + message_stop")
	c.Eq("message_delta", out[0].Type, "first event")
	c.Eq("max_tokens", out[0].Delta.StopReason, "stop reason")
	c.Eq("message_stop", out[1].Type, "second event")
}

// TestSDKEventAdapterRefusesMisorderedEvents pins the accumulator contract the
// adapter enforces instead of guessing: a delta or a stop for a block that was
// never started is a decoder bug.
func TestSDKEventAdapterRefusesMisorderedEvents(t *testing.T) {
	c := assert.NewAborting(t)
	adapter := &anthropic.SDKEventAdapter{}
	_, err := adapter.Add(msg.BlockDelta{Index: 3, Kind: msg.DeltaText, Text: "x"})
	c.Error(err, "a delta for an unstarted block must be refused")

	adapter = &anthropic.SDKEventAdapter{}
	_, err = adapter.Add(msg.BlockStart{Index: 0, Block: msg.Text{}})
	c.NoError(err, "start")
	_, err = adapter.Add(msg.BlockStart{Index: 0, Block: msg.Text{}})
	c.Error(err, "a second start for the same index must be refused")
}
