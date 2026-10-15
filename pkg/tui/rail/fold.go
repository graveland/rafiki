// SPDX-License-Identifier: Apache-2.0

package rail

// Fold returns the rows visible when every child in collapsed has its
// descendants hidden, at any depth.
//
// nodes must be in display order (Nodes()): a row's descendants are then the
// contiguous run of deeper rows after it, which is what makes this a single
// pass with no parent lookups. A collapsed row with descendants comes back
// Folded, its Attention the sum of its own and every descendant's, hidden
// collapsed ones included, so a badge never disappears into a fold. A
// collapsed row with no descendants is an ordinary leaf. The input is not
// modified; collapse state is the caller's and a nested row keeps its own
// entry while an ancestor hides it.
func Fold(nodes []Node, collapsed map[string]bool) []Node {
	if len(collapsed) == 0 {
		return nodes
	}
	out := make([]Node, 0, len(nodes))
	for i := 0; i < len(nodes); i++ {
		n := nodes[i]
		end := i + 1
		for end < len(nodes) && nodes[end].Depth > n.Depth {
			end++
		}
		if collapsed[n.ChildID] && end > i+1 {
			n.Folded = true
			for _, d := range nodes[i+1 : end] {
				n.Attention += d.Attention
			}
			i = end - 1
		}
		out = append(out, n)
	}
	return out
}

// Ancestors lists the ids above childID, nearest first. The walk is bounded by
// the row count so a malformed parent cycle terminates.
func (r *Rail) Ancestors(childID string) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	cur, ok := r.nodes[childID]
	for i := 0; ok && i < len(r.nodes); i++ {
		parent, found := r.nodes[cur.ParentID]
		if !found {
			break
		}
		out = append(out, parent.ChildID)
		cur = parent
	}
	return out
}
