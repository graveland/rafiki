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
	return f.Subject.match(ev.GetChildId(), ln)
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
		d := ln.DescendantDepth(s.ChildID, childID)
		if d < 0 {
			return false
		}
		if s.MaxDepth > 0 && d > s.MaxDepth {
			return false
		}
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
	case *rafikiv1.Event_Error:
		return "error"
	case *rafikiv1.Event_Retry:
		return "retry"
	default:
		return "unknown"
	}
}
