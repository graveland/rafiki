package childstore

import (
	"sort"

	"go.graveland.dev/rafiki/pkg/protocol"
)

// maxChainDepth bounds the parent walk in IsDescendant. Lineage labels are
// daemon-written and cannot legitimately form a cycle, but a corrupt or
// hand-edited state record must not wedge a caller in an infinite loop —
// this predicate runs on the authority path for every steering verb.
const maxChainDepth = 64

// ParentLabel reads the parent child id from a label map. ok is false for a
// top-level child.
func ParentLabel(labels map[string]string) (string, bool) {
	parent := labels[LabelParent]
	if parent == "" {
		return "", false
	}
	return parent, true
}

// ParentOf returns the direct parent's child id.
func (s *Store) ParentOf(childID string) (string, bool) {
	var parent string
	var has bool
	s.readLabels(childID, func(labels map[string]string) {
		parent, has = ParentLabel(labels)
	})
	return parent, has
}

// readLabels calls fn with childID's live label map under the session lock
// and reports whether the child exists. The tree predicates run on every
// list and steering call, so they read the one label they need in place
// instead of paying for a Snapshot's deep copy; fn must not retain the map.
func (s *Store) readLabels(childID string, fn func(labels map[string]string)) bool {
	sess, ok := s.sessions.Load(childID)
	if !ok {
		return false
	}
	sess.mu.Lock()
	defer sess.mu.Unlock()
	fn(sess.Labels)
	return true
}

func (s *Store) exists(childID string) bool {
	_, ok := s.sessions.Load(childID)
	return ok
}

// RootOf returns the top-level ancestor's child id. A top-level child is its
// own root. Returns "" when childID is unknown.
func (s *Store) RootOf(childID string) string {
	var root string
	if !s.readLabels(childID, func(labels map[string]string) {
		root = labels[LabelRoot]
	}) {
		return ""
	}
	if root != "" {
		return root
	}
	return childID
}

// IsDescendant reports whether candidateID is anywhere beneath ancestorID.
// A child is NOT a descendant of itself.
//
// This is the authority predicate for every steering verb: an agent may see
// and steer only its own descendants. A false positive here is a cross-agent
// authority hole, so it walks the real parent chain rather than trusting the
// root label, which is a cache.
func (s *Store) IsDescendant(ancestorID, candidateID string) bool {
	if ancestorID == "" || candidateID == "" || ancestorID == candidateID {
		return false
	}
	if !s.exists(ancestorID) {
		return false
	}
	cur := candidateID
	for range maxChainDepth {
		parent, ok := s.ParentOf(cur)
		if !ok {
			return false
		}
		if parent == ancestorID {
			return true
		}
		cur = parent
	}
	return false
}

// DescendantDepth returns how many parent links separate candidateID from
// ancestorID: 1 for a direct child, 2 for a grandchild. It returns -1 when
// candidateID is not beneath ancestorID at all, which callers must treat as
// "refuse", never as depth 0.
//
// This is IsDescendant's arithmetic sibling and exists because a subscription
// subject may be depth-bounded (eventlog.Subject.MaxDepth): "direct children
// only" is not expressible with a boolean predicate. Like IsDescendant it
// walks the real parent chain rather than trusting the root label, which is a
// cache, and it is bounded by maxChainDepth for the same reason.
func (s *Store) DescendantDepth(ancestorID, candidateID string) int {
	if ancestorID == "" || candidateID == "" || ancestorID == candidateID {
		return -1
	}
	if !s.exists(ancestorID) {
		return -1
	}
	if !s.exists(candidateID) {
		return -1
	}
	cur := candidateID
	for depth := 1; depth <= maxChainDepth; depth++ {
		parent, ok := s.ParentOf(cur)
		if !ok {
			return -1
		}
		if parent == ancestorID {
			return depth
		}
		cur = parent
	}
	return -1
}

// AbsoluteDepth returns how many parent links separate childID from its
// top-level ancestor. A top-level child is 0. It returns -1 — the refuse
// sentinel, the same convention DescendantDepth uses — when the depth cannot
// be determined: childID is unknown, or its stored parent chain cannot be
// walked to a root within maxChainDepth hops (corrupt state, a hand-edited
// record, a cycle). Callers must treat -1 as "refuse", never as "top level"
// and never as the real depth 64: a chain that resolves on its final
// permitted hop legitimately IS 64 deep, so only -1 keeps "exactly 64" and
// "unknown, at least 64" distinguishable.
//
// This is the number RAFIKI_MAX_DEPTH bounds. It is computed from the stored
// parent chain rather than from any grant arithmetic, because a chain of
// grants is a chain of numbers a prompt-injected coordinator could be talked
// into getting wrong, while the chain of labels is what actually happened.
func (s *Store) AbsoluteDepth(childID string) int {
	if childID == "" {
		return -1
	}
	if !s.exists(childID) {
		return -1
	}
	depth := 0
	cur := childID
	for range maxChainDepth {
		parent, ok := s.ParentOf(cur)
		if !ok {
			return depth
		}
		depth++
		cur = parent
	}
	// The walk exhausted maxChainDepth hops without hitting a root, so depth
	// is exactly 64 — but that number is only real if cur is the root. One
	// more hop decides: a parent still being present means the chain
	// continues past the bound and the true depth is unknown, so report the
	// refuse sentinel rather than a number that a legitimate 64-deep chain
	// would also produce.
	if _, ok := s.ParentOf(cur); ok {
		return -1
	}
	return depth
}

// LiveDescendantCount counts descendants of ancestorID that have not exited.
//
// "Live", not "ever spawned": a long-running coordinator that has cycled
// through twenty workers must still be able to spawn a twenty-first, or the
// cap is a leak rather than a limit. Exited children are still in the store
// (GetRecent serves their rings after exit) so they must be filtered
// here rather than assumed absent.
//
// Native children are also excluded: a Claude Code Task subagent the proxy
// synthesized is a real conversation with a real cost, but it is not one of
// the cross-process agents the MaxChildren grant budgets.
func (s *Store) LiveDescendantCount(ancestorID string) int {
	n := 0
	for _, snap := range s.Descendants(ancestorID) {
		if snap.Status != protocol.StatusExited && !snap.Native {
			n++
		}
	}
	return n
}

// Descendants returns snapshots for every child beneath ancestorID at any
// depth. The root label narrows the candidate set to one subtree with an
// O(1) test per child; the chain walk then runs only over that set.
//
// Native children (Claude Code Task subagents the proxy synthesized) ARE
// returned: agent_list, subtreeSelector and the budget member lists all walk
// this, and a synthesized Task subagent is a real conversation with real cost.
// LiveDescendantCount is the one caller that excludes them, because it is the
// one that feeds the MaxChildren grant.
//
// Store has no label index (see store.go) so this scans List(). That is
// intentional and fine — the store holds tens of children in memory.
func (s *Store) Descendants(ancestorID string) []Snapshot {
	if ancestorID == "" {
		return nil
	}
	root := s.RootOf(ancestorID)
	if root == "" {
		return nil
	}
	var out []Snapshot
	s.sessions.Range(func(id string, sess *Session) bool {
		if id == ancestorID {
			return true
		}
		inTree := false
		s.readLabels(id, func(labels map[string]string) {
			r := labels[LabelRoot]
			inTree = r != "" && r == root
		})
		if inTree && s.IsDescendant(ancestorID, id) {
			out = append(out, sess.Snapshot())
		}
		return true
	})
	sort.Slice(out, func(i, j int) bool {
		return out[i].StartedAt.After(out[j].StartedAt)
	})
	return out
}

// CountDescendants counts the descendants of ancestorID whose status satisfies
// keep, without snapshotting them. The display state asks this once per listed
// child, so copying every descendant just to count it made a list quadratic in
// allocation.
func (s *Store) CountDescendants(ancestorID string, keep func(protocol.Status) bool) int {
	if ancestorID == "" {
		return 0
	}
	root := s.RootOf(ancestorID)
	if root == "" {
		return 0
	}
	n := 0
	s.sessions.Range(func(id string, sess *Session) bool {
		if id == ancestorID {
			return true
		}
		inTree := false
		s.readLabels(id, func(labels map[string]string) {
			r := labels[LabelRoot]
			inTree = r != "" && r == root
		})
		if !inTree || !s.IsDescendant(ancestorID, id) {
			return true
		}
		sess.mu.Lock()
		st := sess.Status
		sess.mu.Unlock()
		if keep(st) {
			n++
		}
		return true
	})
	return n
}

// DescendantCounts returns, for every child that has at least one, how many
// descendants satisfy keep, from one pass over the store. Listing N children
// with a CountDescendants call each is O(N^2) scans; this walks each child's
// parent chain once (bounded by maxChainDepth, like IsDescendant), so a list
// costs O(N * depth). A child with no matching descendant is absent (reads 0).
func (s *Store) DescendantCounts(keep func(protocol.Status) bool) map[string]int {
	type node struct {
		parent string
		live   bool
	}
	nodes := make(map[string]node)
	s.sessions.Range(func(id string, sess *Session) bool {
		var parent string
		sess.mu.Lock()
		parent, _ = ParentLabel(sess.Labels)
		live := keep(sess.Status)
		sess.mu.Unlock()
		nodes[id] = node{parent: parent, live: live}
		return true
	})
	counts := make(map[string]int)
	for id, n := range nodes {
		if !n.live {
			continue
		}
		cur := n.parent
		for range maxChainDepth {
			if cur == "" || cur == id {
				break
			}
			if _, ok := nodes[cur]; !ok {
				break
			}
			counts[cur]++
			cur = nodes[cur].parent
		}
	}
	return counts
}
