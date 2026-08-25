// SPDX-License-Identifier: Apache-2.0

package fundi

import (
	"encoding/json"
	"io"
	"strings"
	"testing"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
)

// captureSink records every native event published.
type captureSink struct{ events []*rafikiv1.Event }

func (c *captureSink) Publish(ev *rafikiv1.Event) { c.events = append(c.events, ev) }

// newDiscardFrontend builds a Frontend whose output goes nowhere, so an
// Emitter can be exercised without a pipe or a reader.
func newDiscardFrontend() *Frontend {
	return NewFrontend(strings.NewReader(""), io.Discard, nil)
}

func TestToolStartPublishesNativeExecutionStart(t *testing.T) {
	sink := &captureSink{}
	em := NewEmitter(newDiscardFrontend(), "anthropic", nil)
	em.SetNativeSink(sink)

	em.ToolStart("tu_1", "bash", json.RawMessage(`{"command":"ls"}`))

	var found *rafikiv1.ToolExecutionStart
	for _, ev := range sink.events {
		if s := ev.GetToolExecutionStart(); s != nil {
			found = s
		}
	}
	if found == nil {
		t.Fatal("no ToolExecutionStart event published")
	}
	if found.GetToolUseId() != "tu_1" {
		t.Errorf("ToolUseId = %q, want %q", found.GetToolUseId(), "tu_1")
	}
	if found.GetName() != "bash" {
		t.Errorf("Name = %q, want %q", found.GetName(), "bash")
	}
}

func TestToolEndPublishesNativeExecutionEndWithDuration(t *testing.T) {
	sink := &captureSink{}
	em := NewEmitter(newDiscardFrontend(), "anthropic", nil)
	em.SetNativeSink(sink)

	em.ToolStart("tu_1", "bash", json.RawMessage(`{}`))
	em.ToolEnd("tu_1", "bash", "output", false)

	var found *rafikiv1.ToolExecutionEnd
	for _, ev := range sink.events {
		if e := ev.GetToolExecutionEnd(); e != nil {
			found = e
		}
	}
	if found == nil {
		t.Fatal("no ToolExecutionEnd event published")
	}
	if found.GetToolUseId() != "tu_1" {
		t.Errorf("ToolUseId = %q, want %q", found.GetToolUseId(), "tu_1")
	}
	if found.GetIsError() {
		t.Error("IsError = true, want false")
	}
	// Duration is wall-clock, so assert only that it was measured, never a value.
	if found.GetDurationMs() < 0 {
		t.Errorf("DurationMs = %d, want >= 0", found.GetDurationMs())
	}
}

// TestToolEndWithoutStartStillPublishes guards the case where a turn is resumed
// mid-tool: ToolEnd can fire with no matching ToolStart in this Emitter's
// lifetime, and it must still report the end rather than dropping it.
func TestToolEndWithoutStartStillPublishes(t *testing.T) {
	sink := &captureSink{}
	em := NewEmitter(newDiscardFrontend(), "anthropic", nil)
	em.SetNativeSink(sink)

	em.ToolEnd("tu_orphan", "bash", "output", true)

	var found *rafikiv1.ToolExecutionEnd
	for _, ev := range sink.events {
		if e := ev.GetToolExecutionEnd(); e != nil {
			found = e
		}
	}
	if found == nil {
		t.Fatal("no ToolExecutionEnd event published for an unstarted tool")
	}
	if !found.GetIsError() {
		t.Error("IsError = false, want true")
	}
	if found.GetDurationMs() != 0 {
		t.Errorf("DurationMs = %d, want 0 for an unstarted tool", found.GetDurationMs())
	}
}

// TestNilSinkToolPathIsNoOp proves the additive-only property: an Emitter with
// no native sink must behave exactly as before.
func TestNilSinkToolPathIsNoOp(t *testing.T) {
	em := NewEmitter(newDiscardFrontend(), "anthropic", nil)
	em.ToolStart("tu_1", "bash", json.RawMessage(`{}`))
	em.ToolEnd("tu_1", "bash", "output", false)
	// No panic, no nil deref. Nothing to assert beyond surviving the calls.
}
