// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"go.graveland.dev/rafiki/pkg/childstore"
	"go.graveland.dev/rafiki/pkg/fundi/tools"
	"go.graveland.dev/rafiki/pkg/presets"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/recall"
	"go.graveland.dev/rafiki/pkg/server"

	"github.com/multigres/testkit/assert"
)

// The wave-1 tests for the MCP face's per-child bindings (review-0 F1): a
// per-child token must never reach its owner's conversation corpus, memory
// namespace, preset namespace or pymodule bucket. The bindings are unit-tested
// here; the tool-listing consequences are pinned in mcp_face_test.go.

// --- authoring refusals -----------------------------------------------------

// TestChildPresetBindingRefusesAuthoring pins the child caller's preset
// binding: put/delete refuse by rule, whatever the store behind them, while
// the read verbs delegate untouched (they are the same read-only facts
// Connect's anyCaller grants a child credential).
func TestChildPresetBindingRefusesAuthoring(t *testing.T) {
	c := assert.NewAborting(t)
	// A nil store behind the binding: the refusal must fire before the store
	// is ever reached, so "presets unavailable" must not win.
	b := childPresetBinding{newPresetBinding(&Controller{}, "u-owner", "")}

	if _, err := b.Put(context.Background(), presets.Spec{}); !errors.Is(err, errPresetChildAuthoring) {
		t.Fatalf("Put = %v, want errPresetChildAuthoring", err)
	}
	c.ErrorIs(b.Delete(context.Background(), "reviewer"), errPresetChildAuthoring, "Delete")

	// The read verbs still delegate to the store — here a store whose
	// embedded nil panics if reached; reads must never be refused by the
	// wrapper, so route them through a recording fake instead.
	reads := &recordingPresetStore{}
	ctrl2 := &Controller{presetStore: reads}
	b2 := childPresetBinding{newPresetBinding(ctrl2, "u-owner", "")}
	if _, err := b2.List(context.Background(), ""); err != nil {
		t.Fatalf("List = %v, want delegated", err)
	}
	if _, err := b2.Get(context.Background(), "reviewer"); err != nil {
		t.Fatalf("Get = %v, want delegated", err)
	}
	_, err := b2.History(context.Background(), "reviewer")
	c.NoError(err, "History")
	c.Eq("u-owner", reads.owner, "reads resolved owner")
}

// recordingPresetStore satisfies presets.Store for the delegation assertions.
type recordingPresetStore struct {
	owner string
}

func (s *recordingPresetStore) Put(_ context.Context, owner string, _ presets.Record) (presets.Record, error) {
	s.owner = owner
	return presets.Record{}, nil
}

func (s *recordingPresetStore) Get(_ context.Context, owner, _ string) (presets.Record, error) {
	s.owner = owner
	return presets.Record{}, nil
}

func (s *recordingPresetStore) List(_ context.Context, owner, _ string) ([]presets.Record, error) {
	s.owner = owner
	return nil, nil
}

func (s *recordingPresetStore) History(_ context.Context, owner, _ string) ([]presets.Record, error) {
	s.owner = owner
	return nil, nil
}

func (s *recordingPresetStore) Delete(_ context.Context, owner, _ string) error {
	s.owner = owner
	return nil
}

// lineageStore holds a top-level child and one it spawned, for the preset
// authoring rule: the operator's own session authors, an agent's child reads.
func lineageStore() *childstore.Store {
	st := childstore.New()
	st.Insert(&childstore.Session{
		ChildID: "c_root", Status: protocol.StatusIdle,
		Kind: protocol.KindClaude, StartedAt: time.Now(),
	})
	st.Insert(&childstore.Session{
		ChildID: "c_kid", Status: protocol.StatusIdle,
		Kind: protocol.KindClaude, StartedAt: time.Now(),
		Labels: map[string]string{childstore.LabelParent: "c_root", childstore.LabelRoot: "c_root"},
	})
	return st
}

// TestPresetStoreForChildAuthorsOnlyAtTopLevel pins the child caller's preset
// rule: a top-level child authors under its owner, stamped as the writer; a
// parented child and an unknown id get the read-only childPresetBinding.
func TestPresetStoreForChildAuthorsOnlyAtTopLevel(t *testing.T) {
	c := assert.NewAborting(t)
	ctx := context.Background()
	ctrl := &Controller{st: lineageStore(), presetStore: newFakePresetStore("u-owner")}

	rec, err := ctrl.presetStoreForChild("u-owner", "c_root").Put(ctx, presets.Spec{Name: "p"})
	c.NoError(err, "top-level Put")
	c.Eq("c_root", rec.WrittenByChild, "WrittenByChild")
	c.NoError(ctrl.presetStoreForChild("u-owner", "c_root").Delete(ctx, "p"), "top-level Delete")

	for _, id := range []string{"c_kid", "c_gone", ""} {
		_, err := ctrl.presetStoreForChild("u-owner", id).Put(ctx, presets.Spec{Name: "p"})
		c.ErrorIs(err, errPresetChildAuthoring, "Put as %q", id)
		c.ErrorIs(ctrl.presetStoreForChild("u-owner", id).Delete(ctx, "p"), errPresetChildAuthoring, "Delete as %q", id)
	}
}

// TestChildRecallIdentityNeverAdmin pins the one recall identity rule: a
// per-child credential resolves to its OWNER's non-admin identity
// (recallOwner) — IsAdmin survives only for a real user credential — so a
// child of an admin is bound to that admin's own rows, never Scope{All: true}.
func TestChildRecallIdentityNeverAdmin(t *testing.T) {
	ck := assert.NewAborting(t)

	user := recallOwner(&server.Identity{UserID: "u-owner", IsAdmin: true, Via: server.ProvenanceUser})
	ck.True(user.IsAdmin, "a real user credential keeps its admin bit")

	child := &server.Identity{UserID: "u-owner", IsAdmin: true, Via: server.ProvenanceChildToken, ChildID: "c-1"}
	owner := recallOwner(child)
	ck.False(owner.IsAdmin, "a child credential must never be admin, even of an admin owner")
	ck.Eq("u-owner", owner.UserID, "child resolves to its owner's user id")

	c := &Controller{recall: &recallRuntime{st: &fakeRecallStore{}}}
	b := newRecallBinding(c, recallOwner(child)).(*recallBinding)
	ck.EqDeep(recall.Scope{OwnerUserID: "u-owner"}, b.scope, "child binding scope must be owner-scoped, never All")
	ck.Eq("u-owner", b.owner, "child binding memory owner")
}

// --- subtree conversation scope ---------------------------------------------

// TestMCPChildConversationReaderScopeIsSubtree is the database proof of the
// child caller's conversation boundary: the reader resolves the caller's
// subtree — its own conversation by session id, a claude descendant's by
// external_ref — and its owner's other conversations never appear, in search
// or export.
func TestMCPChildConversationReaderScopeIsSubtree(t *testing.T) {
	c := assert.NewAborting(t)
	pool := openTestPool(t)
	ctx := context.Background()
	uniq := fmt.Sprintf("c_%d", time.Now().UnixNano())

	own := insertConvForCost(t, pool, "")          // fundi caller: session-id route
	desc := insertConvForCost(t, pool, uniq+"kid") // claude descendant: ref route
	outside := insertConvForCost(t, pool, uniq+"sib")
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM conversations.conversation_turn WHERE conversation_id = ANY($1::uuid[])`,
			[]string{own, desc, outside})
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM conversations.conversation WHERE id = ANY($1::uuid[])`,
			[]string{own, desc, outside})
	})
	insertTurnForCost(t, pool, own, "test/model", 1000, 100)
	insertTurnForCost(t, pool, desc, "test/model", 1000, 100)
	insertTurnForCost(t, pool, outside, "test/model", 1000, 100)

	st := childstore.New()
	dir := t.TempDir()
	ctrl := NewController(st, filepath.Join(dir, "state"), filepath.Join(dir, "logs"),
		filepath.Join(dir, "c.sock"), nil, pool, nil, false, ctx, nil, nil, nil, nil)
	caller := uniq + "caller"
	st.Insert(&childstore.Session{
		ChildID: caller, SessionID: own, Status: protocol.StatusIdle,
		Kind: protocol.KindFundi, StartedAt: time.Now(),
	})
	st.Insert(&childstore.Session{
		ChildID: uniq + "kid", Status: protocol.StatusIdle,
		Kind: protocol.KindClaude, StartedAt: time.Now(),
		Labels: map[string]string{childstore.LabelParent: caller, childstore.LabelRoot: caller},
	})
	st.Insert(&childstore.Session{
		ChildID: uniq + "sib", Status: protocol.StatusIdle,
		Kind: protocol.KindClaude, StartedAt: time.Now(),
		Labels: map[string]string{childstore.LabelParent: uniq + "root", childstore.LabelRoot: uniq + "root"},
	})

	r := newMCPChildConversationReader(ctrl, caller)

	rows, err := r.ConversationSearch(ctx, tools.ConversationQuery{Limit: 50})
	c.NoError(err, "search")
	got := map[string]bool{}
	for _, row := range rows {
		got[row.ID] = true
	}
	c.False(!got[own] || !got[desc], "search = %v, want the subtree rows %s and %s", got, own, desc)
	c.False(got[outside], "search returned the sibling conversation %s: the owner's corpus leaked", outside)

	// Export answers not-found for the sibling — the scope-miss rule, never a
	// permission error that confirms existence.
	if _, err := r.ConversationExport(ctx, outside); err == nil {
		t.Fatal("export of an out-of-subtree conversation succeeded")
	}
	if _, err := r.ConversationExport(ctx, own); err != nil {
		t.Fatalf("export of own conversation: %v", err)
	}

	// The catalogue runs under the same scope; the models rollup counts only
	// the subtree's conversations.
	res, err := r.RunQuery(ctx, "models", tools.CatalogueFilter{})
	c.NoError(err, "query")
	convs := 0
	for _, row := range res.Rows {
		if len(row) == 3 && row[1].IsInt {
			convs += int(row[1].Int)
		}
	}
	c.Eq(2, convs, "models conversations")
}

// TestMCPFaceChildConversationsBindingIsSubtree pins the getServer DECISION,
// not just the reader: a per-child caller's conversation_search runs through
// newMCPChildConversationReader. Reverting the `if isChild` branch to the
// owner reader (newMCPConversationReader, ScopeOwner) makes the sibling row
// reappear here, because this drives the tool through the SDK session
// getServer builds — the same path a real child's RAFIKI_MCP_TOKEN drives.
func TestMCPFaceChildConversationsBindingIsSubtree(t *testing.T) {
	c := assert.NewAborting(t)
	pool := openTestPool(t)
	ctx := context.Background()
	uniq := fmt.Sprintf("c_%d", time.Now().UnixNano())

	own := insertConvForCost(t, pool, "")             // fundi caller: session-id route
	desc := insertConvForCost(t, pool, uniq+"kid")    // claude descendant: ref route
	sibling := insertConvForCost(t, pool, uniq+"sib") // the owner's OTHER child
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM conversations.conversation WHERE id = ANY($1::uuid[])`,
			[]string{own, desc, sibling})
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM conversations.users WHERE username = 'mcp-child-scope-owner' AND deleted_at IS NULL`)
	})
	// Own all three rows by the owner user, so this test is a true leak
	// oracle: reverting the binding to the owner reader (ScopeOwner) surfaces
	// the sibling here, it does not merely return nothing.
	var ownerID string
	c.NoError(pool.QueryRow(ctx,
		`INSERT INTO conversations.users (username, token_sha256)
		 VALUES ('mcp-child-scope-owner', 'mcp-child-scope-owner:' || gen_random_uuid()::text)
		 RETURNING id::text`).Scan(&ownerID), "insert owner user")
	if _, err := pool.Exec(ctx,
		`UPDATE conversations.conversation SET owner_user_id = $1::uuid
		  WHERE id = ANY($2::uuid[])`, ownerID, []string{own, desc, sibling}); err != nil {
		t.Fatalf("own conversations: %v", err)
	}
	st := childstore.New()
	dir := t.TempDir()
	ctrl := NewController(st, filepath.Join(dir, "state"), filepath.Join(dir, "logs"),
		filepath.Join(dir, "c.sock"), nil, pool, nil, false, ctx, nil, nil, nil, nil)
	caller := uniq + "caller"
	st.Insert(&childstore.Session{
		ChildID: caller, SessionID: own, Status: protocol.StatusIdle,
		Kind: protocol.KindFundi, StartedAt: time.Now(),
	})
	st.Insert(&childstore.Session{
		ChildID: uniq + "kid", Status: protocol.StatusIdle,
		Kind: protocol.KindClaude, StartedAt: time.Now(),
		Labels: map[string]string{childstore.LabelParent: caller, childstore.LabelRoot: caller},
	})
	st.Insert(&childstore.Session{
		ChildID: uniq + "sib", Status: protocol.StatusIdle,
		Kind: protocol.KindClaude, StartedAt: time.Now(),
		Labels: map[string]string{childstore.LabelParent: uniq + "root", childstore.LabelRoot: uniq + "root"},
	})

	face := newMCPFace(discardLogger(), nil, nil, "test")
	face.SetController(ctrl)
	child := httptest.NewRequest(http.MethodPost, mcpFacePath, nil)
	child = child.WithContext(server.WithIdentity(child.Context(), &server.Identity{
		UserID: "u-owner", ChildID: caller, Via: server.ProvenanceChildToken,
	}))
	cs := mcpConnect(t, face.getServer(child))
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "conversation_search"})
	c.NoError(err, "conversation_search")
	// The tool renders rows by conversation id, so the ids are the oracle:
	// both in-subtree rows surface, the sibling's never does.
	text := mcpCallText(t, res)
	c.StrContains(text, desc, "conversation_search did not surface the in-subtree descendant")
	c.StrContains(text, own, "conversation_search did not surface the caller's own conversation")
	c.NotStrContains(text, sibling, "conversation_search leaked the owner's other child's conversation")
}
