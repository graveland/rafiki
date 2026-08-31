// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"

	"go.graveland.dev/rafiki/pkg/connectapi"
	"go.graveland.dev/rafiki/pkg/control"
	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/nativebus"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/server"
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
	// The owner is read from the CONTEXT, never the request: it is matched by
	// executor admission selectors, so a client that could name it could claim
	// to be any owner. This mirrors dispatcher.spawn reading conn.Identity().
	//
	// server.UserTokenAuth.Middleware is what put it there — the Connect
	// routes mount inside the proxy face's middleware stack
	// (server.Handler.Mount, wired in proxy.go), so a remote caller's
	// credential has already been resolved to a user by the time a handler
	// runs. The zero value is correct and expected on the unix socket, which
	// authenticates nobody because the socket itself is the credential.
	//
	// This was hardcoded to users.Identity{}, which was invisible while the
	// only reachable mount was that socket.
	res, err := l.c.Spawn(ctx, req, spawnOwner(ctx))
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

// spawnOwner maps the proxy face's authenticated identity onto the daemon's.
// Two types for one concept, because pkg/server predates pkg/users and the
// face's Identity is a pointer whose nil means "no user" — the case every
// unix-socket call takes.
func spawnOwner(ctx context.Context) users.Identity {
	id := server.IdentityFromContext(ctx)
	if id == nil {
		return users.Identity{}
	}
	return users.Identity{UserID: id.UserID, Username: id.Username}
}
