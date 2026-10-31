// SPDX-License-Identifier: Apache-2.0

package childstoredb

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/multigres/testkit/assert"

	"go.graveland.dev/rafiki/pkg/childstore"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/users"
	"go.graveland.dev/rafiki/pkg/usersdb"
)

func forkChildID(base string) string {
	return fmt.Sprintf("c_fork_%s_%d", base, time.Now().UnixNano())
}

// insertRefConversation writes a conversation whose external_ref is ref (NULL
// when ref is empty) and returns its id.
func insertRefConversation(t *testing.T, pool *pgxpool.Pool, ref string) string {
	t.Helper()
	var externalRef any
	if ref != "" {
		externalRef = ref
	}
	var id string
	err := pool.QueryRow(context.Background(),
		`INSERT INTO conversations.conversation (origin_entrypoint, driven_by, external_ref)
		 VALUES ('test','client',$1) RETURNING id::text`, externalRef).Scan(&id)
	assert.NewAborting(t).NoError(err, "insert conversation %q", ref)
	return id
}

func insertForkOwner(t *testing.T, pool *pgxpool.Pool, username string) string {
	t.Helper()
	u, _, err := usersdb.NewPostgresStore(pool).Create(context.Background(), users.NewUser{Username: username})
	assert.NewAborting(t).NoError(err, "create user")
	return u.ID
}

func upsertForkChild(t *testing.T, s *Store, rec childstore.ChildRecord) {
	t.Helper()
	if rec.Status == "" {
		rec.Status = string(protocol.StatusIdle)
	}
	if rec.SpawnedAt.IsZero() {
		rec.SpawnedAt = time.Now()
	}
	assert.NewAborting(t).NoError(s.Upsert(context.Background(), rec), "upsert %s", rec.ChildID)
}

func TestForkSourceFundi(t *testing.T) {
	c := assert.NewCollecting(t)
	pool := scratchPool(t)
	s := New(pool)
	ctx := context.Background()

	owner := insertForkOwner(t, pool, "fork-fundi")
	convID := insertRefConversation(t, pool, "")
	childID := forkChildID("fundi")
	upsertForkChild(t, s, childstore.ChildRecord{
		ChildID:        childID,
		ConversationID: convID,
		OwnerUserID:    owner,
		Kind:           protocol.KindFundi,
		Labels: map[string]string{
			childstore.LabelParent: "parent-child",
			childstore.LabelRoot:   "root-child",
		},
	})

	got, ok, err := s.ForkSource(ctx, childID)
	c.NoError(err, "fork source")
	c.True(ok, "a known child answers")
	c.Eq(childID, got.ChildID, "child id")
	c.Eq(owner, got.OwnerUserID, "owner")
	c.Eq(protocol.KindFundi, got.Kind, "kind")
	c.Eq(convID, got.ConversationID, "a fundi child's own conversation")
	c.Eq("parent-child", got.ParentID, "parent label")
	c.Eq("root-child", got.RootID, "root label")
	c.False(got.Closed, "a live child is not closed")
}

func TestForkSourceClaudeByExternalRef(t *testing.T) {
	c := assert.NewCollecting(t)
	pool := scratchPool(t)
	s := New(pool)
	ctx := context.Background()

	childID := forkChildID("claude")
	// The thread conversation is created FIRST, so a naive "first conversation
	// mentioning this child" would pick it. It must not: the bare child id is
	// the root thread.
	threadConv := insertRefConversation(t, pool, childID+":thread")
	rootConv := insertRefConversation(t, pool, childID)
	upsertForkChild(t, s, childstore.ChildRecord{
		ChildID: childID,
		Kind:    protocol.KindClaude,
		Labels:  map[string]string{childstore.LabelRoot: "root-child"},
	})

	got, ok, err := s.ForkSource(ctx, childID)
	c.NoError(err, "fork source")
	c.True(ok, "a claude child answers")
	c.Eq(protocol.KindClaude, got.Kind, "kind")
	c.Eq(rootConv, got.ConversationID, "a claude child resolves its root thread by external_ref")
	c.NotEq(threadConv, got.ConversationID, "a thread conversation is not the child's conversation")
	c.Eq("root-child", got.RootID, "root label")
	c.Eq("", got.ParentID, "no parent label")

	// A child with only a thread conversation resolves no conversation: the
	// bare id is the root thread and nothing else is.
	onlyThreadID := forkChildID("claude-thread-only")
	insertRefConversation(t, pool, onlyThreadID+":thread")
	upsertForkChild(t, s, childstore.ChildRecord{ChildID: onlyThreadID, Kind: protocol.KindClaude})

	got, ok, err = s.ForkSource(ctx, onlyThreadID)
	c.NoError(err, "fork source thread only")
	c.True(ok, "the child answers")
	c.Eq("", got.ConversationID, "a thread-only child has no conversation id")
}

func TestForkSourceClosedChild(t *testing.T) {
	c := assert.NewCollecting(t)
	pool := scratchPool(t)
	s := New(pool)
	ctx := context.Background()

	convID := insertRefConversation(t, pool, "")
	childID := forkChildID("closed")
	upsertForkChild(t, s, childstore.ChildRecord{
		ChildID:        childID,
		ConversationID: convID,
		Kind:           protocol.KindFundi,
	})
	assert.NewAborting(t).NoError(s.Delete(ctx, childID), "close the child")

	got, ok, err := s.ForkSource(ctx, childID)
	c.NoError(err, "fork source")
	c.True(ok, "a closed child still answers")
	c.True(got.Closed, "the row is closed")
	c.Eq(convID, got.ConversationID, "a closed child keeps its conversation")
}

func TestForkSourceMissing(t *testing.T) {
	c := assert.NewCollecting(t)
	pool := scratchPool(t)
	s := New(pool)

	got, ok, err := s.ForkSource(context.Background(), forkChildID("missing"))
	c.NoError(err, "an unknown child is not an error")
	c.False(ok, "an unknown child answers ok=false")
	c.EqDeep(childstore.ForkSource{}, got, "an unknown child answers the zero value")
}

func TestForkSourceNonObjectLabelsIsError(t *testing.T) {
	c := assert.NewCollecting(t)
	pool := scratchPool(t)
	s := New(pool)
	ctx := context.Background()

	childID := forkChildID("badlabels")
	upsertForkChild(t, s, childstore.ChildRecord{ChildID: childID, Kind: protocol.KindFundi})
	_, err := pool.Exec(ctx, `UPDATE conversations.child SET labels = '[]'::jsonb WHERE child_id = $1`, childID)
	assert.NewAborting(t).NoError(err, "corrupt labels")

	_, ok, err := s.ForkSource(ctx, childID)
	c.False(ok, "a row whose labels are not an object does not answer")
	c.Error(err, "labels that are not a JSON object are an error")
}
