package childstore

import "context"

// LineageMember is one child in an ancestor's lineage: the ancestor itself or
// any descendant at any depth, live or closed. It is the database-free shape a
// lineage source returns, so callers that only need identity/scope (subtree
// conversation scope, subtree spend, budget enforcement) need not know how the
// rows were read.
type LineageMember struct {
	ChildID        string
	SessionID      string // child.session_id ("" if none)
	ConversationID string // child.conversation_id ("" if none)
	Closed         bool   // child.closed_at IS NOT NULL
}

// LineageSource resolves an ancestor's full lineage. Unlike a live-state read,
// it covers closed descendants too, so a closed child stays in its ancestors'
// lineage for subtree scoping and spend.
//
// Lineage returns the ancestor's OWN row (when it exists) and every descendant
// at any depth, live or closed. An unknown ancestor yields (nil, nil).
type LineageSource interface {
	Lineage(ctx context.Context, ancestorChildID string) ([]LineageMember, error)
}

// LineageRow pairs a member with the labels it was read with, so a caller can
// walk the parent chain without a second lookup. Labels are the raw child
// labels (rafiki/parent, rafiki/root, and their legacy fundi/ spellings).
type LineageRow struct {
	Member LineageMember
	Labels map[string]string
}

// DescendantsOf returns the ancestor's own row (when present in rows) and
// every row beneath it, walking the parent labels (LabelParent, legacy
// fundi/parent) with the same bound as IsDescendant (maxChainDepth). A row
// whose chain does not reach the ancestor within the bound is excluded; a
// cycle can never loop (the bound ends it); rows are returned in input order.
//
// It is pure Go: no database, no store. The rows are what a lineage source
// read, and this is the in-memory walk shared by every implementation.
func DescendantsOf(rows []LineageRow, ancestorChildID string) []LineageMember {
	if ancestorChildID == "" {
		return nil
	}
	labels := make(map[string]map[string]string, len(rows))
	for _, r := range rows {
		if _, ok := labels[r.Member.ChildID]; !ok {
			labels[r.Member.ChildID] = r.Labels
		}
	}
	var out []LineageMember
	for _, r := range rows {
		if r.Member.ChildID == ancestorChildID {
			out = append(out, r.Member)
			continue
		}
		if reachesAncestor(labels, r.Member.ChildID, ancestorChildID) {
			out = append(out, r.Member)
		}
	}
	return out
}

// reachesAncestor reports whether cur's parent chain reaches ancestor within
// the IsDescendant bound. It walks the labels the rows were read with; a hop
// whose labels are absent from the input set cannot be confirmed and ends the
// walk, matching IsDescendant's verify-on-read against a live store.
func reachesAncestor(labels map[string]map[string]string, cur, ancestor string) bool {
	for range maxChainDepth {
		l, ok := labels[cur]
		if !ok {
			return false
		}
		parent, ok := labelLookup(l, LabelParent, legacyLabelParent)
		if !ok {
			return false
		}
		if parent == ancestor {
			return true
		}
		cur = parent
	}
	return false
}

// RootLabel returns the top-level ancestor's child id stored in labels
// (LabelRoot, else legacy fundi/root), or "" when neither is present — a
// top-level child is its own root, which the caller resolves.
func RootLabel(labels map[string]string) string {
	v, _ := labelLookup(labels, LabelRoot, legacyLabelRoot)
	return v
}
