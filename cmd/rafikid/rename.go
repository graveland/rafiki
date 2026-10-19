// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"

	"google.golang.org/protobuf/types/known/timestamppb"

	"go.graveland.dev/rafiki/pkg/connectapi"
	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/protocol"
)

// Rename sets childID's name and the name of every conversation linked to it.
// Both halves are idempotent, so a retry after a partial failure converges.
// A child that is not registered (never existed, or already closed) is
// ErrNotFound.
func (c *Controller) Rename(ctx context.Context, childID, name string) error {
	if _, ok := c.st.Get(childID); !ok {
		return &connectapi.ControllerError{
			Code:    protocol.ErrNotFound,
			Message: fmt.Sprintf("agent %s is not registered", childID),
		}
	}
	if err := c.st.Rename(childID, name); err != nil {
		return fmt.Errorf("rename child %s: %w", childID, err)
	}
	if err := c.writeRecord(childID); err != nil {
		return err
	}
	if err := c.nameConversations(ctx, childID, name); err != nil {
		return err
	}
	c.publishEvent(childID, &rafikiv1.Event{
		ChildId: childID,
		Ts:      timestamppb.Now(),
		Payload: &rafikiv1.Event_ChildRenamed{ChildRenamed: &rafikiv1.ChildRenamed{Name: name}},
	})
	return nil
}

// nameConversations renames every conversation linked to a child, by the same
// three arms as stampConversationsClosed: the child's own conversation
// (fundi), external_ref = child id (claude root thread), and child_id + ":" +
// thread (claude subagent threads), with _ and % escaped in the LIKE.
func (c *Controller) nameConversations(ctx context.Context, childID, name string) error {
	if c.pool == nil {
		return nil
	}
	const q = `
UPDATE conversations.conversation c SET name = $2, updated_at = now()
 WHERE c.name IS DISTINCT FROM $2
   AND (c.id = (SELECT conversation_id FROM conversations.child WHERE child_id = $1)
        OR c.external_ref = $1
        OR c.external_ref LIKE replace(replace($1,'_','\_'),'%','\%') || ':%' ESCAPE '\')`
	if _, err := c.pool.Exec(ctx, q, childID, name); err != nil {
		return fmt.Errorf("rename conversations %s: %w", childID, err)
	}
	return nil
}
