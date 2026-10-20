// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"fmt"

	"google.golang.org/protobuf/types/known/timestamppb"

	"go.graveland.dev/rafiki/pkg/capture"
	"go.graveland.dev/rafiki/pkg/childstore"
	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/store"
	"go.graveland.dev/rafiki/pkg/tasks"
)

// ensureLedgerConversation returns the conversation id that keys the task
// ledger owned by snap's child, creating the row for a child kind whose
// conversations are captured by the proxy (a claude child) rather than minted
// by an engine.
//
// A fundi child's engine mints its own conversation UUID, so its ledger is
// keyed by that id; an empty SessionID means the engine has not created it
// yet, and the caller must ask again rather than invent one.
//
// Any other kind is keyed the way the proxy face keys the same child's
// traffic — external_ref = child id, client-driven (proxyChildEnv stamps the
// child id into X-Rafiki-Session) — so the row this call creates and the row
// the proxy's first request would create are the same row. Exactly one of the
// two creates it and the other falls through to a SELECT, which never updates
// it, so the remaining attributes mirror what pkg/server/proxy.go's
// beginCapture writes for a daemon-spawned child:
//
//   - origin_entrypoint is X-Rafiki-Source, which the daemon sets to the
//     child's kind, so snap.Kind is the same value.
//   - owner_user_id is the identity the proxy's authenticator resolves from
//     the child's credential, which for a child is OwnerUserIDForChild: the
//     child's own row, else the same parent-chain walk this repeats for rows
//     that predate the owner column.
//   - name, model and persona are deliberately left unset: the proxy path
//     never writes them (Rename fills name in later), and a value written here
//     would contradict the proxy having created the row first.
//
// A daemon with no capture store has no conversation table to key against; the
// synthetic "child:<id>" keeps the in-memory ledger working, the same
// degradation mcpLedger applies for a pool-less daemon.
func (c *Controller) ensureLedgerConversation(ctx context.Context, snap childstore.Snapshot) (string, error) {
	if snap.Kind == protocol.KindFundi {
		return snap.SessionID, nil
	}
	if c.captureStore == nil {
		return "child:" + snap.ChildID, nil
	}
	owner := snap.OwnerUserID
	if owner == "" && c.st != nil {
		owner, _ = c.OwnerUserIDForChild(snap.ChildID)
	}
	id, err := c.captureStore.EnsureConversationByExternalRef(ctx, capture.ConversationRef{
		ExternalRef:      snap.ChildID,
		DrivenBy:         string(store.DrivenByClient),
		OriginEntrypoint: snap.Kind,
		OwnerUserID:      owner,
	})
	if err != nil {
		return "", fmt.Errorf("resolve task ledger for child %s: %w", snap.ChildID, err)
	}
	return id, nil
}

// tasksFor returns the task ledger whose mutations publish a TasksChanged
// event on childID's bus. Nil when the daemon has no ledger at all.
//
// The event names childID — the ledger's OWNER — as its child_id, because that
// is what a renderer and the cockpit route on; the conversation id it was
// keyed by never leaves this package.
func (c *Controller) tasksFor(childID string) tasks.Store {
	if c.tasks == nil {
		return nil
	}
	return tasks.Notifying(c.tasks, func(n tasks.Notice) {
		c.publishTasksChanged(childID, n)
	})
}

// publishTasksChanged reports a committed ledger mutation to childID's bus.
// The notice already carries the rows as they read after the store returned,
// so nothing is re-read here.
func (c *Controller) publishTasksChanged(childID string, n tasks.Notice) {
	rows := make([]*rafikiv1.TaskChange, 0, len(n.Tasks))
	for _, t := range n.Tasks {
		rows = append(rows, &rafikiv1.TaskChange{
			Handle:     t.Handle,
			Content:    t.Content,
			ActiveForm: t.ActiveForm,
			Status:     string(t.Status),
			Assignee:   t.Assignee,
			DropReason: t.DropReason,
		})
	}
	c.publishEvent(childID, &rafikiv1.Event{
		ChildId: childID,
		Ts:      timestamppb.Now(),
		Payload: &rafikiv1.Event_TasksChanged{
			TasksChanged: &rafikiv1.TasksChanged{Kind: n.Kind, Tasks: rows},
		},
	})
}
