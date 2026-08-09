package childstore

// Lineage label keys. These are daemon-written: the controller computes them
// at spawn and re-stamps them at resume. They are never accepted from a
// caller — SpawnRequest.Labels rejects the whole rafiki/ prefix.
const (
	LabelParent = "rafiki/parent"
	LabelRoot   = "rafiki/root"

	// Pre-rename spellings. Records persisted before the fundi -> rafiki
	// identity consolidation still carry these, so readers tolerate them.
	// Nothing writes them.
	legacyLabelParent = "fundi/parent"
	legacyLabelRoot   = "fundi/root"
)

// maxChainDepth bounds the parent walk in IsDescendant. Lineage labels are
// daemon-written and cannot legitimately form a cycle, but a corrupt or
// hand-edited state record must not wedge a caller in an infinite loop —
// this predicate runs on the authority path for every steering verb.
const maxChainDepth = 64

// labelLookup reads key from labels, falling back to the pre-rename spelling.
func labelLookup(labels map[string]string, key, legacy string) (string, bool) {
	if v, ok := labels[key]; ok && v != "" {
		return v, true
	}
	if v, ok := labels[legacy]; ok && v != "" {
		return v, true
	}
	return "", false
}

// ParentOf returns the direct parent's child id. The second return is false
// when childID is unknown or is a top-level child with no parent.
func (s *Store) ParentOf(childID string) (string, bool) {
	snap, ok := s.Get(childID)
	if !ok {
		return "", false
	}
	return labelLookup(snap.Labels, LabelParent, legacyLabelParent)
}

// RootOf returns the top-level ancestor's child id. A top-level child is its
// own root. Returns "" when childID is unknown.
func (s *Store) RootOf(childID string) string {
	snap, ok := s.Get(childID)
	if !ok {
		return ""
	}
	if root, ok := labelLookup(snap.Labels, LabelRoot, legacyLabelRoot); ok {
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
	if _, ok := s.Get(ancestorID); !ok {
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

// Descendants returns snapshots for every child beneath ancestorID at any
// depth. The root label narrows the candidate set to one subtree with an
// O(1) test per child; the chain walk then runs only over that set.
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
	for _, snap := range s.List() {
		if snap.ChildID == ancestorID {
			continue
		}
		if r, ok := labelLookup(snap.Labels, LabelRoot, legacyLabelRoot); !ok || r != root {
			continue
		}
		if s.IsDescendant(ancestorID, snap.ChildID) {
			out = append(out, snap)
		}
	}
	return out
}
