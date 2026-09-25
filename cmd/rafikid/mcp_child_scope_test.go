// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"go.graveland.dev/rafiki/pkg/childstore"
	"go.graveland.dev/rafiki/pkg/fundi/tools"
	"go.graveland.dev/rafiki/pkg/presets"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/server"
	"go.graveland.dev/rafiki/pkg/users"
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
	// A nil store behind the binding: the refusal must fire before the store
	// is ever reached, so "presets unavailable" must not win.
	b := childPresetBinding{newPresetBinding(&Controller{}, "u-owner", "")}

	if _, err := b.Put(context.Background(), presets.Spec{}); !errors.Is(err, errPresetChildAuthoring) {
		t.Fatalf("Put = %v, want errPresetChildAuthoring", err)
	}
	if err := b.Delete(context.Background(), "reviewer"); !errors.Is(err, errPresetChildAuthoring) {
		t.Fatalf("Delete = %v, want errPresetChildAuthoring", err)
	}

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
	if _, err := b2.History(context.Background(), "reviewer"); err != nil {
		t.Fatalf("History = %v, want delegated", err)
	}
	if reads.owner != "u-owner" {
		t.Fatalf("reads resolved owner %q, want u-owner (unchanged binding)", reads.owner)
	}
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

// TestChildPyModuleStoreRefusesAuthoring pins the child caller's pymodule
// store: put/delete refuse by rule; get delegates (the corpus it reads is
// exactly the one the child's own executor binding can run).
func TestChildPyModuleStoreRefusesAuthoring(t *testing.T) {
	ctrl := &Controller{}
	w := &childPyModuleStore{newMCPPyModuleStore(ctrl, users.Identity{UserID: "u-owner"})}

	if _, _, err := w.Put(context.Background(), "local", "m", "code", ""); !errors.Is(err, errPyModuleChildAuthoring) {
		t.Fatalf("Put = %v, want errPyModuleChildAuthoring", err)
	}
	if _, err := w.Delete(context.Background(), "local", "m"); !errors.Is(err, errPyModuleChildAuthoring) {
		t.Fatalf("Delete = %v, want errPyModuleChildAuthoring", err)
	}

	// Get delegates to the store. A nil pymoduleStore would panic, so prove
	// the delegation with a stubbed Controller is impossible — instead prove
	// the child store IS the wrapped store by construction: the same owner id
	// rides through (a non-nil ctrl with a real store is DB territory, covered
	// by the store's own tests).
	if w.ownerUserID != "u-owner" {
		t.Fatalf("wrapped store owner = %q, want u-owner", w.ownerUserID)
	}
	var _ tools.PyModuleStore = w
}

// TestRecallBindingRefusesChildCaller pins the recall block's child rule:
// every owner dimension of that binding is the OWNER's (scope and MemoryOwner
// are user ids), and with no per-child namespace to bind instead the child
// gets nil — the blueprints' decline — never an owner-scoped binding.
func TestRecallBindingRefusesChildCaller(t *testing.T) {
	c := &Controller{recall: &recallRuntime{st: &fakeRecallStore{}}}
	if rb := newRecallBinding(c, users.Identity{UserID: "u-owner"}, true); rb != nil {
		t.Fatal("child caller bound a recall binding, want nil")
	}
	// The user caller keeps its binding (owner == the caller).
	if rb := newRecallBinding(c, users.Identity{UserID: "u-owner"}, false); rb == nil {
		t.Fatal("user caller lost its recall binding")
	}
}

// --- subtree conversation scope ---------------------------------------------

// TestMCPChildConversationReaderScopeIsSubtree is the database proof of the
// child caller's conversation boundary: the reader resolves the caller's
// subtree — its own conversation by session id, a claude descendant's by
// external_ref — and its owner's other conversations never appear, in search
// or export.
func TestMCPChildConversationReaderScopeIsSubtree(t *testing.T) {
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
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	got := map[string]bool{}
	for _, row := range rows {
		got[row.ID] = true
	}
	if !got[own] || !got[desc] {
		t.Fatalf("search = %v, want the subtree rows %s and %s", got, own, desc)
	}
	if got[outside] {
		t.Fatalf("search returned the sibling conversation %s: the owner's corpus leaked", outside)
	}

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
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	convs := 0
	for _, row := range res.Rows {
		if len(row) == 3 && row[1].IsInt {
			convs += int(row[1].Int)
		}
	}
	if convs != 2 {
		t.Fatalf("models conversations = %d, want 2 (the subtree, never the sibling)", convs)
	}
}

// TestMCPFaceChildConversationsBindingIsSubtree pins the getServer DECISION,
// not just the reader: a per-child caller's conversation_search runs through
// newMCPChildConversationReader. Reverting the `if isChild` branch to the
// owner reader (newMCPConversationReader, ScopeOwner) makes the sibling row
// reappear here, because this drives the tool through the SDK session
// getServer builds — the same path a real child's RAFIKI_MCP_TOKEN drives.
func TestMCPFaceChildConversationsBindingIsSubtree(t *testing.T) {
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
	if err := pool.QueryRow(ctx,
		`INSERT INTO conversations.users (username, token_sha256)
		 VALUES ('mcp-child-scope-owner', 'mcp-child-scope-owner:' || gen_random_uuid()::text)
		 RETURNING id::text`).Scan(&ownerID); err != nil {
		t.Fatalf("insert owner user: %v", err)
	}
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
	if err != nil {
		t.Fatalf("conversation_search: %v", err)
	}
	// The tool renders rows by conversation id, so the ids are the oracle:
	// both in-subtree rows surface, the sibling's never does.
	text := mcpCallText(t, res)
	if !strings.Contains(text, desc) {
		t.Fatalf("conversation_search did not surface the in-subtree descendant %s:\n%s", desc, text)
	}
	if !strings.Contains(text, own) {
		t.Fatalf("conversation_search did not surface the caller's own conversation %s:\n%s", own, text)
	}
	if strings.Contains(text, sibling) {
		t.Fatalf("conversation_search leaked the owner's other child's conversation %s:\n%s", sibling, text)
	}
}
