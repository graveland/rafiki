// SPDX-License-Identifier: Apache-2.0

// Package rail is the cockpit's tree: which children exist, how they are
// related, what each is doing, and which of them need you.
//
// Like package session it is a pure state machine over events -- no bubbletea,
// no network, no clock beyond the cursor floor. That is what lets the attention
// arithmetic in attention.go be tested exactly, which matters because a wrong
// badge is a wrong number rather than a crash and will not announce itself.
package rail

import (
	"sort"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
)

// ParentLabel is the daemon-written label carrying a child's parent id. The
// daemon also writes rafiki/root; the rail does not need it, because it
// reconstructs the tree from parent edges.
const ParentLabel = "rafiki/parent"

// LiveStatuses is every protocol.Status except "exited".
//
// The set of eight is CLOSED and there is NO "running" status. A filter written
// from intuition as status IN ('running', ...) selects nothing and silently
// empties the rail; this repo has shipped that exact bug once already, in the
// recovery predicate.
func LiveStatuses() []string {
	return []string{
		"spawning", "idle", "streaming", "tool_running",
		"compacting", "blocked_ui", "shutting_down",
	}
}

// Node is one child's row.
type Node struct {
	ChildID  string
	Name     string
	ParentID string
	Depth    int

	// Status is the last known agent state, one of protocol.Status's eight.
	Status   string
	Exited   bool
	ExitCode *int32
	Retrying bool

	// Latest is the highest ordinal known to exist for this child; it drives the
	// activity indicator. RailCursor is the highest ordinal RECEIVED on the rail
	// stream and is the reconnect resume point. Seen is the highest delivered to
	// the focus session while focused, and is the attention watermark.
	// CountedThrough is the highest ordinal already counted into Attention.
	//
	// Conflating any of these is how a badge double-counts across a reconnect.
	Latest         int32
	RailCursor     int32
	Seen           int32
	CountedThrough int32
	HasSeen        bool

	Attention int
}

// Rail is the tree.
type Rail struct {
	nodes   map[string]*Node
	focused string
}

// New returns an empty rail.
func New() *Rail { return &Rail{nodes: make(map[string]*Node)} }

// Len is the number of rows.
func (r *Rail) Len() int { return len(r.nodes) }

// Get returns one node by id.
func (r *Rail) Get(childID string) (Node, bool) {
	n, ok := r.nodes[childID]
	if !ok {
		return Node{}, false
	}
	return *n, true
}

// Seed installs or refreshes membership from ListChildren.
//
// Callers pass every summary; Seed drops the exited ones itself, so the "never
// resurrect a historical exit" rule lives in one place rather than at each call
// site. It is idempotent and safe to call again on reconnect: a child already
// in the rail keeps its watermark and badge, so re-seeding to discover children
// spawned during a disconnect does not silently mark everything read.
func (r *Rail) Seed(summaries []*rafikiv1.ChildSummary) {
	for _, s := range summaries {
		if s.GetStatus() == "exited" {
			continue
		}
		if existing, ok := r.nodes[s.GetChildId()]; ok {
			// Refresh only what the daemon is authoritative for. Never touch
			// Seen/CountedThrough/Attention -- those are this client's reading
			// history and a re-seed is not a read.
			existing.Name = s.GetName()
			if p := s.GetLabels()[ParentLabel]; p != "" {
				existing.ParentID = p
			}
			continue
		}
		n := &Node{
			ChildID:  s.GetChildId(),
			Name:     s.GetName(),
			ParentID: s.GetLabels()[ParentLabel],
			Status:   s.GetStatus(),
		}
		// Seeding is a CLEAN BOARD: everything that happened before you attached
		// counts as read. Attaching is not a claim to have read anything; it is
		// a claim to be watching from now. Seeding at 0 instead opens every
		// child with a badge and makes the next-attention key walk a backlog
		// nobody intends to read.
		if s.LatestOrdinal != nil {
			n.Latest = s.GetLatestOrdinal()
			n.RailCursor = n.Latest
			n.Seen = n.Latest
			n.CountedThrough = n.Latest
			n.HasSeen = true
		}
		r.nodes[n.ChildID] = n
	}
	r.recomputeDepths()
}

// Apply folds one rail-stream event into the tree.
func (r *Rail) Apply(ev *rafikiv1.Event) {
	if ev == nil {
		return
	}
	cid := ev.GetChildId()
	if cid == "" {
		return
	}

	// child_spawned is the ONLY event that may introduce a row. It is published
	// on the NEW child's own bus at its own ordinal 0, so the envelope's child
	// id is already the new child -- there is no parent-side special case.
	//
	// A child that spawned while this client was disconnected is therefore NOT
	// discoverable from the live stream, because its child_spawned is in the
	// past and the server replays only children named in the cursor. The
	// cockpit closes that by re-seeding when it sees an event for a child it
	// does not know; see Cockpit.applyEvent.
	if cs := ev.GetChildSpawned(); cs != nil {
		if _, exists := r.nodes[cid]; !exists {
			r.nodes[cid] = &Node{
				ChildID:  cid,
				Name:     cs.GetName(),
				ParentID: cs.GetParentId(),
				Status:   "spawning",
			}
			r.recomputeDepths()
		}
	}

	n, ok := r.nodes[cid]
	if !ok {
		return
	}

	if ev.Ordinal != nil {
		ord := ev.GetOrdinal()
		if ord > n.Latest {
			n.Latest = ord
		}
		if ord > n.RailCursor {
			n.RailCursor = ord
		}
	}

	switch p := ev.Payload.(type) {
	case *rafikiv1.Event_AgentStatus:
		n.Status = p.AgentStatus.GetState()
		// Any status transition means the retry resolved one way or the other.
		n.Retrying = false
	case *rafikiv1.Event_Retry:
		n.Retrying = true
	case *rafikiv1.Event_ChildExited:
		n.Exited = true
		n.Status = "exited"
		n.ExitCode = p.ChildExited.ExitCode
		n.Retrying = false
	}

	r.countAttention(n, ev)
}

// Nodes returns every row in display order: depth-first from each root,
// siblings by name then id so the order is stable across renders.
func (r *Rail) Nodes() []Node {
	children := make(map[string][]*Node, len(r.nodes))
	var roots []*Node
	for _, n := range r.nodes {
		if _, ok := r.nodes[n.ParentID]; n.ParentID == "" || !ok {
			roots = append(roots, n)
			continue
		}
		children[n.ParentID] = append(children[n.ParentID], n)
	}
	byName := func(s []*Node) {
		sort.Slice(s, func(i, j int) bool {
			if s[i].Name != s[j].Name {
				return s[i].Name < s[j].Name
			}
			return s[i].ChildID < s[j].ChildID
		})
	}
	byName(roots)
	for k := range children {
		byName(children[k])
	}

	out := make([]Node, 0, len(r.nodes))
	var walk func(n *Node)
	walk = func(n *Node) {
		out = append(out, *n)
		for _, c := range children[n.ChildID] {
			walk(c)
		}
	}
	for _, root := range roots {
		walk(root)
	}
	return out
}

// recomputeDepths refreshes every node's Depth from its parent chain. The walk
// is bounded by len(nodes) so a cycle -- which the daemon's lineage rules make
// impossible, but which a malformed label could still express -- terminates
// rather than hanging the render loop.
func (r *Rail) recomputeDepths() {
	for _, n := range r.nodes {
		depth, cur := 0, n
		for i := 0; i < len(r.nodes); i++ {
			parent, ok := r.nodes[cur.ParentID]
			if !ok {
				break
			}
			depth++
			cur = parent
		}
		n.Depth = depth
	}
}
