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
	transcript := make([]*Node, 0, len(sn.nodes))
	for n := range sn.Transcript() {
		transcript = append(transcript, n)
	}
	return selectPage(transcript, func(n *Node) Ref { return n.Ref }, q)
}

// selectPage applies a PageQuery to a chronological transcript, reading only
// each item's Ref. Snapshot.Page runs it over nodes and Store.Page over a path
// whose content has not been fetched, so the two cannot disagree on which
// items make a page.
func selectPage[T any](transcript []T, ref func(T) Ref, q PageQuery) ([]T, error) {
	if q.Limit != nil && *q.Limit <= 0 {
		return nil, fmt.Errorf("conv: page: limit %d is not positive", *q.Limit)
	}
	in := make([]T, 0, len(transcript))
	for _, it := range transcript {
		r := ref(it)
		if q.AfterOrdinal != nil && r.Ordinal <= *q.AfterOrdinal {
			continue
		}
		if q.Before != nil && !r.CreatedAt.Before(*q.Before) {
			continue
		}
		in = append(in, it)
	}
	if q.Limit == nil || len(in) <= *q.Limit {
		return in, nil
	}
	// edge is the oldest item the limit keeps; floor is where that item's
	// same-CreatedAt run begins — one past the newest in-bounds item older than
	// edge with a different CreatedAt — so the widening is exactly "extend the
	// page back over adjacent items sharing the edge's timestamp".
	edge := ref(in[len(in)-*q.Limit])
	floor := -1
	for _, it := range in {
		r := ref(it)
		if r.Ordinal < edge.Ordinal && !r.CreatedAt.Equal(edge.CreatedAt) && r.Ordinal+1 > floor {
			floor = r.Ordinal + 1
		}
	}
	if floor <= ref(in[0]).Ordinal {
		return in, nil
	}
	kept := make([]T, 0, len(in))
	for _, it := range in {
		if ref(it).Ordinal >= floor {
			kept = append(kept, it)
		}
	}
	return kept, nil
}
