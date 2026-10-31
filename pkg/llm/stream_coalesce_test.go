// SPDX-License-Identifier: Apache-2.0

package llm

import (
	"context"
	"strings"
	"testing"

	"go.graveland.dev/rafiki/pkg/msg"

	"github.com/multigres/testkit/assert"
)

// toolInputStreamEvents is a complete stream with one tool_use block whose
// input arrives as n small input-json fragments.
func toolInputStreamEvents(n int) []msg.StreamEvent {
	events := []msg.StreamEvent{
		messageStartEvent(),
		msg.BlockStart{Index: 0, Block: msg.ToolUse{ID: "tu_1", Name: "write"}},
		msg.BlockDelta{Index: 0, Kind: msg.DeltaInputJSON, Text: `{"data":"`},
	}
	frag := strings.Repeat("x", 100)
	for i := 0; i < n; i++ {
		events = append(events, msg.BlockDelta{Index: 0, Kind: msg.DeltaInputJSON, Text: frag})
	}
	events = append(events,
		msg.BlockDelta{Index: 0, Kind: msg.DeltaInputJSON, Text: `"}`},
		msg.BlockStop{Index: 0},
		msg.MessageDelta{StopReason: msg.StopToolUse, Usage: msg.Usage{Output: msg.Ptr(5)}})
	return events
}

// TestSend_StreamHandlerCoalescesDeltas pins the handler's cost bound: every
// accumulated view is a deep copy of the response, so calling the handler per
// delta costs O(N^2) bytes in a streamed tool argument. Deltas are coalesced to
// one call per streamHandlerMinInterval; a block start and a block stop always
// get their own call, and the final view is complete.
func TestSend_StreamHandlerCoalescesDeltas(t *testing.T) {
	c := assert.NewCollecting(t)
	const fragments = 2000
	sender := newFakeStreamingSender(toolInputStreamEvents(fragments)...)
	conv := newTestConversation(t, sender)

	var views []*msg.Response
	_, err := conv.Send(context.Background(), UserText("hi"),
		WithStreamHandler(func(r *msg.Response) { views = append(views, r) }))
	c.Require().NoError(err, "Send")

	t.Logf("handler calls: %d for %d delta events", len(views), fragments+2)
	// The scripted stream is delivered in well under one interval, so the only
	// calls are the start and the stop (a slow machine may add a delta call).
	c.Require().Greater(1, len(views), "BlockStart and BlockStop must each call the handler")
	c.True(len(views) <= 10, "handler called %d times for %d deltas; coalescing is broken", len(views), fragments+2)

	first := views[0]
	c.Require().Eq(1, len(first.Content), "first view blocks")
	tu, ok := first.Content[0].(msg.ToolUse)
	c.Require().True(ok, "first view must be the opened tool_use block")
	c.False(strings.Contains(string(tu.Input), "xxx"), "BlockStart's call must see the block before any delta")

	last, ok := views[len(views)-1].Content[0].(msg.ToolUse)
	c.Require().True(ok, "last view tool_use")
	c.Eq(len(`{"data":"`)+fragments*100+len(`"}`), len(last.Input), "BlockStop's call must carry every fragment")
}

// BenchmarkSend_StreamedToolInput measures what the handler path allocates for
// a 200 KB streamed tool argument.
func BenchmarkSend_StreamedToolInput(b *testing.B) {
	events := toolInputStreamEvents(2000)
	sender := newFakeStreamingSender(events...)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		sender.streamCalls = 0
		sender.scripts = []streamScript{{events: events}}
		sender.newCalls = 0
		conv := newBenchConversation(b, sender)
		if _, err := conv.Send(context.Background(), UserText("hi"),
			WithStreamHandler(func(*msg.Response) {})); err != nil {
			b.Fatal(err)
		}
	}
}

func newBenchConversation(b *testing.B, sender Sender) *Conversation {
	b.Helper()
	c, err := NewClient(
		WithProviderSender("anthropic", sender),
		WithDefaultModel("claude-haiku-4-5"),
	)
	if err != nil {
		b.Fatal(err)
	}
	conv, err := c.Conversation(context.Background(),
		NewConversation("", "test"), Model("claude-haiku-4-5"), SystemText("sys"))
	if err != nil {
		b.Fatal(err)
	}
	return conv
}
