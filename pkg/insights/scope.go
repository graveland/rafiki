// SPDX-License-Identifier: Apache-2.0

package insights

// Scope is what a query is allowed to see. The zero value DENIES -- it must
// never be treated as "no filter". This mirrors two zero-value traps already
// in this codebase (inbox.ModePrompt = iota meaning "prompt"; MaxCost == 0
// meaning unlimited): a Scope{} that reached a query builder as "everything"
// would be the worst version of that pattern, so it reads as "nothing"
// instead.
//
// Unexported fields mean a Scope cannot be built by a struct literal outside
// this package -- the only ways to get one are ScopeAll, ScopeOwner or
// ScopeSubtree.
type Scope struct {
	all     bool
	ownerID string
	// subtree, when set, is the ONLY admissible boundary: the scope admits
	// exactly the conversations the selector names -- by conversation id,
	// external_ref or external_ref prefix -- never an owner filter (an agent
	// subtree may be an anonymous lineage with no owner at all). Exclusive
	// with all/ownerID by construction: ScopeSubtree sets only this field.
	subtree *SubtreeSelector
}

// ScopeAll admits every owner's rows.
func ScopeAll() Scope { return Scope{all: true} }

// ScopeOwner admits only rows owned by ownerID. ScopeOwner("") is
// deliberately still invalid (see valid()) -- an empty id is not an owner.
func ScopeOwner(ownerID string) Scope { return Scope{ownerID: ownerID} }

// ScopeSubtree admits exactly the conversations of one agent subtree, as the
// caller's daemon built it (see SubtreeSelector for the three correlation
// routes). An EMPTY selector fails closed: the zero rows it names render an
// always-false condition, the same rule the zero Scope obeys.
//
// This is the boundary a per-child credential reads conversations through
// (the MCP face's conversation tools): a child sees its own subtree's
// conversations, never its owner's whole corpus. Unlike ScopeOwner there is
// no owner dimension -- a subtree may be an anonymous lineage -- so the
// selector itself is the whole boundary and must be built from stored state,
// never from a request argument.
func ScopeSubtree(sel SubtreeSelector) Scope { return Scope{subtree: &sel} }

// valid reports whether s actually admits anything. The zero value is
// invalid and denies everything; an empty subtree selector denies too.
func (s Scope) valid() bool {
	return s.all || s.ownerID != "" || (s.subtree != nil && !s.subtree.empty())
}

// cond renders s as a SQL boolean expression. ownerCol is the column (or
// qualified column) holding the owning user's id, e.g. "c.owner_user_id";
// idCol and refCol spell the conversation's id and external_ref columns the
// same way, used only by the subtree arm -- ("c.owner_user_id", "c.id",
// "c.external_ref") for a query that aliases the conversation as c, or
// ("owner_user_id", "id", "external_ref") for a probe on the bare table.
// Appends any needed parameter to a and returns the expression text. An
// invalid scope renders as an always-false condition -- fail closed, never
// fail open -- so a bug that lets a zero-value Scope reach a query builder
// denies every row instead of leaking every row.
func (s Scope) cond(a *argList, ownerCol, idCol, refCol string) string {
	switch {
	case s.subtree != nil:
		if s.subtree.empty() {
			return "1=0"
		}
		// starts_with rather than LIKE because a child id contains '_'
		// ("c_01M2…"), which LIKE reads as a single-character wildcard -- an
		// escaping bug that would silently over-match a sibling. The same
		// WHERE clause SubtreeCost rolls spend up with, so scope and cost
		// agree on what "one subtree" is.
		return `(` + idCol + ` = ANY(` + a.next(nonNilUUIDs(s.subtree.ConversationIDs)) + `::uuid[])
		 OR ` + refCol + ` = ANY(` + a.next(nonNilStrings(s.subtree.ExternalRefs)) + `::text[])
		 OR EXISTS (SELECT 1 FROM unnest(` + a.next(nonNilStrings(s.subtree.ExternalRefPrefixes)) + `::text[]) p WHERE starts_with(` + refCol + `, p)))`
	case !s.valid():
		return "1=0"
	case s.all:
		return "1=1"
	default:
		return ownerCol + " = " + a.next(s.ownerID) + "::uuid"
	}
}
