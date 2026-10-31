// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/multigres/testkit/assert"

	"go.graveland.dev/rafiki/pkg/childstore"
	"go.graveland.dev/rafiki/pkg/childstoredb"
	"go.graveland.dev/rafiki/pkg/connectapi"
	"go.graveland.dev/rafiki/pkg/llm"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/store"
	"go.graveland.dev/rafiki/pkg/tags"
	"go.graveland.dev/rafiki/pkg/tagsdb"
	"go.graveland.dev/rafiki/pkg/users"
	"go.graveland.dev/rafiki/pkg/usersdb"
)

// forkFixture is a DB-backed controller with the stores a fork reads: the
// child store (authority and the parent chain), the daemon's one conversation
// store (the nodes), and the tag store.
type forkFixture struct {
	ctrl  *Controller
	pool  *pgxpool.Pool
	store *childstoredb.Store
	tags  *tagsdb.Store
	owner string
}

func newForkFixture(t *testing.T) *forkFixture {
	t.Helper()
	ck := assert.NewAborting(t)
	pool := scratchPool(t)
	st := childstoredb.New(pool)
	ctrl := newTestController(t)
	ctrl.pool = pool
	ctrl.children = st
	ctrl.forks = st
	ctrl.convs = convStoreFor(t, pool)
	tagStore := tagsdb.New(pool)
	ctrl.tagStore = tagStore

	u, _, err := usersdb.NewPostgresStore(pool).Create(context.Background(),
		users.NewUser{Username: uniqueForkName("owner")})
	ck.NoError(err, "create owner")
	return &forkFixture{ctrl: ctrl, pool: pool, store: st, tags: tagStore, owner: u.ID}
}

// uniqueForkName makes a per-run name: the test database is shared and never
// reset, so a fixed literal would collide with an earlier run's rows.
func uniqueForkName(base string) string {
	return fmt.Sprintf("fk_%s_%d", base, time.Now().UnixNano())
}

func (f *forkFixture) manager() *tagManager { return newTagManager(f.ctrl, f.tags) }

// child writes one durable child row. parent/root are its lineage labels.
func (f *forkFixture) child(t *testing.T, id, parent, root, owner, convID string) {
	t.Helper()
	labels := map[string]string{}
	if parent != "" {
		labels[childstore.LabelParent] = parent
	}
	if root != "" {
		labels[childstore.LabelRoot] = root
	}
	assert.NewAborting(t).NoError(f.store.Upsert(context.Background(), childstore.ChildRecord{
		ChildID:        id,
		OwnerUserID:    owner,
		Kind:           protocol.KindFundi,
		ConversationID: convID,
		Status:         string(protocol.StatusIdle),
		SpawnedAt:      time.Now(),
		Labels:         labels,
	}), "upsert child %s", id)
}

// conversation inserts a conversation row and returns its id.
func (f *forkFixture) conversation(t *testing.T) string {
	t.Helper()
	var id string
	assert.NewAborting(t).NoError(f.pool.QueryRow(context.Background(),
		`INSERT INTO conversations.conversation (origin_entrypoint, driven_by)
		 VALUES ('fork-test', 'server') RETURNING id::text`).Scan(&id), "insert conversation")
	return id
}

// turn appends one completed prompt/reply pair and returns the reply's row.
func (f *forkFixture) turn(t *testing.T, convID string) store.Appended {
	t.Helper()
	appendNode(t, f.pool, convID, anthropic.NewUserMessage(anthropic.NewTextBlock("prompt")), nil)
	return appendNode(t, f.pool, convID, anthropic.NewAssistantMessage(anthropic.NewTextBlock("reply")), nil)
}

// pendingToolCall appends an assistant row whose tool call was never answered.
func (f *forkFixture) pendingToolCall(t *testing.T, convID string) store.Appended {
	t.Helper()
	return appendNode(t, f.pool, convID, anthropic.NewAssistantMessage(
		anthropic.NewToolUseBlock("toolu_pending", map[string]any{"command": "ls"}, "bash")), nil)
}

// forkableChild seeds a child whose conversation holds `turns` completed turns.
func (f *forkFixture) forkableChild(t *testing.T, id, parent, root, owner string, turns int) (string, []store.Appended) {
	t.Helper()
	convID := f.conversation(t)
	nodes := make([]store.Appended, 0, turns)
	for range turns {
		nodes = append(nodes, f.turn(t, convID))
	}
	f.child(t, id, parent, root, owner, convID)
	return convID, nodes
}

// requireExactControllerError asserts the error is a ControllerError with exactly
// this code and message — the strings are caller-visible.
func requireExactControllerError(t *testing.T, err error, code, message string) {
	t.Helper()
	ck := assert.NewAborting(t)
	var ce *connectapi.ControllerError
	if !errors.As(err, &ce) {
		t.Fatalf("want *connectapi.ControllerError, got %T: %v", err, err)
	}
	ck.Eq(code, ce.Code, "code")
	ck.Eq(message, ce.Message, "message")
}

func forkRequest(callerChildID string, from *protocol.ForkFrom, fromTag string) protocol.SpawnRequest {
	return protocol.SpawnRequest{
		Kind:          protocol.KindFundi,
		ParentChildID: callerChildID,
		ForkFrom:      from,
		FromTag:       fromTag,
	}
}

func TestForkFromSelfAllowed(t *testing.T) {
	ck := assert.NewCollecting(t)
	f := newForkFixture(t)
	self := uniqueForkName("self")
	_, nodes := f.forkableChild(t, self, "", self, f.owner, 1)

	node, err := f.ctrl.resolveFork(context.Background(),
		forkRequest(self, &protocol.ForkFrom{ChildID: self}, ""), users.Identity{UserID: f.owner})
	ck.Require().NoError(err, "a child may fork itself")
	ck.Eq(nodes[0].ID, node, "the default cursor is the last completed turn")
}

func TestForkFromDescendantAllowed(t *testing.T) {
	ck := assert.NewCollecting(t)
	f := newForkFixture(t)
	self := uniqueForkName("top")
	desc := uniqueForkName("desc")
	_, nodes := f.forkableChild(t, desc, self, self, f.owner, 1)
	f.child(t, self, "", self, f.owner, "")

	node, err := f.ctrl.resolveFork(context.Background(),
		forkRequest(self, &protocol.ForkFrom{ChildID: desc}, ""), users.Identity{UserID: f.owner})
	ck.Require().NoError(err, "an ancestor may fork its descendant")
	ck.Eq(nodes[0].ID, node, "node")
}

func TestForkFromClosedDescendantAllowed(t *testing.T) {
	ck := assert.NewCollecting(t)
	f := newForkFixture(t)
	ctx := context.Background()
	self := uniqueForkName("top")
	desc := uniqueForkName("closed")
	_, nodes := f.forkableChild(t, desc, self, self, f.owner, 1)
	f.child(t, self, "", self, f.owner, "")
	ck.Require().NoError(f.store.Delete(ctx, desc), "close the descendant")

	node, err := f.ctrl.resolveFork(ctx, forkRequest(self, &protocol.ForkFrom{ChildID: desc}, ""),
		users.Identity{UserID: f.owner})
	ck.Require().NoError(err, "a closed descendant's history outlives it")
	ck.Eq(nodes[0].ID, node, "node")
}

func TestForkFromSiblingRefused(t *testing.T) {
	f := newForkFixture(t)
	parent := uniqueForkName("parent")
	self := uniqueForkName("me")
	sib := uniqueForkName("sib")
	f.forkableChild(t, self, parent, parent, f.owner, 1)
	f.forkableChild(t, sib, parent, parent, f.owner, 1)
	f.child(t, parent, "", parent, f.owner, "")

	_, err := f.ctrl.resolveFork(context.Background(),
		forkRequest(self, &protocol.ForkFrom{ChildID: sib}, ""), users.Identity{UserID: f.owner})
	requireExactControllerError(t, err, protocol.ErrPermissionDenied,
		fmt.Sprintf("agent %s is not visible to you", sib))
}

func TestForkFromParentRefused(t *testing.T) {
	f := newForkFixture(t)
	parent := uniqueForkName("parent")
	self := uniqueForkName("me")
	f.forkableChild(t, parent, "", parent, f.owner, 1)
	f.forkableChild(t, self, parent, parent, f.owner, 1)

	_, err := f.ctrl.resolveFork(context.Background(),
		forkRequest(self, &protocol.ForkFrom{ChildID: parent}, ""), users.Identity{UserID: f.owner})
	requireExactControllerError(t, err, protocol.ErrPermissionDenied,
		fmt.Sprintf("agent %s is not visible to you", parent))
}

func TestForkFromForeignOwnerRefusedLikeUnknown(t *testing.T) {
	ck := assert.NewCollecting(t)
	f := newForkFixture(t)
	ctx := context.Background()
	other, _, err := usersdb.NewPostgresStore(f.pool).Create(ctx,
		users.NewUser{Username: uniqueForkName("other")})
	ck.Require().NoError(err, "create the other owner")

	foreign := uniqueForkName("foreign")
	f.forkableChild(t, foreign, "", foreign, other.ID, 1)
	unknown := uniqueForkName("unknown")

	_, foreignErr := f.ctrl.resolveFork(ctx, forkRequest("", &protocol.ForkFrom{ChildID: foreign}, ""),
		users.Identity{UserID: f.owner})
	_, unknownErr := f.ctrl.resolveFork(ctx, forkRequest("", &protocol.ForkFrom{ChildID: unknown}, ""),
		users.Identity{UserID: f.owner})

	ck.Require().Error(foreignErr, "a foreign child must be refused")
	ck.Require().Error(unknownErr, "an unknown child must be refused")
	var fe, ue *connectapi.ControllerError
	ck.Require().True(errors.As(foreignErr, &fe), "foreign error type")
	ck.Require().True(errors.As(unknownErr, &ue), "unknown error type")
	ck.Eq(protocol.ErrPermissionDenied, fe.Code, "foreign code")
	ck.Eq(fe.Code, ue.Code, "a foreign id and an unknown id answer the same code")
	// The two texts differ only in the id the caller itself supplied, so a
	// caller learns nothing about which children exist: one template answers
	// both, and the id in it is the one it just asked about.
	ck.Eq(fmt.Sprintf("agent %s is not visible to you", foreign), fe.Message, "foreign text")
	ck.Eq(fmt.Sprintf("agent %s is not visible to you", unknown), ue.Message, "unknown text")
}

func TestForkToClaudeRefused(t *testing.T) {
	f := newForkFixture(t)
	req := forkRequest("", &protocol.ForkFrom{ChildID: "whatever"}, "")
	req.Kind = protocol.KindClaude

	_, err := f.ctrl.resolveFork(context.Background(), req, users.Identity{UserID: f.owner})
	requireExactControllerError(t, err, protocol.ErrInvalidArgs,
		"a fork runs as kind=fundi; claude and script children cannot be forked to")
}

func TestForkFromAndFromTagExclusive(t *testing.T) {
	f := newForkFixture(t)

	_, err := f.ctrl.resolveFork(context.Background(),
		forkRequest("", &protocol.ForkFrom{ChildID: "whatever"}, "some-tag"),
		users.Identity{UserID: f.owner})
	requireExactControllerError(t, err, protocol.ErrInvalidArgs,
		"fork_from and from_tag are mutually exclusive")
}

func TestForkDefaultCursorSkipsPendingToolCall(t *testing.T) {
	ck := assert.NewCollecting(t)
	f := newForkFixture(t)
	self := uniqueForkName("pending")
	convID, nodes := f.forkableChild(t, self, "", self, f.owner, 1)
	f.pendingToolCall(t, convID)

	node, err := f.ctrl.resolveFork(context.Background(),
		forkRequest(self, &protocol.ForkFrom{ChildID: self}, ""), users.Identity{UserID: f.owner})
	ck.Require().NoError(err, "the default cursor skips the dangling tool call")
	ck.Eq(nodes[0].ID, node, "the last COMPLETED turn, not the head")
}

func TestForkCursorMustBeACompletedTurn(t *testing.T) {
	ck := assert.NewCollecting(t)
	f := newForkFixture(t)
	ctx := context.Background()
	self := uniqueForkName("cursor")
	convID, nodes := f.forkableChild(t, self, "", self, f.owner, 1)
	pending := f.pendingToolCall(t, convID)

	_, err := f.ctrl.resolveFork(ctx,
		forkRequest(self, &protocol.ForkFrom{ChildID: self, Cursor: fmt.Sprintf("m:%d", pending.Ordinal)}, ""),
		users.Identity{UserID: f.owner})
	requireExactControllerError(t, err, protocol.ErrInvalidArgs,
		fmt.Sprintf("cursor m:%d is not a completed turn (an assistant message with no pending tool call)", pending.Ordinal))

	// The ordinal form is accepted with or without a message subscript, and
	// names the same node either way.
	for _, cursor := range []string{fmt.Sprintf("m:%d", nodes[0].Ordinal), fmt.Sprintf("m:%d.0", nodes[0].Ordinal)} {
		node, err := f.ctrl.resolveFork(ctx,
			forkRequest(self, &protocol.ForkFrom{ChildID: self, Cursor: cursor}, ""),
			users.Identity{UserID: f.owner})
		ck.Require().NoError(err, "cursor %q", cursor)
		ck.Eq(nodes[0].ID, node, "cursor %q names the reply", cursor)
	}

	_, err = f.ctrl.resolveFork(ctx,
		forkRequest(self, &protocol.ForkFrom{ChildID: self, Cursor: "turn-3"}, ""),
		users.Identity{UserID: f.owner})
	requireExactControllerError(t, err, protocol.ErrInvalidArgs,
		`malformed cursor "turn-3" (want m:<ordinal>)`)
}

func TestForkFromClaudeSourceResolvesRootThread(t *testing.T) {
	ck := assert.NewCollecting(t)
	f := newForkFixture(t)
	ctx := context.Background()
	src := uniqueForkName("claude")

	convID := f.conversation(t)
	node := f.turn(t, convID)
	_, err := f.pool.Exec(ctx,
		`UPDATE conversations.conversation SET external_ref = $1 WHERE id = $2::uuid`, src, convID)
	ck.Require().NoError(err, "label the root thread")
	ck.Require().NoError(f.store.Upsert(ctx, childstore.ChildRecord{
		ChildID:   src,
		Kind:      protocol.KindClaude,
		Status:    string(protocol.StatusIdle),
		SpawnedAt: time.Now(),
		Labels:    map[string]string{childstore.LabelRoot: src},
	}), "upsert the claude child")

	got, err := f.ctrl.resolveFork(ctx, forkRequest("", &protocol.ForkFrom{ChildID: src}, ""),
		users.Identity{UserID: f.owner})
	ck.Require().NoError(err, "a claude child's root thread is forkable")
	ck.Eq(node.ID, got, "the root thread's completed turn")
}

// TestForkResumeKeepsTheOriginalNode is design D11: the fork records the
// concrete node at spawn, so moving the tag afterwards never reaches it and a
// resume reopens the conversation the fork already created.
func TestForkResumeKeepsTheOriginalNode(t *testing.T) {
	ck := assert.NewCollecting(t)
	f := newForkFixture(t)
	ctx := context.Background()
	caller := connectapi.TagCaller{Owner: users.Identity{UserID: f.owner}}
	src := uniqueForkName("tagged")
	convID, nodes := f.forkableChild(t, src, "", src, f.owner, 1)

	name := uniqueForkName("point")
	created, err := f.manager().Tag(ctx, caller, connectapi.TagInput{
		Name: name, ChildID: src, Scope: tags.ScopeOwner,
	})
	ck.Require().NoError(err, "create the tag")
	ck.Eq(nodes[0].ID, created.NodeID, "the tag names the completed turn")

	spawned, err := f.ctrl.resolveFork(ctx, forkRequest("", nil, name), users.Identity{UserID: f.owner})
	ck.Require().NoError(err, "fork from the tag")
	ck.Eq(nodes[0].ID, spawned, "the tag resolves to its node")

	// The spawn stores the resolved node on the child's record; that record is
	// the ONLY source a resume reads.
	forkID := uniqueForkName("fork")
	ck.Require().NoError(f.store.Upsert(ctx, childstore.RecordFromSnapshot(childstore.Snapshot{
		ChildID:        forkID,
		OwnerUserID:    f.owner,
		Kind:           protocol.KindFundi,
		ForkFromNodeID: spawned,
		Status:         protocol.StatusIdle,
		StartedAt:      time.Now(),
		Labels:         map[string]string{childstore.LabelRoot: forkID},
	})), "store the fork's record")

	// A second completed turn, then the tag moves onto it.
	later := f.turn(t, convID)
	_, err = f.manager().Tag(ctx, caller, connectapi.TagInput{
		TagID: created.TagID, ExpectNodeID: nodes[0].ID, Cursor: fmt.Sprintf("m:%d", later.Ordinal),
	})
	ck.Require().NoError(err, "move the tag")

	// Resume: rebuild the session from the durable record.
	recs, err := f.store.List(ctx)
	ck.Require().NoError(err, "list children")
	var resumed *childstore.Session
	for _, rec := range recs {
		if rec.ChildID == forkID {
			resumed = childstore.SessionFromRecord(rec)
		}
	}
	ck.Require().True(resumed != nil, "the fork's record")
	ck.Eq(spawned, resumed.ForkFromNodeID, "moving the tag never reaches an existing fork")

	// The record's node is what the engine forks from, and the edge it writes
	// is the ORIGINAL node.
	client, err := llm.NewClient(llm.WithStore(f.pool), llm.WithDefaultModel("anthropic/claude-sonnet-4-5"))
	ck.Require().NoError(err, "llm client")
	forked, err := client.Conversation(ctx, llm.NewConversation(f.owner, "agent"),
		llm.ForkFrom(resumed.ForkFromNodeID))
	ck.Require().NoError(err, "create the forked conversation")
	var edge string
	ck.Require().NoError(f.pool.QueryRow(ctx,
		`SELECT fork_from::text FROM conversations.conversation WHERE id = $1::uuid`,
		forked.ID).Scan(&edge), "read the fork edge")
	ck.Eq(spawned, edge, "the conversation forks from the node resolved at spawn")
}

// mustListTags lists the caller's tags through the manager.
func mustListTags(t *testing.T, f *forkFixture, prefix string) []tags.Tag {
	t.Helper()
	rows, err := f.manager().ListTags(context.Background(),
		connectapi.TagCaller{Owner: users.Identity{UserID: f.owner}}, prefix, 500, "")
	assert.NewAborting(t).NoError(err, "list tags")
	return rows
}

// TestSpawnRuntimeCarriesTheResolvedForkNode pins the one hop between the
// resolved node and the engine: the runtime options carry it, which is what
// fundi.Config turns into llm.ForkFrom.
func TestSpawnRuntimeCarriesTheResolvedForkNode(t *testing.T) {
	ck := assert.NewCollecting(t)
	f := newForkFixture(t)
	req := protocol.SpawnRequest{Kind: protocol.KindFundi, Model: "anthropic/claude-sonnet-4-5"}

	ro, err := f.ctrl.agentRuntimeOptionsFor(req, "c_fork_runtime", false, "", f.owner, f.owner, "node-1")
	ck.Require().NoError(err, "runtime options")
	ck.Eq("node-1", ro.ForkFromNodeID, "the runtime carries the node the spawn resolved")

	plain, err := f.ctrl.agentRuntimeOptionsFor(req, "c_fork_runtime", false, "", f.owner, f.owner, "")
	ck.Require().NoError(err, "runtime options without a fork")
	ck.Eq("", plain.ForkFromNodeID, "a spawn with no fork carries none")
}
