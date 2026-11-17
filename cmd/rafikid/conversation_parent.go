// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"log/slog"
)

// stampConversationParentSQL fills a child's root conversation with the root
// conversation of the child that spawned it. A conversation row is created
// before the daemon learns which child it belongs to, so the link is written
// here, from the child row, rather than at insert. It only fills a NULL, and
// only with a BROWSABLE parent conversation (node_count > 0 OR fork_from is
// set, the same predicate ListConversations uses for eligibility): parenting
// to a row the listing excludes would hide the child, because a browser walks
// down from an eligible row and never reaches a child whose parent it cannot
// list. A script child's task-ledger row is exactly that shape -- no messages,
// no fork_from -- so a script coordinator's workers stay at top level.
const stampConversationParentSQL = `
WITH me AS (
    SELECT ch.child_id,
           COALESCE(ch.labels->>'rafiki/parent', ch.labels->>'fundi/parent') AS parent_id,
           COALESCE(ch.conversation_id,
                    (SELECT v.id FROM conversations.conversation v
                      WHERE v.external_ref = ch.child_id
                      ORDER BY v.created_at LIMIT 1)) AS conv_id
      FROM conversations.child ch WHERE ch.child_id = $1
), parent AS (
    SELECT COALESCE(p.conversation_id,
                    (SELECT v.id FROM conversations.conversation v
                      WHERE v.external_ref = p.child_id
                      ORDER BY v.created_at LIMIT 1)) AS conv_id
      FROM conversations.child p JOIN me ON p.child_id = me.parent_id
)
UPDATE conversations.conversation c
   SET parent_conversation_id = parent.conv_id
  FROM me, parent, conversations.conversation pc
 WHERE c.id = me.conv_id
   AND pc.id = parent.conv_id
   AND (pc.node_count > 0 OR pc.fork_from IS NOT NULL)
   AND c.parent_conversation_id IS NULL
   AND parent.conv_id <> c.id`

// The outcomes stampConversationParentStateSQL reports after a stamp that
// changed no row.
const (
	// stampStateStamped: the conversation already names a parent.
	stampStateStamped = "stamped"
	// stampStateParentless: the child carries no parent label, so there is no
	// parent conversation to find, now or later.
	stampStateParentless = "parentless"
	// stampStatePending: the child names a parent whose conversation does not
	// exist yet, or exists but is not browsable, so a later row write must look
	// again. Neither is terminal: a conversation row is created before its
	// first message lands, so the parent may become browsable later.
	stampStatePending = "pending"
)

// stampConversationParentStateSQL classifies a zero-row stamp, resolving the
// child's conversation the same two ways the stamp does: the stored
// conversation_id, else the oldest conversation whose external_ref is the
// child id. It answers one row for every child that exists, and pending for a
// child that does not, so a vanished row is retried rather than memoized. Both
// terminal outcomes are memoizable; only pending keeps retrying.
const stampConversationParentStateSQL = `
SELECT COALESCE((
    SELECT CASE
               WHEN c.parent_conversation_id IS NOT NULL THEN '` + stampStateStamped + `'
               WHEN COALESCE(ch.labels->>'rafiki/parent', ch.labels->>'fundi/parent', '') = ''
                   THEN '` + stampStateParentless + `'
               ELSE '` + stampStatePending + `'
           END
      FROM conversations.child ch
      LEFT JOIN conversations.conversation c
        ON c.id = COALESCE(ch.conversation_id,
               (SELECT v.id FROM conversations.conversation v
                 WHERE v.external_ref = ch.child_id
                 ORDER BY v.created_at LIMIT 1))
     WHERE ch.child_id = $1
), '` + stampStatePending + `')`

// stampConversationParent records, on a child's root conversation, the root
// conversation of the child that spawned it. It only fills a NULL, only with a
// browsable parent conversation, and a parent whose conversation does not
// exist yet (or is not browsable yet) is retried on the child's next row
// write.
//
// A claude child leaves conversations.child.conversation_id NULL and links by
// external_ref, so both sides resolve their conversation the same two-arm way:
// the stored conversation_id, else the oldest conversation whose external_ref
// is the child id. The guard refuses a self-parent (a conversation that
// resolved to itself) and never touches updated_at -- the stamp is
// bookkeeping about the tree's shape, not activity on it.
//
// Once there is nothing left to do the outcome is memoized per child id on
// the Controller and later row writes for that child skip the lookups
// entirely. That covers both terminal outcomes: a conversation that already
// carries a parent (this daemon stamped it, or an earlier one did) and a child
// with no parent label at all. The parent-conversation-missing and
// parent-not-browsable cases stay unmemoized and keep retrying, which is what
// carries a child whose parent's own conversation is written after its first
// row write, or whose parent's conversation gains its first message later.
func (c *Controller) stampConversationParent(ctx context.Context, childID string) error {
	if c.pool == nil {
		return nil
	}
	if _, done := c.stampedParents.Load(childID); done {
		return nil
	}
	tag, err := c.pool.Exec(ctx, stampConversationParentSQL, childID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() > 0 {
		c.stampedParents.Store(childID, struct{}{})
		slog.Debug("stamped conversation parent", "childId", childID)
		return nil
	}
	var state string
	if err := c.pool.QueryRow(ctx, stampConversationParentStateSQL, childID).Scan(&state); err != nil {
		return err
	}
	if state == stampStateStamped || state == stampStateParentless {
		c.stampedParents.Store(childID, struct{}{})
	}
	return nil
}
