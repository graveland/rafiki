// SPDX-License-Identifier: Apache-2.0

package connectapi

import (
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
func (s *Server) SetChildLister(l ChildLister) { s.children = l }

// toProtoChild maps one summary onto the wire type. The two optional int
// fields stay nil when the source is nil — an exited child has no pid, a live
// one has no exit code, and 0 is a legal value for both.
func toProtoChild(c protocol.ChildSummary) *rafikiv1.ChildSummary {
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
	return out
}
