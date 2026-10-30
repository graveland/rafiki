// SPDX-License-Identifier: Apache-2.0

package conv

import "fmt"

// Page returns one page of the transcript: the nodes left after the bounds,
// newest N kept when a limit is set, widened backwards so a page never splits
// nodes that share a CreatedAt.
//
// Compaction tails never appear and never count toward the limit: they are
// copies of earlier rows that render nothing, so counting them would short a
// page.
func (sn *Snapshot) Page(q PageQuery) ([]*Node, error) {
	if q.Limit != nil && *q.Limit <= 0 {
		return nil, fmt.Errorf("conv: page: limit %d is not positive", *q.Limit)
	}
	in := make([]*Node, 0, len(sn.nodes))
	for n := range sn.Transcript() {
		if q.AfterOrdinal != nil && n.Ordinal <= *q.AfterOrdinal {
			continue
		}
		if q.Before != nil && !n.CreatedAt.Before(*q.Before) {
			continue
		}
		in = append(in, n)
	}
	if q.Limit == nil || len(in) <= *q.Limit {
		return in, nil
	}
	// edge is the oldest node the limit keeps; floor is where that node's
	// same-CreatedAt run begins — one past the newest in-bounds node older than
	// edge with a different CreatedAt — so the widening is exactly "extend the
	// page back over adjacent nodes sharing the edge's timestamp".
	edge := in[len(in)-*q.Limit]
	floor := -1
	for _, n := range in {
		if n.Ordinal < edge.Ordinal && !n.CreatedAt.Equal(edge.CreatedAt) && n.Ordinal+1 > floor {
			floor = n.Ordinal + 1
		}
	}
	if floor <= in[0].Ordinal {
		return in, nil
	}
	kept := make([]*Node, 0, len(in))
	for _, n := range in {
		if n.Ordinal >= floor {
			kept = append(kept, n)
		}
	}
	return kept, nil
}
