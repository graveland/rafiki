// SPDX-License-Identifier: Apache-2.0

package integration_test

// Child credentials on the recall surface, end to end against a REAL daemon.
// A per-child secret — the fake-claude child's RAFIKI_MCP_TOKEN, or a script
// child's per-child socket, which injects the same secret — may call the
// owner-scoped Connect verbs (Recall, RecallContext, GetMemory, MemoryTree,
// PutMemory, DeleteMemory) and reaches exactly its OWNER's non-admin rows.
// RecallBackfill and RecallStatus stay userOnly and are refused.
//
// Only this file can prove the binding on the real daemon: the recall
// Scope/MemoryOwner resolution and the policy gate live in cmd/rafikid, and
// the child-credential identity (owner UserID, never admin) is minted by the
// daemon at spawn — a unit fixture cannot see any of it.
//
// Same rules as mcp_test.go: -count=1 always, because go test caching cannot
// see through to a daemon subprocess.

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5/pgxpool"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/gen/rafiki/v1/rafikiv1connect"
	"go.graveland.dev/rafiki/pkg/recall"
	"go.graveland.dev/rafiki/pkg/users"
	"go.graveland.dev/rafiki/pkg/usersdb"

	"github.com/multigres/testkit/assert"
)

// ─── helpers ─────────────────────────────────────────────────────────────────

// authorize attaches a bearer credential to one Connect request. An empty
// token sends none — the daemon's control unix socket is then the credential
// (local trust).
func authorize[T any](token string, req *connect.Request[T]) *connect.Request[T] {
	if token != "" {
		req.Header().Set("Authorization", "Bearer "+token)
	}
	return req
}

func recallHitNames(hits []*rafikiv1.RecallHit) []string {
	names := make([]string, 0, len(hits))
	for _, h := range hits {
		names = append(names, h.GetName())
	}
	return names
}

// putRecallMemory writes one memory as the credential's own owner — the only
// way a memory is ever written (the wire carries no owner).
func putRecallMemory(t *testing.T, client rafikiv1connect.ControlClient, ctx context.Context, token, path, name, body string) {
	t.Helper()
	_, err := client.PutMemory(ctx, authorize(token, connect.NewRequest(&rafikiv1.PutMemoryRequest{
		Path: path, Name: name, Body: body,
	})))
	assert.NewAborting(t).NoError(err, "PutMemory(%s/%s) as %s", path, name, token)
}

// createRecallUser mints a user directly in the store, bypassing CreateUser
// (which can never set the admin bit) so the admin-owner test can have an
// admin whose child is spawned under it. The token authenticates against the
// same database the daemon reads.
func createRecallUser(t *testing.T, pool *pgxpool.Pool, username string, admin bool) (id, token string) {
	t.Helper()
	st := usersdb.NewPostgresStore(pool)
	u, tok, err := st.Create(context.Background(), users.NewUser{Username: username, IsAdmin: admin, MintToken: true})
	assert.NewAborting(t).NoError(err, "create user %s", username)
	t.Cleanup(func() {
		rctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := st.Delete(rctx, username); err != nil {
			t.Logf("delete user %s: %v", username, err)
		}
	})
	return u.ID, tok
}

// seedRecallWindow inserts one conversation-derived window row owned by
// ownerID, as the indexer would after sealing a segment. It is the cheap way
// to give another user a row that the CONVERSATION sources (unlike memories)
// would leak if a caller's scope were daemon-wide.
//
// It removes the row (and the bare conversation it inserted) on cleanup: the
// whole suite shares one database that is not reset between runs, so a window
// left behind is matched again by every later run of the same query and makes
// a global hit count grow run over run. The window's FK to conversation does
// not cascade, so the window goes first.
func seedRecallWindow(t *testing.T, pool *pgxpool.Pool, ownerID, text string) {
	t.Helper()
	convID := insertTestConversation(t, pool)
	_, err := pool.Exec(context.Background(), `INSERT INTO conversations.conversation_window
		(conversation_id, owner_user_id, seq, ordinal_from, ordinal_to, text, sealed, extractor_version)
		VALUES ($1::uuid, $2::uuid, 1, 0, 1, $3, true, $4)`,
		convID, ownerID, text, recall.ExtractorVersion)
	assert.NewAborting(t).NoError(err, "insert conversation_window for %s", ownerID)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if _, err := pool.Exec(ctx,
			`DELETE FROM conversations.conversation_window WHERE conversation_id = $1::uuid`, convID); err != nil {
			t.Logf("delete conversation_window for %s: %v", convID, err)
		}
		if _, err := pool.Exec(ctx,
			`DELETE FROM conversations.conversation WHERE id = $1::uuid`, convID); err != nil {
			t.Logf("delete conversation %s: %v", convID, err)
		}
	})
}

// ─── the tests ───────────────────────────────────────────────────────────────

// TestChildRecallReadsOwnerMemoryOnly: a per-child credential's Recall reaches
// its OWNER's memory and never another user's. Fails if the child binding ever
// resolved another owner, or bound the memory source to anything but the
// caller's own owner — the line whose deletion makes it fail is
// `q.MemoryOwner = owner.UserID` in connectRecall.Recall
// (cmd/rafikid/connect_recall.go), fed by recallIdentity->recallOwner.
func TestChildRecallReadsOwnerMemoryOnly(t *testing.T) {
	t.Parallel()
	ck := assert.NewAborting(t)
	d, dumps := bootMCPChildDaemon(t)

	// User A owns the child: a child must be spawned through a session that
	// RECORDS an owner (ChildForMCPToken refuses an unowned row), so the
	// owner's MCP session is the path a real per-child credential is minted
	// through.
	tokenA := d.createMCPUser(t)
	sessA := mcpConnect(t, d.proxyURL, tokenA)
	childA := mcpSpawnClaudeChild(t, sessA, "recall-owner-a")
	mcpToken := waitClaudeDump(t, d, dumps, childA).envValue("RAFIKI_MCP_TOKEN")
	ck.NotEq("", mcpToken, "the spawned claude child's environment carries no RAFIKI_MCP_TOKEN")

	tokenB := d.createMCPUser(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	client := d.connectClient()

	const q = "pineapple"
	putRecallMemory(t, client, ctx, tokenA, "child-recall", "a-mem", q+" owner A's body")
	putRecallMemory(t, client, ctx, tokenB, "child-recall", "b-mem", q+" owner B's body")

	// The child's credential searches both users' memories by a term in each
	// body; only its owner's may surface.
	resp, err := client.Recall(ctx, authorize(mcpToken, connect.NewRequest(&rafikiv1.RecallRequest{
		Query: q, Sources: []string{"memory"},
	})))
	ck.NoError(err, "child Recall")
	names := recallHitNames(resp.Msg.GetHits())
	ck.Contains(names, "a-mem", "the child's Recall must return its OWNER's memory; hits = %v", names)
	ck.NotContains(names, "b-mem", "the child's Recall leaked another user's memory; hits = %v", names)

	// Control: the owner's own user credential sees the same hit through the
	// same query, so the hit is real and only the owner dimension differs.
	ownerResp, err := client.Recall(ctx, authorize(tokenA, connect.NewRequest(&rafikiv1.RecallRequest{
		Query: q, Sources: []string{"memory"},
	})))
	ck.NoError(err, "owner Recall")
	ck.Contains(recallHitNames(ownerResp.Msg.GetHits()), "a-mem", "the owner's own Recall must see the memory the child sees")
}

// TestChildRecallOfAdminOwnerIsNotDaemonWide: a child whose owner is an ADMIN
// is bound to that admin's OWN rows, never Scope{All: true}. The child's
// recall identity carries its owner's UserID but is never admin, so its scope
// is the owner's — a conversation-derived window belonging to a DIFFERENT user
// must not surface.
//
// The watch line is recallOwner's admin clause
// (`IsAdmin: id.IsAdmin && id.IsUserCredential()`, cmd/rafikid/recall.go) and
// the scope rule it feeds, recallScopeFor's OwnerUserID branch
// (cmd/rafikid/connect_recall.go). Deleting `q.Scope = recallScopeFor(owner)`
// makes the child's call fail; a scope that resolved to All for an
// admin-owned child would return the other user's window and fail the empty
// assertion. Note that a per-child credential never carries the admin bit at
// all (childTokenLookup returns UserID/Via only), so the clause is the
// defence-in-depth that keeps that invariant true if owner resolution ever
// learns to consult the owner's users row.
func TestChildRecallOfAdminOwnerIsNotDaemonWide(t *testing.T) {
	t.Parallel()
	ck := assert.NewAborting(t)
	dsn := os.Getenv("RAFIKI_TEST_DSN")
	if dsn == "" {
		t.Skip("RAFIKI_TEST_DSN not set")
	}
	d, dumps := bootMCPChildDaemon(t)
	pool := openPool(t, dsn)

	_, adminToken := createRecallUser(t, pool, "recall-admin-"+nextDaemonID(), true)
	otherID, _ := createRecallUser(t, pool, "recall-other-"+nextDaemonID(), false)

	// The child: owned by the ADMIN, spawned through the admin's MCP session.
	adminSess := mcpConnect(t, d.proxyURL, adminToken)
	child := mcpSpawnClaudeChild(t, adminSess, "recall-admin-child")
	mcpToken := waitClaudeDump(t, d, dumps, child).envValue("RAFIKI_MCP_TOKEN")
	ck.NotEq("", mcpToken, "the admin-owned child's environment carries no RAFIKI_MCP_TOKEN")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	client := d.connectClient()

	// A per-run query term. The seeded window is a raw insert the shared test
	// database does not reset between runs, so a fixed term would accumulate
	// matching rows and make the daemon-wide positive control below creep past
	// one hit on a second run. nextDaemonID's hyphens are stripped so the term
	// stays a single BM25 token in both the indexed text and the query.
	q := "zqwinadmin" + strings.ReplaceAll(nextDaemonID(), "-", "")
	seedRecallWindow(t, pool, otherID, q+" the other user's captured conversation window")

	// Positive control: the admin's OWN credential is daemon-wide, so IT sees
	// the seeded window — proving the row is searchable and the query matches.
	adminResp, err := client.Recall(ctx, authorize(adminToken, connect.NewRequest(&rafikiv1.RecallRequest{
		Query: q, Sources: []string{"window"},
	})))
	ck.NoError(err, "admin Recall")
	ck.Eq(1, len(adminResp.Msg.GetHits()), "admin positive control: the seeded window must be searchable, hits = %v", recallHitNames(adminResp.Msg.GetHits()))

	// The admin-OWNED child must NOT see it: its scope is the admin's own
	// rows, never the daemon's.
	childResp, err := client.Recall(ctx, authorize(mcpToken, connect.NewRequest(&rafikiv1.RecallRequest{
		Query: q, Sources: []string{"window"},
	})))
	ck.NoError(err, "child Recall")
	ck.Eq(0, len(childResp.Msg.GetHits()), "the admin-owned child's Recall returned another user's conversation hit; a child of an admin must be owner-scoped, never Scope{All:true}; hits = %v", recallHitNames(childResp.Msg.GetHits()))
}

// TestChildRecallWritesAreTheOwners: the child's PutMemory lands in its
// OWNER's namespace — the owner's own GetMemory returns it and a different
// user's does not. Fails if PutMemory's owner resolution
// (`rt.st.PutMemory(ctx, recallIdentity(ctx).UserID, mem)`,
// cmd/rafikid/connect_recall.go) ever used another owner.
func TestChildRecallWritesAreTheOwners(t *testing.T) {
	t.Parallel()
	ck := assert.NewAborting(t)
	d, dumps := bootMCPChildDaemon(t)

	tokenA := d.createMCPUser(t)
	sessA := mcpConnect(t, d.proxyURL, tokenA)
	childA := mcpSpawnClaudeChild(t, sessA, "recall-writer-a")
	mcpToken := waitClaudeDump(t, d, dumps, childA).envValue("RAFIKI_MCP_TOKEN")
	ck.NotEq("", mcpToken, "the spawned claude child's environment carries no RAFIKI_MCP_TOKEN")

	tokenB := d.createMCPUser(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	client := d.connectClient()

	const path, name, body = "child-writes", "written-by-child", "the child wrote this"
	putRecallMemory(t, client, ctx, mcpToken, path, name, body)

	// The OWNER's credential reads the child's memory back.
	ownerResp, err := client.GetMemory(ctx, authorize(tokenA, connect.NewRequest(&rafikiv1.GetMemoryRequest{Path: path, Name: name})))
	ck.NoError(err, "owner GetMemory of the child's memory")
	ck.Eq(body, ownerResp.Msg.GetMemory().GetBody(), "owner GetMemory body")

	// A different user's credential does not.
	_, err = client.GetMemory(ctx, authorize(tokenB, connect.NewRequest(&rafikiv1.GetMemoryRequest{Path: path, Name: name})))
	ck.Eq(connect.CodeNotFound, connect.CodeOf(err), "a different user's GetMemory of the child's memory = %v, want %v", err, connect.CodeNotFound)
}

// TestChildRecallBackfillAndStatusRefused: RecallBackfill and RecallStatus
// stay userOnly, so a per-child credential is refused PermissionDenied by the
// policy gate before any handler runs. Fails if the `"RecallBackfill":
// policyUserOnly` or `"RecallStatus": policyUserOnly` entries in
// connectPolicyTable (cmd/rafikid/connect_policy.go) are widened.
func TestChildRecallBackfillAndStatusRefused(t *testing.T) {
	t.Parallel()
	ck := assert.NewAborting(t)
	d, dumps := bootMCPChildDaemon(t)

	token := d.createMCPUser(t)
	sess := mcpConnect(t, d.proxyURL, token)
	child := mcpSpawnClaudeChild(t, sess, "recall-refused-child")
	mcpToken := waitClaudeDump(t, d, dumps, child).envValue("RAFIKI_MCP_TOKEN")
	ck.NotEq("", mcpToken, "the spawned claude child's environment carries no RAFIKI_MCP_TOKEN")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	client := d.connectClient()

	_, err := client.RecallBackfill(ctx, authorize(mcpToken, connect.NewRequest(&rafikiv1.RecallBackfillRequest{MaxCostUsd: 1})))
	ck.Eq(connect.CodePermissionDenied, connect.CodeOf(err), "child RecallBackfill = %v, want %v", err, connect.CodePermissionDenied)

	_, err = client.RecallStatus(ctx, authorize(mcpToken, connect.NewRequest(&rafikiv1.RecallStatusRequest{})))
	ck.Eq(connect.CodePermissionDenied, connect.CodeOf(err), "child RecallStatus = %v, want %v", err, connect.CodePermissionDenied)
}

// TestScriptChildCallsRecall drives Recall through a REAL script child's
// per-child socket — the credential a script child's pymodule would use. The
// socket injects the same per-child secret (childsock.Serve's Authorization
// injection, cmd/rafikid/script_runtime.go's mintMCPToken), so the call
// resolves to the child's ProvenanceChildToken identity and reaches its
// owner's memory.
//
// Substitution note: rather than embed a Recall call in the pymodule and
// scrape its result out of the child's log, this dials the child's own socket
// directly. The credential path under test — the socket's injected per-child
// secret, the policyOwnerScoped admit, and the owner scope — is identical;
// only the caller on the other end of the socket differs.
func TestScriptChildCallsRecall(t *testing.T) {
	t.Parallel()
	ck := assert.NewAborting(t)
	if _, err := lookPython3(); err != nil {
		t.Skip("python3 not available: script children need an interpreter")
	}
	d := bootDaemonDB(t, nextDaemonID(), noRealProviderEnv()...)
	token, configDir := scriptUser(t, d)
	opClient := faceClient(t, d, token)

	putPymodule(t, d, configDir, "recall_sleeper_it", scriptSleeperCode)
	scriptID := d.scriptChildSpawn(t, opClient, &rafikiv1.SpawnRequest{
		Cwd:    t.TempDir(),
		Kind:   "script",
		Script: &rafikiv1.SpawnRequest_ScriptSpec{Repo: "local", Script: "recall_sleeper_it"},
	})
	sockPath := scriptSocketPath(d, scriptID)
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(sockPath); err == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if _, err := os.Stat(sockPath); err != nil {
		t.Fatalf("per-child socket never appeared at %s: %v\n\n-- daemon stderr tail --\n%s", sockPath, err, d.stderr.tail(8000))
	}
	t.Cleanup(func() {
		kctx, kcancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer kcancel()
		if _, err := opClient.Kill(kctx, connect.NewRequest(&rafikiv1.KillRequest{
			ChildId: scriptID, ShutdownTimeoutMs: 2000, KillTimeoutMs: 2000,
		})); err != nil {
			t.Logf("kill the sleeper %s: %v", scriptID, err)
		}
	})
	childClient := childConnectClient(t, sockPath)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	const q = "scriptpineapple"
	putRecallMemory(t, opClient, ctx, token, "script-recall", "s-mem", q+" the script child owner's body")

	resp, err := childClient.Recall(ctx, connect.NewRequest(&rafikiv1.RecallRequest{Query: q, Sources: []string{"memory"}}))
	ck.NoError(err, "Recall through the script child's socket")
	ck.Contains(recallHitNames(resp.Msg.GetHits()), "s-mem", "the script child's Recall must reach its owner's memory; hits = %v", recallHitNames(resp.Msg.GetHits()))
}
