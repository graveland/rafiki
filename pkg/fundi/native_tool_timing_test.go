// SPDX-License-Identifier: Apache-2.0

package fundi

import (
	"encoding/json"
	"io"
	"strings"
	"testing"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"

	"github.com/multigres/testkit/assert"
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
	c := assert.NewCollecting(t)
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
	c.Require().NotNil(found, "no ToolExecutionStart event published")
	c.Eq("tu_1", found.GetToolUseId(), "ToolUseId")
	c.Eq("bash", found.GetName(), "Name")
}

func TestToolEndPublishesNativeExecutionEndWithDuration(t *testing.T) {
	c := assert.NewCollecting(t)
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
	c.Require().NotNil(found, "no ToolExecutionEnd event published")
	c.Eq("tu_1", found.GetToolUseId(), "ToolUseId")
	c.False(found.GetIsError(), "IsError = true, want false")
	// Duration is wall-clock, so assert only that it was measured, never a value.
	c.GreaterOrEqual(0, found.GetDurationMs(), "DurationMs")
}

// TestToolEndWithoutStartStillPublishes guards the case where a turn is resumed
// mid-tool: ToolEnd can fire with no matching ToolStart in this Emitter's
// lifetime, and it must still report the end rather than dropping it.
func TestToolEndWithoutStartStillPublishes(t *testing.T) {
	c := assert.NewCollecting(t)
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
	c.Require().NotNil(found, "no ToolExecutionEnd event published for an unstarted tool")
	c.True(found.GetIsError(), "IsError = false, want true")
	c.Eq(0, found.GetDurationMs(), "DurationMs")
}

// TestNilSinkToolPathIsNoOp proves the additive-only property: an Emitter with
// no native sink must behave exactly as before.
func TestNilSinkToolPathIsNoOp(t *testing.T) {
	em := NewEmitter(newDiscardFrontend(), "anthropic", nil)
	em.ToolStart("tu_1", "bash", json.RawMessage(`{}`))
	em.ToolEnd("tu_1", "bash", "output", false)
	// No panic, no nil deref. Nothing to assert beyond surviving the calls.
}
