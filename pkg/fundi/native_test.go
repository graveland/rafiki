package fundi_test

import (
	"bytes"
	"testing"

	"go.graveland.dev/rafiki/pkg/fundi"
	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
)

type capturingSink struct{ events []*rafikiv1.Event }

func (c *capturingSink) Publish(ev *rafikiv1.Event) { c.events = append(c.events, ev) }

func TestEmitterPublishesNativeUserMessage(t *testing.T) {
	var out bytes.Buffer
	fe := fundi.NewFrontend(bytes.NewReader(nil), &out, nil)
	em := fundi.NewEmitter(fe, "anthropic", nil)

	sink := &capturingSink{}
	em.SetNativeSink(sink)

	em.UserMessage("hello there")

	if len(sink.events) != 1 {
		t.Fatalf("got %d native events, want 1", len(sink.events))
	}
	um := sink.events[0].GetUserMessage()
	if um == nil {
		t.Fatal("event is not a user message")
	}
	if len(um.Content) != 1 || um.Content[0].GetText().GetText() != "hello there" {
		t.Fatalf("unexpected content: %+v", um.Content)
	}
}

// A nil sink must be a complete no-op: existing callers pass none.
func TestEmitterWithNoSinkDoesNotPanic(t *testing.T) {
	var out bytes.Buffer
	fe := fundi.NewFrontend(bytes.NewReader(nil), &out, nil)
	em := fundi.NewEmitter(fe, "anthropic", nil)

	em.UserMessage("hello there")

	if out.Len() == 0 {
		t.Fatal("pi frame output disappeared; the pi path must be unchanged")
	}
}
