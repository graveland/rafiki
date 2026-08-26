// SPDX-License-Identifier: Apache-2.0

package eventlog_test

import (
	"testing"

	"go.graveland.dev/rafiki/pkg/eventlog"
	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
)

func TestTierOf(t *testing.T) {
	durable := []*rafikiv1.Event{
		{Payload: &rafikiv1.Event_AgentStatus{AgentStatus: &rafikiv1.AgentStatus{}}},
		{Payload: &rafikiv1.Event_ChildSpawned{ChildSpawned: &rafikiv1.ChildSpawned{}}},
		{Payload: &rafikiv1.Event_ChildExited{ChildExited: &rafikiv1.ChildExited{}}},
		{Payload: &rafikiv1.Event_TurnStart{TurnStart: &rafikiv1.TurnStart{}}},
		{Payload: &rafikiv1.Event_TurnEnd{TurnEnd: &rafikiv1.TurnEnd{}}},
		{Payload: &rafikiv1.Event_ToolExecutionStart{ToolExecutionStart: &rafikiv1.ToolExecutionStart{}}},
		{Payload: &rafikiv1.Event_ToolExecutionEnd{ToolExecutionEnd: &rafikiv1.ToolExecutionEnd{}}},
		{Payload: &rafikiv1.Event_UserMessage{UserMessage: &rafikiv1.UserMessage{}}},
		{Payload: &rafikiv1.Event_AssistantMessage{AssistantMessage: &rafikiv1.AssistantMessage{}}},
		{Payload: &rafikiv1.Event_Error{Error: &rafikiv1.ErrorEvent{}}},
		{Payload: &rafikiv1.Event_Retry{Retry: &rafikiv1.Retry{}}},
	}
	for _, ev := range durable {
		if got := eventlog.TierOf(ev); got != eventlog.TierDurable {
			t.Errorf("TierOf(%s) = %v, want durable", eventlog.TypeName(ev), got)
		}
	}

	delta := &rafikiv1.Event{Payload: &rafikiv1.Event_ContentBlockDelta{ContentBlockDelta: &rafikiv1.ContentBlockDelta{}}}
	if got := eventlog.TierOf(delta); got != eventlog.TierEphemeral {
		t.Errorf("TierOf(content_block_delta) = %v, want ephemeral", got)
	}
}

// A new payload added to the proto must be classified deliberately. Falling
// through to a default would silently make it ephemeral — i.e. invisible to
// every resumable consumer — which is the failure mode this test exists to
// prevent.
func TestEveryEventTypeHasATier(t *testing.T) {
	names := eventlog.AllTypeNames()
	fields := (&rafikiv1.Event{}).ProtoReflect().Descriptor().Oneofs().ByName("payload").Fields()
	if len(names) != fields.Len() {
		t.Fatalf("AllTypeNames has %d entries but Event.payload has %d fields; classify the new one in tier.go", len(names), fields.Len())
	}
	have := make(map[string]bool, len(names))
	for _, n := range names {
		have[n] = true
	}
	for i := range fields.Len() {
		if n := string(fields.Get(i).Name()); !have[n] {
			t.Errorf("Event.payload field %q is not classified in tier.go", n)
		}
	}
}
