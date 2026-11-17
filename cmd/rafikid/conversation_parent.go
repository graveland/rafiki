// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"log/slog"
)

// stampConversationParent records, on a child's root conversation, the root
// conversation of the child that spawned it. A conversation row is created
// before the daemon learns which child it belongs to, so the link is written
// here, from the child row, rather than at insert. It only fills a NULL, and
// a parent whose conversation does not exist yet is retried on the child's
// next row write.
//
// A claude child leaves conversations.child.conversation_id NULL and links by
// external_ref, so both sides resolve their conversation the same three-arm
// way a reader does: the stored conversation_id, else the oldest conversation
// whose external_ref is the child id. The guard refuses a self-parent (a
// conversation that resolved to itself) and never touches updated_at -- the
// stamp is bookkeeping about the tree's shape, not activity on it.
func (c *Controller) stampConversationParent(ctx context.Context, childID string) error {
	if c.pool == nil {
		return nil
	}
	const sql = `
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
  FROM me, parent
 WHERE c.id = me.conv_id
   AND c.parent_conversation_id IS NULL
   AND parent.conv_id IS NOT NULL
   AND parent.conv_id <> c.id`
	tag, err := c.pool.Exec(ctx, sql, childID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() > 0 {
		slog.Debug("stamped conversation parent", "childId", childID)
	}
	return nil
}
