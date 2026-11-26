// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"strings"
	"sync"

	"go.graveland.dev/rafiki/pkg/childstore"
	"go.graveland.dev/rafiki/pkg/insights"
)

// closedUsageMemo holds, per CLOSED lineage member, the usage rows that member
// owns, keyed by child id.
//
// The memo rests on ONE fact: nothing the daemon does writes a closed child's
// ROW. Close refuses a live child, and the native birth path refuses a
// tombstoned row (the live-only upsert), so a closed member stays closed and an
// entry needs no invalidation and no TTL. An entry holding NO rows is a real hit
// — the closed children whose conversations hold no turns — which is why the map
// is read with presence rather than emptiness.
//
// It caches the CONVERSATION's usage, and a conversation can outlive its row: a
// closed NATIVE thread child's branch conversation keeps growing when its parent
// is resumed and re-enters the thread (EnsureThreadChild refuses the closed row
// and the turns land in "<parent>:<thread>" anyway). A memoised entry can
// therefore lag a conversation an OPEN member still spends into, which is why
// usageForMembers lets FRESH rows win the dedupe rather than trusting the memo.
//
// One staleness source is BOUNDED and deliberately left: a cockpit
// target-and-descendants close is Kill then Close back to back, and an in-flight
// turn's usage is written by failCompletion on a detached context after the
// process dies. A subtree read inside that window memoises the closed member
// without its last turn — and no OPEN member's fresh read reaches a closed
// fundi/claude child's own conversation, so fresh-first cannot help there. That is
// at most one turn per closed child, for the life of the daemon, accepted.
//
// An OPEN member is never memoised: it is spending as we speak.
type closedUsageMemo struct {
	mu   sync.Mutex
	rows map[string][]insights.ConversationUsage
}

// get returns a closed member's rows and whether it has been read at all.
func (m *closedUsageMemo) get(childID string) ([]insights.ConversationUsage, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	rows, ok := m.rows[childID]
	return rows, ok
}

// put records a closed member's rows for the life of the daemon. The slice is
// shared with every subsequent read, so callers treat what they get back as
// read-only.
//
// An EMPTY child id is refused: the key identifies the member the rows belong
// to, and a blank one would be a row set owned by every empty-id member (and
// handed back to whichever asked first).
func (m *closedUsageMemo) put(childID string, rows []insights.ConversationUsage) {
	if childID == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.rows == nil {
		m.rows = make(map[string][]insights.ConversationUsage)
	}
	m.rows[childID] = rows
}

// usageForMembers returns the usage rows of every member, unpriced, from at most
// one round trip.
//
// Memoised closed members are answered from closedUsageMemo; every OPEN member
// and every closed member not yet memoised goes into ONE UsageByConversation
// call. The read is then split back per member, the closed ones are memoised,
// and the union comes back DEDUPLICATED BY (conversation, model): a conversation
// two members can reach — a claude parent's thread branch and the native
// subagent row that claims it — is one conversation's spend.
//
// A FRESH row always wins that dedupe, whatever order the members arrive in.
// Member order is the lineage query's result order (no ORDER BY), so "first
// seen" would decide it by heap layout, and the wrong winner is the expensive
// direction: a closed native member's memo can be a turn — or a whole resumed
// session — behind the branch conversation its open parent is still spending
// into, and taking the stale row under-counts its ancestors' budget.
func (c *Controller) usageForMembers(ctx context.Context, members []childstore.LineageMember) ([]insights.ConversationUsage, error) {
	if c.coster == nil {
		return nil, errors.New("no cost source configured")
	}

	var (
		sel     insights.SubtreeSelector
		queried []childstore.LineageMember
	)
	byChild := make(map[string][]insights.ConversationUsage, len(members))
	asked := make(map[string]struct{}, len(members))
	for _, m := range members {
		// Only a CLOSED member is ever in the memo (the put below is gated on it),
		// so an open member always misses here and is read.
		if rows, ok := c.closedUsage.get(m.ChildID); ok {
			byChild[m.ChildID] = rows
			continue
		}
		// A nested script child can appear in two members' lines; the selector
		// names it once.
		if _, dup := asked[m.ChildID]; dup {
			continue
		}
		asked[m.ChildID] = struct{}{}
		queried = append(queried, m)
		addLineageMember(&sel, m)
	}

	fresh, err := c.coster.UsageByConversation(ctx, sel)
	if err != nil {
		return nil, err
	}
	for _, m := range queried {
		rows := usageOwnedBy(fresh, m)
		byChild[m.ChildID] = rows
		if m.Closed {
			c.closedUsage.put(m.ChildID, rows)
		}
	}

	// Fresh first, explicitly: a member read THIS call outranks one the memo
	// answered, so a stale memoised row can never displace a fresh one for the
	// same (conversation, model). Two passes because member order carries no
	// meaning worth depending on.
	readNow := make(map[string]struct{}, len(queried))
	for _, m := range queried {
		readNow[m.ChildID] = struct{}{}
	}
	seen := make(map[usageKey]struct{}, len(fresh))
	var out []insights.ConversationUsage
	for _, freshPass := range []bool{true, false} {
		for _, m := range members {
			if _, isFresh := readNow[m.ChildID]; isFresh != freshPass {
				continue
			}
			for _, row := range byChild[m.ChildID] {
				key := usageKey{conversationID: row.ConversationID, model: row.Model}
				if _, dup := seen[key]; dup {
					continue
				}
				seen[key] = struct{}{}
				out = append(out, row)
			}
		}
	}
	return out, nil
}

// usageKey identifies one usage row: a conversation's tokens on one model. Two
// members reaching the same conversation produce this key twice.
type usageKey struct {
	conversationID string
	model          string
}

// memberOwnsUsage reports whether a usage row belongs to a member. The three
// arms are the three routes addLineageMember asks the query for, read back per
// member: the conversation is the member's own (its conversation_id, or the
// session id a fundi child runs its turns in), or the row's external ref names
// the child or one of its thread branches ("<child id>:<thread>") — the branch
// a Claude Code WebFetch/WebSearch helper forks, which is a tool call its parent
// made.
func memberOwnsUsage(m childstore.LineageMember, u insights.ConversationUsage) bool {
	if u.ConversationID != "" && (u.ConversationID == m.ConversationID || u.ConversationID == m.SessionID) {
		return true
	}
	if m.ChildID == "" || u.ExternalRef == "" {
		return false
	}
	return u.ExternalRef == m.ChildID || strings.HasPrefix(u.ExternalRef, m.ChildID+threadRefSep)
}

// usageOwnedBy selects the rows among usage that a member owns.
func usageOwnedBy(usage []insights.ConversationUsage, m childstore.LineageMember) []insights.ConversationUsage {
	var out []insights.ConversationUsage
	for _, u := range usage {
		if memberOwnsUsage(m, u) {
			out = append(out, u)
		}
	}
	return out
}

// usageOfMembers is usageOwnedBy over several members, deduplicated by
// (conversation, model), so pricing a subtree counts a shared conversation once.
func usageOfMembers(usage []insights.ConversationUsage, members []childstore.LineageMember) []insights.ConversationUsage {
	seen := make(map[usageKey]struct{}, len(usage))
	var out []insights.ConversationUsage
	for _, m := range members {
		for _, u := range usageOwnedBy(usage, m) {
			key := usageKey{conversationID: u.ConversationID, model: u.Model}
			if _, dup := seen[key]; dup {
				continue
			}
			seen[key] = struct{}{}
			out = append(out, u)
		}
	}
	return out
}
