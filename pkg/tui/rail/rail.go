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
	"sync"

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

// Working reports whether a child is mid-turn: streaming a reply, executing a
// tool, or compacting its context. The set of statuses is CLOSED
// (protocol.Status's eight) and there is NO "running" status -- a predicate
// written from intuition as status == "running" matches nothing and silently
// does nothing, which this repo has shipped once already in the recovery path.
func Working(status string) bool {
	switch status {
	case "streaming", "tool_running", "compacting":
		return true
	}
	return false
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

	// SessionID is the child's session/conversation id from ListChildren, so
	// the task box can address the ledger by conversation without another RPC.
	SessionID string

	// Cwd is the child's working directory, from ListChildren. It has no
	// event-stream source -- ChildSpawned carries no cwd -- so a freshly
	// spawned child shows it only once the cockpit's reseed-on-unknown-child
	// path re-seeds from ListChildren.
	Cwd string

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

	// Cost is this child's spend in USD, summed from turn_end. CostThrough is
	// the highest ordinal already added, and is a SEPARATE watermark from
	// Latest/RailCursor/Seen/CountedThrough for the same reason those are
	// separate from each other: the rail and focus subscriptions overlap on
	// the durable tier, so the same turn_end arrives twice and a sum with no
	// watermark doubles the bill.
	Cost         float64
	CostThrough  int32
	HasCostFloor bool

	// CostLive is the RUNNING total for the turn currently in flight, from
	// the latest AssistantMessage.cost_usd -- reset to 0 the instant a
	// TurnEnd for this child lands and folds its final value into Cost. A
	// human watching the cockpit sees TotalCost() move on every LLM reply
	// instead of only at the end of a whole exchange.
	//
	// This only ever moves for whichever child is currently FOCUSED: the
	// rail-wide subscription (Types(), wired into the rail stream by
	// pkg/tui/streams) deliberately excludes assistant_message -- see
	// Types()'s doc -- so it never reaches this field for an unfocused row.
	// Only the focused child's own dedicated event stream carries
	// AssistantMessage. Every other row's Cost only advances at TurnEnd,
	// same as before this field existed. Do not read CostLive on an
	// unfocused row as "the fleet's live spend" -- it is simply never
	// updated there.
	CostLive float64

	Attention int
}

// TotalCost is what should be DISPLAYED: settled cost from completed turns
// plus whatever the currently in-flight turn has cost so far.
func (n Node) TotalCost() float64 {
	return n.Cost + n.CostLive
}

// Rail is the tree.
//
// It is mutex-guarded because its Cursor method is handed to the rail stream as
// a callback and is therefore invoked from the stream's goroutine, while Apply
// and the render path run on the bubbletea goroutine. The lock costs nothing at
// UI rates and removes the whole class of bug; "pure state machine" here means
// no I/O and no clock, not single-threaded by assumption.
//
// Every exported method takes the lock, and the accessors return COPIES
// (Node values, a fresh slice) so a caller can never hold a pointer into the
// map after the lock is released.
type Rail struct {
	mu      sync.Mutex
	nodes   map[string]*Node
	focused string
}

// New returns an empty rail.
func New() *Rail { return &Rail{nodes: make(map[string]*Node)} }

// Len is the number of rows.
func (r *Rail) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.nodes)
}

// Get returns one node by id.
func (r *Rail) Get(childID string) (Node, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	n, ok := r.nodes[childID]
	if !ok {
		return Node{}, false
	}
	return *n, true
}

// Remove drops one node and reparents nothing: a closed child's descendants
// are closed with it or already gone, and re-seeding is what repairs the tree
// if that turns out to be wrong.
//
// This exists for CLOSE specifically, and close is the only caller that needs
// it. Every other way a child leaves the rail is driven by an event -- an exit
// marks the row rather than dropping it, because the exit code is worth seeing.
// A closed child publishes nothing ever again (it is gone from the daemon's
// store), so if this did not drop the row, nothing would until the next reseed.
func (r *Rail) Remove(childID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.nodes[childID]; !ok {
		return
	}
	delete(r.nodes, childID)
	if r.focused == childID {
		r.focused = ""
	}
	r.recomputeDepths()
}

// Seed installs or refreshes membership from ListChildren.
//
// Callers pass every summary; Seed drops the exited ones itself, so the "never
// resurrect a historical exit" rule lives in one place rather than at each call
// site. It is idempotent and safe to call again on reconnect: a child already
// in the rail keeps its watermark and badge, so re-seeding to discover children
// spawned during a disconnect does not silently mark everything read.
func (r *Rail) Seed(summaries []*rafikiv1.ChildSummary) {
	r.mu.Lock()
	defer r.mu.Unlock()
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
			existing.SessionID = s.GetSessionId()
			existing.Cwd = s.GetCwd()
			continue
		}
		n := &Node{
			ChildID:   s.GetChildId(),
			Name:      s.GetName(),
			ParentID:  s.GetLabels()[ParentLabel],
			Status:    s.GetStatus(),
			SessionID: s.GetSessionId(),
			Cwd:       s.GetCwd(),
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
	r.mu.Lock()
	defer r.mu.Unlock()
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
	case *rafikiv1.Event_AssistantMessage:
		// The running total for the in-flight turn ONLY -- not accumulated,
		// just the latest reading, matching how Engine.events() computes it
		// (this turn's cost so far, deliberately excluding every completed
		// PRIOR turn -- see AssistantMessage.cost_usd's proto doc). Adding
		// this directly to n.Cost (the settled total from completed turns)
		// in TotalCost() is what makes the number correct; publishing a
		// conversation-lifetime figure here instead would double-count every
		// prior turn on every in-flight reply.
		if c := p.AssistantMessage.CostUsd; c != nil {
			n.CostLive = *c
		}
	case *rafikiv1.Event_TurnEnd:
		// turn_end carries ONE turn's cost -- Emitter.AgentEnd resets its
		// usage accumulator -- so these sum. The ordinal guard is what stops
		// the overlapping rail and focus streams billing twice.
		if c := p.TurnEnd.CostUsd; c != nil && ev.Ordinal != nil {
			if ord := ev.GetOrdinal(); !n.HasCostFloor || ord > n.CostThrough {
				n.Cost += *c
				n.CostThrough = ord
				n.HasCostFloor = true
			}
		}
		// The turn this was tracking has ended either way -- even a
		// duplicate/rejected-by-ordinal-guard TurnEnd means live tracking
		// for it is done.
		n.CostLive = 0
	}

	r.countAttention(n, ev)
}

// Nodes returns every row in display order: depth-first from each root,
// siblings by name then id so the order is stable across renders.
func (r *Rail) Nodes() []Node {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.nodesLocked()
}

// SetCost seeds a child's spend from ListChildren.
//
// It ASSIGNS rather than adds: the rail resumes from the log head, so turns
// that happened before this client attached are never replayed, and the seed
// is the only source for them. Calling it twice must not double the number.
func (r *Rail) SetCost(childID string, cost float64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	n, ok := r.nodes[childID]
	if !ok {
		return
	}
	// The MAXIMUM, not an assignment. conversation_turn rows land after the
	// turn_end event that reported them, so a re-seed can carry a rollup
	// computed before the newest turns were visible. Assigning it would drop
	// cost this rail had already counted -- permanently, because those
	// ordinals are below CostThrough and Apply will never re-add them.
	//
	// Taking the max is safe in the other direction too: the seed is
	// authoritative for turns that predate this client's attachment, which the
	// stream never replays, and those only ever make the number bigger.
	if cost > n.Cost {
		n.Cost = cost
	}
}

// SubtreeCost sums childID and every descendant.
//
// Computed here rather than fetched, because the rail already holds the tree.
// Summing PER CHILD also sidesteps the correlation hazard a server-side
// rollup carries: a fundi child's conversation is found by UUID and a proxy
// child's by external_ref, and a subtree routinely mixes them.
func (r *Rail) SubtreeCost(childID string) float64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	children := make(map[string][]string, len(r.nodes))
	for id, n := range r.nodes {
		if n.ParentID != "" {
			children[n.ParentID] = append(children[n.ParentID], id)
		}
	}
	var walk func(string) float64
	seen := make(map[string]bool, len(r.nodes))
	walk = func(id string) float64 {
		if seen[id] {
			return 0
		}
		seen[id] = true
		total := 0.0
		if n, ok := r.nodes[id]; ok {
			total = n.TotalCost()
		}
		for _, kid := range children[id] {
			total += walk(kid)
		}
		return total
	}
	return walk(childID)
}

// nodesLocked is Nodes without the lock, for callers that already hold it.
// sync.Mutex is NOT reentrant: NextAttention calling the exported Nodes would
// deadlock the render loop on the first keypress.
func (r *Rail) nodesLocked() []Node {
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
