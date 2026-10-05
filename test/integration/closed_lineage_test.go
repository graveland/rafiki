// SPDX-License-Identifier: Apache-2.0

package integration_test

// Closing a child must change NOTHING a parent or a search can see.
//
// The design split is: live-state reads (ListChildren, Kill, Send, Close,
// StreamEvents, SetBudget) use LIVE rows only, exactly as before; lineage reads
// (a child credential's subtree conversation scope, subtree spend, budget
// enforcement) cover ALL descendants, live or closed. `Close` tombstones the
// child row (child.closed_at) and removes it from the in-memory live store, so
// before wave 2 a closed descendant silently vanished from its ancestors'
// subtreeSelector — and with it from a parent's ConversationSearch/Export and
// from its subtree spend accounting.
//
// These tests drive a REAL daemon over its Connect unix socket and (for the
// child-credential cases) a REAL per-child secret minted at spawn, so they
// exercise cmd/rafikid's wiring — the lineage source hung off the Postgres
// child store (wireLineageSource), childConversationScope's subtreeSelector
// call, and the connect search adapter's Closed/ClosedAtUnix mapping — that no
// unit fixture can see.
//
// Database hygiene (CLAUDE.md): RAFIKI_TEST_DSN points at a disposable but
// NON-resetting database shared by the whole suite, so every row seeded here
// carries a per-run unique marker/id and is deleted by a t.Cleanup, and no
// assertion depends on a global row count.
//
// -count=1 always: go test caching cannot see through to a daemon subprocess.

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5/pgxpool"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/gen/rafiki/v1/rafikiv1connect"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/rpcreason"

	"github.com/multigres/testkit/assert"
)

// ─── helpers ─────────────────────────────────────────────────────────────────

// uniqueMarker returns a per-run BM25/ILIKE-safe single token. nextDaemonID's
// hyphens are stripped so the marker stays one token in the stored message text
// and in the query, and so it never collides with a prior run's residue in the
// shared, non-resetting test database.
func uniqueMarker(prefix string) string {
	return prefix + strings.ReplaceAll(nextDaemonID(), "-", "")
}

// seedChildConversation inserts one conversation linked to childID by a
// thread-branch external_ref (childID + ":" + marker) plus a single role=user
// message whose text carries marker, so insights.Search's Text filter (an
// ILIKE over the earliest user message) matches exactly this row. ownerUserID
// may be "" (an unattributed conversation); pass a user id when the scope under
// test is ScopeOwner.
//
// The ref is the childID PREFIX form, not the bare child id: the daemon keeps a
// per-(external_ref, driven_by) unique index and may already hold a conversation
// whose external_ref is exactly the child id, and subtreeSelector names every
// branch of a child by the prefix anyway (childID+":", threadRefSep) — so a
// branch ref is both collision-free and in scope.
//
// The cleanup deletes precisely what it inserted, through a FRESH pool: the
// suite shares one non-resetting database, so this row would otherwise be
// matched again by every later run of the same query. It runs on a fresh pool
// because a caller's pool may be closed by a defer before t.Cleanup runs.
func seedChildConversation(t *testing.T, pool *pgxpool.Pool, dsn, childID, ownerUserID, marker string) string {
	t.Helper()
	ctx := context.Background()

	var owner *string
	if ownerUserID != "" {
		owner = &ownerUserID
	}
	externalRef := childID + ":" + marker
	var convID string
	if err := pool.QueryRow(ctx, `
		INSERT INTO conversations.conversation
		    (origin_entrypoint, driven_by, external_ref, owner_user_id)
		VALUES ('integration-test', 'server', $1, $2) RETURNING id::text`,
		externalRef, owner).Scan(&convID); err != nil {
		t.Fatalf("insert conversation for child %s: %v", childID, err)
	}

	content := fmt.Sprintf(`[{"type":"text","text":%q}]`, marker+" seeded first message")
	if _, err := pool.Exec(ctx, `
		INSERT INTO conversations.conversation_message
		    (conversation_id, ordinal, role, content)
		VALUES ($1::uuid, 0, 'user', $2::jsonb)`, convID, content); err != nil {
		t.Fatalf("insert message for conversation %s: %v", convID, err)
	}

	t.Cleanup(func() {
		p, err := pgxpool.New(context.Background(), dsn)
		if err != nil {
			t.Logf("drop conversation %s: pool: %v", convID, err)
			return
		}
		defer p.Close()
		// Delete the non-cascading dependents first. Close nudges the recall
		// indexer, which may seal a window/summary for the now-closed
		// conversation — those FKs do not cascade, and neither does
		// conversation_message's.
		for _, q := range []string{
			`DELETE FROM conversations.conversation_window WHERE conversation_id = $1::uuid`,
			`DELETE FROM conversations.conversation_summary WHERE conversation_id = $1::uuid`,
			`DELETE FROM conversations.recall_summary_failure WHERE conversation_id = $1::uuid`,
			`DELETE FROM conversations.conversation_message WHERE conversation_id = $1::uuid`,
			`DELETE FROM conversations.conversation_attachment WHERE conversation_id = $1::uuid`,
			`DELETE FROM conversations.conversation WHERE id = $1::uuid`,
		} {
			if _, err := p.Exec(context.Background(), q, convID); err != nil {
				t.Logf("drop conversation %s (%s): %v", convID, q, err)
			}
		}
	})
	return convID
}

// searchByText issues one ConversationSearch for marker, optionally filtered by
// closed ("" | "open" | "closed"), as the credential token names ("" = the
// local socket's operator trust). It fails the test on any RPC error; the
// caller asserts on the returned rows.
func searchByText(t *testing.T, client rafikiv1connect.ControlClient, ctx context.Context, token, marker, closed string) []*rafikiv1.ConversationSummary {
	t.Helper()
	resp, err := client.ConversationSearch(ctx, authorize(token, connect.NewRequest(
		&rafikiv1.ConversationSearchRequest{Text: marker, Closed: closed})))
	assert.NewAborting(t).NoError(err, "ConversationSearch(text=%q, closed=%q) as %q", marker, closed, token)
	return resp.Msg.GetRows()
}

// rowIDs returns the conversation ids of a search result.
func rowIDs(rows []*rafikiv1.ConversationSummary) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.GetId())
	}
	return out
}

// ─── the tests ───────────────────────────────────────────────────────────────

// TestClosedLineageChildCredentialStillSearchesAClosedDescendant is the
// headline: a parent's per-child credential still finds and exports a CLOSED
// descendant's conversation.
//
// It dies against the pre-change subtree scope. subtreeSelector (limits.go)
// before wave 2 read only the live in-memory set (c.st.Get and
// c.st.Descendants). Connect Close tombstones the descendant's row and calls
// c.st.Delete, so the descendant leaves the live set — and with the lineage
// union removed, the descendant's id is no longer in the selector's
// external-ref lists. The parent's ConversationSearch then matches nothing and
// ConversationExport answers not-found. Only the union with
// childstore.LineageSource (wired from the Postgres child store) keeps the
// closed descendant in scope.
func TestClosedLineageChildCredentialStillSearchesAClosedDescendant(t *testing.T) {
	t.Parallel()
	ck := assert.NewAborting(t)
	dsn := os.Getenv("RAFIKI_TEST_DSN")
	if dsn == "" {
		t.Skip("RAFIKI_TEST_DSN not set")
	}
	d, dumps := bootMCPChildDaemon(t)
	pool := openPool(t, dsn)

	// The caller: a real claude child, the only kind that carries a per-child
	// MCP secret (proxyChildEnv mints one for kind=claude on a proxied daemon),
	// spawned through its owner's session so its row is well-formed.
	ownerToken := d.createMCPUser(t)
	userSess := mcpConnect(t, d.proxyURL, ownerToken)
	parent := mcpSpawnClaudeChild(t, userSess, "closed-lineage-parent")
	parentToken := waitClaudeDump(t, d, dumps, parent).envValue("RAFIKI_MCP_TOKEN")
	ck.NotEq("", parentToken, "the parent claude child's environment carries no RAFIKI_MCP_TOKEN")

	// The descendant: a real child parented under the caller.
	descendant := d.spawnChildUnder(t, parent)

	marker := uniqueMarker("zqclosedlin")
	convID := seedChildConversation(t, pool, dsn, descendant, "", marker)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	client := d.connectClient()

	// Before the close the descendant is live, so the parent's subtree names its
	// conversation — the positive control that keeps the post-close assertion
	// from passing for a search that never worked.
	before := searchByText(t, client, ctx, parentToken, marker, "")
	ck.Eq(1, len(before), "parent search before close = %v, want exactly the descendant's conversation", rowIDs(before))
	ck.Eq(convID, before[0].GetId(), "parent search before close returned the wrong conversation")

	// Close the descendant through the daemon's Connect Close (operator trust on
	// the local socket), which tombstones the child row and stamps its
	// conversations closed.
	killAndForget(t, d, descendant)

	// Headline: the closed descendant's conversation is STILL in the parent's
	// subtree scope.
	after := searchByText(t, client, ctx, parentToken, marker, "")
	ck.Eq(1, len(after), "parent search AFTER close = %v, want exactly the descendant's conversation — a closed descendant must stay in its ancestor's subtree scope", rowIDs(after))
	ck.Eq(convID, after[0].GetId(), "parent search after close returned a different conversation")

	// And Export of it still succeeds through the same child credential.
	exp, err := client.ConversationExport(ctx, authorize(parentToken, connect.NewRequest(
		&rafikiv1.ConversationExportRequest{ConversationId: convID})))
	ck.NoError(err, "parent ConversationExport of the closed descendant's conversation")
	if err == nil {
		ck.Eq(convID, exp.Msg.GetConversationId(), "export returned a different conversation")
	}
}

// TestClosedLineageSearchClosedFilter: after a child is closed, a USER
// credential's ConversationSearch can separate the now-closed conversation from
// the still-open ones, and each row carries closed_at_unix.
//
// The Close call stamps conversation.closed_at on every conversation linked to
// the child (stampConversationsClosed); the search adapter forwards the
// "closed"/"open" filter to insights.Search and maps the stamped column onto
// the optional ClosedAtUnix. This is the end-to-end proof of both halves: the
// filter selects the right rows, and a closed row is distinguishable from an
// open one on the wire.
func TestClosedLineageSearchClosedFilter(t *testing.T) {
	t.Parallel()
	ck := assert.NewAborting(t)
	dsn := os.Getenv("RAFIKI_TEST_DSN")
	if dsn == "" {
		t.Skip("RAFIKI_TEST_DSN not set")
	}
	d := bootDaemonDB(t, nextDaemonID(), noRealProviderEnv()...)
	pool := openPool(t, dsn)

	// A user credential so the scope is ScopeOwner(user id); the seeded
	// conversation is owned by that same user.
	ownerID, ownerToken := createRecallUser(t, pool, "closed-filter-"+nextDaemonID(), false)

	// A real child to close. Its kind is irrelevant to the stamp — the
	// conversation links by a thread-branch external_ref (childID + ":" +
	// marker), one of the three arms stampConversationsClosed covers.
	child := d.spawnChild(t)
	marker := uniqueMarker("zqclosedfilt")
	convID := seedChildConversation(t, pool, dsn, child, ownerID, marker)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	client := d.connectClient()

	// Before close: the row is open, so "closed" does not return it.
	openBefore := searchByText(t, client, ctx, ownerToken, marker, "closed")
	ck.Eq(0, len(openBefore), "a not-yet-closed conversation matched closed:closed = %v", rowIDs(openBefore))

	killAndForget(t, d, child)

	// After close: "closed" returns exactly it, carrying closed_at_unix.
	closedRows := searchByText(t, client, ctx, ownerToken, marker, "closed")
	ck.Eq(1, len(closedRows), "closed:closed = %v, want exactly the closed child's conversation", rowIDs(closedRows))
	if len(closedRows) == 1 {
		row := closedRows[0]
		ck.Eq(convID, row.GetId(), "closed:closed returned the wrong conversation")
		ck.NotNil(row.ClosedAtUnix, "the closed row carries no closed_at_unix; the adapter dropped conversation.closed_at")
	}

	// "open" no longer returns it.
	openAfter := searchByText(t, client, ctx, ownerToken, marker, "open")
	ck.Eq(0, len(openAfter), "closed:open still returned the closed conversation = %v", rowIDs(openAfter))
}

// TestClosedLineageParentScopeUnchangedByClose pins that closing a descendant
// does not remove it from its ancestor's lineage SCOPE (the union of
// subtreeSelector). It asserts selector/scope membership, not a cost figure:
//
// Fallback assertion, and why: the cost rollup (subtreeSpend -> insights
// .SubtreeCost) prices a turn by consulting the daemon's model catalog
// (ModelCatalog.Pricing), whose snapshot is fetched from OpenRouter. This
// harness blanks OPENROUTER_API_KEY (noRealProviderEnv), so the catalog is
// empty and every seeded turn prices at 0 — a priced-turn assertion would pass
// for the wrong reason. What IS observable end to end, and is the SAME union
// the spend rollup reads, is subtreeSelector's result: it feeds both
// insights.SubtreeCost (spend) and ScopeSubtree (a child credential's
// conversation reads). So this asserts the selector still names the
// descendant's conversation AFTER the close, through the parent credential's
// ConversationExport (which resolves the conversation only if its id is in the
// caller's scope). Removing the lineage union makes the post-close export
// answer not-found.
func TestClosedLineageParentScopeUnchangedByClose(t *testing.T) {
	t.Parallel()
	ck := assert.NewAborting(t)
	dsn := os.Getenv("RAFIKI_TEST_DSN")
	if dsn == "" {
		t.Skip("RAFIKI_TEST_DSN not set")
	}
	d, dumps := bootMCPChildDaemon(t)
	pool := openPool(t, dsn)

	ownerToken := d.createMCPUser(t)
	userSess := mcpConnect(t, d.proxyURL, ownerToken)
	parent := mcpSpawnClaudeChild(t, userSess, "closed-lineage-cost-parent")
	parentToken := waitClaudeDump(t, d, dumps, parent).envValue("RAFIKI_MCP_TOKEN")
	ck.NotEq("", parentToken, "the parent claude child's environment carries no RAFIKI_MCP_TOKEN")

	descendant := d.spawnChildUnder(t, parent)
	marker := uniqueMarker("zqclosedcost")
	convID := seedChildConversation(t, pool, dsn, descendant, "", marker)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	client := d.connectClient()

	export := func(when string) {
		t.Helper()
		_, err := client.ConversationExport(ctx, authorize(parentToken, connect.NewRequest(
			&rafikiv1.ConversationExportRequest{ConversationId: convID})))
		ck.NoError(err, "parent export of the descendant's conversation %s close (selector must name it)", when)
	}
	export("before")

	killAndForget(t, d, descendant)

	export("after")
}

// TestClosedLineageStillRefusesActingOnAClosedChild: the live-state rule is
// UNCHANGED. Closing a child keeps its lineage, but the verbs that ACT on a
// live child (here Send) still refuse the now-closed child: Close removed it
// from the live store, so Send answers child-not-found.
//
// The positive control (Send to the still-live child succeeds) keeps the
// refusal from passing for a Send that is simply broken, and pins the split the
// design draws — a closed child is remembered for lineage reads, not revived
// for live-state ones.
func TestClosedLineageStillRefusesActingOnAClosedChild(t *testing.T) {
	t.Parallel()
	ck := assert.NewAborting(t)
	dsn := os.Getenv("RAFIKI_TEST_DSN")
	if dsn == "" {
		t.Skip("RAFIKI_TEST_DSN not set")
	}
	d := bootDaemonDB(t, nextDaemonID(), noRealProviderEnv()...)

	parent := d.spawnChild(t)
	descendant := d.spawnChildUnder(t, parent)
	client := d.control(t)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Positive control: the live child accepts a prompt.
	sendPrompt(t, client, descendant, "integration test: live-state positive control")

	killAndForget(t, d, descendant)

	// The closed child is refused, by the same rule as any unknown child.
	_, err := client.Send(ctx, connect.NewRequest(&rafikiv1.SendRequest{
		ChildId: descendant,
		Mode:    rafikiv1.SendMode_SEND_MODE_PROMPT,
		Blocks:  []*rafikiv1.ContentBlock{{Block: &rafikiv1.ContentBlock_Text{Text: &rafikiv1.TextBlock{Text: "must be refused"}}}},
	}))
	ck.Eq(connect.CodeNotFound, connect.CodeOf(err), "Send to a closed child = %v, want %v", err, connect.CodeNotFound)
	ck.Eq(protocol.ErrChildNotFound, rpcreason.Reason(err), "Send to a closed child reason = %q, want %q", rpcreason.Reason(err), protocol.ErrChildNotFound)
}
