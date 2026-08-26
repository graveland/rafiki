// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"fmt"

	"go.graveland.dev/rafiki/pkg/connectapi"
	"go.graveland.dev/rafiki/pkg/control"
	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/inbox"
	"go.graveland.dev/rafiki/pkg/nativebus"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/users"
)

// nativeEventSource adapts the nativebus registry to connectapi.EventSource,
// avoiding a method-name collision with Controller.Subscribe (which already
// exists with a different signature for the frame-based control protocol).
type nativeEventSource struct {
	native *nativebus.Registry
}

func (s *nativeEventSource) Subscribe(childID string) (<-chan *rafikiv1.Event, func()) {
	return s.native.Subscribe(childID)
}

func (s *nativeEventSource) SubscribeAll() (<-chan *rafikiv1.Event, func()) {
	return s.native.SubscribeAll()
}

// Subscribe satisfies connectapi.EventSource via the nativeEventSource adapter.
// The Controller's own Subscribe method serves the frame-based protocol;
// this is a distinct signature for the Connect-based StreamEvents path.
func (c *Controller) nativeEventSource() *nativeEventSource {
	return &nativeEventSource{native: c.native}
}

// ListChildren satisfies connectapi.ChildLister. An empty statuses means no
// filter.
//
// The Snapshot -> ChildSummary mapping is control.SnapshotToSummary, not a
// local reimplementation: it joins provider and model, nils the PID for an
// exited child, converts time.Time to Unix millis, and sources the context
// window through the catalog func. Every one of those is easy to get subtly
// wrong by hand.
func (c *Controller) ListChildren(statuses []string) []protocol.ChildSummary {
	snaps := c.List(protocol.ListFilter{})
	out := make([]protocol.ChildSummary, 0, len(snaps))
	for _, s := range snaps {
		if len(statuses) > 0 && !containsString(statuses, string(s.Status)) {
			continue
		}
		out = append(out, control.SnapshotToSummary(s, c.ContextWindow))
	}
	return out
}

// GetChild satisfies connectapi.ChildLister.
func (c *Controller) GetChild(childID string) (protocol.ChildSummary, bool) {
	snap, ok := c.Get(childID)
	if !ok {
		return protocol.ChildSummary{}, false
	}
	return control.SnapshotToSummary(snap, c.ContextWindow), true
}

// DescendantDepth satisfies eventlog.Lineage.
func (c *Controller) DescendantDepth(ancestorID, candidateID string) int {
	return c.st.DescendantDepth(ancestorID, candidateID)
}

// Labels satisfies eventlog.Lineage.
func (c *Controller) Labels(childID string) (map[string]string, bool) {
	snap, ok := c.st.Get(childID)
	if !ok {
		return nil, false
	}
	return snap.Labels, true
}

func containsString(haystack []string, needle string) bool {
	for _, h := range haystack {
		if h == needle {
			return true
		}
	}
	return false
}

// connectLifecycle adapts *Controller to connectapi.ChildLifecycle, which
// cannot be satisfied by *Controller directly: Controller.Spawn and
// Controller.Kill already exist with different signatures, and renaming them
// would touch every existing caller for no gain.
type connectLifecycle struct{ c *Controller }

func (l connectLifecycle) Spawn(ctx context.Context, p connectapi.SpawnParams) (string, error) {
	req := protocol.SpawnRequest{
		Cwd:              p.Cwd,
		Name:             p.Name,
		Model:            p.Model,
		Kind:             p.Kind,
		Labels:           p.Labels,
		ParentChildID:    p.ParentChildID,
		ExecutorSelector: p.ExecutorSelector,
		MaxDepth:         p.MaxDepth,
		MaxCost:          p.MaxCost,
		MaxChildren:      p.MaxChildren,
	}
	res, err := l.c.Spawn(ctx, req, users.Identity{})
	if err != nil {
		return "", err
	}
	return res.ChildID, nil
}

func (l connectLifecycle) Kill(ctx context.Context, childID string, shutdownMs, killMs int64) (connectapi.KillOutcome, error) {
	res, err := l.c.Kill(ctx, childID, shutdownMs, killMs)
	if err != nil {
		return connectapi.KillOutcome{}, err
	}
	return connectapi.KillOutcome{
		ExitCode:   res.ExitCode,
		Signal:     res.Signal,
		DurationMs: res.DurationMs,
		Escalated:  res.Escalated,
	}, nil
}

// newConnectInbox builds the in-memory Inbox that Connect's Send routes
// through. It reproduces today's ctrl_send behaviour exactly — straight to the
// child, no queue — while giving the durable implementation a place to land.
func newConnectInbox(c *Controller) *inbox.Memory {
	return inbox.NewMemory(func(childID string, m inbox.Inbound) error {
		var frame json.RawMessage
		switch m.Mode {
		case inbox.ModePrompt:
			frame = mustMarshalFrame("prompt", m.Text)
		case inbox.ModeSteer:
			frame = mustMarshalFrame("steer", m.Text)
		case inbox.ModeAbort:
			frame = json.RawMessage(`{"type":"abort"}`)
		default:
			return fmt.Errorf("inbox: unknown mode %v", m.Mode)
		}
		return c.Send(childID, frame)
	})
}

func mustMarshalFrame(kind, message string) json.RawMessage {
	b, err := json.Marshal(struct {
		Type    string `json:"type"`
		Message string `json:"message"`
	}{Type: kind, Message: message})
	if err != nil {
		// Marshalling two strings cannot fail.
		panic(err)
	}
	return b
}
