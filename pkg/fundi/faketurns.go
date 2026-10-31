package fundi

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"

	"go.graveland.dev/rafiki/pkg/llm"
	"go.graveland.dev/rafiki/pkg/msg"
	msganthropic "go.graveland.dev/rafiki/pkg/msg/anthropic"
)

// fakeSender replays pre-recorded assistant messages in order, ignoring the
// request. It is the test seam that lets engine and daemon integration tests
// drive a complete tool-use loop with no API key and no network.
type fakeSender struct {
	mu    sync.Mutex
	next  int
	turns []*msg.Response
}

// LoadFakeSender reads a file of newline-separated JSON Anthropic message
// bodies and returns an llm.Sender that replays them in order. Blank lines are
// ignored; a malformed body is an error rather than a silently skipped turn.
// Once every scripted turn has been served, further calls return an error — an
// over-running loop is a test bug, not something to paper over. A file with
// zero messages (including an empty file, e.g. /dev/null) is not itself an
// error: it produces a sender that is immediately "exhausted", which is
// exactly right for driving a session that only ever needs get_state/no LLM
// call at all (see cmd/rafikid agent --fake-turns's manual acceptance gate).
func LoadFakeSender(path string) (llm.Sender, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("agent: read scripted turns: %w", err)
	}
	s := &fakeSender{}
	for i, line := range strings.Split(string(data), "\n") {
		text := strings.TrimSpace(line)
		if text == "" {
			continue
		}
		resp, err := msganthropic.DecodeResponse([]byte(text))
		if err != nil {
			return nil, fmt.Errorf("agent: scripted turn %s:%d: %w", path, i+1, err)
		}
		s.turns = append(s.turns, resp)
	}
	return s, nil
}

// Send returns the next scripted response.
func (s *fakeSender) Send(ctx context.Context, _ *msg.Request, _ msg.EncodeOptions) (*msg.Response, error) {
	return s.take(ctx)
}

// Stream returns the next scripted response as the event sequence a stream
// would have produced for it. Safe for concurrent use; the agent loop is
// sequential, but nothing in the Sender contract promises that.
func (s *fakeSender) Stream(ctx context.Context, _ *msg.Request, _ msg.EncodeOptions) (msg.StreamDecoder, error) {
	resp, err := s.take(ctx)
	if err != nil {
		return nil, err
	}
	return &fakeStream{resp: resp}, nil
}

// take hands out the next scripted response. It honours ctx, which is not a
// formality: rafiki's agentloop.drive does not check ctx.Err() between
// iterations, so a sender that ignores its context answers one more iteration
// after an abort and the turn completes with a nil error. That made
// Engine.runTurn's abort branch — RepairOrphans included, whose whole job is to
// stop the NEXT API call being rejected for a dangling tool_use — unreachable
// from every fake-turns test, and it silently consumed a scripted message the
// aborted turn should never have seen. A real HTTP sender fails on a cancelled
// context; this one has to as well, or the harness is testing a loop that does
// not exist in production.
func (s *fakeSender) take(ctx context.Context) (*msg.Response, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.next >= len(s.turns) {
		return nil, errors.New("agent: scripted turns exhausted")
	}
	resp := s.turns[s.next]
	s.next++
	return resp, nil
}

// fakeStream replays one scripted response as stream events.
type fakeStream struct {
	resp   *msg.Response
	events []msg.StreamEvent
	next   int
}

func (d *fakeStream) Next() (msg.StreamEvent, error) {
	if d.events == nil {
		d.events = fakeStreamEvents(d.resp)
	}
	if d.next >= len(d.events) {
		return nil, io.EOF
	}
	ev := d.events[d.next]
	d.next++
	return ev, nil
}

func (d *fakeStream) Close() error { return nil }

// fakeStreamEvents renders a whole response as the event sequence a stream
// would have produced for it.
func fakeStreamEvents(resp *msg.Response) []msg.StreamEvent {
	events := []msg.StreamEvent{msg.MessageStart{
		ID:       resp.ID,
		Model:    resp.Model,
		Provider: resp.Provider,
		Usage:    resp.Usage,
	}}
	for i, b := range resp.Content {
		events = append(events, msg.BlockStart{Index: i, Block: b}, msg.BlockStop{Index: i, Block: b})
	}
	return append(events, msg.MessageDelta{StopReason: resp.StopReason, Usage: resp.Usage})
}
