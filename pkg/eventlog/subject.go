// SPDX-License-Identifier: Apache-2.0

package eventlog

import (
	"go.graveland.dev/rafiki/pkg/executors"
	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
)

// Lineage answers the two questions a subject needs about a child. It is an
// interface so this package stays free of childstore — and therefore of the
// daemon — which is what lets the matcher be unit-tested without a Store.
type Lineage interface {
	// DescendantDepth returns hops from ancestor down to candidate, or -1
	// when candidate is not beneath ancestor. See childstore.DescendantDepth.
	DescendantDepth(ancestorID, candidateID string) int
	// Labels returns the child's labels. false means the child is unknown,
	// which every caller must treat as "exclude".
	Labels(childID string) (map[string]string, bool)
}

// Scope is the shape of a subscription's subject.
type Scope int

const (
	// ScopeChild is one child, itself.
	ScopeChild Scope = iota
	// ScopeSubtree is the descendants of ChildID, bounded by MaxDepth. It
	// never includes ChildID itself.
	ScopeSubtree
	// ScopeAll is everything the subscriber is entitled to.
	ScopeAll
)

// Subject names which children a subscription covers.
//
// It is a PREDICATE, evaluated per event — never a set resolved when the
// subscription opens. That is what lets a child spawned after the fact appear
// with no reopening, no poll and no ChildSpawned special case, and it is the
// property the pre-C1a StreamEvents got wrong.
type Subject struct {
	Scope   Scope
	ChildID string // ScopeChild and ScopeSubtree only

	// MaxDepth bounds ScopeSubtree in hops. UNSET (0) means UNLIMITED; 1
	// means direct children only. Zero-means-unlimited is deliberate and
	// matches the spec — see the design §3.3. Do not "fix" it to mean zero
	// hops; that would silently make every depth-defaulted subscription
	// match nothing.
	MaxDepth int

	// IncludeSelf admits ChildID itself under ScopeSubtree. Ignored for
	// ScopeChild and ScopeAll.
	//
	// This is the ONE subject field that widens rather than narrows, and it is
	// safe for a specific reason: it widens by exactly one child, the one the
	// subscriber already named, and authority is still evaluated separately and
	// intersected -- naming a child you are not entitled to admits nothing. The
	// default is false so ScopeSubtree keeps the meaning the agent path depends
	// on: MaxDepth=1 is "my direct children", never "me and my children".
	//
	// The cockpit attached to a child sets it, because otherwise the child it
	// is attached TO is the one row it never hears about -- and its focus
	// stream is ScopeChild, so nothing would reveal that until the user hopped
	// away.
	IncludeSelf bool

	// Selector NARROWS the scope and never widens it. It is not the
	// authority: authority is evaluated separately by the caller and
	// intersected. A malformed selector EXCLUDES.
	Selector string
}

// Filter is a complete subscription predicate.
type Filter struct {
	Subject Subject
	Tier    Tier
	// Types restricts to these event type names (see TypeName). Empty means
	// every type in the tier.
	Types []string
}

// Match reports whether ev belongs to this subscription.
//
// ln is the live lineage (the daemon's childstore, keyed by child id). One
// event — child_closed — is about a child that has already left it, so that
// event carries its own ancestry and is matched against that instead; see
// eventLineage.
func (f Filter) Match(ev *rafikiv1.Event, ln Lineage) bool {
	if ev == nil {
		return false
	}
	if !f.Tier.admits(TierOf(ev)) {
		return false
	}
	if len(f.Types) > 0 {
		name := TypeName(ev)
		found := false
		for _, t := range f.Types {
			if t == name {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	if f.Subject.match(ev.GetChildId(), ln) {
		return true
	}
	// A child_closed event names a child that has just LEFT the store the
	// lineage reads, so the scope question ("is this in my subtree?") and the
	// selector's label lookup can no longer be answered from it. That event
	// carries its own chain and labels precisely so the subscriber still
	// drawing the row can be told the row is gone; ask those instead. This
	// NARROWS nothing and widens nothing: it answers exactly what the store
	// would have answered one moment earlier.
	if own := eventLineage(ev); own != nil {
		return f.Subject.match(ev.GetChildId(), own)
	}
	return false
}

// AdmitsChild reports whether the subject admits childID's events at all,
// before tier and type: what a replay checks before reading a child's log.
func (f Filter) AdmitsChild(childID string, ln Lineage) bool {
	return f.Subject.match(childID, ln)
}

func (s Subject) match(childID string, ln Lineage) bool {
	if childID == "" {
		return false
	}
	switch s.Scope {
	case ScopeChild:
		if childID != s.ChildID {
			return false
		}
	case ScopeSubtree:
		admittedAsSelf := s.IncludeSelf && childID == s.ChildID
		if !admittedAsSelf {
			d := ln.DescendantDepth(s.ChildID, childID)
			if d < 0 {
				return false
			}
			if s.MaxDepth > 0 && d > s.MaxDepth {
				return false
			}
		}
		// Falls through to selectorAdmits below: IncludeSelf widens the SCOPE
		// and must not bypass the narrowing selector.
	case ScopeAll:
		// Every child, subject to the selector below.
	default:
		return false
	}
	return s.selectorAdmits(childID, ln)
}

// selectorAdmits applies the narrowing selector. An unparseable selector, or a
// child lineage has never heard of, EXCLUDES — the same direction as
// executors.Executor.Admits, so an operator typo cannot silently widen a
// subscription.
func (s Subject) selectorAdmits(childID string, ln Lineage) bool {
	if s.Selector == "" {
		return true
	}
	sel, err := executors.ParseSelector(s.Selector)
	if err != nil {
		return false
	}
	labels, ok := ln.Labels(childID)
	if !ok {
		return false
	}
	return sel.Matches(labels)
}

// eventLineage returns a Lineage backed by a child_closed event's own ancestry,
// or nil for every other event.
//
// It exists because the ONE thing a subscriber most needs to hear — "the child
// you are drawing is gone" — is about a child the live lineage can no longer
// resolve: closing deletes it from the childstore, which is what DescendantDepth
// and Labels read. The event therefore carries the chain and the labels, and the
// scope question is answered from those. Every other event is about a child that
// is still in the store, so it keeps using the real lineage and an unknown child
// stays excluded.
func eventLineage(ev *rafikiv1.Event) Lineage {
	cc := ev.GetChildClosed()
	if cc == nil {
		return nil
	}
	return closedLineage{childID: ev.GetChildId(), ancestors: cc.GetAncestors(), labels: cc.GetLabels()}
}

// closedLineage is the Lineage of one child that has left the store, as the
// child_closed event that reported it described the child.
type closedLineage struct {
	childID   string
	ancestors []string // parent first, then upward to the root
	labels    map[string]string
}

// Labels answers for the closed child alone; every other id is unknown, exactly
// as the store would answer.
func (l closedLineage) Labels(childID string) (map[string]string, bool) {
	if childID != l.childID {
		return nil, false
	}
	return l.labels, true
}

// DescendantDepth counts hops from ancestorID down to the closed child, or -1
// when the ancestor is not on its chain. ancestors[0] is the parent, so the
// ancestor at index i is i+1 hops above the child — the same number
// childstore.DescendantDepth would have returned before the close.
func (l closedLineage) DescendantDepth(ancestorID, candidateID string) int {
	if candidateID != l.childID || ancestorID == "" || ancestorID == candidateID {
		return -1
	}
	for i, id := range l.ancestors {
		if id == ancestorID {
			return i + 1
		}
	}
	return -1
}

// TypeName is the wire name of an event's payload, used by Filter.Types and by
// the log's `type` column. It must match the proto field names in
// Event.payload exactly — the column is queried by hand in operational SQL,
// so a drift here is a silently empty result set rather than an error.
func TypeName(ev *rafikiv1.Event) string {
	switch ev.GetPayload().(type) {
	case *rafikiv1.Event_UserMessage:
		return "user_message"
	case *rafikiv1.Event_AssistantMessage:
		return "assistant_message"
	case *rafikiv1.Event_TurnStart:
		return "turn_start"
	case *rafikiv1.Event_TurnEnd:
		return "turn_end"
	case *rafikiv1.Event_ContentBlockDelta:
		return "content_block_delta"
	case *rafikiv1.Event_ToolExecutionStart:
		return "tool_execution_start"
	case *rafikiv1.Event_ToolExecutionEnd:
		return "tool_execution_end"
	case *rafikiv1.Event_AgentStatus:
		return "agent_status"
	case *rafikiv1.Event_ChildSpawned:
		return "child_spawned"
	case *rafikiv1.Event_ChildExited:
		return "child_exited"
	case *rafikiv1.Event_CompactionBoundary:
		return "compaction_boundary"
	case *rafikiv1.Event_ScriptReport:
		return "script_report"
	case *rafikiv1.Event_ScriptOutput:
		return "script_output"
	case *rafikiv1.Event_OperatorState:
		return "operator_state"
	case *rafikiv1.Event_ChildRenamed:
		return "child_renamed"
	case *rafikiv1.Event_TasksChanged:
		return "tasks_changed"
	case *rafikiv1.Event_ChildClosed:
		return "child_closed"
	case *rafikiv1.Event_Error:
		return "error"
	case *rafikiv1.Event_Retry:
		return "retry"
	default:
		return "unknown"
	}
}
