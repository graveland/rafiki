// SPDX-License-Identifier: Apache-2.0

// Package llmtest provides test doubles for the llm sender seam.
package llmtest

import (
	"context"
	"fmt"
	"io"
	"sync"

	"go.graveland.dev/rafiki/pkg/msg"
	msganthropic "go.graveland.dev/rafiki/pkg/msg/anthropic"
)

// Fake is a scripted llm.Sender: call N answers with Errs[N] when that is
// non-nil and Responses[N] otherwise, and the last entry repeats once the
// script is exhausted. Every request is recorded, so a test can assert on the
// shape that went out as well as on the reply.
//
// It imports no part of pkg/llm, so a package's own in-package tests can use it
// too; that it satisfies llm.Sender is pinned by this package's own test.
type Fake struct {
	mu        sync.Mutex
	Responses []*msg.Response
	Errs      []error // Errs[i] returned instead of Responses[i] when non-nil
	Requests  []*msg.Request
}

// Send records req and answers from the script.
func (f *Fake) Send(_ context.Context, req *msg.Request, _ msg.EncodeOptions) (*msg.Response, error) {
	resp, err := f.answer(req)
	if err != nil {
		return nil, err
	}
	return resp, nil
}

// Stream records req and replays the scripted response as the event sequence a
// real stream would have produced for it.
func (f *Fake) Stream(_ context.Context, req *msg.Request, _ msg.EncodeOptions) (msg.StreamDecoder, error) {
	resp, err := f.answer(req)
	if err != nil {
		return nil, err
	}
	return &decoder{events: Events(resp)}, nil
}

// answer records req and resolves this call's script entry under the mutex.
func (f *Fake) answer(req *msg.Request) (*msg.Response, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Requests = append(f.Requests, req)
	i := len(f.Requests) - 1
	if i < len(f.Errs) && f.Errs[i] != nil {
		return nil, f.Errs[i]
	}
	switch {
	case len(f.Responses) == 0:
		return nil, fmt.Errorf("llmtest: no scripted response for call %d", i)
	case i >= len(f.Responses):
		return f.Responses[len(f.Responses)-1], nil
	}
	return f.Responses[i], nil
}

// Calls returns how many requests this fake has served.
func (f *Fake) Calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.Requests)
}

// FromAnthropicJSON decodes an Anthropic response body into a native response,
// so a fake written as the wire JSON it stands for converts in one line.
func FromAnthropicJSON(body string) *msg.Response {
	resp, err := msganthropic.DecodeResponse([]byte(body))
	if err != nil {
		panic(err)
	}
	return resp
}

// Events renders a whole response as the event sequence a stream would have
// produced for it: a message start, then per block a start, one delta carrying
// the block's content and a stop that finalizes it, then the message delta.
func Events(resp *msg.Response) []msg.StreamEvent {
	events := []msg.StreamEvent{msg.MessageStart{
		ID:       resp.ID,
		Model:    resp.Model,
		Provider: resp.Provider,
		Usage:    resp.Usage,
	}}
	for i, b := range resp.Content {
		switch blk := b.(type) {
		case msg.Text:
			events = append(events,
				msg.BlockStart{Index: i, Block: msg.Text{}},
				msg.BlockDelta{Index: i, Kind: msg.DeltaText, Text: blk.Text})
		case msg.Thinking:
			events = append(events, msg.BlockStart{Index: i, Block: msg.Thinking{Sig: msg.Signature{Dialect: blk.Sig.Dialect}}})
			if blk.Text != "" {
				events = append(events, msg.BlockDelta{Index: i, Kind: msg.DeltaThinking, Text: blk.Text})
			}
			if blk.Sig.Value != "" {
				events = append(events, msg.BlockDelta{Index: i, Kind: msg.DeltaSignature, Text: blk.Sig.Value})
			}
		case msg.ToolUse:
			events = append(events, msg.BlockStart{Index: i, Block: msg.ToolUse{ID: blk.ID, Name: blk.Name}})
			if len(blk.Input) > 0 {
				events = append(events, msg.BlockDelta{Index: i, Kind: msg.DeltaInputJSON, Text: string(blk.Input)})
			}
		default:
			events = append(events, msg.BlockStart{Index: i, Block: b})
		}
		events = append(events, msg.BlockStop{Index: i, Block: b})
	}
	return append(events, msg.MessageDelta{StopReason: resp.StopReason, StopReasonRaw: resp.StopReasonRaw, Usage: resp.Usage})
}

// NewDecoder returns a StreamDecoder replaying events in order, for a fake
// that scripts the stream itself rather than a whole response.
func NewDecoder(events []msg.StreamEvent) msg.StreamDecoder {
	return &decoder{events: events}
}

// decoder replays a fixed event sequence.
type decoder struct {
	events []msg.StreamEvent
	next   int
}

func (d *decoder) Next() (msg.StreamEvent, error) {
	if d.next >= len(d.events) {
		return nil, io.EOF
	}
	ev := d.events[d.next]
	d.next++
	return ev, nil
}

func (d *decoder) Close() error { return nil }
