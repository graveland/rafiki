// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"fmt"

	"google.golang.org/protobuf/types/known/timestamppb"

	"go.graveland.dev/rafiki/pkg/childstore"
	"go.graveland.dev/rafiki/pkg/connectapi"
	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/protocol"
)

// Rename sets childID's name and the name of every conversation linked to it.
// Both halves are idempotent, so a retry after a partial failure converges.
// A child that is not registered (never existed, or already closed) is
// ErrNotFound.
//
// The new name must be free among the child's live siblings, on the same rule
// Spawn admits: the child_sibling_name_live index is the authority, and the
// pre-read is what lets an ordinary refusal name its holder. A holder that IS
// this child is not a conflict — re-stating your own name must stay a no-op.
func (c *Controller) Rename(ctx context.Context, childID, name string) error {
	snap, ok := c.st.Get(childID)
	if !ok {
		return &connectapi.ControllerError{
			Code:    protocol.ErrNotFound,
			Message: fmt.Sprintf("agent %s is not registered", childID),
		}
	}
	if c.children != nil {
		// "" parent (no parent label) is top level, and every top-level agent
		// of the owner shares that sibling set. ParentOf carries the legacy
		// fundi/parent fallback, exactly as childstore.Lineage does.
		parent, _ := c.st.ParentOf(childID)
		holderID, holderStatus, taken, err := c.children.NameHolder(ctx, snap.OwnerUserID, parent, name)
		if err != nil {
			return fmt.Errorf("check name %q: %w", name, err)
		}
		if taken && holderID != childID {
			return nameHeldError(name, holderID, holderStatus)
		}
	}
	old := snap.Name
	if err := c.st.Rename(childID, name); err != nil {
		return fmt.Errorf("rename child %s: %w", childID, err)
	}
	if err := c.writeRecord(childID); err != nil {
		// The rename is only real once the row carries it: a snapshot renamed
		// over a failed write would show a name the database — and every other
		// daemon — does not have. Undo it before reporting, so the caller's
		// error and the state agree.
		_ = c.st.Rename(childID, old)
		if errors.Is(err, childstore.ErrNameTaken) {
			// Free at the pre-read, taken by a sibling since.
			return &connectapi.ControllerError{
				Code:    protocol.ErrAlreadyExists,
				Message: fmt.Sprintf("name %q was taken concurrently; retry", name),
			}
		}
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
