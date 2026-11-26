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
// A closed child never changes: Close refuses a live child, nothing clears
// closed_at, so the rows behind a closed member are fixed and an entry needs no
// invalidation and no TTL. An entry holding NO rows is a real hit — the closed
// children whose conversations hold no turns — which is why the map is read with
// presence rather than emptiness.
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
func (m *closedUsageMemo) put(childID string, rows []insights.ConversationUsage) {
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

	seen := make(map[usageKey]struct{}, len(fresh))
	var out []insights.ConversationUsage
	for _, m := range members {
		for _, row := range byChild[m.ChildID] {
			key := usageKey{conversationID: row.ConversationID, model: row.Model}
			if _, dup := seen[key]; dup {
				continue
			}
			seen[key] = struct{}{}
			out = append(out, row)
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
