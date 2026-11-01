// SPDX-License-Identifier: Apache-2.0

package tagsdb_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/multigres/testkit/assert"

	"go.graveland.dev/rafiki/pkg/store"
	"go.graveland.dev/rafiki/pkg/tags"
	"go.graveland.dev/rafiki/pkg/tagsdb"
	"go.graveland.dev/rafiki/pkg/users"
	"go.graveland.dev/rafiki/pkg/usersdb"
)

// testPool migrates the shared test database and hands back a pool. Every row
// a test writes is created under a per-run unique id and deleted by its
// fixture's t.Cleanup: the database is shared and never reset, so nothing here
// may assert on a global row count or search for a fixed literal.
func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	c := assert.NewAborting(t)
	dsn := os.Getenv("RAFIKI_TEST_DSN")
	if dsn == "" {
		c.Eq("", os.Getenv("RAFIKI_REQUIRE_DB"), "RAFIKI_TEST_DSN not set but RAFIKI_REQUIRE_DB is")
		t.Skip("RAFIKI_TEST_DSN not set; skipping DB-backed tag tests")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	c.NoError(err, "connect")
	c.NoError(store.Migrate(ctx, pool), "migrate")
	t.Cleanup(pool.Close)
	return pool
}

// fixture owns the rows one test writes and removes them afterwards.
type fixture struct {
	t      *testing.T
	s      *tagsdb.Store
	pool   *pgxpool.Pool
	tagIDs []string
	convs  []string
	owners []string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	pool := testPool(t)
	f := &fixture{t: t, s: tagsdb.New(pool), pool: pool}
	t.Cleanup(f.cleanup)
	return f
}

func (f *fixture) cleanup() {
	ctx := context.Background()
	if len(f.tagIDs) > 0 {
		_, _ = f.pool.Exec(ctx, `DELETE FROM conversations.agent_tag WHERE tag_id = ANY($1::uuid[])`, f.tagIDs)
	}
	for _, conv := range f.convs {
		_, _ = f.pool.Exec(ctx, `DELETE FROM conversations.conversation_message WHERE conversation_id = $1::uuid`, conv)
		_, _ = f.pool.Exec(ctx, `DELETE FROM conversations.conversation WHERE id = $1::uuid`, conv)
	}
	for _, owner := range f.owners {
		_, _ = f.pool.Exec(ctx, `DELETE FROM conversations.users WHERE id = $1::uuid`, owner)
	}
}

// unique makes a name unique to this run.
func unique(base string) string {
	return fmt.Sprintf("%s-%d", base, time.Now().UnixNano())
}

func (f *fixture) owner(base string) string {
	t := assert.NewAborting(f.t)
	u, _, err := usersdb.NewPostgresStore(f.pool).Create(context.Background(), users.NewUser{Username: unique(base)})
	t.NoError(err, "create user")
	f.owners = append(f.owners, u.ID)
	return u.ID
}

func (f *fixture) conv() string {
	t := assert.NewAborting(f.t)
	var id string
	err := f.pool.QueryRow(context.Background(),
		`INSERT INTO conversations.conversation (origin_entrypoint, driven_by)
		 VALUES ('test','server') RETURNING id::text`).Scan(&id)
	t.NoError(err, "insert conversation")
	f.convs = append(f.convs, id)
	return id
}

func (f *fixture) node(convID string, ordinal int) string {
	t := assert.NewAborting(f.t)
	var id string
	err := f.pool.QueryRow(context.Background(),
		`INSERT INTO conversations.conversation_message (conversation_id, ordinal, role, content)
		 VALUES ($1::uuid, $2, 'user', '[]'::jsonb) RETURNING id::text`, convID, ordinal).Scan(&id)
	t.NoError(err, "insert message")
	return id
}

// tag builds a lineage tag on convID/nodeID owned by owner.
func (f *fixture) tag(owner, name, scopeRoot, convID, nodeID string) tags.Tag {
	return tags.Tag{
		OwnerUserID:    owner,
		Name:           name,
		Scope:          tags.ScopeLineage,
		ScopeRoot:      scopeRoot,
		SourceChildID:  scopeRoot,
		ConversationID: convID,
		NodeID:         nodeID,
		CreatorChildID: scopeRoot,
		ActorChildID:   scopeRoot,
		Note:           "note for " + name,
	}
}

func (f *fixture) create(tag tags.Tag, replace bool) tags.Tag {
	f.t.Helper()
	got, err := f.s.Create(context.Background(), tag, replace)
	assert.NewAborting(f.t).NoError(err, "create %s", tag.Name)
	f.tagIDs = append(f.tagIDs, got.TagID)
	return got
}

func TestCreateAndGet(t *testing.T) {
	c := assert.NewCollecting(t)
	f := newFixture(t)
	owner, convID := f.owner("tag-create"), f.conv()
	nodeID := f.node(convID, 7)

	want := f.tag(owner, unique("fix"), "root-child", convID, nodeID)
	got := f.create(want, false)

	c.Eq(want.Name, got.Name, "name")
	c.Eq(tags.ScopeLineage, got.Scope, "scope")
	c.Eq(want.ScopeRoot, got.ScopeRoot, "scope root")
	c.Eq(convID, got.ConversationID, "conversation id")
	c.Eq(nodeID, got.NodeID, "node id")
	c.Eq(7, got.Ordinal, "ordinal is joined from the node's message row")
	c.Eq(want.CreatorChildID, got.CreatorChildID, "creator")
	c.Eq(want.ActorChildID, got.ActorChildID, "actor")
	c.Eq(want.Note, got.Note, "note")
	c.NotZero(got.TagID, "create mints a tag id")
	c.False(got.CreatedAt.IsZero(), "created at is set")
	c.Nil(got.SupersededAt, "a fresh tag is current")
	c.Nil(got.DeletedAt, "a fresh tag is live")

	read, ok, err := f.s.Get(context.Background(), owner, got.TagID)
	c.NoError(err, "get")
	c.True(ok, "the tag is live")
	c.Eq(got.TagID, read.TagID, "get returns the same tag")
	c.Eq(nodeID, read.NodeID, "get returns the same node")
	c.Eq(7, read.Ordinal, "get joins the ordinal too")
}

func TestNameTaken(t *testing.T) {
	c := assert.NewCollecting(t)
	f := newFixture(t)
	owner, convID := f.owner("tag-taken"), f.conv()
	name := unique("dup")

	first := f.tag(owner, name, "root-a", convID, f.node(convID, 0))
	f.create(first, false)

	second := f.tag(owner, name, "root-a", convID, f.node(convID, 1))
	_, err := f.s.Create(context.Background(), second, false)
	c.ErrorIs(err, tagsdb.ErrNameTaken, "a second live tag with the same name in the same scope is refused")
}

func TestReplace(t *testing.T) {
	c := assert.NewCollecting(t)
	f := newFixture(t)
	owner, convID := f.owner("tag-replace"), f.conv()
	name := unique("replace")
	oldNode, newNode := f.node(convID, 0), f.node(convID, 1)

	first := f.create(f.tag(owner, name, "root-a", convID, oldNode), false)
	second := f.create(f.tag(owner, name, "root-a", convID, newNode), true)

	c.NotEq(first.TagID, second.TagID, "a replace is a new tag")
	c.Eq(newNode, second.NodeID, "the replacement points at the new node")

	live, err := f.s.ByName(context.Background(), owner, name)
	c.NoError(err, "by name")
	c.Len(live, 1, "the replaced row is no longer live")
	c.Eq(second.TagID, live[0].TagID, "the live holder is the replacement")

	_, ok, err := f.s.Get(context.Background(), owner, first.TagID)
	c.NoError(err, "get replaced")
	c.False(ok, "the replaced tag is no longer live")

	history, err := f.s.History(context.Background(), owner, first.TagID)
	c.NoError(err, "history of the replaced tag")
	c.Len(history, 1, "the replaced row survives as a tombstone")
	c.NotNil(history[0].DeletedAt, "the replace tombstones rather than deletes")
}

func TestMoveHappyPath(t *testing.T) {
	c := assert.NewCollecting(t)
	f := newFixture(t)
	owner, convID := f.owner("tag-move"), f.conv()
	from, to := f.node(convID, 0), f.node(convID, 1)
	created := f.create(f.tag(owner, unique("move"), "root-a", convID, from), false)

	moved, err := f.s.Move(context.Background(), owner, created.TagID, from, tags.Tag{
		SourceChildID:  "other-child",
		ConversationID: convID,
		NodeID:         to,
		ActorChildID:   "mover",
	})
	c.NoError(err, "move")
	c.Eq(created.TagID, moved.TagID, "a move keeps the tag id")
	c.Eq(created.Name, moved.Name, "a move keeps the name")
	c.Eq(created.Scope, moved.Scope, "a move keeps the scope")
	c.Eq(created.ScopeRoot, moved.ScopeRoot, "a move keeps the scope root")
	c.Eq(created.CreatorChildID, moved.CreatorChildID, "a move keeps the creator")
	c.Eq(created.Note, moved.Note, "a move keeps the note")
	c.Eq("other-child", moved.SourceChildID, "a move takes the new source child")
	c.Eq(to, moved.NodeID, "a move takes the new node")
	c.Eq(1, moved.Ordinal, "a move joins the new node's ordinal")
	c.Eq("mover", moved.ActorChildID, "a move records the actor")
	c.Nil(moved.SupersededAt, "the new row is current")

	read, ok, err := f.s.Get(context.Background(), owner, created.TagID)
	c.NoError(err, "get after move")
	c.True(ok, "the tag is still live")
	c.Eq(to, read.NodeID, "get sees the moved node")

	history, err := f.s.History(context.Background(), owner, created.TagID)
	c.NoError(err, "history")
	c.Len(history, 2, "a move appends a row")
	c.Eq(from, history[0].NodeID, "history is oldest first")
	c.NotNil(history[0].SupersededAt, "the old row is superseded")
	c.Eq(to, history[1].NodeID, "the new row is last")
	c.Nil(history[1].SupersededAt, "the new row is current")
}

func TestMoveWithStaleExpectedNodeFails(t *testing.T) {
	c := assert.NewCollecting(t)
	f := newFixture(t)
	owner, convID := f.owner("tag-stale"), f.conv()
	from, to := f.node(convID, 0), f.node(convID, 1)
	created := f.create(f.tag(owner, unique("stale"), "root-a", convID, from), false)

	_, err := f.s.Move(context.Background(), owner, created.TagID, from, tags.Tag{
		ConversationID: convID, NodeID: to,
	})
	c.NoError(err, "the first move is from the expected node")

	_, err = f.s.Move(context.Background(), owner, created.TagID, from, tags.Tag{
		ConversationID: convID, NodeID: from,
	})
	moved := c.Require().ErrorAs[tagsdb.ErrMoved](err, "a move from a stale expected node is refused")
	c.Eq(to, moved.Current, "the refusal names where the tag points now")

	read, _, err := f.s.Get(context.Background(), owner, created.TagID)
	c.NoError(err, "get")
	c.Eq(to, read.NodeID, "a refused move changed nothing")
}

func TestConcurrentMovesOneWins(t *testing.T) {
	c := assert.NewCollecting(t)
	f := newFixture(t)
	owner, convID := f.owner("tag-race"), f.conv()
	from := f.node(convID, 0)
	targets := []string{f.node(convID, 1), f.node(convID, 2)}
	created := f.create(f.tag(owner, unique("race"), "root-a", convID, from), false)

	errs := make([]error, len(targets))
	var wg sync.WaitGroup
	for i, to := range targets {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = f.s.Move(context.Background(), owner, created.TagID, from, tags.Tag{
				ConversationID: convID, NodeID: to,
			})
		}()
	}
	wg.Wait()

	wins, moved := 0, 0
	for _, err := range errs {
		if err == nil {
			wins++
			continue
		}
		var m tagsdb.ErrMoved
		c.True(errors.As(err, &m), "the loser is ErrMoved, got %v", err)
		moved++
	}
	c.Eq(1, wins, "exactly one move wins")
	c.Eq(1, moved, "exactly one move loses to the stale node")

	read, _, err := f.s.Get(context.Background(), owner, created.TagID)
	c.NoError(err, "get")
	c.Contains(targets, read.NodeID, "the tag points at the winning target")
	history, err := f.s.History(context.Background(), owner, created.TagID)
	c.NoError(err, "history")
	c.Len(history, 2, "only the winner appended a row")
}

func TestUntagHidesFromByNameAndList(t *testing.T) {
	c := assert.NewCollecting(t)
	f := newFixture(t)
	owner, convID := f.owner("tag-untag"), f.conv()
	name := unique("untag")
	created := f.create(f.tag(owner, name, "root-a", convID, f.node(convID, 0)), false)

	c.NoError(f.s.Untag(context.Background(), owner, created.TagID, "untagger"), "untag")

	live, err := f.s.ByName(context.Background(), owner, name)
	c.NoError(err, "by name")
	c.Len(live, 0, "an untagged tag is not live")

	list, err := f.s.List(context.Background(), owner, name)
	c.NoError(err, "list")
	c.Len(list, 0, "an untagged tag is not listed")

	_, ok, err := f.s.Get(context.Background(), owner, created.TagID)
	c.NoError(err, "get")
	c.False(ok, "an untagged tag is not live")

	history, err := f.s.History(context.Background(), owner, created.TagID)
	c.NoError(err, "history")
	c.Len(history, 1, "the untag tombstones rather than deletes")
	c.NotNil(history[0].DeletedAt, "the row carries its tombstone")
	c.Eq("untagger", history[0].ActorChildID, "the untag records the actor")

	err = f.s.Untag(context.Background(), owner, created.TagID, "")
	c.ErrorIs(err, tagsdb.ErrNotFound, "untagging twice is ErrNotFound")
}

func TestHistoryKeepsEveryRow(t *testing.T) {
	c := assert.NewCollecting(t)
	f := newFixture(t)
	owner, convID := f.owner("tag-history"), f.conv()
	nodes := []string{f.node(convID, 0), f.node(convID, 1), f.node(convID, 2)}
	created := f.create(f.tag(owner, unique("history"), "root-a", convID, nodes[0]), false)

	prev := nodes[0]
	for _, to := range nodes[1:] {
		_, err := f.s.Move(context.Background(), owner, created.TagID, prev, tags.Tag{
			ConversationID: convID, NodeID: to,
		})
		c.Require().NoError(err, "move to %s", to)
		prev = to
	}
	c.Require().NoError(f.s.Untag(context.Background(), owner, created.TagID, "untagger"), "untag")

	history, err := f.s.History(context.Background(), owner, created.TagID)
	c.NoError(err, "history")
	c.Len(history, 3, "create and two moves append three rows; an untag tombstones the last in place")
	for i, row := range history {
		c.Eq(nodes[i], row.NodeID, "row %d is version %d", i, i)
		c.Eq(created.TagID, row.TagID, "every row shares the tag id")
		if i < 2 {
			c.NotNil(row.SupersededAt, "row %d is superseded by the move after it", i)
		}
	}
	c.Nil(history[2].SupersededAt, "the untag tombstones the current row without superseding it")
	c.NotNil(history[2].DeletedAt, "the last row is the untag tombstone")
}

func TestForeignOwnerSeesNothing(t *testing.T) {
	c := assert.NewCollecting(t)
	f := newFixture(t)
	owner, other := f.owner("tag-owner"), f.owner("tag-other")
	convID := f.conv()
	name := unique("foreign")
	created := f.create(f.tag(owner, name, "root-a", convID, f.node(convID, 0)), false)
	ctx := context.Background()

	_, ok, err := f.s.Get(ctx, other, created.TagID)
	c.NoError(err, "foreign get")
	c.False(ok, "a foreign owner cannot read the tag")

	live, err := f.s.ByName(ctx, other, name)
	c.NoError(err, "foreign by name")
	c.Len(live, 0, "a foreign owner finds no tag by name")

	list, err := f.s.List(ctx, other, "")
	c.NoError(err, "foreign list")
	c.Len(list, 0, "a foreign owner lists nothing")

	history, err := f.s.History(ctx, other, created.TagID)
	c.NoError(err, "foreign history")
	c.Len(history, 0, "a foreign owner sees no history")

	_, err = f.s.Move(ctx, other, created.TagID, created.NodeID, tags.Tag{
		ConversationID: convID, NodeID: created.NodeID,
	})
	c.ErrorIs(err, tagsdb.ErrNotFound, "a foreign owner cannot move the tag")

	err = f.s.Untag(ctx, other, created.TagID, "")
	c.ErrorIs(err, tagsdb.ErrNotFound, "a foreign owner cannot untag the tag")

	read, ok, err := f.s.Get(ctx, owner, created.TagID)
	c.NoError(err, "owner get")
	c.True(ok, "the tag is untouched")
	c.Nil(read.DeletedAt, "the tag is still live")
}

func TestSameNameDifferentScopeCoexists(t *testing.T) {
	c := assert.NewCollecting(t)
	f := newFixture(t)
	owner, convID := f.owner("tag-scopes"), f.conv()
	name := unique("shared")
	lineage := f.create(f.tag(owner, name, "root-a", convID, f.node(convID, 0)), false)
	tree := f.tag(owner, name, "root-a", convID, f.node(convID, 1))
	tree.Scope = tags.ScopeTree
	treeTag := f.create(tree, false)

	live, err := f.s.ByName(context.Background(), owner, name)
	c.NoError(err, "by name")
	c.Len(live, 2, "the same name may exist in two scopes")
	c.ElementsMatch([]string{lineage.TagID, treeTag.TagID}, []string{live[0].TagID, live[1].TagID}, "both tags are live")
}

func TestListPrefixAndOrder(t *testing.T) {
	c := assert.NewCollecting(t)
	f := newFixture(t)
	owner, convID := f.owner("tag-list"), f.conv()
	prefix := unique("list")
	for i, suffix := range []string{"b", "a", "c"} {
		f.create(f.tag(owner, prefix+"-"+suffix, "root-a", convID, f.node(convID, i)), false)
	}

	got, err := f.s.List(context.Background(), owner, prefix)
	c.NoError(err, "list by prefix")
	c.Len(got, 3, "every tag with the prefix is listed")
	c.Eq(prefix+"-a", got[0].Name, "list is in name order")
	c.Eq(prefix+"-b", got[1].Name, "list is in name order")
	c.Eq(prefix+"-c", got[2].Name, "list is in name order")

	none, err := f.s.List(context.Background(), owner, unique("absent"))
	c.NoError(err, "list with no match")
	c.NotNil(none, "List never returns nil")
	c.Len(none, 0, "no tag carries an absent prefix")
}

func TestUnknownTagIsNotFound(t *testing.T) {
	c := assert.NewCollecting(t)
	f := newFixture(t)
	owner := f.owner("tag-unknown")
	ctx := context.Background()

	_, ok, err := f.s.Get(ctx, owner, "00000000-0000-7000-8000-000000000000")
	c.NoError(err, "get unknown")
	c.False(ok, "an unknown tag is not live")

	_, ok, err = f.s.Get(ctx, owner, "not-a-uuid")
	c.NoError(err, "get a malformed id")
	c.False(ok, "a malformed id answers not-found rather than an error")

	err = f.s.Untag(ctx, owner, "00000000-0000-7000-8000-000000000000", "")
	c.ErrorIs(err, tagsdb.ErrNotFound, "untag unknown")

	_, err = f.s.Move(ctx, owner, "00000000-0000-7000-8000-000000000000", "", tags.Tag{})
	c.ErrorIs(err, tagsdb.ErrNotFound, "move unknown")
}

// A tag with no owner stores owner_user_id NULL, and "" is the only spelling
// of that owner on this side of the store. NULL-owner tags are reachable — a
// unix-socket or admin caller creates them — so both halves of the SQL have to
// treat "" as ONE value that is not any real owner's: the
// IS NOT DISTINCT FROM matching (via the NULLIF-normalized owner parameter),
// and agent_tag_live_name's `NULLS NOT DISTINCT` uniqueness.
func TestNullOwnerTagIsItsOwnNamespace(t *testing.T) {
	c := assert.NewCollecting(t)
	f := newFixture(t)
	owner, convID := f.owner("tag-null"), f.conv()
	ctx := context.Background()
	name := unique("nullowner")

	created := f.create(f.tag("", name, "root-a", convID, f.node(convID, 0)), false)
	c.Eq("", created.OwnerUserID, "a tag created with no owner reads back as no owner")

	// (a) "" finds it; a real owner does not.
	read, ok, err := f.s.Get(ctx, "", created.TagID)
	c.NoError(err, "get as the NULL owner")
	c.True(ok, "a NULL-owner tag is live for the NULL owner")
	c.Eq(created.TagID, read.TagID, "get returns the same tag")

	_, ok, err = f.s.Get(ctx, owner, created.TagID)
	c.NoError(err, "get as a real owner")
	c.False(ok, "a real owner cannot read a NULL-owner tag by id")

	live, err := f.s.ByName(ctx, "", name)
	c.NoError(err, "by name as the NULL owner")
	c.Len(live, 1, "the NULL owner finds its own tag by name")
	c.Eq(created.TagID, live[0].TagID, "the same tag")

	live, err = f.s.ByName(ctx, owner, name)
	c.NoError(err, "by name as a real owner")
	c.Len(live, 0, "a real owner does not find the NULL-owner tag")

	// (b) IS NOT DISTINCT FROM: a real owner's row of the same name is not
	// returned for "", and the NULL-owner row is not returned for it.
	owned := f.create(f.tag(owner, name, "root-a", convID, f.node(convID, 1)), false)

	live, err = f.s.ByName(ctx, "", name)
	c.NoError(err, "by name as the NULL owner, after a real owner's tag")
	c.Len(live, 1, "the real owner's row is not returned for the NULL owner")
	c.Eq(created.TagID, live[0].TagID, "still the NULL owner's own tag")

	live, err = f.s.ByName(ctx, owner, name)
	c.NoError(err, "by name as the real owner")
	c.Len(live, 1, "the NULL-owner row is not returned for a real owner")
	c.Eq(owned.TagID, live[0].TagID, "the real owner's own tag")

	_, ok, err = f.s.Get(ctx, "", owned.TagID)
	c.NoError(err, "get the real owner's tag as the NULL owner")
	c.False(ok, "the NULL owner cannot read a real owner's tag")

	// (c) NULLS NOT DISTINCT: "" collides with "" on
	// (scope, scope_root, name), while the real owner's identical name
	// coexists — a NULL owner is one namespace, not an unbounded one.
	_, err = f.s.Create(ctx, f.tag("", name, "root-a", convID, f.node(convID, 2)), false)
	c.ErrorIs(err, tagsdb.ErrNameTaken, "a second live NULL-owner tag with the same name in the same scope is refused")

	live, err = f.s.ByName(ctx, "", name)
	c.NoError(err, "by name after the refused create")
	c.Len(live, 1, "the refused create left one live NULL-owner tag")
	c.Eq(created.TagID, live[0].TagID, "the original holder")
}
