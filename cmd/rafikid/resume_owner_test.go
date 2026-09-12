package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"go.graveland.dev/rafiki/pkg/childstore"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/quota"
	"go.graveland.dev/rafiki/pkg/users"
	"go.graveland.dev/rafiki/pkg/usersdb"
)

// ─── fakes ───────────────────────────────────────────────────────────────────

// ownerResolverStore is a users.Store whose LookupUsername is scripted and
// counted. The embedded interface stays nil: every method but LookupUsername
// panics if the code under test ever calls it, which is the loudest way to
// find out the helper reached for the wrong method.
type ownerResolverStore struct {
	users.Store
	ids map[string]string
	err error
	// lookups records every LookupUsername call, name by name.
	lookups []string
}

func (s *ownerResolverStore) LookupUsername(_ context.Context, name string) (string, error) {
	s.lookups = append(s.lookups, name)
	if s.err != nil {
		return "", s.err
	}
	id, ok := s.ids[name]
	if !ok {
		return "", users.ErrNotFound
	}
	return id, nil
}

// countingUserStore delegates to a real store and counts lookups, so a test
// can assert the resolver was consulted through a genuine Postgres path — and
// that a revert of a resume call site to a literal "" (which would consult it
// zero times) is caught.
type countingUserStore struct {
	users.Store
	lookups []string
}

func (s *countingUserStore) LookupUsername(ctx context.Context, name string) (string, error) {
	s.lookups = append(s.lookups, name)
	return s.Store.LookupUsername(ctx, name)
}

// ─── resumeOwnerUserID ──────────────────────────────────────────────────────

// A snapshot that already carries the id is authoritative: the label must
// never even be looked up, because the label can be stale while the id was
// resolved at spawn time.
func TestResumeOwnerUserIDPrefersTheSnapshotID(t *testing.T) {
	ctrl := newTestController(t)
	store := &ownerResolverStore{ids: map[string]string{"carol": "u_carol"}}
	ctrl.users = store

	snap := childstore.Snapshot{
		OwnerUserID: "u_direct",
		Labels:      map[string]string{"owner": "carol"},
	}
	if got := ctrl.resumeOwnerUserID(t.Context(), "c_x", snap); got != "u_direct" {
		t.Fatalf("resumeOwnerUserID = %q, want the snapshot id", got)
	}
	if len(store.lookups) != 0 {
		t.Fatalf("LookupUsername consulted %v for a snapshot that already carries the id", store.lookups)
	}
}

func TestResumeOwnerUserIDResolvesTheLabel(t *testing.T) {
	ctrl := newTestController(t)
	store := &ownerResolverStore{ids: map[string]string{"carol": "u_carol"}}
	ctrl.users = store

	snap := childstore.Snapshot{Labels: map[string]string{"owner": "carol"}}
	if got := ctrl.resumeOwnerUserID(t.Context(), "c_x", snap); got != "u_carol" {
		t.Fatalf("resumeOwnerUserID = %q, want the resolved id", got)
	}
	if len(store.lookups) != 1 || store.lookups[0] != "carol" {
		t.Fatalf("lookups = %v, want exactly [carol]", store.lookups)
	}
}

// A name that resolves to no active user is an answer, not a failure: the
// child continues unattributed.
func TestResumeOwnerUserIDContinuesUnattributedOnAMiss(t *testing.T) {
	ctrl := newTestController(t)
	ctrl.users = &ownerResolverStore{ids: map[string]string{}}

	snap := childstore.Snapshot{Labels: map[string]string{"owner": "ghost"}}
	if got := ctrl.resumeOwnerUserID(t.Context(), "c_x", snap); got != "" {
		t.Fatalf("resumeOwnerUserID = %q, want empty (attribution is best-effort, never guessed)", got)
	}
}

// A store that cannot answer is not an answer either: continue unattributed
// rather than refusing the resume over a bookkeeping field.
func TestResumeOwnerUserIDContinuesUnattributedOnAStoreFailure(t *testing.T) {
	ctrl := newTestController(t)
	ctrl.users = &ownerResolverStore{err: errors.New("dial: connection refused")}

	snap := childstore.Snapshot{Labels: map[string]string{"owner": "carol"}}
	if got := ctrl.resumeOwnerUserID(t.Context(), "c_x", snap); got != "" {
		t.Fatalf("resumeOwnerUserID = %q, want empty on a store failure", got)
	}
}

func TestResumeOwnerUserIDWithoutALabelOrStore(t *testing.T) {
	ctrl := newTestController(t) // users store nil (no RAFIKI_DB wiring in tests)

	if got := ctrl.resumeOwnerUserID(t.Context(), "c_x", childstore.Snapshot{}); got != "" {
		t.Fatalf("resumeOwnerUserID = %q, want empty with no label and no store", got)
	}
	ctrl.users = &ownerResolverStore{}
	if got := ctrl.resumeOwnerUserID(t.Context(), "c_x", childstore.Snapshot{}); got != "" {
		t.Fatalf("resumeOwnerUserID = %q, want empty with no label", got)
	}
}

// ─── the quota_status knock-on ───────────────────────────────────────────────

// TestResumedOwnerLabelReachesQuotaData confirms what the CLAUDE.md
// quota_status entry used to claim was impossible: a child resumed from a row
// that carries only the owner's USERNAME now answers quota_status with real
// captured data, because resumeOwnerUserID resolves the label and the resolved
// id is what newControllerQuotaReader binds. Seeded against real Postgres —
// both the users row and the captured rate-limit snapshot.
func TestResumedChildQuotaStatusReturnsCapturedData(t *testing.T) {
	pool := openTestPool(t)
	store := usersdb.NewPostgresStore(pool)
	username := fmt.Sprintf("resume-quota-it-%d", time.Now().UnixNano())
	u, _, err := store.Create(t.Context(), username)
	if err != nil {
		t.Fatalf("seed user: %v", err)
	}
	t.Cleanup(func() { _ = store.Delete(context.Background(), username) })

	util5 := 0.31
	reset5 := time.Now().Add(3 * time.Hour).UTC()
	qs := quota.NewStore(pool)
	if err := qs.Upsert(t.Context(), u.ID, quota.Status{
		OrganizationID: "org_resume_it",
		FiveH:          quota.Window{Utilization: &util5, ResetAt: &reset5, Status: "allowed"},
		OverallStatus:  "allowed",
	}); err != nil {
		t.Fatalf("seed quota row: %v", err)
	}

	ctrl := newTestController(t)
	ctrl.pool = pool
	ctrl.users = store

	// The resume path: label only (the shape a restart leaves behind), resolved
	// through the store, then bound into the reader exactly as
	// agentRuntimeOptions does.
	resolved := ctrl.resumeOwnerUserID(t.Context(), "c_resume_quota",
		childstore.Snapshot{Labels: map[string]string{"owner": username}})
	if resolved != u.ID {
		t.Fatalf("resumeOwnerUserID = %q, want %q", resolved, u.ID)
	}
	qr := newControllerQuotaReader(ctrl, resolved)
	st, ok, err := qr.RateLimitStatus(t.Context())
	if err != nil {
		t.Fatalf("RateLimitStatus: %v", err)
	}
	if !ok {
		t.Fatal("RateLimitStatus reported no data for a user with a captured snapshot — " +
			"this is the 'resumed child loses quota visibility' bug this plan closes")
	}
	if st.OrganizationID != "org_resume_it" {
		t.Errorf("OrganizationID = %q, want org_resume_it", st.OrganizationID)
	}
}

// ─── the two resume call sites ───────────────────────────────────────────────
//
// The plan warns that controller.go's two agentRunner call sites are easy to
// re-break independently — a revert of either to a literal "" passes every
// helper-level test above. These two drive the real Resume / RespawnChild
// paths and observe the CONVERSATION ROW, which is the far end of the pipe.

// resumedConversation waits for the resumed child's engine to resolve (and
// note) its conversation id, then returns it.
func resumedConversation(t *testing.T, ctrl *Controller, childID string) string {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if snap, ok := ctrl.st.Get(childID); ok && snap.SessionID != "" {
			return snap.SessionID
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("resumed child %s never noted a conversation id (the engine never resolved one)", childID)
	return ""
}

// seedControllerUser inserts one real users row via the production store and
// returns its id.
func seedControllerUser(t *testing.T, pool *pgxpool.Pool, username string) string {
	t.Helper()
	id, _, err := usersdb.NewPostgresStore(pool).Create(t.Context(), username)
	if err != nil {
		t.Fatalf("seed user %s: %v", username, err)
	}
	t.Cleanup(func() {
		if err := usersdb.NewPostgresStore(pool).Delete(context.Background(), username); err != nil {
			t.Logf("cleanup user %s: %v", username, err)
		}
	})
	return id.ID
}

// seedExitedFundiChild inserts an exited fundi child row directly, in the
// shape the caller names: withOwnerID for the rows modern daemons write,
// with a label-only shape for the 266 rows written by older daemons.
func seedExitedFundiChild(t *testing.T, ctrl *Controller, childID, ownerID string, labels map[string]string) {
	t.Helper()
	s := &childstore.Session{
		ChildID: childID,
		Kind:    protocol.KindFundi,
		Model:   "anthropic/claude-x",
		Cwd:     t.TempDir(),
		Status:  protocol.StatusExited,
	}
	if ownerID != "" {
		s.OwnerUserID = ownerID
	}
	s.Labels = labels
	ctrl.st.Insert(s)
}

func dropConversation(t *testing.T, pool *pgxpool.Pool, conversationID string) {
	t.Helper()
	if conversationID == "" {
		return
	}
	if _, err := pool.Exec(context.Background(),
		`DELETE FROM conversations.conversation WHERE id = $1`, conversationID); err != nil {
		t.Logf("cleanup conversation %s: %v", conversationID, err)
	}
}

// TestResumeAttributesTheConversationFromTheSnapshotID guards the resumeInternal
// call site: a resume of a child whose row carries OwnerUserID must land that
// id on the new conversation. A revert of the site to a literal "" leaves the
// row NULL and fails here.
func TestResumeAttributesTheConversationFromTheSnapshotID(t *testing.T) {
	pool := openTestPool(t)
	ownerID := seedControllerUser(t, pool, fmt.Sprintf("resume-id-it-%d", os.Getpid()))
	ctrl := newTestController(t)
	ctrl.pool = pool

	const childID = "c_resume_owned"
	seedExitedFundiChild(t, ctrl, childID, ownerID, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := ctrl.Resume(ctx, childID, ""); err != nil {
		t.Fatalf("resume: %v", err)
	}

	convID := resumedConversation(t, ctrl, childID)
	t.Cleanup(func() { dropConversation(t, pool, convID) })

	var owner *string
	if err := pool.QueryRow(ctx,
		`SELECT owner_user_id::text FROM conversations.conversation WHERE id = $1`, convID).Scan(&owner); err != nil {
		t.Fatalf("read conversation: %v", err)
	}
	if owner == nil || *owner != ownerID {
		t.Fatalf("resumed conversation owner_user_id = %v, want %s — Resume dropped the owner", owner, ownerID)
	}
}

// TestResumeAttributesTheConversationFromTheOwnerLabel guards the same site
// for the older-row shape: no id, only the owner's USERNAME in Labels. The
// label must resolve through the users store onto the new conversation.
func TestResumeAttributesTheConversationFromTheOwnerLabel(t *testing.T) {
	pool := openTestPool(t)
	username := fmt.Sprintf("resume-label-it-%d", time.Now().UnixNano())
	wantID := seedControllerUser(t, pool, username)
	ctrl := newTestController(t)
	ctrl.pool = pool
	resolver := &countingUserStore{Store: usersdb.NewPostgresStore(pool)}
	ctrl.users = resolver

	const childID = "c_resume_labeled"
	seedExitedFundiChild(t, ctrl, childID, "", map[string]string{"owner": username})

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := ctrl.Resume(ctx, childID, ""); err != nil {
		t.Fatalf("resume: %v", err)
	}

	convID := resumedConversation(t, ctrl, childID)
	t.Cleanup(func() { dropConversation(t, pool, convID) })

	var owner *string
	if err := pool.QueryRow(ctx,
		`SELECT owner_user_id::text FROM conversations.conversation WHERE id = $1`, convID).Scan(&owner); err != nil {
		t.Fatalf("read conversation: %v", err)
	}
	if owner == nil || *owner != wantID {
		t.Fatalf("resumed conversation owner_user_id = %v, want %s (resolved from the owner label)", owner, wantID)
	}
	if len(resolver.lookups) != 1 || resolver.lookups[0] != username {
		t.Fatalf("LookupUsername calls = %v, want exactly [%s] — a call site reverted to a literal \"\" would consult the store zero times", resolver.lookups, username)
	}
}

// TestRespawnChildAttributesTheConversationFromTheSnapshotID guards the second
// call site, independently — RespawnChild is a separate function and reverts
// separately.
func TestRespawnChildAttributesTheConversationFromTheSnapshotID(t *testing.T) {
	pool := openTestPool(t)
	ownerID := seedControllerUser(t, pool, fmt.Sprintf("respawn-id-it-%d", os.Getpid()))
	ctrl := newTestController(t)
	ctrl.pool = pool

	const childID = "c_respawn_owned"
	seedExitedFundiChild(t, ctrl, childID, ownerID, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if _, err := ctrl.RespawnChild(ctx, childID, ""); err != nil {
		t.Fatalf("respawn: %v", err)
	}

	convID := resumedConversation(t, ctrl, childID)
	t.Cleanup(func() { dropConversation(t, pool, convID) })

	var owner *string
	if err := pool.QueryRow(ctx,
		`SELECT owner_user_id::text FROM conversations.conversation WHERE id = $1`, convID).Scan(&owner); err != nil {
		t.Fatalf("read conversation: %v", err)
	}
	if owner == nil || *owner != ownerID {
		t.Fatalf("respawned conversation owner_user_id = %v, want %s — RespawnChild dropped the owner", owner, ownerID)
	}
}

// TestAgentRuntimeOptionsCarriesTheOwnerID pins the mapping itself: whatever
// id the caller resolved reaches BOTH the conversation (ro.OwnerUserID) and
// quota_status (the captured reader's user id). The quota reader takes no id
// in its methods — the constructor is the binding — so the captured field is
// the only place this can be asserted.
func TestAgentRuntimeOptionsCarriesTheOwnerID(t *testing.T) {
	ctrl := newTestController(t)
	ctrl.pool = openTestPool(t)

	req := protocol.SpawnRequest{Kind: protocol.KindFundi, Cwd: t.TempDir(), Model: "anthropic/claude-x"}
	ro, err := ctrl.agentRuntimeOptions(req, "c_owned", false, "carol", "u_carol")
	if err != nil {
		t.Fatalf("agentRuntimeOptions: %v", err)
	}
	if ro.OwnerUserID != "u_carol" {
		t.Errorf("ro.OwnerUserID = %q, want %q — the owner never reaches the conversation", ro.OwnerUserID, "u_carol")
	}
	qr, ok := ro.Quota.(*quotaReader)
	if !ok || qr == nil {
		t.Fatalf("ro.Quota = %T (%v), want *quotaReader", ro.Quota, ro.Quota)
	}
	if qr.userID != "u_carol" {
		t.Errorf("quota reader userID = %q, want %q — a resumed child's quota_status would answer 'no data captured'", qr.userID, "u_carol")
	}

	// The anonymous shape stays legal and unattributed: empty in, empty out,
	// and quota_status answers "no data captured" rather than guessing.
	ro2, err := ctrl.agentRuntimeOptions(req, "c_anon", false, "carol", "")
	if err != nil {
		t.Fatalf("agentRuntimeOptions (anonymous): %v", err)
	}
	if ro2.OwnerUserID != "" {
		t.Errorf("ro.OwnerUserID = %q for an anonymous spawn, want empty", ro2.OwnerUserID)
	}
}
