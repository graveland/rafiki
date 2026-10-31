// SPDX-License-Identifier: Apache-2.0

package fundi

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/multigres/testkit/assert"

	"go.graveland.dev/rafiki/pkg/conv"
	"go.graveland.dev/rafiki/pkg/eventconv"
	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/llm"
	"go.graveland.dev/rafiki/pkg/store"
)

type lockedSink struct {
	mu     sync.Mutex
	events []*rafikiv1.Event
}

func (s *lockedSink) Publish(ev *rafikiv1.Event) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, ev)
}

func (s *lockedSink) snapshot() []*rafikiv1.Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*rafikiv1.Event(nil), s.events...)
}

// newRowEngine builds a store-backed engine whose native events land in the
// returned sink, so a test can compare them with what GetHistory would report.
func newRowEngine(t *testing.T, ts fakeToolSet, bodies ...string) (*Engine, *lockedSink, func() []*rafikiv1.Event) {
	t.Helper()
	c := assert.NewAborting(t)
	silenceSlog(t)
	if testing.Short() {
		t.Skip("row timestamps need a database")
	}
	pool, _ := dbTestPool(t)
	client, err := llm.NewClient(
		llm.WithProviderSender("anthropic", scriptedSender(t, bodies...)),
		llm.WithDefaultModel("claude-x"),
		llm.WithStore(pool))
	c.NoError(err)
	sink := &lockedSink{}
	fe := NewFrontend(strings.NewReader(""), &syncBuffer{}, nil)
	eng, err := NewEngine(EngineConfig{
		Client:     client,
		Tools:      ts,
		Provider:   "anthropic",
		ModelID:    "claude-x",
		Name:       "w1",
		NativeSink: sink,
		ConvOpts:   []llm.ConvOption{llm.Entrypoint("agent"), llm.ByExternalRef(fmt.Sprintf("row-ts-%d", time.Now().UnixNano()))},
	}, fe)
	c.NoError(err)
	eng.Start()
	fe.handler = eng
	history := func() []*rafikiv1.Event {
		nodes, err := conv.NewStore(store.NewConvBackend(pool), conv.Options{}).Transcript(context.Background(), eng.conv.ID)
		c.NoError(err, "load history")
		return eventconv.EventsFromNodes("c", nodes)
	}
	return eng, sink, history
}

func isToolResult(ev *rafikiv1.Event) bool {
	um := ev.GetUserMessage()
	return um != nil && len(um.GetContent()) > 0 && um.GetContent()[0].GetToolResult() != nil
}

func userEvents(evs []*rafikiv1.Event, toolResults bool) []*rafikiv1.Event {
	var out []*rafikiv1.Event
	for _, ev := range evs {
		if ev.GetUserMessage() != nil && isToolResult(ev) == toolResults {
			out = append(out, ev)
		}
	}
	return out
}

func assistantEvents(evs []*rafikiv1.Event) []*rafikiv1.Event {
	var out []*rafikiv1.Event
	for _, ev := range evs {
		if ev.GetAssistantMessage() != nil {
			out = append(out, ev)
		}
	}
	return out
}

func userText(ev *rafikiv1.Event) string {
	var b strings.Builder
	for _, blk := range ev.GetUserMessage().GetContent() {
		b.WriteString(blk.GetText().GetText())
	}
	return b.String()
}

func assertSameTs(t *testing.T, what string, live, hist []*rafikiv1.Event) {
	t.Helper()
	c := assert.NewCollecting(t)
	c.Require().Len(live, len(hist), "%s: live events vs history rows", what)
	for i := range live {
		c.True(live[i].GetTs().AsTime().Equal(hist[i].GetTs().AsTime()),
			"%s[%d]: live ts %v, history created_at %v", what, i, live[i].GetTs().AsTime(), hist[i].GetTs().AsTime())
	}
}

func TestNativeAssistantMessageTsIsRowCreatedAt(t *testing.T) {
	eng, sink, history := newRowEngine(t, fakeToolSet{}, sampleEndTurn)
	eng.HandlePrompt("go")
	eng.Wait()

	assertSameTs(t, "assistant", assistantEvents(sink.snapshot()), assistantEvents(history()))
}

func TestNativeUserMessageTsIsRowCreatedAt(t *testing.T) {
	eng, sink, history := newRowEngine(t, fakeToolSet{}, sampleEndTurn)
	eng.HandlePrompt("go")
	eng.Wait()

	live := userEvents(sink.snapshot(), false)
	assert.NewAborting(t).Len(live, 1, "prompt echo published once")
	assertSameTs(t, "user", live, userEvents(history(), false))
}

const twoToolUses = `{
 "id":"msg_1","type":"message","role":"assistant","model":"claude-x",
 "stop_reason":"tool_use",
 "content":[{"type":"tool_use","id":"tu_slow","name":"slow","input":{}},
            {"type":"tool_use","id":"tu_fast","name":"fast","input":{}}],
 "usage":{"input_tokens":10,"output_tokens":5,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}}`

func toolResultID(ev *rafikiv1.Event) string {
	return ev.GetUserMessage().GetContent()[0].GetToolResult().GetToolUseId()
}

// The slow tool finishes AFTER the fast one, so completion order is the
// reverse of request order; results must still publish in ordinal order.
func TestNativeToolResultTsIsRowCreatedAt(t *testing.T) {
	fastDone := make(chan struct{})
	ts := fakeToolSet{
		"slow": func(ctx context.Context, _ json.RawMessage) (string, error) {
			select {
			case <-fastDone:
			case <-ctx.Done():
			}
			return "slow-out", nil
		},
		"fast": func(context.Context, json.RawMessage) (string, error) {
			close(fastDone)
			return "fast-out", nil
		},
	}
	eng, sink, history := newRowEngine(t, ts, twoToolUses, sampleEndTurn)
	eng.HandlePrompt("go")
	eng.Wait()

	live := userEvents(sink.snapshot(), true)
	hist := userEvents(history(), true)
	c := assert.NewAborting(t)
	c.Len(live, 2, "one result event per tool")
	c.Eq("tu_slow", toolResultID(live[0]), "ordinal order, first")
	c.Eq("tu_fast", toolResultID(live[1]), "ordinal order, second")
	assertSameTs(t, "tool results", live, hist)
}

// Two steers drain together and persist as ONE merged row, so they publish as
// ONE event carrying that row's content and created_at.
func TestSteerEventsMatchPersistedRows(t *testing.T) {
	var eng *Engine
	ts := fakeToolSet{
		"bash": func(context.Context, json.RawMessage) (string, error) {
			eng.HandleSteer("one")
			eng.HandleSteer("two")
			return "ok", nil
		},
	}
	var sink *lockedSink
	var history func() []*rafikiv1.Event
	eng, sink, history = newRowEngine(t, ts, sampleResp, sampleEndTurn)
	eng.HandlePrompt("go")
	eng.Wait()

	live := userEvents(sink.snapshot(), false)
	hist := userEvents(history(), false)
	c := assert.NewAborting(t)
	c.Len(hist, 2, "prompt row + one merged steer row")
	c.Len(live, 2, "one live event per persisted row")
	c.Eq("one\ntwo", userText(live[1]), "merged steer content")
	c.Eq(userText(hist[1]), userText(live[1]), "live content equals the stored row's")
	assertSameTs(t, "user", live, hist)
}

func TestTurnStartTurnEndTsUnchanged(t *testing.T) {
	before := time.Now()
	eng, sink, history := newRowEngine(t, fakeToolSet{}, sampleEndTurn)
	eng.HandlePrompt("go")
	eng.Wait()
	after := time.Now()

	rows := map[time.Time]bool{}
	for _, ev := range history() {
		rows[ev.GetTs().AsTime()] = true
	}
	var seen int
	for _, ev := range sink.snapshot() {
		if ev.GetTurnStart() == nil && ev.GetTurnEnd() == nil {
			continue
		}
		seen++
		at := ev.GetTs().AsTime()
		c := assert.NewCollecting(t)
		c.False(rows[at], "%T ts %v is a row's created_at; only message events take row time", ev.GetPayload(), at)
		c.False(at.Before(before) || at.After(after), "%T ts %v outside the emit window [%v, %v]", ev.GetPayload(), at, before, after)
	}
	assert.NewAborting(t).Eq(2, seen, "TurnStart and TurnEnd both published")
}

// The pi child's frames keep their order: the user echo precedes agent_start.
func TestUserEchoPrecedesAgentStartPiFrames(t *testing.T) {
	eng, out := newTestEngine(t, fakeToolSet{}, sampleEndTurn)
	eng.HandlePrompt("go")
	eng.Wait()

	got := frameTypes(t, out.String())
	assert.NewAborting(t).Eq("message_start,message_end,agent_start", strings.Join(got[:3], ","), "leading frames")
}

// Images ride the durable user_message in llm.UserContent's order — images
// first, the text last — and an image with no data is skipped entirely.
func TestPublishUserRowCarriesImages(t *testing.T) {
	c := assert.NewAborting(t)
	em := NewEmitter(NewFrontend(strings.NewReader(""), &syncBuffer{}, nil), "anthropic", nil)
	sink := &lockedSink{}
	em.SetNativeSink(sink)

	em.publishUserRow(llm.UserContent("look", []llm.UserImage{
		{MediaType: "image/png", Data: []byte{1, 2, 3}},
		{MediaType: "image/png"},
		{MediaType: "image/jpeg", Data: []byte{4}},
	}), store.Appended{CreatedAt: time.Now()})

	evs := sink.snapshot()
	c.Require().Len(evs, 1, "native events")
	got := evs[0].GetUserMessage().GetContent()
	c.Require().Len(got, 3, "empty-Data image skipped")
	c.Eq("image/png", got[0].GetImage().GetMediaType(), "content[0] media type")
	c.EqDiff([]byte{1, 2, 3}, got[0].GetImage().GetData(), "content[0] data")
	c.Eq("image/jpeg", got[1].GetImage().GetMediaType(), "content[1] media type")
	c.Eq("look", got[2].GetText().GetText(), "content[2] text")
	c.Eq(int32(2), got[2].GetIndex(), "content[2] index")
}
