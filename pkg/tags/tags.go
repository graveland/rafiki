// SPDX-License-Identifier: Apache-2.0

// Package tags is the pgx-free tag domain. A tag names a conversation node
// that another child may fork from, scoped to a lineage, a tree, or an owner.
// It is stdlib-only so the CLI, the daemon and the executor can all reason
// about scope without a database driver.
package tags

import (
	"fmt"
	"regexp"
	"slices"
	"sort"
	"time"
)

// Scope says who may see and fork from a tag.
type Scope int

const (
	// ScopeUnset is the zero value. It is never stored: a tag always has one
	// of the scopes below.
	ScopeUnset Scope = iota
	// ScopeLineage limits a tag to one child's lineage — the root child, its
	// descendants, and its ancestors.
	ScopeLineage
	// ScopeTree limits a tag to the members of one agent tree.
	ScopeTree
	// ScopeOwner limits a tag to its owner's children.
	ScopeOwner
)

// String returns the stored spelling of s, or "" for ScopeUnset.
func (s Scope) String() string {
	switch s {
	case ScopeLineage:
		return "lineage"
	case ScopeTree:
		return "tree"
	case ScopeOwner:
		return "owner"
	}
	return ""
}

// ParseScope reads the exact lowercase scope name the database stores.
func ParseScope(s string) (Scope, error) {
	switch s {
	case "lineage":
		return ScopeLineage, nil
	case "tree":
		return ScopeTree, nil
	case "owner":
		return ScopeOwner, nil
	}
	return ScopeUnset, fmt.Errorf("tags: unknown tag scope %q", s)
}

// Tag is one version of one named tag. A move supersedes the current row and
// appends the next; an untag tombstones the current row. Rows are otherwise
// immutable, which is why every version carries the same TagID.
type Tag struct {
	TagID       string
	OwnerUserID string // "" = no owner (SQL NULL)
	Name        string
	Scope       Scope
	ScopeRoot   string
	// SourceChildID is the child whose conversation the node belongs to.
	SourceChildID  string
	ConversationID string
	NodeID         string
	// Ordinal is the node's insertion ordinal in its conversation, joined in
	// from conversation_message. It is the insertion sequence, not a position.
	Ordinal int
	// CreatorChildID is the child that created the tag; "" means a user
	// credential did. ActorChildID is who wrote THIS row ("" for a user).
	CreatorChildID string
	ActorChildID   string
	Note           string
	CreatedAt      time.Time
	SupersededAt   *time.Time
	DeletedAt      *time.Time
}

// Position is a caller's place in its owner's agent tree. A user credential
// has no ChildID.
type Position struct {
	ChildID string
	// Ancestors is the caller's parent chain, nearest first, closed included;
	// the top-level ancestor is last.
	Ancestors []string
}

// TopLevel returns the caller's top-level ancestor, or the caller itself when
// it has none. A user credential has no position and answers "".
func (p Position) TopLevel() string {
	if len(p.Ancestors) > 0 {
		return p.Ancestors[len(p.Ancestors)-1]
	}
	return p.ChildID
}

const (
	// MaxNameLen is the longest tag name, in bytes.
	MaxNameLen = 64
	// MaxNoteLen is the longest tag note, in bytes.
	MaxNoteLen = 1024
)

// nameRe is the tag-name alphabet: lowercase letters, digits, dot, underscore
// and hyphen, starting with a letter or digit. Length is checked against
// MaxNameLen so the constant stays the single source of truth.
var nameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)

// ValidName reports whether name may be a tag name.
func ValidName(name string) error {
	if len(name) == 0 || len(name) > MaxNameLen || !nameRe.MatchString(name) {
		return fmt.Errorf("tag name %q must be 1-64 of [a-z0-9._-], starting with a letter or digit", name)
	}
	return nil
}

// InScope reports whether p may fork from t. rootAncestors is the lineage
// root's own ancestor chain (nearest first), used only for lineage scope.
//
// A user credential (p.ChildID == "") is in scope for every tag.
func InScope(t Tag, p Position, rootAncestors []string) bool {
	if p.ChildID == "" {
		return true
	}
	switch t.Scope {
	case ScopeLineage:
		return p.ChildID == t.ScopeRoot ||
			slices.Contains(p.Ancestors, t.ScopeRoot) ||
			slices.Contains(rootAncestors, p.ChildID)
	case ScopeTree:
		return p.TopLevel() == t.ScopeRoot
	case ScopeOwner:
		return true
	}
	return false
}

// CanModify reports whether p may move or untag t: a user credential, the
// creating child, or an ancestor of it (creatorAncestors, closed included).
// A user-created tag (CreatorChildID == "") can be modified only by a user.
func CanModify(t Tag, p Position, creatorAncestors []string) bool {
	if p.ChildID == "" {
		return true
	}
	if t.CreatorChildID == "" {
		return false
	}
	return p.ChildID == t.CreatorChildID || slices.Contains(creatorAncestors, p.ChildID)
}

// Rank orders in-scope candidates nearest first, stably. All lineage tags come
// before tree, and all tree before owner. Within lineage: the tag rooted at the
// caller first, then tags whose root is one of the caller's ancestors by that
// ancestor's index (nearest first), then the rest — the caller is an ancestor
// of the root — newest first. Remaining ties go to newer CreatedAt, then the
// smaller TagID.
func Rank(cands []Tag, p Position) []Tag {
	ranked := make([]rankedTag, len(cands))
	for i, t := range cands {
		ranked[i] = rankedTag{tag: t, key: keyFor(t, p)}
	}
	sort.SliceStable(ranked, func(i, j int) bool {
		a, b := ranked[i].key, ranked[j].key
		if a.scope != b.scope {
			return a.scope < b.scope
		}
		if a.group != b.group {
			return a.group < b.group
		}
		if a.ancestor != b.ancestor {
			return a.ancestor < b.ancestor
		}
		if !a.created.Equal(b.created) {
			return a.created.After(b.created)
		}
		return a.tagID < b.tagID
	})
	out := make([]Tag, len(ranked))
	for i, r := range ranked {
		out[i] = r.tag
	}
	return out
}

// rankedTag pairs a candidate with its sort key, so two versions of one tag
// (same TagID) never collide in a key table.
type rankedTag struct {
	tag Tag
	key rankKey
}

// rankKey is one candidate's sort position. scope is the scope group (lineage,
// tree, owner, unset), group/ancestor are the lineage sub-ordering, and the
// rest break ties.
type rankKey struct {
	scope    int
	group    int
	ancestor int
	created  time.Time
	tagID    string
}

func keyFor(t Tag, p Position) rankKey {
	k := rankKey{created: t.CreatedAt, tagID: t.TagID}
	switch t.Scope {
	case ScopeLineage:
		k.scope = 0
		switch {
		case t.ScopeRoot == p.ChildID:
			k.group, k.ancestor = 0, 0
		case slices.Contains(p.Ancestors, t.ScopeRoot):
			k.group, k.ancestor = 1, slices.Index(p.Ancestors, t.ScopeRoot)
		default:
			// The caller is an ancestor of the root: no distance to rank by,
			// so the newest tag wins.
			k.group, k.ancestor = 2, 0
		}
	case ScopeTree:
		k.scope = 1
	case ScopeOwner:
		k.scope = 2
	default:
		// An unset scope is never stored; it sorts last rather than silently
		// claiming lineage's first place.
		k.scope = 3
	}
	return k
}
