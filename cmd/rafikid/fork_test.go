// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/multigres/testkit/assert"

	"go.graveland.dev/rafiki/pkg/childstore"
	"go.graveland.dev/rafiki/pkg/childstoredb"
	"go.graveland.dev/rafiki/pkg/connectapi"
	"go.graveland.dev/rafiki/pkg/conv"
	"go.graveland.dev/rafiki/pkg/msg"
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

// appendBoundary appends one compaction boundary under convID and returns its
// committed row. A boundary is appended as a ROOT of that kind, exactly as a
// real compaction writes it: Append takes a root's Kind from Append.Root and
// drops a NewNode.Kind that lands as a root, so the root form is the one that
// actually records a compaction summary (pkg/conv/append.go). DerivedFromHead
// names the row the summary stands in for, which is empty on a fresh
// conversation and is then simply no derived-from.
func appendBoundary(t *testing.T, pool *pgxpool.Pool, convID string, kind string) store.Appended {
	t.Helper()
	ck := assert.NewAborting(t)
	res, err := convStoreFor(t, pool).Open(convID, conv.AsOwner()).Append(context.Background(), conv.Append{
		Root:            kind,
		DerivedFromHead: true,
		Nodes: []conv.NewNode{{
			Msg:  msg.Message{Role: msg.User, Content: []msg.Block{msg.Text{Text: "summary"}}},
			Kind: kind,
		}},
	})
	ck.NoError(err, "append boundary")
	n := res.Nodes[len(res.Nodes)-1]
	return store.Appended{ID: n.ID, CreatedAt: n.CreatedAt, Ordinal: n.Ordinal, Depth: n.Depth}
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

	node, _, err := f.ctrl.resolveFork(context.Background(),
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

	node, _, err := f.ctrl.resolveFork(context.Background(),
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

	node, _, err := f.ctrl.resolveFork(ctx, forkRequest(self, &protocol.ForkFrom{ChildID: desc}, ""),
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

	_, _, err := f.ctrl.resolveFork(context.Background(),
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

	_, _, err := f.ctrl.resolveFork(context.Background(),
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

	_, _, foreignErr := f.ctrl.resolveFork(ctx, forkRequest("", &protocol.ForkFrom{ChildID: foreign}, ""),
		users.Identity{UserID: f.owner})
	_, _, unknownErr := f.ctrl.resolveFork(ctx, forkRequest("", &protocol.ForkFrom{ChildID: unknown}, ""),
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

	_, _, err := f.ctrl.resolveFork(context.Background(), req, users.Identity{UserID: f.owner})
	requireExactControllerError(t, err, protocol.ErrInvalidArgs,
		"a fork runs as kind=fundi; claude and script children cannot be forked to")
}

func TestForkFromAndFromTagExclusive(t *testing.T) {
	f := newForkFixture(t)

	_, _, err := f.ctrl.resolveFork(context.Background(),
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

	node, _, err := f.ctrl.resolveFork(context.Background(),
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

	_, _, err := f.ctrl.resolveFork(ctx,
		forkRequest(self, &protocol.ForkFrom{ChildID: self, Cursor: fmt.Sprintf("m:%d", pending.Ordinal)}, ""),
		users.Identity{UserID: f.owner})
	requireExactControllerError(t, err, protocol.ErrInvalidArgs,
		fmt.Sprintf("cursor m:%d is not a completed turn (an assistant message with no pending tool call, or a compaction boundary)", pending.Ordinal))

	// The ordinal form is accepted with or without a message subscript, and
	// names the same node either way.
	for _, cursor := range []string{fmt.Sprintf("m:%d", nodes[0].Ordinal), fmt.Sprintf("m:%d.0", nodes[0].Ordinal)} {
		node, _, err := f.ctrl.resolveFork(ctx,
			forkRequest(self, &protocol.ForkFrom{ChildID: self, Cursor: cursor}, ""),
			users.Identity{UserID: f.owner})
		ck.Require().NoError(err, "cursor %q", cursor)
		ck.Eq(nodes[0].ID, node, "cursor %q names the reply", cursor)
	}

	_, _, err = f.ctrl.resolveFork(ctx,
		forkRequest(self, &protocol.ForkFrom{ChildID: self, Cursor: "turn-3"}, ""),
		users.Identity{UserID: f.owner})
	requireExactControllerError(t, err, protocol.ErrInvalidArgs,
		`malformed cursor "turn-3" (want m:<ordinal>)`)
}

// TestForkFromCompactionBoundaryAllowed pins the relaxation a compacted
// conversation needs: a compaction summary root is a legal fork point, so a
// conversation whose only node is a boundary still has a node to fork from.
// Without it the empty cursor answers "no completed turn to fork from", because
// the summary is a user-role node and isCompletedTurn required an assistant one.
func TestForkFromCompactionBoundaryAllowed(t *testing.T) {
	ck := assert.NewCollecting(t)
	f := newForkFixture(t)
	self := uniqueForkName("compacted")
	convID := f.conversation(t)
	boundary := appendBoundary(t, f.pool, convID, conv.KindCompactionSummary)
	f.child(t, self, "", self, f.owner, convID)

	node, _, err := f.ctrl.resolveFork(context.Background(),
		forkRequest(self, &protocol.ForkFrom{ChildID: self}, ""), users.Identity{UserID: f.owner})
	ck.Require().NoError(err, "a compaction boundary is a completed turn")
	ck.Eq(boundary.ID, node, "the default cursor is the boundary")
}

// TestForkFromCompactionTailPrefersTheAssistantTurn pins the clause's width: a
// verbatim tail after the boundary still wins the empty cursor, so the summary
// never outranks a real completed turn.
func TestForkFromCompactionTailPrefersTheAssistantTurn(t *testing.T) {
	ck := assert.NewCollecting(t)
	f := newForkFixture(t)
	self := uniqueForkName("tail")
	convID := f.conversation(t)
	boundary := appendBoundary(t, f.pool, convID, conv.KindCompactionSummary)
	reply := appendNode(t, f.pool, convID,
		anthropic.NewAssistantMessage(anthropic.NewTextBlock("reply")), nil)
	f.child(t, self, "", self, f.owner, convID)

	node, _, err := f.ctrl.resolveFork(context.Background(),
		forkRequest(self, &protocol.ForkFrom{ChildID: self}, ""), users.Identity{UserID: f.owner})
	ck.Require().NoError(err, "the verbatim tail is forkable")
	ck.Eq(reply.ID, node, "the last completed turn, not the boundary")
	ck.NotEq(boundary.ID, node, "the boundary must not outrank the tail")
}

// TestForkFromCompactionLeavesPlainUserMessagesUnforkable keeps the relaxation
// exactly as wide as documented: a user-role node that is NOT a compaction
// boundary is still no fork point, and the existing refusal still answers.
func TestForkFromCompactionLeavesPlainUserMessagesUnforkable(t *testing.T) {
	f := newForkFixture(t)
	self := uniqueForkName("plain")
	convID := f.conversation(t)
	appendNode(t, f.pool, convID, anthropic.NewUserMessage(anthropic.NewTextBlock("prompt")), nil)
	f.child(t, self, "", self, f.owner, convID)

	_, _, err := f.ctrl.resolveFork(context.Background(),
		forkRequest(self, &protocol.ForkFrom{ChildID: self}, ""), users.Identity{UserID: f.owner})
	requireExactControllerError(t, err, protocol.ErrFailedPrecondition,
		"agent has no completed turn to fork from")
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

	got, _, err := f.ctrl.resolveFork(ctx, forkRequest("", &protocol.ForkFrom{ChildID: src}, ""),
		users.Identity{UserID: f.owner})
	ck.Require().NoError(err, "a claude child's root thread is forkable")
	ck.Eq(node.ID, got, "the root thread's completed turn")
}

// forkTestModel is a provider-qualified model providers.Default() knows, so a
// real fundi spawn can build an engine without an API key or a network.
const forkTestModel = "anthropic/claude-sonnet-4-5"

// TestForkResumeKeepsTheOriginalNode is design D11, driven through the real
// entry points: Spawn resolves the tag to a concrete node and records it, a
// later move of the tag never reaches that fork, and Resume reopens the
// conversation the fork already created with its edge unchanged.
func TestForkResumeKeepsTheOriginalNode(t *testing.T) {
	ck := assert.NewAborting(t)
	f := newForkFixture(t)
	ctx := t.Context()
	caller := tagCaller(f.owner, "")

	src := uniqueForkName("tagged")
	srcConv, nodes := f.forkableChild(t, src, "", src, f.owner, 1)
	name := uniqueForkName("point")
	tag, err := f.manager().Tag(ctx, caller, connectapi.TagInput{
		Name: name, ChildID: src, Scope: tags.ScopeOwner,
	})
	ck.Require().NoError(err, "create the tag")
	ck.Eq(nodes[0].ID, tag.NodeID, "the tag names the completed turn")

	res, err := f.ctrl.Spawn(ctx, protocol.SpawnRequest{
		Kind:    protocol.KindFundi,
		Cwd:     t.TempDir(),
		Model:   forkTestModel,
		FromTag: name,
		Name:    uniqueForkName("fork"),
	}, users.Identity{UserID: f.owner})
	ck.Require().NoError(err, "spawn from the tag")

	convID, edge := waitForChildConversation(t, f, res.ChildID)
	ck.Eq(nodes[0].ID, edge, "the new child's conversation forks from the node the tag named")
	ck.Eq(nodes[0].ID, mustSnapshot(t, f.ctrl, res.ChildID).ForkFromNodeID,
		"Spawn did not store the resolved node on the child's session")

	// The tag moves on. A fork records the CONCRETE node, so the move must not
	// reach the child that already resolved it.
	later := f.turn(t, srcConv)
	_, err = f.manager().Tag(ctx, caller, connectapi.TagInput{
		TagID: tag.TagID, ExpectNodeID: nodes[0].ID, Cursor: cursorAt(later),
	})
	ck.Require().NoError(err, "move the tag")

	killCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err = f.ctrl.Kill(killCtx, res.ChildID, 2*time.Second, 500*time.Millisecond)
	ck.Require().NoError(err, "kill")
	waitForExited(t, f.ctrl.st, res.ChildID, 5*time.Second)

	ck.Eq(nodes[0].ID, mustSnapshot(t, f.ctrl, res.ChildID).ForkFromNodeID,
		"the durable record lost the fork node")

	_, err = f.ctrl.Resume(ctx, res.ChildID, "")
	ck.Require().NoError(err, "resume")

	afterConv, afterEdge := waitForChildConversation(t, f, res.ChildID)
	ck.Eq(convID, afterConv, "a resume reopens the conversation the fork created")
	ck.Eq(nodes[0].ID, afterEdge, "a resume re-resolved the fork: the edge followed the moved tag")
	ck.Eq(nodes[0].ID, mustSnapshot(t, f.ctrl, res.ChildID).ForkFromNodeID,
		"the resumed session lost the fork node")
}

// TestResumedForkChildStillForksFromItsStoredNode is the other half of the
// resume contract: a child whose conversation does not exist yet (a recovered
// row) must CREATE it as a fork of the node the record carries. This is what
// fails if agentRunner stops reading the node off the snapshot, or the record
// stops carrying it.
func TestResumedForkChildStillForksFromItsStoredNode(t *testing.T) {
	ck := assert.NewAborting(t)
	f := newForkFixture(t)
	ctx := t.Context()

	src := uniqueForkName("stored")
	_, nodes := f.forkableChild(t, src, "", src, f.owner, 1)

	childID := uniqueForkName("recovered")
	rec := childstore.RecordFromSnapshot(childstore.Snapshot{
		ChildID:        childID,
		OwnerUserID:    f.owner,
		Kind:           protocol.KindFundi,
		Cwd:            t.TempDir(),
		Model:          forkTestModel,
		ForkFromNodeID: nodes[0].ID,
		Status:         protocol.StatusExited,
		StartedAt:      time.Now(),
		Labels:         map[string]string{childstore.LabelRoot: childID},
	})
	ck.Require().NoError(f.store.Upsert(ctx, rec), "seed the record")
	// Read it back the way boot recovery does: the durable row, decoded through
	// SessionFromRecord, so the config JSONB is in the loop too.
	f.ctrl.st.Insert(sessionFromStore(t, f, childID))

	_, err := f.ctrl.Resume(ctx, childID, "")
	ck.Require().NoError(err, "resume the recovered child")

	_, edge := waitForChildConversation(t, f, childID)
	ck.Eq(nodes[0].ID, edge, "the resumed engine created its conversation unforked: the stored node was dropped")
}

// sessionFromStore reads a child back out of the durable store.
func sessionFromStore(t *testing.T, f *forkFixture, childID string) *childstore.Session {
	t.Helper()
	recs, err := f.store.List(context.Background())
	assert.NewAborting(t).NoError(err, "list children")
	for _, rec := range recs {
		if rec.ChildID == childID {
			return childstore.SessionFromRecord(rec)
		}
	}
	t.Fatalf("no durable row for %s", childID)
	return nil
}

// waitForChildConversation polls for the conversation the child's engine
// created (external_ref = child id) and returns its id and fork edge.
func waitForChildConversation(t *testing.T, f *forkFixture, childID string) (string, string) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		var (
			id   string
			edge *string
		)
		err := f.pool.QueryRow(context.Background(),
			`SELECT id::text, fork_from::text FROM conversations.conversation WHERE external_ref = $1`,
			childID).Scan(&id, &edge)
		switch {
		case err == nil:
			if edge == nil {
				return id, ""
			}
			return id, *edge
		case errors.Is(err, pgx.ErrNoRows):
			if time.Now().After(deadline) {
				t.Fatalf("child %s never created a conversation", childID)
			}
			time.Sleep(50 * time.Millisecond)
		default:
			t.Fatalf("read the conversation of %s: %v", childID, err)
		}
	}
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

// liveSpawner registers id in the controller's in-memory child store as a
// spawner beneath parent. Spawn reads the caller's lineage, owner and limits
// from there, not from the durable rows the fixture seeds.
func (f *forkFixture) liveSpawner(t *testing.T, id, parent string) {
	t.Helper()
	f.ctrl.st.Insert(&childstore.Session{
		ChildID:     id,
		OwnerUserID: f.owner,
		Kind:        protocol.KindFundi,
		Status:      protocol.StatusIdle,
		MaxDepth:    1,
		MaxChildren: 2,
		Labels: map[string]string{
			childstore.LabelParent: parent,
			childstore.LabelRoot:   parent,
		},
	})
	t.Cleanup(func() { f.ctrl.st.Delete(id) })
}

// A spawn whose fork is REFUSED must leave nothing behind. resolveFork runs
// before the name claim, the childID mint and the initial row, so a refusal
// starts no process and holds no name: the sibling's fork attempt answers
// not-visible and a later spawn of the same name is admitted. This is the
// ordering pin — an initial row written before the fork resolved would occupy
// the name, and the retry would answer "taken".
func TestRefusedForkLeavesNoChildRowOrName(t *testing.T) {
	ck := assert.NewAborting(t)
	f := newForkFixture(t)
	ctx := t.Context()
	parent, self, sib := uniqueForkName("parent"), uniqueForkName("me"), uniqueForkName("sib")
	f.forkableChild(t, parent, "", parent, f.owner, 1)
	f.forkableChild(t, self, parent, parent, f.owner, 1)
	f.forkableChild(t, sib, parent, parent, f.owner, 1)
	f.liveSpawner(t, self, parent)

	name := uniqueForkName("contested")
	req := protocol.SpawnRequest{
		Kind:          protocol.KindFundi,
		Cwd:           t.TempDir(),
		Model:         forkTestModel,
		ParentChildID: self,
		ForkFrom:      &protocol.ForkFrom{ChildID: sib},
		Name:          name,
	}
	_, err := f.ctrl.Spawn(ctx, req, users.Identity{UserID: f.owner})
	requireExactControllerError(t, err, protocol.ErrPermissionDenied,
		fmt.Sprintf("agent %s is not visible to you", sib))

	recs, err := f.store.List(ctx)
	ck.Require().NoError(err, "list the child rows")
	for _, rec := range recs {
		ck.False(rec.OwnerUserID == f.owner && rec.Name == name,
			"the refused fork wrote a child row named %q (child %s)", name, rec.ChildID)
	}
	holder, _, taken, err := f.store.NameHolder(ctx, f.owner, self, name)
	ck.Require().NoError(err, "name holder")
	ck.False(taken, "the refused fork claimed the name (held by %s)", holder)

	// The name is still free: an ordinary spawn under the same parent takes it.
	req.ForkFrom = nil
	res, err := f.ctrl.Spawn(ctx, req, users.Identity{UserID: f.owner})
	ck.Require().NoError(err, "a later spawn with the same name")
	ck.Eq(name, mustSnapshot(t, f.ctrl, res.ChildID).Name, "the retry took the name")
}

// TestSpawnForkStampsForkedFromLabel pins the provenance label through a real
// Controller.Spawn: a forked child's durable row carries rafiki/forked-from =
// the source child id — the arm `rafiki get` and the list table read, while
// the conversation's immutable edge holds the node. Both entry points are
// pinned, fork_from and from_tag, because they learn the source differently.
func TestSpawnForkStampsForkedFromLabel(t *testing.T) {
	ck := assert.NewCollecting(t)
	f := newForkFixture(t)
	owner := users.Identity{UserID: f.owner}
	ctx := context.Background()

	src := spawnAliasChild(t, f.ctrl, owner, protocol.SpawnRequest{
		Name:  uniqueForkName("src"),
		Model: "openrouter/~deepseek/deepseek-flash-latest",
	})
	srcConv, ok, err := f.store.ForkSource(ctx, src.ChildID)
	ck.Require().NoError(err, "read the source child row")
	ck.Require().True(ok, "the spawned child row exists")
	ck.NotEq("", srcConv.ConversationID, "a spawned fundi child has a conversation")
	appendNode(t, f.pool, srcConv.ConversationID, anthropic.NewUserMessage(anthropic.NewTextBlock("prompt")), nil)
	appendNode(t, f.pool, srcConv.ConversationID, anthropic.NewAssistantMessage(anthropic.NewTextBlock("reply")), nil)

	forked := spawnAliasChild(t, f.ctrl, owner, protocol.SpawnRequest{
		Name:     uniqueForkName("forked"),
		Model:    "openrouter/~deepseek/deepseek-flash-latest",
		ForkFrom: &protocol.ForkFrom{ChildID: src.ChildID},
	})
	snap := mustSnapshot(t, f.ctrl, forked.ChildID)
	ck.Eq(src.ChildID, snap.Labels["rafiki/forked-from"], "rafiki/forked-from names the source child")

	// The from_tag path learns the source from the winning tag, not from the
	// request: the label must still name it.
	tagName := uniqueForkName("labeltag")
	created := f.createTag(t, tagCaller(owner.UserID, src.ChildID), connectapi.TagInput{
		Name: tagName, Scope: tags.ScopeOwner, ChildID: src.ChildID,
	})

	tagged := spawnAliasChild(t, f.ctrl, owner, protocol.SpawnRequest{
		Name:    uniqueForkName("bytag"),
		Model:   "openrouter/~deepseek/deepseek-flash-latest",
		FromTag: tagName,
	})
	snap = mustSnapshot(t, f.ctrl, tagged.ChildID)
	ck.Eq(created.NodeID != "", true, "the tag names a node")
	ck.Eq(src.ChildID, snap.Labels["rafiki/forked-from"], "a from_tag fork labels the tag's child too")

	// The label is spawn-only metadata: a resume copies the stored row's
	// labels and re-stamps only cwd/pid/kind, so the provenance survives.
	killCtx, killCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer killCancel()
	_, err = f.ctrl.Kill(killCtx, forked.ChildID, 2*time.Second, 500*time.Millisecond)
	ck.Require().NoError(err, "stop the forked child")
	waitForExited(t, f.ctrl.st, forked.ChildID, 5*time.Second)
	rtCtx, rtCancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer rtCancel()
	_, err = f.ctrl.Resume(rtCtx, forked.ChildID, "")
	ck.Require().NoError(err, "resume the forked child")
	ck.Eq(src.ChildID, mustSnapshot(t, f.ctrl, forked.ChildID).Labels["rafiki/forked-from"],
		"resume must keep the original spawn's rafiki/forked-from")
}
