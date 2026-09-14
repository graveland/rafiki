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
// this package -- the only way to get one is ScopeAll or ScopeOwner.
type Scope struct {
	all     bool
	ownerID string
}

// ScopeAll admits every owner's rows.
func ScopeAll() Scope { return Scope{all: true} }

// ScopeOwner admits only rows owned by ownerID. ScopeOwner("") is
// deliberately still invalid (see valid()) -- an empty id is not an owner.
func ScopeOwner(ownerID string) Scope { return Scope{ownerID: ownerID} }

// valid reports whether s actually admits anything. The zero value is
// invalid and denies everything.
func (s Scope) valid() bool { return s.all || s.ownerID != "" }

// cond renders s as a SQL boolean expression over ownerCol (a column or
// qualified column holding the owning user's id, e.g. "c.owner_user_id"),
// appending any needed parameter to a and returning the expression text. An
// invalid scope renders as an always-false condition -- fail closed, never
// fail open -- so a bug that lets a zero-value Scope reach a query builder
// denies every row instead of leaking every row.
func (s Scope) cond(a *argList, ownerCol string) string {
	switch {
	case !s.valid():
		return "1=0"
	case s.all:
		return "1=1"
	default:
		return ownerCol + " = " + a.next(s.ownerID) + "::uuid"
	}
}
