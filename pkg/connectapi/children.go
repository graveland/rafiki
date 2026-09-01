// SPDX-License-Identifier: Apache-2.0

package connectapi

import (
	"context"

	"go.graveland.dev/rafiki/pkg/eventlog"
	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/protocol"
)

// ChildLister is the narrow slice of the daemon's Controller this package
// needs to answer ListChildren and GetChild.
//
// It carries protocol.ChildSummary rather than a package-local struct:
// pkg/protocol is zero-dependency pure data, the summary is already the
// daemon's canonical child shape, and the Snapshot -> ChildSummary mapping
// (pkg/control's SnapshotToSummary) is non-obvious enough that reimplementing
// it would introduce bugs rather than remove a dependency.
type ChildLister interface {
	// ListChildren returns every child whose status is in statuses. An empty
	// or nil statuses means no filter.
	ListChildren(statuses []string) []protocol.ChildSummary
	// GetChild returns one child; false means unknown.
	GetChild(childID string) (protocol.ChildSummary, bool)
}

// SetChildLister attaches the child source. Post-construction setter for the
// same reason as SetChildResolver: the Controller is built after this Server.
func (s *Server) SetChildLister(l ChildLister) { s.children.Store(&l) }

// toProtoChild maps one summary onto the wire type. The two optional int
// fields stay nil when the source is nil — an exited child has no pid, a live
// one has no exit code, and 0 is a legal value for both.
func toProtoChild(c protocol.ChildSummary, elog eventlog.Store, ctx context.Context) *rafikiv1.ChildSummary {
	out := &rafikiv1.ChildSummary{
		ChildId:       c.ChildID,
		Name:          c.Name,
		Kind:          c.Kind,
		Status:        c.Status,
		Model:         c.Model,
		Cwd:           c.Cwd,
		SessionId:     c.SessionID,
		StartedAt:     c.StartedAt,
		LastActivity:  c.LastActivity,
		Labels:        c.Labels,
		ContextWindow: int32(c.ContextWindow),
	}
	if c.PID != nil {
		pid := int32(*c.PID)
		out.Pid = &pid
	}
	if c.ExitCode != nil {
		code := int32(*c.ExitCode)
		out.ExitCode = &code
	}
	if c.CostUSD != nil {
		cost := *c.CostUSD
		out.CostUsd = &cost
	}
	if elog != nil && ctx != nil {
		if latest, err := elog.Latest(ctx, c.ChildID); err == nil {
			out.LatestOrdinal = &latest
		}
	}
	return out
}

// SpawnParams is the narrow set of spawn inputs a client controls. The three
// budget fields are POINTERS and must stay pointers all the way to the daemon:
// unset and zero mean different things, in opposite directions per field
// (unset depth = 1, zero = may not spawn; unset cost = unlimited, zero = spend
// nothing; unset children = 4). Flattening any of them to a value silently
// converts one meaning into the other.
type SpawnParams struct {
	Cwd              string
	Name             string
	Model            string
	Kind             string
	ParentChildID    string
	ExecutorSelector string
	Labels           map[string]string
	MaxDepth         *int
	MaxCost          *float64
	MaxChildren      *int
}

// ChildLifecycle is the narrow slice of the daemon's Controller needed to
// create and end children.
type ChildLifecycle interface {
	// Spawn creates a child and returns its id.
	Spawn(ctx context.Context, p SpawnParams) (string, error)
	// Kill ends a child and reports how it ended.
	Kill(ctx context.Context, childID string, shutdownTimeoutMs, killTimeoutMs int64) (KillOutcome, error)
	// Close finalizes an exited child: it leaves the daemon's store and can
	// never be resumed again. The transcript is NOT deleted. Closing a live
	// child is an error, not an implicit kill.
	Close(ctx context.Context, childID string) error
}

// KillOutcome mirrors protocol.KillResponseData, which is what the daemon's
// Kill actually returns. There is deliberately no status string: that struct
// has none, and a client wanting the settled status calls GetChild.
type KillOutcome struct {
	ExitCode   *int
	Signal     string
	DurationMs int64
	Escalated  bool
}

// SetChildLifecycle attaches the spawn/kill source.
func (s *Server) SetChildLifecycle(l ChildLifecycle) { s.lifecycle.Store(&l) }
