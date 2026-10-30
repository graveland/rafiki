package msg_test

import (
	"encoding/json"
	"testing"

	"github.com/multigres/testkit/assert"
	"go.graveland.dev/rafiki/pkg/msg"
)

func addAll(c *assert.C, a *msg.Accumulator, events ...msg.StreamEvent) {
	for i, ev := range events {
		c.NoError(a.Add(ev), "Add event %d (%T)", i, ev)
	}
}

func textAt(c *assert.C, r *msg.Response, i int) msg.Text {
	b, ok := r.Content[i].(msg.Text)
	c.True(ok, "content[%d] is a Text, got %T", i, r.Content[i])
	return b
}

func thinkingAt(c *assert.C, r *msg.Response, i int) msg.Thinking {
	b, ok := r.Content[i].(msg.Thinking)
	c.True(ok, "content[%d] is a Thinking, got %T", i, r.Content[i])
	return b
}

func toolUseAt(c *assert.C, r *msg.Response, i int) msg.ToolUse {
	b, ok := r.Content[i].(msg.ToolUse)
	c.True(ok, "content[%d] is a ToolUse, got %T", i, r.Content[i])
	return b
}

func opaqueAt(c *assert.C, r *msg.Response, i int) msg.Opaque {
	b, ok := r.Content[i].(msg.Opaque)
	c.True(ok, "content[%d] is an Opaque, got %T", i, r.Content[i])
	return b
}

func TestAccumulateTextAndThinking(t *testing.T) {
	c := assert.NewAborting(t)
	a := msg.NewAccumulator()
	c.False(a.HasContent(), "an empty accumulator has no content")

	addAll(c, a,
		msg.MessageStart{ID: "msg_1", Model: "claude-x", Usage: msg.Usage{Input: msg.Ptr(10)}},
		msg.BlockStart{Index: 0, Block: msg.Text{Text: "Hel"}},
		msg.BlockDelta{Index: 0, Kind: msg.DeltaText, Text: "lo"},
		msg.BlockStop{Index: 0},
		msg.BlockStart{Index: 1, Block: msg.Thinking{Text: "thin", Sig: msg.Signature{Dialect: msg.DialectAnthropic, Value: "sig"}}},
		msg.BlockDelta{Index: 1, Kind: msg.DeltaThinking, Text: "king"},
		msg.BlockDelta{Index: 1, Kind: msg.DeltaSignature, Text: "-more"},
		msg.BlockStop{Index: 1},
	)
	c.True(a.HasContent(), "a started block is content")

	r := a.Response()
	c.Eq("msg_1", r.ID)
	c.Eq("claude-x", r.Model)
	c.Len(r.Content, 2)
	c.Eq("Hello", textAt(c, r, 0).Text, "initial text is kept and deltas append")
	c.Eq("thinking", thinkingAt(c, r, 1).Text, "initial thinking text is kept and deltas append")
}

func TestAccumulateToolInputReplacesPlaceholder(t *testing.T) {
	c := assert.NewAborting(t)
	a := msg.NewAccumulator()
	addAll(c, a,
		msg.BlockStart{Index: 0, Block: msg.ToolUse{ID: "toolu_1", Name: "read", Input: json.RawMessage(`{}`)}},
		msg.BlockDelta{Index: 0, Kind: msg.DeltaInputJSON, Text: `{"a":`},
		msg.BlockDelta{Index: 0, Kind: msg.DeltaInputJSON, Text: `1}`},
		msg.BlockStop{Index: 0},
	)
	c.Eq(`{"a":1}`, string(toolUseAt(c, a.Response(), 0).Input),
		"the first fragment replaces the empty-object placeholder and later ones append")
}

func TestAccumulateToolInputEmptyStringStart(t *testing.T) {
	c := assert.NewAborting(t)
	a := msg.NewAccumulator()
	addAll(c, a,
		msg.BlockStart{Index: 0, Block: msg.ToolUse{ID: "toolu_1", Name: "read", Input: json.RawMessage(`""`)}},
		msg.BlockStop{Index: 0},
	)
	c.Eq(`{}`, string(toolUseAt(c, a.Response(), 0).Input),
		"an empty-string start with no deltas finalizes to an empty object")
}

func TestAccumulateToolInputInvalidDroppedAtStop(t *testing.T) {
	c := assert.NewAborting(t)
	a := msg.NewAccumulator()
	addAll(c, a,
		msg.BlockStart{Index: 0, Block: msg.ToolUse{ID: "toolu_1", Name: "read", Input: json.RawMessage(`{}`)}},
		msg.BlockDelta{Index: 0, Kind: msg.DeltaInputJSON, Text: `{"a":`},
		msg.BlockStop{Index: 0},
	)
	c.Eq(`{}`, string(toolUseAt(c, a.Response(), 0).Input),
		"an incomplete input is dropped rather than handed to a tool")
}

func TestAccumulateSnapshotOpenToolIsPrefix(t *testing.T) {
	c := assert.NewAborting(t)
	a := msg.NewAccumulator()
	addAll(c, a,
		msg.BlockStart{Index: 0, Block: msg.ToolUse{ID: "toolu_1", Name: "read", Input: json.RawMessage(`{}`)}},
		msg.BlockDelta{Index: 0, Kind: msg.DeltaInputJSON, Text: `{"a":`},
	)
	first := a.Response()
	c.Eq(`{"a":`, string(toolUseAt(c, first, 0).Input), "an open ToolUse exposes its JSON prefix")

	addAll(c, a, msg.BlockDelta{Index: 0, Kind: msg.DeltaInputJSON, Text: `1}`})
	second := a.Response()
	c.Eq(`{"a":1}`, string(toolUseAt(c, second, 0).Input))
	c.Eq(`{"a":`, string(toolUseAt(c, first, 0).Input), "a later Add must not rewrite an earlier snapshot")
}

func TestAccumulateUsageDeltaDoesNotClobberInputWithZero(t *testing.T) {
	c := assert.NewAborting(t)
	a := msg.NewAccumulator()
	addAll(c, a,
		msg.MessageStart{ID: "msg_1", Usage: msg.Usage{Input: msg.Ptr(100)}},
		msg.MessageDelta{Usage: msg.Usage{Input: msg.Ptr(0)}},
	)
	u := a.Response().Usage
	c.NotNil(u.Input)
	c.Eq(int64(100), msg.Val(u.Input), "a reported zero must not erase a counted input")
}

func TestAccumulateUsageDeltaBackfillsInput(t *testing.T) {
	c := assert.NewAborting(t)
	a := msg.NewAccumulator()
	addAll(c, a,
		msg.MessageStart{ID: "msg_1", Usage: msg.Usage{Input: msg.Ptr(0)}},
		msg.MessageDelta{Usage: msg.Usage{Input: msg.Ptr(50)}},
	)
	u := a.Response().Usage
	c.NotNil(u.Input)
	c.Eq(int64(50), msg.Val(u.Input), "a later non-zero count fills a zero the start reported")
}

func TestAccumulateUsageDeltaFillsNil(t *testing.T) {
	c := assert.NewAborting(t)
	a := msg.NewAccumulator()
	addAll(c, a,
		msg.MessageStart{ID: "msg_1"},
		msg.MessageDelta{Usage: msg.Usage{CacheRead: msg.Ptr(0)}},
	)
	u := a.Response().Usage
	c.NotNil(u.CacheRead, "a reported zero must stay distinguishable from not reported")
	c.Eq(int64(0), msg.Val(u.CacheRead))
	c.Nil(u.Input, "a count nobody reported stays nil")
}

func TestAccumulateUsageOutputAlwaysOverwrites(t *testing.T) {
	c := assert.NewAborting(t)
	a := msg.NewAccumulator()
	addAll(c, a,
		msg.MessageStart{ID: "msg_1", Usage: msg.Usage{Output: msg.Ptr(10)}},
		msg.MessageDelta{Usage: msg.Usage{Output: msg.Ptr(0)}},
	)
	u := a.Response().Usage
	c.NotNil(u.Output)
	c.Eq(int64(0), msg.Val(u.Output), "output is a final total, so a reported zero wins")
}

func TestAccumulateSignatureDialectPreserved(t *testing.T) {
	c := assert.NewAborting(t)
	a := msg.NewAccumulator()
	addAll(c, a,
		msg.BlockStart{Index: 0, Block: msg.Thinking{Sig: msg.Signature{Dialect: msg.DialectResponsesAPI}}},
		msg.BlockDelta{Index: 0, Kind: msg.DeltaThinking, Text: "why"},
		msg.BlockDelta{Index: 0, Kind: msg.DeltaSignature, Text: "abc"},
		msg.BlockStop{Index: 0},
	)
	th := thinkingAt(c, a.Response(), 0)
	c.Eq("why", th.Text)
	c.Eq("abc", th.Sig.Value, "signature deltas append to the signature")
	c.Eq(msg.DialectResponsesAPI, th.Sig.Dialect, "the codec's preset dialect is kept as given")
}

func TestAccumulateProviderPreserved(t *testing.T) {
	c := assert.NewAborting(t)

	served := msg.NewAccumulator()
	addAll(c, served, msg.MessageStart{ID: "msg_1", Model: "claude-x", Provider: "Anthropic"})
	c.Eq("Anthropic", served.Response().Provider)

	absent := msg.NewAccumulator()
	addAll(c, absent, msg.MessageStart{ID: "msg_1", Model: "claude-x"})
	c.Eq("", absent.Response().Provider, "an absent provider is never inferred from the configured sender")
}

func TestAccumulateFinalBlockReplacement(t *testing.T) {
	c := assert.NewAborting(t)
	a := msg.NewAccumulator()

	serverTool := func(input string) msg.Opaque {
		return msg.Opaque{
			Provider: msg.DialectAnthropic,
			Raw:      json.RawMessage(`{"type":"server_tool_use","id":"srv_1","name":"web_search","input":` + input + `}`),
		}
	}

	addAll(c, a,
		msg.BlockStart{Index: 0, Block: msg.Text{Text: "par"}},
		msg.BlockDelta{Index: 0, Kind: msg.DeltaText, Text: "tial"},
		msg.BlockStop{Index: 0, Block: msg.Text{Text: "final with citations"}},
		msg.BlockStart{Index: 1, Block: serverTool(`{}`)},
		msg.BlockStop{Index: 1, Block: serverTool(`{"query":"x"}`)},
	)

	r := a.Response()
	c.Len(r.Content, 2)
	c.Eq("final with citations", textAt(c, r, 0).Text,
		"a finalized replacement supersedes the accumulated text")

	got := opaqueAt(c, r, 1)
	c.Eq(msg.DialectAnthropic, got.Provider)
	c.Eq(`{"type":"server_tool_use","id":"srv_1","name":"web_search","input":{"query":"x"}}`, string(got.Raw),
		"a finalized server tool replaces the open one")
	_, isToolUse := r.Content[1].(msg.ToolUse)
	c.False(isToolUse, "an Opaque server tool must never be reinterpreted as a client ToolUse")
}

func TestAccumulateFinalBlockSnapshotIsolation(t *testing.T) {
	c := assert.NewAborting(t)
	a := msg.NewAccumulator()
	addAll(c, a,
		msg.MessageStart{ID: "msg_1", Usage: msg.Usage{Input: msg.Ptr(10)}},
		msg.BlockStart{Index: 0, Block: msg.Opaque{
			Provider: msg.DialectAnthropic,
			Raw:      json.RawMessage(`{"type":"server_tool_use","id":"srv_1","input":{}}`),
		}},
		msg.BlockStop{Index: 0, Block: msg.Opaque{
			Provider: msg.DialectAnthropic,
			Raw:      json.RawMessage(`{"type":"server_tool_use","id":"srv_1","input":{"q":"x"}}`),
		}},
		msg.BlockStart{Index: 1, Block: msg.Image{MediaType: "image/png", Data: []byte{1, 2, 3}}},
		msg.BlockStop{Index: 1},
	)

	first := a.Response()
	opaque := opaqueAt(c, first, 0)
	opaque.Raw[0] = 'X'
	image, ok := first.Content[1].(msg.Image)
	c.True(ok)
	image.Data[0] = 9
	*first.Usage.Input = 999

	second := a.Response()
	c.Eq(`{"type":"server_tool_use","id":"srv_1","input":{"q":"x"}}`, string(opaqueAt(c, second, 0).Raw),
		"mutating a snapshot's Opaque bytes must not reach the accumulator")
	image2, ok := second.Content[1].(msg.Image)
	c.True(ok)
	c.EqDeep([]byte{1, 2, 3}, image2.Data, "mutating a snapshot's image bytes must not reach the accumulator")
	c.Eq(int64(10), msg.Val(second.Usage.Input), "mutating a snapshot's usage must not reach the accumulator")
}

func TestAccumulateRejectsReplacementKindMismatch(t *testing.T) {
	c := assert.NewAborting(t)

	a := msg.NewAccumulator()
	addAll(c, a, msg.BlockStart{Index: 0, Block: msg.Opaque{
		Provider: msg.DialectAnthropic,
		Raw:      json.RawMessage(`{"type":"server_tool_use","id":"srv_1"}`),
	}})
	err := a.Add(msg.BlockStop{Index: 0, Block: msg.ToolUse{ID: "toolu_1", Name: "read", Input: json.RawMessage(`{}`)}})
	c.Error(err)
	c.ErrorContains(err, "msg: accumulate:")
	c.ErrorContains(err, "started as a msg.Opaque")
	c.Eq(msg.DialectAnthropic, opaqueAt(c, a.Response(), 0).Provider, "a refused replacement leaves the block as it was")

	b := msg.NewAccumulator()
	addAll(c, b, msg.BlockStart{Index: 0, Block: msg.Text{Text: "x"}})
	err = b.Add(msg.BlockStop{Index: 0, Block: msg.ToolUse{ID: "toolu_1", Name: "read"}})
	c.Error(err)
	c.ErrorContains(err, "started as a msg.Text")
}

func TestAccumulateRejectsUnknownIndex(t *testing.T) {
	c := assert.NewAborting(t)
	a := msg.NewAccumulator()

	err := a.Add(msg.BlockDelta{Index: 0, Kind: msg.DeltaText, Text: "x"})
	c.Error(err)
	c.ErrorContains(err, "msg: accumulate:")
	c.ErrorContains(err, "never started")

	err = a.Add(msg.BlockStop{Index: 2})
	c.Error(err)
	c.ErrorContains(err, "never started")
}

func TestAccumulateRejectsWrongDeltaKind(t *testing.T) {
	c := assert.NewAborting(t)
	a := msg.NewAccumulator()
	addAll(c, a,
		msg.BlockStart{Index: 0, Block: msg.Thinking{}},
		msg.BlockStart{Index: 1, Block: msg.Text{}},
		msg.BlockStart{Index: 2, Block: msg.Opaque{Provider: msg.DialectAnthropic, Raw: json.RawMessage(`{"type":"redacted_thinking","data":"x"}`)}},
	)

	err := a.Add(msg.BlockDelta{Index: 0, Kind: msg.DeltaText, Text: "x"})
	c.Error(err, "text deltas belong to Text blocks")
	c.ErrorContains(err, "msg: accumulate:")

	err = a.Add(msg.BlockDelta{Index: 1, Kind: msg.DeltaInputJSON, Text: "{}"})
	c.Error(err, "input deltas belong to ToolUse blocks")

	err = a.Add(msg.BlockDelta{Index: 2, Kind: msg.DeltaThinking, Text: "x"})
	c.Error(err, "an Opaque block accepts no generic deltas")
	c.ErrorContains(err, "accepts no deltas")
}

func TestAccumulateRejectsDoubleStart(t *testing.T) {
	c := assert.NewAborting(t)
	a := msg.NewAccumulator()
	addAll(c, a, msg.BlockStart{Index: 0, Block: msg.Text{Text: "x"}})

	err := a.Add(msg.BlockStart{Index: 0, Block: msg.Text{Text: "y"}})
	c.Error(err)
	c.ErrorContains(err, "msg: accumulate:")
	c.ErrorContains(err, "second start for block 0")
	c.Len(a.Response().Content, 1, "a refused start must not add a block")
}

func TestDeliversContent(t *testing.T) {
	c := assert.NewAborting(t)

	for _, tc := range []struct {
		ev   msg.StreamEvent
		want bool
	}{
		{ev: msg.MessageStart{}, want: false},
		{ev: msg.BlockStart{Index: 0, Block: msg.Text{}}, want: true},
		{ev: msg.BlockDelta{Index: 0, Kind: msg.DeltaText}, want: true},
		{ev: msg.BlockStop{Index: 0}, want: true},
		{ev: msg.MessageDelta{}, want: false},
		{ev: &msg.BlockDelta{Index: 0, Kind: msg.DeltaText}, want: true},
		{ev: &msg.MessageStart{}, want: false},
	} {
		c.Eq(tc.want, msg.DeliversContent(tc.ev), "DeliversContent(%T)", tc.ev)
	}
}
