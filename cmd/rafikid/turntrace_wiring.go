// SPDX-License-Identifier: Apache-2.0

package main

import (
	"log/slog"
	"strings"

	"go.opentelemetry.io/otel/trace"

	"go.graveland.dev/rafiki/pkg/childstore"
	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/telemetry"
	"go.graveland.dev/rafiki/pkg/turntrace"
)

// EnableTracing builds the span observer over this controller's child
// store. Called once by main before any child spawns or recovers.
//
// That once-before-any-spawn contract is what makes the unsynchronized field
// writes below safe: no goroutine can be reading c.trace while it is being set,
// and nothing assigns it again afterwards.
func (c *Controller) EnableTracing(tp trace.TracerProvider, policy telemetry.Policy, rec turntrace.Recorder) *turntrace.Observer {
	if tp == nil {
		c.tracerProvider = nil
		c.trace = nil
		return nil
	}
	c.tracerProvider = tp
	c.trace = turntrace.New(tp, policy, c.childMeta, rec)
	return c.trace
}

// observeEvent feeds one event to the span observer, containing any panic.
//
// publishEvent is the daemon's single delivery path, so a panic in the observer
// would take the daemon down AND skip the event-log append and fan-out that
// follow it. Telemetry is strictly best-effort, and the recover belongs here,
// on the daemon side, rather than in pkg/turntrace: this is the layer that owns
// "observability must never break the thing it observes".
func (c *Controller) observeEvent(childID string, ev *rafikiv1.Event) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("span observer panicked; the event is still published",
				"childId", childID, "panic", r)
		}
	}()
	c.trace.Observe(childID, ev)
}

// childMeta describes one child to the span observer: the identity a turn span
// is stamped with. It is a MetaFunc, so the observer calls it BEFORE taking its
// own lock — see turntrace.MetaFunc's contract.
//
// It reads the live child store only (c.st), never the durable one: a child's
// name, kind, model, owner and lineage labels are all in memory for as long as
// the child can have an open turn, and a database read per turn would put a
// query on the publish path for nothing. A child the store does not know
// reports false, and the observer falls back to ChildMeta{ChildID: childID}.
func (c *Controller) childMeta(childID string) (turntrace.ChildMeta, bool) {
	if c.st == nil {
		return turntrace.ChildMeta{}, false
	}
	snap, ok := c.st.Get(childID)
	if !ok {
		return turntrace.ChildMeta{}, false
	}
	meta := turntrace.ChildMeta{
		ChildID:  childID,
		Name:     snap.Name,
		Kind:     snap.Kind,
		Model:    snap.Model,
		Owner:    snap.Labels["owner"],
		ParentID: snap.Labels[childstore.LabelParent],
		RootID:   snap.Labels[childstore.LabelRoot],
	}
	// The provider a span is stamped with is the provider this child's model
	// routes through, derived the same way pkg/llm splits a model id: the first
	// path segment. A model with no segment names no provider, and claiming the
	// whole id as one would make a content rule match a provider that does not
	// exist. claude's model ids are OpenRouter-free and always Anthropic's.
	switch snap.Kind {
	case protocol.KindFundi:
		if provider, _, found := strings.Cut(meta.Model, "/"); found {
			meta.Provider = provider
		}
	case protocol.KindClaude:
		meta.Provider = "anthropic"
	}
	// Snapshot carries no conversation id (the conversation is resolved by the
	// engine, after spawn), so gen_ai.conversation.id is omitted rather than
	// guessed from the external ref.
	return meta, true
}
