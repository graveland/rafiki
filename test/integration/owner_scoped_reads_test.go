// SPDX-License-Identifier: Apache-2.0

package integration_test

// Owner-scoped agent reads, end to end against a REAL daemon: a per-child
// credential reads every agent its OWNER owns, while acting on one stays on
// the spawn tree.
//
// The read boundary (ChildScope.AuthorizeRead, Controller.canReadChild), the
// owner-scoped conversation scope (childConversationScope -> ScopeOwner) and
// ListChildren's scope parameter are all decided in cmd/rafikid from stored
// lineage the wire never carries, and the per-child credential itself is minted
// by the daemon at spawn. A unit fixture builds its own wiring; only a real
// daemon proves the daemon's.
//
// Who holds the credential, and why the two children are different kinds: a
// per-child secret is minted only for the kinds the daemon launches with one —
// a claude child (proxyChildEnv delivers RAFIKI_MCP_TOKEN in its environment)
// or a script child (its per-child Connect socket injects the same secret). A
// fundi child runs IN-PROCESS with no environment and no socket, so it can
// never hold a credential. The READER here is therefore a claude child, and the
// readable sibling is a fundi child driven by the hidden --fake-turns sender —
// the one kind that produces a real persisted transcript without a provider.
// Both are top-level children of one user, so the sibling is outside the
// reader's subtree: that is the whole point of the widening under test.
//
// Same rules as mcp_test.go and closed_lineage_test.go: -count=1 always,
// because go test caching cannot see through to a daemon subprocess, and the
// shared test database is NOT reset between runs, so every seeded row carries a
// per-run unique marker and is deleted by a t.Cleanup.

import (
	"context"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/jackc/pgx/v5/pgxpool"

	rafikiv1 "go.graveland.dev/rafiki/pkg/gen/rafiki/v1"
	"go.graveland.dev/rafiki/pkg/gen/rafiki/v1/rafikiv1connect"
	"go.graveland.dev/rafiki/pkg/protocol"
	"go.graveland.dev/rafiki/pkg/recall"

	"github.com/multigres/testkit/assert"
)

// ─── helpers ─────────────────────────────────────────────────────────────────

// ownerScopedCredentialChild spawns one top-level claude child through its
// owner's MCP session and returns the child id plus the per-child secret the
// daemon delivered to it — the credential every read in this file is made
// with. Spawning through the owner's session is what stamps the child's
// owner_user_id (an unowned row is admin/UDS-only, so an owner-scope test
// needs an owned lineage).
func ownerScopedCredentialChild(t *testing.T, d *daemon, dumps, userToken, name string) (childID, credential string) {
	t.Helper()
	sess := mcpConnect(t, d.proxyURL, userToken)
	childID = mcpSpawnClaudeChild(t, sess, name)
	credential = waitClaudeDump(t, d, dumps, childID).envValue("RAFIKI_MCP_TOKEN")
	assert.NewAborting(t).NotEq("", credential,
		"the spawned claude child's environment carries no RAFIKI_MCP_TOKEN")
	return childID, credential
}

// ownerScopedSpawnFundi spawns one top-level fundi child AS token — the daemon
// stamps its owner from the credential the mount resolved — driven by the
// hidden --fake-turns sender, so a prompt runs one scripted turn with no
// provider call. It returns the child id.
func ownerScopedSpawnFundi(t *testing.T, ctx context.Context, client rafikiv1connect.ControlClient, token, name string) string {
	t.Helper()
	ck := assert.NewAborting(t)
	resp, err := client.Spawn(ctx, authorize(token, connect.NewRequest(&rafikiv1.SpawnRequest{
		Cwd:       t.TempDir(),
		Name:      name,
		NoSession: true,
		Kind:      protocol.KindFundi,
		Model:     "anthropic/claude-x",
		ExtraArgs: []string{"--fake-turns", telemetryFakeTurns(t)},
	})))
	ck.NoError(err, "Spawn(fundi %s)", name)
	ck.NotEq("", resp.Msg.GetChildId(), "spawn returned empty childId")
	return resp.Msg.GetChildId()
}

// ownerScopedHistory issues one GetHistory as token ("" = the local socket's
// operator trust) and hands back whatever the daemon answered.
func ownerScopedHistory(ctx context.Context, client rafikiv1connect.ControlClient, token, childID string) ([]*rafikiv1.Event, error) {
	resp, err := client.GetHistory(ctx, authorize(token, connect.NewRequest(
		&rafikiv1.GetHistoryRequest{ChildId: childID})))
	if err != nil {
		return nil, err
	}
	return resp.Msg.GetEvents(), nil
}

// waitChildHistory polls GetHistory over the operator socket until the child's
// conversation holds at least want events. A fundi child's conversation id is
// only recorded once its first turn has run, so before that GetHistory answers
// NotFound — this waits on the daemon's own answer rather than on a clock, and
// the operator read is the CONTROL: once it has the events, the
// credential-scoped read under test is deterministic.
func waitChildHistory(t *testing.T, ctx context.Context, client rafikiv1connect.ControlClient, childID string, want int) []*rafikiv1.Event {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		events, err := ownerScopedHistory(ctx, client, "", childID)
		lastErr = err
		if err == nil && len(events) >= want {
			return events
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("child %s's conversation never reached %d events within 30s (last error: %v)", childID, want, lastErr)
	return nil
}

// ownerScopedEventTexts collects the text of every user and assistant block in
// an event slice — what a reader of a transcript would actually see.
func ownerScopedEventTexts(events []*rafikiv1.Event) []string {
	var out []string
	for _, ev := range events {
		var blocks []*rafikiv1.ContentBlock
		switch {
		case ev.GetUserMessage() != nil:
			blocks = ev.GetUserMessage().GetContent()
		case ev.GetAssistantMessage() != nil:
			blocks = ev.GetAssistantMessage().GetContent()
		}
		for _, b := range blocks {
			if tb := b.GetText(); tb != nil {
				out = append(out, tb.GetText())
			}
		}
	}
	return out
}

// ownerScopedContainsText reports whether any event text carries want.
func ownerScopedContainsText(texts []string, want string) bool {
	for _, s := range texts {
		if strings.Contains(s, want) {
			return true
		}
	}
	return false
}

// ownerScopedSummaryIDs maps a ListChildren answer to its child ids.
func ownerScopedSummaryIDs(rows []*rafikiv1.ChildSummary) []string {
	out := make([]string, 0, len(rows))
	for _, c := range rows {
		out = append(out, c.GetChildId())
	}
	return out
}

// ownerScopedSorted returns a sorted copy, so two child-id sets compare
// independently of the daemon's ordering.
func ownerScopedSorted(ids []string) []string {
	out := append([]string(nil), ids...)
	sort.Strings(out)
	return out
}

// ownerScopedList issues one ListChildren as token at the given scope.
func ownerScopedList(t *testing.T, ctx context.Context, client rafikiv1connect.ControlClient, token string, scope rafikiv1.ListScope) []*rafikiv1.ChildSummary {
	t.Helper()
	resp, err := client.ListChildren(ctx, authorize(token, connect.NewRequest(
		&rafikiv1.ListChildrenRequest{Scope: scope})))
	assert.NewAborting(t).NoError(err, "ListChildren(scope=%s) as %q", scope, token)
	return resp.Msg.GetChildren()
}

// ownerScopedSearch issues one ConversationSearch as token for text.
func ownerScopedSearch(t *testing.T, ctx context.Context, client rafikiv1connect.ControlClient, token, text string) []*rafikiv1.ConversationSummary {
	t.Helper()
	resp, err := client.ConversationSearch(ctx, authorize(token, connect.NewRequest(
		&rafikiv1.ConversationSearchRequest{Text: text})))
	assert.NewAborting(t).NoError(err, "ConversationSearch(text=%q) as %q", text, token)
	return resp.Msg.GetRows()
}

// seedWindowOn inserts one sealed conversation_window for an EXISTING
// conversation, owned by ownerUserID — the row recall's window source reads —
// and removes it again on cleanup.
//
// seedRecallWindow does the same for a conversation of its own; the recall
// parity test needs the window on the SAME conversation ConversationSearch
// matches (its Text filter reads the earliest user message), so one row can be
// compared across the two verbs.
func seedWindowOn(t *testing.T, pool *pgxpool.Pool, convID, ownerUserID, text string) {
	t.Helper()
	_, err := pool.Exec(context.Background(), `INSERT INTO conversations.conversation_window
		(conversation_id, owner_user_id, seq, ordinal_from, ordinal_to, text, sealed, extractor_version)
		VALUES ($1::uuid, $2::uuid, 1, 0, 1, $3, true, $4)`,
		convID, ownerUserID, text, recall.ExtractorVersion)
	assert.NewAborting(t).NoError(err, "insert conversation_window for %s", convID)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if _, err := pool.Exec(ctx,
			`DELETE FROM conversations.conversation_window WHERE conversation_id = $1::uuid`, convID); err != nil {
			t.Logf("delete conversation_window for %s: %v", convID, err)
		}
	})
}

// ─── the tests ───────────────────────────────────────────────────────────────

// TestOwnerScopedReadsSiblingHistoryOverConnect is the headline: a child
// credential's GetHistory reaches a SIBLING — another top-level agent of the
// same owner, outside its spawn subtree — and sees that sibling's events.
//
// Fails if AuthorizeRead falls back to Authorize (the sibling would refuse), or
// if canReadChild's owner arm is dropped.
func TestOwnerScopedReadsSiblingHistoryOverConnect(t *testing.T) {
	t.Parallel()
	ck := assert.NewAborting(t)
	dsn := os.Getenv("RAFIKI_TEST_DSN")
	if dsn == "" {
		t.Skip("RAFIKI_TEST_DSN not set")
	}
	d, dumps := bootMCPChildDaemon(t)
	pool := openPool(t, dsn)
	client := d.connectClient()

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	userToken := d.createMCPUser(t)
	childA, credentialA := ownerScopedCredentialChild(t, d, dumps, userToken, "owner-reads-history-a")
	childB := ownerScopedSpawnFundi(t, ctx, client, userToken, "owner-reads-history-b")

	// The fixture's premise: two distinct TOP-LEVEL children of ONE user. A's
	// read of B is a same-owner read only because of the second half.
	ck.NotEq(childB, childA, "the reader and the sibling must be different agents")
	ck.Eq(childOwnerUserID(t, pool, childA), childOwnerUserID(t, pool, childB),
		"the two children must share one owner, or this is a foreign-owner read")

	marker := uniqueMarker("zqownsibhist")
	sendPrompt(t, client, childB, marker+" the sibling's first prompt")

	// The operator's own read is the control: once it sees the turn, B's
	// transcript is committed and A's read is deterministic.
	operatorEvents := waitChildHistory(t, ctx, client, childB, 2)
	ck.True(ownerScopedContainsText(ownerScopedEventTexts(operatorEvents), marker),
		"the operator's read of B never saw the prompt; the fixture produced no transcript to read")

	events, err := ownerScopedHistory(ctx, client, credentialA, childB)
	ck.NoError(err, "child A's GetHistory of its sibling B (same owner, outside A's subtree)")
	ck.Eq(len(operatorEvents), len(events),
		"A's read of B returned %d events, the operator's returned %d", len(events), len(operatorEvents))
	texts := ownerScopedEventTexts(events)
	ck.True(ownerScopedContainsText(texts, marker),
		"A's read of B did not carry B's prompt %q; texts = %v", marker, texts)
	ck.True(ownerScopedContainsText(texts, "telemetry fixture reply"),
		"A's read of B did not carry B's scripted reply; texts = %v", texts)
	for _, ev := range events {
		ck.Eq(childB, ev.GetChildId(), "A's read of B returned an event attributed to another child")
	}
}

// TestOwnerScopedReadsConversationSearchFindsSibling: the conversation reads
// widen with the agent reads — A's ConversationSearch returns B's conversation
// and ConversationExport of B resolves B's transcript.
//
// Fails if childConversationScope keeps the subtree boundary for an OWNED
// lineage (ScopeSubtree), which would hide B from both verbs.
func TestOwnerScopedReadsConversationSearchFindsSibling(t *testing.T) {
	t.Parallel()
	ck := assert.NewAborting(t)
	d, dumps := bootMCPChildDaemon(t)
	client := d.connectClient()

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	userToken := d.createMCPUser(t)
	_, credentialA := ownerScopedCredentialChild(t, d, dumps, userToken, "owner-reads-search-a")
	childB := ownerScopedSpawnFundi(t, ctx, client, userToken, "owner-reads-search-b")

	marker := uniqueMarker("zqownsibsearch")
	sendPrompt(t, client, childB, marker+" the sibling's searchable prompt")
	waitChildHistory(t, ctx, client, childB, 2)

	// A fundi child's conversation id is its session id, which is what the
	// search row and the export must both name.
	convB := getChild(t, client, childB).GetSessionId()
	ck.NotEq("", convB, "the fundi sibling has no session/conversation id after its turn")

	rows := ownerScopedSearch(t, ctx, client, credentialA, marker)
	ck.Eq(1, len(rows), "A's ConversationSearch = %v, want exactly the sibling's conversation", rowIDs(rows))
	if len(rows) == 1 {
		ck.Eq(convB, rows[0].GetId(), "A's search returned a different conversation than B's")
	}

	// ConversationExport resolves a CHILD id to that child's root conversation
	// (pkg/insights isChildID), so this is A exporting B by name.
	exp, err := client.ConversationExport(ctx, authorize(credentialA, connect.NewRequest(
		&rafikiv1.ConversationExportRequest{ConversationId: childB})))
	ck.NoError(err, "A's ConversationExport of sibling B's child id")
	if err == nil {
		ck.Eq(convB, exp.Msg.GetConversationId(), "the export resolved a different conversation than B's")
		turns := exp.Msg.GetTurns()
		ck.True(len(turns) > 0, "A's export of B returned no turns")
		var exported []string
		for _, turn := range turns {
			exported = append(exported, string(turn.GetContent()))
		}
		joined := strings.Join(exported, "\n")
		ck.True(strings.Contains(joined, marker), "A's export of B did not carry B's prompt %q; turns = %v", marker, exported)
		ck.True(strings.Contains(joined, "telemetry fixture reply"), "A's export of B did not carry B's scripted reply; turns = %v", exported)
	}
}

// TestOwnerScopedReadsForeignOwnerRefusedIdentically: a child of a DIFFERENT
// user and an unknown id get one identical PermissionDenied on GetHistory, so a
// caller cannot probe ids, and cannot learn which of the two it hit.
//
// Fails if canReadChild's owner comparison is dropped (a foreign child would be
// admitted) or if the refusal names the reason (the two answers would differ).
func TestOwnerScopedReadsForeignOwnerRefusedIdentically(t *testing.T) {
	t.Parallel()
	ck := assert.NewAborting(t)
	dsn := os.Getenv("RAFIKI_TEST_DSN")
	if dsn == "" {
		t.Skip("RAFIKI_TEST_DSN not set")
	}
	d, dumps := bootMCPChildDaemon(t)
	pool := openPool(t, dsn)
	client := d.connectClient()

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	// The caller's own child: the positive control, and the lineage whose owner
	// the refusal is measured against.
	userToken := d.createMCPUser(t)
	childA, credentialA := ownerScopedCredentialChild(t, d, dumps, userToken, "owner-reads-foreign-a")

	// A child of a different user. createRecallUser mints the user and its
	// token against the same database the daemon reads, which is what the
	// Connect mount authenticates.
	_, otherToken := createRecallUser(t, pool, "owner-reads-foreign-other-"+nextDaemonID(), false)
	childC := ownerScopedSpawnFundi(t, ctx, client, otherToken, "owner-reads-foreign-c")

	// Positive control: A reads ITSELF — the owner arm admits the caller, so a
	// read gate that refused everything could not pass the assertions below.
	if _, err := client.GetChild(ctx, authorize(credentialA, connect.NewRequest(
		&rafikiv1.GetChildRequest{ChildId: childA}))); err != nil {
		t.Fatalf("child A's GetChild of itself = %v, want success", err)
	}

	unknown := "c_no_such_child_" + strings.ReplaceAll(nextDaemonID(), "-", "")
	_, foreignErr := ownerScopedHistory(ctx, client, credentialA, childC)
	_, unknownErr := ownerScopedHistory(ctx, client, credentialA, unknown)

	ck.Require().NotNil(foreignErr, "GetHistory of a foreign owner's child did not fail")
	ck.Require().NotNil(unknownErr, "GetHistory of an unknown id did not fail")
	ck.Eq(connect.CodePermissionDenied, connect.CodeOf(foreignErr),
		"GetHistory of a foreign owner's child = %v, want %v", foreignErr, connect.CodePermissionDenied)
	ck.Eq(connect.CodePermissionDenied, connect.CodeOf(unknownErr),
		"GetHistory of an unknown id = %v, want %v", unknownErr, connect.CodePermissionDenied)
	ck.Eq(strings.ReplaceAll(foreignErr.Error(), childC, unknown), unknownErr.Error(),
		"a foreign child and an unknown id must answer identically, or ids can be probed")
}

// TestOwnerScopedReadsListChildrenScope: the scope parameter narrows a child
// credential's list to its subtree by default, widens it to its owner on
// demand with in_subtree marking what it may only read, and is a no-op for a
// user credential (which has no position in the tree).
//
// A child of a DIFFERENT user is live on the same daemon throughout: without it
// "the owner's set" and "every live agent" would be indistinguishable, and the
// owner filter could be dropped with every assertion still green.
//
// Fails if LIST_SCOPE_OWNER is served the subtree or every live agent, if
// in_subtree is left unset on an owner list, or if the flag leaks onto a caller
// it does not describe.
func TestOwnerScopedReadsListChildrenScope(t *testing.T) {
	t.Parallel()
	ck := assert.NewAborting(t)
	dsn := os.Getenv("RAFIKI_TEST_DSN")
	if dsn == "" {
		t.Skip("RAFIKI_TEST_DSN not set")
	}
	d, dumps := bootMCPChildDaemon(t)
	pool := openPool(t, dsn)
	client := d.connectClient()

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	userToken := d.createMCPUser(t)
	childA, credentialA := ownerScopedCredentialChild(t, d, dumps, userToken, "owner-reads-list-a")
	childB := ownerScopedSpawnFundi(t, ctx, client, userToken, "owner-reads-list-b")

	// Another user's live child, on this same daemon: it is exactly what the
	// owner filter must exclude.
	_, otherToken := createRecallUser(t, pool, "owner-reads-list-other-"+nextDaemonID(), false)
	childC := ownerScopedSpawnFundi(t, ctx, client, otherToken, "owner-reads-list-c")

	// Default and SUBTREE: A's own descendants only, and A spawned nothing.
	ck.Eq(0, len(ownerScopedList(t, ctx, client, credentialA, rafikiv1.ListScope_LIST_SCOPE_UNSPECIFIED)),
		"a child credential's default list must be its own subtree, which is empty here")
	ck.Eq(0, len(ownerScopedList(t, ctx, client, credentialA, rafikiv1.ListScope_LIST_SCOPE_SUBTREE)),
		"LIST_SCOPE_SUBTREE must be the same subtree the default serves")

	// OWNER: the sibling, marked as NOT in the subtree — and never the caller
	// itself, which knows its own row.
	ownerRows := ownerScopedList(t, ctx, client, credentialA, rafikiv1.ListScope_LIST_SCOPE_OWNER)
	ck.EqDiff([]string{childB}, ownerScopedSummaryIDs(ownerRows),
		"a child credential's owner list must be exactly its owner's other agents; got %v", ownerScopedSummaryIDs(ownerRows))
	if len(ownerRows) == 1 {
		ck.NotNil(ownerRows[0].InSubtree,
			"in_subtree must be SET on a child credential's owner list, even when false")
		ck.False(ownerRows[0].GetInSubtree(),
			"the sibling is outside the caller's subtree, so in_subtree must be false")
	}

	// A user credential has no position in the tree, so every value is the
	// same set and in_subtree says nothing.
	want := ownerScopedSorted([]string{childA, childB})
	for _, scope := range []rafikiv1.ListScope{
		rafikiv1.ListScope_LIST_SCOPE_UNSPECIFIED,
		rafikiv1.ListScope_LIST_SCOPE_SUBTREE,
		rafikiv1.ListScope_LIST_SCOPE_OWNER,
	} {
		rows := ownerScopedList(t, ctx, client, userToken, scope)
		ck.EqDiff(want, ownerScopedSorted(ownerScopedSummaryIDs(rows)),
			"a user credential's ListChildren(scope=%s) must be the same own-agent set (never %s)", scope, childC)
		for _, row := range rows {
			ck.Nil(row.InSubtree,
				"in_subtree must be unset for a user credential (scope=%s, child %s)", scope, row.GetChildId())
		}
	}
}

// TestOwnerScopedReadsStillCannotSteerSibling: the READ widening must not
// reach the ACT verbs. A's Send to the sibling it can now read is still
// PermissionDenied, with the act refusal — not the read one.
//
// Fails if Send is switched to AuthorizeRead, or if the read refusal text
// leaks into the act path (which would mean the two boundaries had merged).
func TestOwnerScopedReadsStillCannotSteerSibling(t *testing.T) {
	t.Parallel()
	ck := assert.NewAborting(t)
	d, dumps := bootMCPChildDaemon(t)
	client := d.connectClient()

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	userToken := d.createMCPUser(t)
	_, credentialA := ownerScopedCredentialChild(t, d, dumps, userToken, "owner-reads-steer-a")
	childB := ownerScopedSpawnFundi(t, ctx, client, userToken, "owner-reads-steer-b")

	_, err := client.Send(ctx, authorize(credentialA, connect.NewRequest(&rafikiv1.SendRequest{
		ChildId: childB,
		Mode:    rafikiv1.SendMode_SEND_MODE_PROMPT,
		Blocks: []*rafikiv1.ContentBlock{{
			Block: &rafikiv1.ContentBlock_Text{Text: &rafikiv1.TextBlock{Text: "you can read me, not steer me"}},
		}},
	})))
	ck.Eq(connect.CodePermissionDenied, connect.CodeOf(err),
		"A's Send to a readable sibling = %v, want %v", err, connect.CodePermissionDenied)
	if err != nil {
		ck.True(strings.Contains(err.Error(), "not a descendant of yours"),
			"the act refusal must be the subtree sentence; got %q", err.Error())
		ck.False(strings.Contains(err.Error(), "not one you can read"),
			"the READ refusal answered an ACT verb, so the two boundaries merged; got %q", err.Error())
	}

	// The sibling is untouched: still live, and nothing was delivered to it.
	ck.NotEq(string(protocol.StatusExited), getChild(t, client, childB).GetStatus(),
		"the refused Send changed the sibling's status")
}

// TestOwnerScopedReadsRecallParity: for one child credential, the conversation
// ids ConversationSearch reaches equal the ones Recall reaches — both are the
// OWNER's corpus, not the caller's subtree. Recall already gave a child exactly
// that reach (ownerScoped, recallOwner); this pins that the conversation reads
// now agree with it.
//
// A second conversation carrying the same per-run term belongs to a DIFFERENT
// user, and an admin's read of the term is the control that both rows are
// otherwise findable: without it, two empty answers would compare equal and the
// test would pass for the wrong reason.
//
// Fails if childConversationScope returns ScopeSubtree for an owned lineage
// (search would miss the owner's own row) or ScopeAll / a wider scope (search
// would return the other user's row).
func TestOwnerScopedReadsRecallParity(t *testing.T) {
	t.Parallel()
	ck := assert.NewAborting(t)
	dsn := os.Getenv("RAFIKI_TEST_DSN")
	if dsn == "" {
		t.Skip("RAFIKI_TEST_DSN not set")
	}
	d, dumps := bootMCPChildDaemon(t)
	pool := openPool(t, dsn)
	client := d.connectClient()

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	userToken := d.createMCPUser(t)
	childA, credentialA := ownerScopedCredentialChild(t, d, dumps, userToken, "owner-reads-parity-a")
	childB := ownerScopedSpawnFundi(t, ctx, client, userToken, "owner-reads-parity-b")
	ownerID := childOwnerUserID(t, pool, childA)

	// A different user, with a child of their own and a conversation carrying
	// the same term.
	otherID, otherToken := createRecallUser(t, pool, "owner-reads-parity-other-"+nextDaemonID(), false)
	childC := ownerScopedSpawnFundi(t, ctx, client, otherToken, "owner-reads-parity-c")
	// The admin is the positive control: daemon-wide reach, so it sees both
	// rows and proves the term matches them.
	_, adminToken := createRecallUser(t, pool, "owner-reads-parity-admin-"+nextDaemonID(), true)

	// One per-run term, in BOTH indexes: the earliest user message (what
	// ConversationSearch's Text filter reads) and a sealed window (what
	// recall's window source reads). The rows hang off a SIBLING and a foreign
	// user's child, so neither is in A's subtree.
	term := uniqueMarker("zqownsparity")
	ownConv := seedChildConversation(t, pool, dsn, childB, ownerID, term)
	seedWindowOn(t, pool, ownConv, ownerID, term+" the owner's captured conversation window")
	foreignConv := seedChildConversation(t, pool, dsn, childC, otherID, term)
	seedWindowOn(t, pool, foreignConv, otherID, term+" another user's captured conversation window")

	// Control: the admin reaches both rows through both verbs, so a nil answer
	// below is scope, never absence.
	ck.EqDiff(ownerScopedSorted([]string{ownConv, foreignConv}),
		ownerScopedSorted(rowIDs(ownerScopedSearch(t, ctx, client, adminToken, term))),
		"the admin's ConversationSearch must see both seeded conversations")
	adminRecall, err := client.Recall(ctx, authorize(adminToken, connect.NewRequest(
		&rafikiv1.RecallRequest{Query: term, Sources: []string{"window"}})))
	ck.NoError(err, "admin Recall")
	ck.EqDiff(ownerScopedSorted([]string{ownConv, foreignConv}),
		ownerScopedSorted(recallConversationIDs(adminRecall.Msg.GetHits())),
		"the admin's Recall must see both seeded windows")

	// The child credential: the owner's row through both verbs, and the other
	// user's row through neither.
	searchIDs := ownerScopedSorted(rowIDs(ownerScopedSearch(t, ctx, client, credentialA, term)))
	recallResp, err := client.Recall(ctx, authorize(credentialA, connect.NewRequest(
		&rafikiv1.RecallRequest{Query: term, Sources: []string{"window"}})))
	ck.NoError(err, "child A's Recall")
	recallIDs := ownerScopedSorted(recallConversationIDs(recallResp.Msg.GetHits()))

	ck.EqDiff([]string{ownConv}, searchIDs,
		"child A's ConversationSearch = %v, want exactly its OWNER's conversation %s", searchIDs, ownConv)
	ck.EqDiff([]string{ownConv}, recallIDs,
		"child A's Recall = %v, want exactly its OWNER's conversation %s", recallIDs, ownConv)
	ck.EqDiff(recallIDs, searchIDs,
		"parity: the conversations ConversationSearch reaches must equal the ones Recall reaches for the same child credential")
}

// recallConversationIDs maps a Recall answer to the conversation ids its hits
// name, deduplicated and sorted.
func recallConversationIDs(hits []*rafikiv1.RecallHit) []string {
	seen := map[string]struct{}{}
	var out []string
	for _, h := range hits {
		id := h.GetConversationId()
		if id == "" {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}
