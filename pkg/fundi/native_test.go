package fundi_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/anthropics/anthropic-sdk-go"

	"go.graveland.dev/rafiki/pkg/child"
	"go.graveland.dev/rafiki/pkg/fundi"
	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/llm"

	"github.com/multigres/testkit/assert"
)

type capturingSink struct{ events []*rafikiv1.Event }

func (c *capturingSink) Publish(ev *rafikiv1.Event) { c.events = append(c.events, ev) }

func TestEmitterPublishesNativeUserMessage(t *testing.T) {
	c := assert.NewAborting(t)
	var out bytes.Buffer
	fe := fundi.NewFrontend(bytes.NewReader(nil), &out, nil)
	em := fundi.NewEmitter(fe, "anthropic", nil)

	sink := &capturingSink{}
	em.SetNativeSink(sink)

	em.UserMessage("hello there", nil)

	c.Len(sink.events, 1, "got %d native events, want 1", len(sink.events))
	um := sink.events[0].GetUserMessage()
	c.NotNil(um, "event is not a user message")
	if len(um.Content) != 1 || um.Content[0].GetText().GetText() != "hello there" {
		t.Fatalf("unexpected content: %+v", um.Content)
	}
}

// A nil sink must be a complete no-op: existing callers pass none.
func TestEmitterWithNoSinkDoesNotPanic(t *testing.T) {
	var out bytes.Buffer
	fe := fundi.NewFrontend(bytes.NewReader(nil), &out, nil)
	em := fundi.NewEmitter(fe, "anthropic", nil)

	em.UserMessage("hello there", nil)

	assert.NewAborting(t).NotEq(0, out.Len(), "pi frame output disappeared; the pi path must be unchanged")
}

// Images ride the native user_message event only, in llm.UserContent's order:
// images first, the text last, and an image with no data skipped entirely.
func TestEmitterUserMessageCarriesImages(t *testing.T) {
	c := assert.NewAborting(t)
	var out bytes.Buffer
	fe := fundi.NewFrontend(bytes.NewReader(nil), &out, nil)
	em := fundi.NewEmitter(fe, "anthropic", nil)

	sink := &capturingSink{}
	em.SetNativeSink(sink)

	em.UserMessage("look", []llm.UserImage{
		{MediaType: "image/png", Data: []byte{1, 2, 3}},
		{MediaType: "image/png"},
		{MediaType: "image/jpeg", Data: []byte{4}},
	})

	c.Len(sink.events, 1, "got %d native events, want 1", len(sink.events))
	um := sink.events[0].GetUserMessage()
	c.NotNil(um, "event is not a user message")
	c.Len(um.Content, 3, "got %d content blocks, want 3 (empty-Data image skipped)", len(um.Content))
	img0 := um.Content[0].GetImage()
	c.Require().NotNil(img0, "content[0] is not an image")
	c.Eq("image/png", img0.MediaType, "content[0] media type")
	c.EqDiff([]byte{1, 2, 3}, img0.Data, "content[0] data")
	c.Eq(int32(0), um.Content[0].Index, "content[0] index")
	img1 := um.Content[1].GetImage()
	c.Require().NotNil(img1, "content[1] is not an image")
	c.Eq("image/jpeg", img1.MediaType, "content[1] media type")
	c.EqDiff([]byte{4}, img1.Data, "content[1] data")
	c.Eq(int32(1), um.Content[1].Index, "content[1] index")
	c.Eq("look", um.Content[2].GetText().GetText(), "content[2] text")
	c.Eq(int32(2), um.Content[2].Index, "content[2] index")
}

// An image-only prompt has no text block at all — matching llm.UserContent,
// which omits empty text rather than sending a content-free block.
func TestEmitterImageOnlyPromptHasNoTextBlock(t *testing.T) {
	c := assert.NewAborting(t)
	var out bytes.Buffer
	fe := fundi.NewFrontend(bytes.NewReader(nil), &out, nil)
	em := fundi.NewEmitter(fe, "anthropic", nil)

	sink := &capturingSink{}
	em.SetNativeSink(sink)

	em.UserMessage("", []llm.UserImage{{MediaType: "image/png", Data: []byte{1}}})

	um := sink.events[0].GetUserMessage()
	c.Require().NotNil(um, "event is not a user message")
	c.Len(um.Content, 1, "got %d content blocks, want 1", len(um.Content))
	c.NotNil(um.Content[0].GetImage(), "sole content block is not an image")
}

// A multi-call agentic turn must publish the FINAL call's usage on turn_end —
// the prompt size the NEXT call carries — never the sum across calls. An
// agentic turn makes one LLM call per tool round and every call re-reads the
// cached prefix, so summing cache_read counts the context once per call: a
// real conversation that never sent more than ~104k of a 1M window once read
// as "3888k", which is Σ over 45 calls. cost_usd keeps turn-total semantics
// and agent_end's pi usage stays the summed throughput.
func TestTurnEndUsageIsTheFinalCallNotTheSum(t *testing.T) {
	c := assert.NewCollecting(t)
	var out bytes.Buffer
	fe := fundi.NewFrontend(bytes.NewReader(nil), &out, nil)
	em := fundi.NewEmitter(fe, "anthropic", nil)
	sink := &capturingSink{}
	em.SetNativeSink(sink)

	assistantTurn := func(input, cacheRead int64) {
		t.Helper()
		raw := fmt.Sprintf(`{"id":"msg_%d","type":"message","role":"assistant","model":"m",
			"stop_reason":"tool_use","content":[{"type":"text","text":"x"}],
			"usage":{"input_tokens":%d,"output_tokens":10,"cache_read_input_tokens":%d}}`,
			input, input, cacheRead)
		var resp anthropic.Message
		c.Require().NoError(json.Unmarshal([]byte(raw), &resp))
		em.AssistantTurn(&resp)
	}

	assistantTurn(1000, 50_000) // call 1
	assistantTurn(2000, 60_000) // call 2, the final one
	em.AgentEnd()

	var te *rafikiv1.TurnEnd
	for _, ev := range sink.events {
		if p := ev.GetTurnEnd(); p != nil {
			te = p
		}
	}
	c.Require().NotNil(te, "no turn_end event published")
	u := te.GetUsage()
	if u.GetInputTokens() != 2000 || u.GetCacheReadTokens() != 60_000 {
		t.Errorf("turn_end usage = input %d cache_read %d, want the FINAL call's 2000/60000 (a sum reads 3000/110000)",
			u.GetInputTokens(), u.GetCacheReadTokens())
	}
	c.NotNil(te.CostUsd, "turn_end.cost_usd unset; it keeps turn-total semantics and must stay present")

	// The pi agent_end frame still carries the SUMMED throughput (the fundi
	// extension usage its consumers read), so the two vocabularies stay
	// distinct: turn_end = reading, agent_end = turn total.
	var agentEndUsage child.PiUsage
	found := false
	for _, line := range strings.Split(out.String(), "\n") {
		var frame struct {
			Type  string         `json:"type"`
			Usage *child.PiUsage `json:"usage"`
		}
		if json.Unmarshal([]byte(line), &frame) != nil || frame.Type != "agent_end" {
			continue
		}
		if frame.Usage != nil {
			agentEndUsage, found = *frame.Usage, true
		}
	}
	c.Require().True(found, "agent_end frame carried no usage")
	c.False(agentEndUsage.Input != 3000 || agentEndUsage.CacheRead != 110_000, "agent_end usage = input %d cache_read %d, want the turn sum 3000/110000", agentEndUsage.Input, agentEndUsage.CacheRead)
}
