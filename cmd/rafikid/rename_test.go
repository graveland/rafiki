// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"testing"
	"time"

	"go.graveland.dev/rafiki/pkg/childstore"
	"go.graveland.dev/rafiki/pkg/childstoredb"
	"go.graveland.dev/rafiki/pkg/protocol"

	"github.com/multigres/testkit/assert"
)

// TestNameConversationsRenamesEveryLinkedConversation pins the same three
// linkage arms stampConversationsClosed uses; D is the look-alike an
// unescaped LIKE would rename.
func TestNameConversationsRenamesEveryLinkedConversation(t *testing.T) {
	ck := assert.NewCollecting(t)
	pool := scratchPool(t)
	ctx := context.Background()

	insertConversation := func(externalRef string) string {
		t.Helper()
		var id string
		ck.Require().NoError(pool.QueryRow(ctx,
			`INSERT INTO conversations.conversation (origin_entrypoint, driven_by, external_ref, name)
			 VALUES ('test','server', NULLIF($1,''), 'old') RETURNING id::text`, externalRef).Scan(&id), "insert conversation %q", externalRef)
		return id
	}
	convA := insertConversation("")
	convB := insertConversation("c_1")
	convC := insertConversation("c_1:t1")
	convD := insertConversation("cX1:t1")
	convE := insertConversation("unrelated")

	ck.Require().NoError(childstoredb.New(pool).Upsert(ctx, childstore.ChildRecord{
		ChildID:        "c_1",
		ConversationID: convA,
		Kind:           protocol.KindFundi,
		Status:         string(protocol.StatusExited),
		SpawnedAt:      time.Now(),
	}), "upsert child")

	c := &Controller{pool: pool}
	ck.Require().NoError(c.nameConversations(ctx, "c_1", "shiny"), "nameConversations")

	nameOf := func(id string) string {
		t.Helper()
		var n string
		ck.Require().NoError(pool.QueryRow(ctx,
			`SELECT coalesce(name,'') FROM conversations.conversation WHERE id = $1::uuid`, id).Scan(&n), "read name for %s", id)
		return n
	}
	for _, id := range []string{convA, convB, convC} {
		ck.Eq("shiny", nameOf(id), "linked conversation %s", id)
	}
	for _, id := range []string{convD, convE} {
		ck.Eq("old", nameOf(id), "unlinked conversation %s", id)
	}
}

func TestRenameUnregisteredChildIsNotFound(t *testing.T) {
	c := &Controller{st: childstore.New()}
	err := c.Rename(context.Background(), "c_missing", "x")
	assert.NewAborting(t).NotNil(err, "rename of unknown child")
}

func TestRenamePublishesChildRenamedAndUpdatesTheStore(t *testing.T) {
	ck := assert.NewCollecting(t)
	c := displayServiceController(t)
	insertDisplayChild(c.st, "c_1", "", "", protocol.StatusIdle)
	ch, cancel := c.native.Subscribe("c_1")
	defer cancel()

	ck.Require().NoError(c.Rename(context.Background(), "c_1", "shiny"), "Rename")

	snap, _ := c.st.Get("c_1")
	ck.Eq("shiny", snap.Name, "store name")
	var got string
	for done := false; !done; {
		select {
		case ev := <-ch:
			if r := ev.GetChildRenamed(); r != nil {
				got = r.GetName()
			}
		default:
			done = true
		}
	}
	ck.Eq("shiny", got, "child_renamed event name")
}
