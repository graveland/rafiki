// SPDX-License-Identifier: Apache-2.0

package llm

import (
	"context"
	"fmt"
	"math"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/packages/ssestream"
	"github.com/jackc/pgx/v5/pgxpool"

	"go.graveland.dev/rafiki/pkg/capture"
	convpkg "go.graveland.dev/rafiki/pkg/conv"
	"go.graveland.dev/rafiki/pkg/msg"
	msganthropic "go.graveland.dev/rafiki/pkg/msg/anthropic"
	"go.graveland.dev/rafiki/pkg/routing"
	"go.graveland.dev/rafiki/pkg/store"

	"github.com/multigres/testkit/assert"
)

// appendNode persists one message under the conversation's head, the fixture
// shape the trim and compaction tests seed before a send.
func appendNode(t *testing.T, pool *pgxpool.Pool, convID string, param anthropic.MessageParam) {
	t.Helper()
	ck := assert.NewAborting(t)
	message, err := msganthropic.FromParam(param)
	ck.NoError(err, "convert message")
	handle := convpkg.NewStore(store.NewConvBackend(pool), convpkg.Options{}).Open(convID, convpkg.AsOwner())
	_, err = handle.Append(context.Background(), convpkg.Append{FromHead: true, Nodes: []convpkg.NewNode{{Msg: message}}})
	ck.NoError(err, "append node")
}

// Integration tests need TimescaleDB (RAFIKI_TEST_DSN, see store/migrate_test.go).
func convTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	c := assert.NewAborting(t)
	dsn := os.Getenv("RAFIKI_TEST_DSN")
	if dsn == "" {
		c.Eq("", os.Getenv("RAFIKI_REQUIRE_DB"), "RAFIKI_TEST_DSN not set but RAFIKI_REQUIRE_DB is — the integration job must provide it")
		t.Skip("RAFIKI_TEST_DSN not set; skipping integration test")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, dsn)
	c.NoError(err, "connect admin")
	t.Cleanup(admin.Close)
	name := fmt.Sprintf("rafiki_llm_%d", time.Now().UnixNano())
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatalf("create scratch db: %v", err)
	}
	t.Cleanup(func() { _, _ = admin.Exec(context.Background(), "DROP DATABASE "+name+" WITH (FORCE)") })
	cfg, err := pgxpool.ParseConfig(dsn)
	c.NoError(err)
	cfg.ConnConfig.Database = name
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	c.NoError(err)
	t.Cleanup(pool.Close)
	c.NoError(store.Migrate(ctx, pool), "migrate")
	return pool
}

func testClient(t *testing.T, pool *pgxpool.Pool, sender Sender) *Client {
	t.Helper()
	c, err := NewClient(
		WithProviderSender("anthropic", sender),
		WithStore(pool),
		WithCatalog(seededCatalog(t)),
		WithDefaultModel("haiku-latest"), // conversations created without Model() intend this default
		WithLogger(testLogger(t)),
	)
	assert.NewAborting(t).NoError(err)
	return c
}

// TestSendParamsSetsSessionIDFromConversationID proves the fundi-native path
// (Conversation.Send -> Client.SendParams), not just the reverse-proxy face,
// threads OpenRouter's sticky-routing session id through — and that the value
// is rafiki's own conversation id, matching what pkg/server/proxy.go sends for
// passthrough clients. pkg/llm/sender_provider_test.go covers the other half:
// that a session id on the context really reaches OpenRouter as the
// x-session-id header.
func TestSendParamsSetsSessionIDFromConversationID(t *testing.T) {
	ck := assert.NewCollecting(t)
	pool := convTestPool(t)
	ctx := context.Background()

	openrouter := &scriptedSender{scripts: []func(anthropic.MessageNewParams) (*anthropic.Message, error){
		respondText("ok"),
	}}
	c, err := NewClient(
		WithProviderSender("anthropic", &scriptedSender{}),
		WithProviderSender("openrouter", openrouter),
		WithStore(pool),
		WithCatalog(seededCatalog(t)),
		WithLogger(testLogger(t)),
	)
	ck.Require().NoError(err)

	conv, err := c.Conversation(ctx, NewConversation("", "test"), Model("openrouter/moonshotai/kimi-k3"))
	ck.Require().NoError(err, "Conversation")
	if _, err := conv.Send(ctx, UserText("hi")); err != nil {
		t.Fatalf("Send: %v", err)
	}

	ck.Require().Len(openrouter.lastCtx, 1, "openrouter sender called %d times, want 1", len(openrouter.lastCtx))
	sid, _ := openrouter.lastCtx[0].Value(sessionIDContextKey{}).(string)
	ck.Require().NotEq("", sid, "no session id reached the OpenRouter sender's context")
	ck.Eq(conv.ID, sid, "session id")
}

// TestConversationCostRollsUpCompletedTurnsPerModel locks down the fundi-side
// "cost_total" rollup: completed turns are priced at each turn's own served
// model, summed across the conversation. It is the llm.Client half of the same
// arithmetic the proxy's costFields does, so a conversation that changes model
// mid-flight is billed per model, not flat.
func TestConversationCostRollsUpCompletedTurnsPerModel(t *testing.T) {
	ck := assert.NewCollecting(t)
	pool := convTestPool(t)
	ctx := context.Background()

	// Served model + fixed token counts: input 100, output 50 per turn.
	const served = `{"id":"msg_x","type":"message","role":"assistant","model":"claude-sonnet-5",
		"content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn",
		"usage":{"input_tokens":100,"output_tokens":50,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}}`
	sender := &scriptedSender{scripts: []func(anthropic.MessageNewParams) (*anthropic.Message, error){
		func(anthropic.MessageNewParams) (*anthropic.Message, error) { return cannedMessage(served), nil },
		func(anthropic.MessageNewParams) (*anthropic.Message, error) { return cannedMessage(served), nil },
	}}

	cat := routing.NewModelCatalog(nil, time.Hour, testLogger(t))
	cat.SeedForTest([]routing.CatalogEntry{
		{ID: "anthropic/claude-sonnet-5", Created: 2, Pricing: &routing.ModelPricing{
			PromptUSD: 0.001, CompletionUSD: 0.002,
		}},
	})
	c, err := NewClient(
		WithProviderSender("anthropic", sender),
		WithStore(pool),
		WithCatalog(cat),
		WithDefaultModel("haiku-latest"),
		WithLogger(testLogger(t)),
	)
	ck.Require().NoError(err)
	conv, err := c.Conversation(ctx, NewConversation("", "test"), Model("sonnet-latest"))
	ck.Require().NoError(err, "Conversation")
	if _, err := conv.Send(ctx, UserText("one")); err != nil {
		t.Fatalf("Send 1: %v", err)
	}
	if _, err := conv.Send(ctx, UserText("two")); err != nil {
		t.Fatalf("Send 2: %v", err)
	}

	got := c.ConversationCost(ctx, conv.ID)
	// 2 turns × (100×0.001 + 50×0.002) = 2 × 0.2 = 0.4
	want := 0.4
	ck.LessOrEqual(1e-9, math.Abs(got-want), "ConversationCost = %v, want %v", got, want)

	// A store-less client reports 0 rather than a bogus figure.
	bare, err := NewClient(WithProviderSender("anthropic", sender), WithDefaultModel("haiku-latest"))
	ck.Require().NoError(err)
	ck.Eq(0, bare.ConversationCost(ctx, "some-id"), "ConversationCost without a store")
}

func TestConversationSendPersistsBothGranularities(t *testing.T) {
	ck := assert.NewCollecting(t)
	pool := convTestPool(t)
	ctx := context.Background()
	sender := &scriptedSender{scripts: []func(anthropic.MessageNewParams) (*anthropic.Message, error){
		respondText("first reply"),
		respondText("second reply"),
	}}
	c := testClient(t, pool, sender)

	conv, err := c.Conversation(ctx,
		NewConversation("", "test"),
		Model("sonnet-latest"), // must resolve at creation via the seeded catalog
		SystemText("you are a test"),
	)
	ck.Require().NoError(err, "Conversation")

	resp, err := conv.Send(ctx, UserText("hello"))
	ck.Require().NoError(err, "Send")
	ck.Require().Eq("first reply", resp.Content[0].Text, "resp =")

	// Model resolved once at creation.
	ck.Eq("claude-sonnet-5", string(sender.lastReq[0].Model), "model")
	// Default cache policy: one 5m breakpoint (empty TTL) on the last system
	// block. Callers wanting 1h opt in via WithCache (see DefaultCachePolicy).
	sys := sender.lastReq[0].System
	ck.False(len(sys) != 1 || sys[0].CacheControl.Type == "" || sys[0].CacheControl.TTL != "", "system breakpoint wrong (want ephemeral/5m-default): %+v", sys)

	// Second send loads history: request must carry 3 messages.
	if _, err := conv.Send(ctx, UserText("and again")); err != nil {
		t.Fatalf("Send 2: %v", err)
	}
	ck.Eq(3, len(sender.lastReq[1].Messages), "second request has")

	// Message granularity: 4 rows (user, assistant, user, assistant).
	var msgCount int
	var roles []string
	rows, err := pool.Query(ctx, `SELECT role FROM conversations.conversation_message
		WHERE conversation_id=$1::uuid ORDER BY ordinal`, conv.ID)
	ck.Require().NoError(err)
	for rows.Next() {
		var r string
		_ = rows.Scan(&r)
		roles = append(roles, r)
		msgCount++
	}
	rows.Close()
	ck.False(msgCount != 4 || strings.Join(roles, ",") != "user,assistant,user,assistant", "message rows = %v, want user,assistant,user,assistant", roles)

	// Turn granularity: 2 complete turns, protocol anthropic, prefix_hash set,
	// and prefix_hash IDENTICAL across the conversation (stable prefix).
	var turns, complete, hashes int
	ck.Require().NoError(pool.QueryRow(ctx, `SELECT count(*), count(*) FILTER (WHERE status='complete'),
		count(DISTINCT prefix_hash) FROM conversations.conversation_turn
		WHERE conversation_id=$1::uuid AND protocol='anthropic'`, conv.ID).Scan(&turns, &complete, &hashes))
	ck.False(turns != 2 || complete != 2, "turns=%d complete=%d, want 2/2", turns, complete)
	ck.Eq(1, hashes, "prefix_hash values across the conversation")

	// Assistant rows carry token counts.
	var inTok int64
	if err := pool.QueryRow(ctx, `SELECT input_tokens FROM conversations.conversation_message
		WHERE conversation_id=$1::uuid AND ordinal=1`, conv.ID).Scan(&inTok); err != nil || inTok != 10 {
		t.Errorf("assistant input_tokens = %d err=%v, want 10", inTok, err)
	}
}

func TestConversationTrimRetryKeepsPrefixAndRows(t *testing.T) {
	ck := assert.NewCollecting(t)
	pool := convTestPool(t)
	ctx := context.Background()

	// Fail the first attempt as prompt-too-large, succeed the second.
	sender := &scriptedSender{scripts: []func(anthropic.MessageNewParams) (*anthropic.Message, error){
		respondErr(promptTooLargeErr()),
		respondText("after trim"),
	}}
	c := testClient(t, pool, sender)
	conv, err := c.Conversation(ctx, NewConversation("", "test"), Model("claude-haiku-4-5"),
		SystemText("sys"))
	ck.Require().NoError(err)

	// Seed enough history that the default policy can drop the middle.
	big := strings.Repeat("y", 80*1024)
	for i := range 5 {
		role := anthropic.MessageParamRoleUser
		if i%2 == 1 {
			role = anthropic.MessageParamRoleAssistant
		}
		msg := anthropic.MessageParam{Role: role,
			Content: []anthropic.ContentBlockParamUnion{anthropic.NewTextBlock(big)}}
		appendNode(t, pool, conv.ID, msg)
	}

	if _, err := conv.Send(ctx, UserText("question")); err != nil {
		t.Fatalf("Send with trim-retry: %v", err)
	}

	// The retry sent fewer messages than the first attempt.
	ck.Require().Len(sender.lastReq, 2, "attempts = %d, want 2", len(sender.lastReq))
	ck.Less(len(sender.lastReq[0].Messages), len(sender.lastReq[1].Messages), "retry not trimmed")

	// prefix_hash identical across the failed and successful attempts —
	// trimming must not touch the cached tools+system prefix.
	var hashes, turns int
	ck.Require().NoError(pool.QueryRow(ctx, `SELECT count(DISTINCT prefix_hash), count(*)
		FROM conversations.conversation_turn WHERE conversation_id=$1::uuid`, conv.ID).Scan(&hashes, &turns))
	ck.Eq(2, turns, "turn rows")
	ck.Eq(1, hashes, "prefix_hash differs across trim-retry attempts (")
	// One error row (attempt 1) + one complete (attempt 2).
	var errRows, completeRows int
	_ = pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE status='error'),
		count(*) FILTER (WHERE status='complete') FROM conversations.conversation_turn
		WHERE conversation_id=$1::uuid`, conv.ID).Scan(&errRows, &completeRows)
	ck.False(errRows != 1 || completeRows != 1, "turn statuses error=%d complete=%d, want 1/1", errRows, completeRows)

	// Trim is request-assembly-time only: all 5 seeded rows + user + assistant
	// still present, untouched.
	var msgRows int
	ck.Require().NoError(pool.QueryRow(ctx, `SELECT count(*) FROM conversations.conversation_message
		WHERE conversation_id=$1::uuid`, conv.ID).Scan(&msgRows))
	ck.Eq(7, msgRows, "stored message rows")
}

func TestUnfinishedConversationsAndResumeCounter(t *testing.T) {
	ck := assert.NewCollecting(t)
	pool := convTestPool(t)
	ctx := context.Background()
	sender := &scriptedSender{scripts: []func(anthropic.MessageNewParams) (*anthropic.Message, error){
		respondText("ok"),
	}}
	c := testClient(t, pool, sender)

	// Conversation A: a pending turn (call never resolved).
	convA, err := c.Conversation(ctx, NewConversation("", "test"))
	ck.Require().NoError(err)
	if _, _, err := c.capture.InsertTurnIntent(ctx, capture.TurnIntent{
		ConversationID: convA.ID, Ordinal: 1, Model: "claude-haiku-4-5",
	}); err != nil {
		t.Fatal(err)
	}

	// Conversation B: an assistant tool_use with no matching tool_result.
	convB, err := c.Conversation(ctx, NewConversation("", "test"))
	ck.Require().NoError(err)
	assistant := anthropic.NewAssistantMessage(
		anthropic.NewToolUseBlock("toolu_orphan", map[string]any{"a": 1}, "service_status"))
	appendNode(t, pool, convB.ID, assistant)
	// Conversation C: clean (a full Send).
	convC, err := c.Conversation(ctx, NewConversation("", "test"))
	ck.Require().NoError(err)
	if _, err := convC.Send(ctx, UserText("hi")); err != nil {
		t.Fatal(err)
	}

	unfinished, err := store.UnfinishedConversations(ctx, pool, store.DrivenByServer)
	ck.Require().NoError(err)
	byID := map[string]store.Unfinished{}
	for _, u := range unfinished {
		byID[u.ConversationID] = u
	}
	ck.Require().Len(byID, 2, "unfinished = %d conversations, want 2 (got %+v)", len(byID), unfinished)
	if u := byID[convA.ID]; u.PendingTurns != 1 {
		t.Errorf("conv A pending turns = %d, want 1", u.PendingTurns)
	}
	if u := byID[convB.ID]; len(u.OrphanToolUses) != 1 || u.OrphanToolUses[0] != "toolu_orphan" {
		t.Errorf("conv B orphans = %v, want [toolu_orphan]", u.OrphanToolUses)
	}
	_, ok := byID[convC.ID]
	ck.False(ok, "clean conversation reported as unfinished")

	// Resume counter round-trip.
	if n, err := store.IncrementResumeAttempts(ctx, pool, convB.ID); err != nil || n != 1 {
		t.Errorf("first increment = %d err=%v, want 1", n, err)
	}
	n, _ := store.IncrementResumeAttempts(ctx, pool, convB.ID)
	ck.Eq(2, n, "second increment")
}

// TestConversationStoresPrefixOnChange proves the in-process (direct) path
// records turn prefix metadata for parity with the proxy path: prefix_content
// on the first turn, NULL on an unchanged second turn (same static prefix), and
// cache_breakpoints on every turn.
func TestConversationStoresPrefixOnChange(t *testing.T) {
	ck := assert.NewCollecting(t)
	pool := convTestPool(t)
	ctx := context.Background()
	sender := &scriptedSender{scripts: []func(anthropic.MessageNewParams) (*anthropic.Message, error){
		respondText("one"), respondText("two"),
	}}
	c := testClient(t, pool, sender)

	conv, err := c.Conversation(ctx, NewConversation("", "test"),
		Model("sonnet-latest"), SystemText("you are a test"))
	ck.Require().NoError(err, "Conversation")
	if _, err := conv.Send(ctx, UserText("hello")); err != nil {
		t.Fatalf("Send 1: %v", err)
	}
	if _, err := conv.Send(ctx, UserText("again")); err != nil {
		t.Fatalf("Send 2: %v", err)
	}

	rows, err := pool.Query(ctx,
		`SELECT prefix_content IS NOT NULL, cache_breakpoints IS NOT NULL
		   FROM conversations.conversation_turn WHERE conversation_id=$1::uuid ORDER BY created_at`, conv.ID)
	ck.Require().NoError(err)
	defer rows.Close()
	type row struct{ hasPrefix, hasBreakpoints bool }
	var got []row
	for rows.Next() {
		var r row
		ck.Require().NoError(rows.Scan(&r.hasPrefix, &r.hasBreakpoints))
		got = append(got, r)
	}
	ck.Require().Len(got, 2, "turns = %d, want 2", len(got))
	ck.True(got[0].hasPrefix, "turn 1 prefix_content is NULL, want the request envelope stored")
	ck.False(got[1].hasPrefix, "turn 2 prefix_content is set, want NULL (prefix unchanged from turn 1)")
	for i, r := range got {
		if !r.hasBreakpoints {
			t.Errorf("turn %d cache_breakpoints is NULL, want recorded", i+1)
		}
	}
}

func TestWithCache_EachApplicationGetsItsOwnPolicy(t *testing.T) {
	c := assert.NewAborting(t)
	// One reused ConvOption must not share a CachePolicy pointer across
	// conversations: Conversation() clamps Breakpoints through cfg.cache, and
	// with a shared pointer the first clamp would leak into later uses.
	opt := WithCache(CachePolicy{SystemTTL: Cache1h, MessagesTTL: Cache5m, Breakpoints: 9})
	var cfg1, cfg2 convConfig
	opt(&cfg1)
	opt(&cfg2)
	c.NotEq(cfg2.cache, cfg1.cache, "WithCache applications share one CachePolicy pointer")
	cfg1.cache.Breakpoints = 3 // what Conversation()'s clamp does
	c.Eq(9, cfg2.cache.Breakpoints, "clamping one conversation's policy leaked: Breakpoints")
}

func TestBlockWithCacheControl_CoversEveryCacheableUnionMember(t *testing.T) {
	c := assert.NewCollecting(t)
	// Exhaustiveness guard against SDK bumps: if a new ContentBlockParamUnion
	// member carries cache_control and blockWithCacheControl doesn't handle
	// it, a stale marker on reloaded history couldn't be cleared and a
	// request could exceed the API's 4-breakpoint limit.
	marker := anthropic.NewCacheControlEphemeralParam()
	ut := reflect.TypeOf(anthropic.ContentBlockParamUnion{})
	tested := 0
	for i := 0; i < ut.NumField(); i++ {
		f := ut.Field(i)
		if !strings.HasPrefix(f.Name, "Of") || f.Type.Kind() != reflect.Pointer {
			continue
		}
		cc, ok := f.Type.Elem().FieldByName("CacheControl")
		if !ok || cc.Type != reflect.TypeOf(marker) {
			continue // member cannot carry cache_control (thinking blocks etc.)
		}
		member := reflect.New(f.Type.Elem())
		member.Elem().FieldByName("CacheControl").Set(reflect.ValueOf(marker))
		var b anthropic.ContentBlockParamUnion
		reflect.ValueOf(&b).Elem().Field(i).Set(member)

		got := blockWithCacheControl(b, anthropic.CacheControlEphemeralParam{})
		ptr := got.GetCacheControl()
		c.False(ptr == nil || ptr.Type != "" || ptr.TTL != "", "%s: blockWithCacheControl did not clear cache_control (got %+v) — add it to the switch", f.Name, ptr)
		if orig := b.GetCacheControl(); orig == nil || orig.Type == "" {
			t.Errorf("%s: input block was mutated; blockWithCacheControl must copy", f.Name)
		}
		tested++
	}
	c.Require().GreaterOrEqual(14, tested, "only") // the cacheable members as of anthropic-sdk-go v1.56
}

func TestConversationWithToolChoice(t *testing.T) {
	ck := assert.NewCollecting(t)
	pool := convTestPool(t)
	ctx := context.Background()
	sender := &scriptedSender{scripts: []func(anthropic.MessageNewParams) (*anthropic.Message, error){
		respondText("tool choice test"),
		respondText("tool choice test 2"),
	}}
	c := testClient(t, pool, sender)

	conv, err := c.Conversation(ctx,
		NewConversation("", "test"),
		Model("sonnet-latest"),
		SystemText("you are a test"))
	ck.Require().NoError(err, "Conversation")

	tools := []anthropic.ToolUnionParam{
		anthropic.ToolUnionParamOfTool(
			anthropic.ToolInputSchemaParam{Type: "object"},
			"report_findings",
		),
	}

	resp, err := conv.Send(ctx, UserText("test"), WithTools(tools), WithToolChoice("report_findings"))
	ck.Require().NoError(err, "Send")
	ck.Require().Eq("tool choice test", resp.Content[0].Text, "unexpected response")

	ck.Require().NotNil(sender.lastReq[0].ToolChoice.OfTool, "ToolChoice.OfTool is nil, expected tool choice to be set")
	ck.Eq("report_findings", sender.lastReq[0].ToolChoice.OfTool.Name, "ToolChoice.OfTool.Name")

	// Send without WithToolChoice should have zero ToolChoice
	resp2, err := conv.Send(ctx, UserText("test 2"))
	ck.Require().NoError(err, "Send 2")
	ck.Require().Eq("tool choice test 2", resp2.Content[0].Text, "unexpected response 2")

	ck.Nil(sender.lastReq[1].ToolChoice.OfTool, "ToolChoice.OfTool should be nil when WithToolChoice not used, got")
}

// Attribution is persisted as a conversations.users id, never a name: the
// username is resolved at read time through the FK. This covers the whole
// round trip — NewConversation's owner id onto conversation.owner_user_id,
// WithAuthorUserID onto conversation_turn.author_user_id, and both back out
// through the views as usernames.
func TestConversationPersistsUserIDsNotNames(t *testing.T) {
	ck := assert.NewCollecting(t)
	pool := convTestPool(t)
	ctx := context.Background()

	var userID string
	ck.Require().NoError(pool.QueryRow(ctx,
		`INSERT INTO conversations.users (username, token_sha256)
		 VALUES ('brent', 'digest-llm') RETURNING id::text`).Scan(&userID), "insert user")

	sender := &scriptedSender{scripts: []func(anthropic.MessageNewParams) (*anthropic.Message, error){
		respondText("reply"),
	}}
	c := testClient(t, pool, sender)

	conv, err := c.Conversation(ctx, NewConversation(userID, "test"), Model("sonnet-latest"))
	ck.Require().NoError(err, "Conversation")
	if _, err := conv.Send(ctx, UserText("hello"), WithAuthorUserID(userID)); err != nil {
		t.Fatalf("Send: %v", err)
	}

	var ownerUserID, authorUserID string
	ck.Require().NoError(pool.QueryRow(ctx,
		`SELECT c.owner_user_id::text, t.author_user_id::text
		   FROM conversations.conversation c
		   JOIN conversations.conversation_turn t ON t.conversation_id = c.id
		  WHERE c.id = $1::uuid`, conv.ID).Scan(&ownerUserID, &authorUserID), "read attribution columns")
	ck.False(ownerUserID != userID || authorUserID != userID, "owner_user_id=%q author_user_id=%q, want %s for both", ownerUserID, authorUserID, userID)

	var ownerName, authorName string
	ck.Require().NoError(pool.QueryRow(ctx,
		`SELECT owner_username, author_username FROM conversations.v_turn
		  WHERE conversation_id = $1::uuid`, conv.ID).Scan(&ownerName, &authorName), "read v_turn usernames")
	ck.False(ownerName != "brent" || authorName != "brent", "v_turn owner_username=%q author_username=%q, want brent for both", ownerName, authorName)
}

// An unattributed conversation — the anonymous proxy path, and every
// daemon-run analyzer conversation — must persist SQL NULL. The columns are
// UUID foreign keys, so an empty Go string reaching them as the empty
// string is a cast error at insert time, not merely a mislabelled row.
func TestConversationEmptyUserIDPersistsAsNull(t *testing.T) {
	ck := assert.NewCollecting(t)
	pool := convTestPool(t)
	ctx := context.Background()
	sender := &scriptedSender{scripts: []func(anthropic.MessageNewParams) (*anthropic.Message, error){
		respondText("reply"),
	}}
	c := testClient(t, pool, sender)

	conv, err := c.Conversation(ctx, NewConversation("", "test"), Model("sonnet-latest"))
	ck.Require().NoError(err, "Conversation")
	if _, err := conv.Send(ctx, UserText("hello")); err != nil {
		t.Fatalf("Send: %v", err)
	}

	var owner, author *string
	ck.Require().NoError(pool.QueryRow(ctx,
		`SELECT c.owner_user_id::text, t.author_user_id::text
		   FROM conversations.conversation c
		   JOIN conversations.conversation_turn t ON t.conversation_id = c.id
		  WHERE c.id = $1::uuid`, conv.ID).Scan(&owner, &author), "read attribution columns")
	ck.False(owner != nil || author != nil, "owner_user_id=%v author_user_id=%v, want NULL for both", owner, author)
}

// Images go FIRST, and an empty text block is omitted rather than sent: a
// content block with no content is a 400 on some providers and noise on the
// rest, and an attachment alone is a legitimate message.
func TestUserContentOrdersImagesFirst(t *testing.T) {
	c := assert.NewCollecting(t)
	blocks := UserContent("what is this?", []UserImage{
		{MediaType: "image/png", Data: []byte("bytes")},
	})
	c.Require().Len(blocks, 2, "got %d blocks, want image then text", len(blocks))
	c.NotNil(blocks[0].OfImage, "first block is not the image")
	c.NotNil(blocks[1].OfText, "second block is not the text")

	only := UserContent("", []UserImage{{MediaType: "image/png", Data: []byte("b")}})
	if len(only) != 1 || only[0].OfImage == nil {
		t.Errorf("an image with no text must send just the image, got %d blocks", len(only))
	}

	c.Eq(1, len(UserContent("hi", nil)), "text with no images")
	// An image carrying no bytes is skipped rather than sent as an empty block.
	c.Eq(1, len(UserContent("hi", []UserImage{{MediaType: "image/png"}})), "empty image data must be skipped, got")
}

// respondWithProvider returns a canned response carrying OpenRouter's
// non-standard top-level "provider" field — the shape a real OpenRouter body
// has, and the only place the serving provider's name arrives.
func respondWithProvider(provider string) func(anthropic.MessageNewParams) (*anthropic.Message, error) {
	return func(anthropic.MessageNewParams) (*anthropic.Message, error) {
		return cannedMessage(`{"id":"msg_or","type":"message","role":"assistant","model":"deepseek/deepseek-v4-pro",` +
			`"content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn",` +
			`"usage":{"input_tokens":10,"output_tokens":5},"provider":"` + provider + `"}`), nil
	}
}

// servedProviderOfLastTurn reads the captured turn's served_provider straight
// out of the database — the value completeTurn wrote. A NULL column reads as
// "" (not reported).
func servedProviderOfLastTurn(t *testing.T, pool *pgxpool.Pool, convID string) string {
	t.Helper()
	var served *string
	assert.NewAborting(t).NoError(pool.QueryRow(context.Background(), `SELECT served_provider
		 FROM conversations.conversation_turn
		 WHERE conversation_id=$1::uuid
		 ORDER BY created_at DESC LIMIT 1`, convID).Scan(&served), "read served_provider")
	if served == nil {
		return ""
	}
	return *served
}

// TestCaptureRecordsServedProvider proves the non-streaming completion path
// records which OpenRouter provider served the turn: a response body with a
// top-level "provider" field must land in conversation_turn.served_provider.
func TestCaptureRecordsServedProvider(t *testing.T) {
	ck := assert.NewCollecting(t)
	pool := convTestPool(t)
	ctx := context.Background()
	sender := &scriptedSender{scripts: []func(anthropic.MessageNewParams) (*anthropic.Message, error){
		respondWithProvider("Together"),
	}}
	c := testClient(t, pool, sender)
	conv, err := c.Conversation(ctx, NewConversation("", "test"))
	ck.Require().NoError(err)
	if _, err := conv.Send(ctx, UserText("hi")); err != nil {
		t.Fatal(err)
	}
	ck.Eq("Together", servedProviderOfLastTurn(t, pool, conv.ID), "captured served_provider")
}

// TestCaptureRecordsServedProviderStreaming proves the same for the streaming
// path, where the provider name arrives inside message_start's message and
// must survive Accumulate into the final message (the ProviderGuard's
// streaming Observe already relies on this).
func TestCaptureRecordsServedProviderStreaming(t *testing.T) {
	ck := assert.NewCollecting(t)
	pool := convTestPool(t)
	ctx := context.Background()
	start := sseEvent("message_start", `{"type":"message_start","message":{"id":"msg_or","type":"message",`+
		`"role":"assistant","model":"deepseek/deepseek-v4-pro","content":[],`+
		`"usage":{"input_tokens":10,"output_tokens":0},"provider":"Together"}}`)
	events := append([]ssestream.Event{start, contentBlockStartEvent()},
		textDeltaEvent("hi"), contentBlockStopEvent(), messageDeltaEvent(), messageStopEvent())
	sender := &fakeStreamingSender{scripts: []streamScript{{events: events}}}
	c := testClient(t, pool, sender)
	conv, err := c.Conversation(ctx, NewConversation("", "test"))
	ck.Require().NoError(err)
	if _, err := conv.Send(ctx, UserText("hi"), WithStreamHandler(func(anthropic.MessageStreamEventUnion) {})); err != nil {
		t.Fatal(err)
	}
	ck.Eq("Together", servedProviderOfLastTurn(t, pool, conv.ID), "captured served_provider")
}

// TestRetractTailUserLeavesLeaf: a retract is a head move, never a delete. The
// oversized first prompt stops being the working set, its row survives as a
// leaf, and a smaller retry becomes its sibling rather than a duplicate.
func TestRetractTailUserLeavesLeaf(t *testing.T) {
	ck := assert.NewCollecting(t)
	pool := convTestPool(t)
	ctx := context.Background()
	c := testClient(t, pool, &scriptedSender{scripts: []func(anthropic.MessageNewParams) (*anthropic.Message, error){
		respondText("ok"),
	}})
	conv, err := c.Conversation(ctx, NewConversation("", "test"), Model("claude-haiku-4-5"))
	ck.Require().NoError(err)

	u, err := conv.AppendUser(ctx, UserText(strings.Repeat("x", 4096)))
	ck.Require().NoError(err, "AppendUser")
	ck.Require().NoError(conv.RetractTailUser(ctx, u.ID), "RetractTailUser")

	hist, err := conv.History(ctx)
	ck.Require().NoError(err, "History")
	ck.Empty(hist, "the retracted prompt is still on the working path")

	var rows int
	ck.Require().NoError(pool.QueryRow(ctx, `SELECT count(*) FROM conversations.conversation_message
		WHERE conversation_id=$1::uuid`, conv.ID).Scan(&rows), "count rows")
	ck.Eq(1, rows, "the retracted row must survive")

	small, err := conv.AppendUser(ctx, UserText("small"))
	ck.Require().NoError(err, "AppendUser after retract")
	ck.NotEq(u.ID, small.ID, "the retry must write a new row")
	hist, err = conv.History(ctx)
	ck.Require().NoError(err, "History")
	ck.Require().Len(hist, 1, "working path")
	ck.Eq(small.ID, hist[0].ID, "working path is the smaller prompt")

	// Both rows hang off the same empty root slot: siblings, not a chain.
	ck.Require().NoError(pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE parent_id IS NULL)
		FROM conversations.conversation_message WHERE conversation_id=$1::uuid`, conv.ID).Scan(&rows), "count roots")
	ck.Eq(2, rows, "the retracted row and its retry are both roots")

	// The retracted row is no longer the head, so it cannot be retracted twice.
	ck.Error(conv.RetractTailUser(ctx, u.ID), "retracting a row that is not the head")
}

// TestResumeContinueIsIdempotent: a resumed conversation cannot duplicate what it
// already holds — not by reloading it, not by re-seeding the prefix it was given,
// and not by continuing it again.
//
// Store-backed, all three are no-ops: the seed chain matches by node hash, and a
// re-sent reply lands under the same parent with the same content, so it IS the
// node already there and the tree hands it back instead of writing a second copy.
// A store-less conversation has no hashes to dedupe by, so ordinal placement is
// the invariant instead: message i lives at ordinal i, so re-seeding a prefix
// rewrites nothing.
func TestResumeContinueIsIdempotent(t *testing.T) {
	t.Run("store", func(t *testing.T) {
		ck := assert.NewCollecting(t)
		pool := convTestPool(t)
		ctx := context.Background()
		c := testClient(t, pool, &scriptedSender{scripts: []func(anthropic.MessageNewParams) (*anthropic.Message, error){
			respondText("the reply"),
			respondText("the reply"),
		}})
		const ref = "resume-idempotent"
		conv, err := c.Conversation(ctx, NewConversation("", "test"), ByExternalRef(ref), Model("claude-haiku-4-5"))
		ck.Require().NoError(err)

		if _, err := conv.Send(ctx, UserText("hello")); err != nil {
			t.Fatalf("Send: %v", err)
		}
		before, err := conv.History(ctx)
		ck.Require().NoError(err, "History")
		ck.Require().Len(before, 2, "user + assistant")

		reloaded, err := c.Conversation(ctx, NewConversation("", "test"), ByExternalRef(ref), Model("claude-haiku-4-5"))
		ck.Require().NoError(err)
		ck.Eq(conv.ID, reloaded.ID, "same conversation")
		after, err := reloaded.History(ctx)
		ck.Require().NoError(err, "History after reload")
		ck.EqDeep(before, after, "the reloaded working path")

		// Re-seed the prefix the conversation was given, then continue again: the
		// seed leaves the head on the row the reply was written under, so the
		// re-sent reply is the node that is already there.
		ck.Require().NoError(reloaded.SeedHistory(ctx, []Message{
			anthropic.NewUserMessage(anthropic.NewTextBlock("hello")),
		}), "re-seeding the same prefix")
		if _, err := reloaded.Continue(ctx); err != nil {
			t.Fatalf("Continue: %v", err)
		}
		resumed, err := reloaded.History(ctx)
		ck.Require().NoError(err, "History after re-seed and continue")
		ck.EqDeep(before, resumed, "a re-seed and a re-continue must not change the working path")

		var rows int
		ck.Require().NoError(pool.QueryRow(ctx, `SELECT count(*) FROM conversations.conversation_message
			WHERE conversation_id=$1::uuid`, conv.ID).Scan(&rows), "count rows")
		ck.Eq(2, rows, "reloading, re-seeding and re-continuing must write nothing")
	})

	t.Run("memory", func(t *testing.T) {
		ck := assert.NewCollecting(t)
		ctx := context.Background()
		sender := &scriptedSender{scripts: []func(anthropic.MessageNewParams) (*anthropic.Message, error){
			respondText("the reply"),
		}}
		conv := newTestConversation(t, sender)
		r0 := anthropic.NewUserMessage(anthropic.NewTextBlock("hello"))
		r1 := anthropic.NewAssistantMessage(anthropic.NewTextBlock("the reply"))
		r2 := anthropic.NewUserMessage(anthropic.NewTextBlock("again"))

		ck.Require().NoError(conv.SeedHistory(ctx, []Message{r0}), "seed the first message")
		ck.Require().NoError(conv.SeedHistory(ctx, []Message{r0, r1, r2}), "complete the prefix")
		seeded, err := conv.History(ctx)
		ck.Require().NoError(err, "History")
		ck.Len(seeded, 3, "completing a seeded prefix must not duplicate it")
		for i, m := range seeded {
			ck.Eq(i, m.Ordinal, "message %d sits at its own ordinal", i)
		}

		if _, err := conv.Continue(ctx); err != nil {
			t.Fatalf("Continue: %v", err)
		}
		before, err := conv.History(ctx)
		ck.Require().NoError(err, "History")
		ck.Require().Len(before, 4, "the seed plus the reply")

		ck.Require().NoError(conv.SeedHistory(ctx, []Message{r0, r1, r2}), "re-seed the same prefix")
		if _, err := conv.Continue(ctx); err != nil {
			t.Fatalf("Continue after the re-seed: %v", err)
		}
		after, err := conv.History(ctx)
		ck.Require().NoError(err, "History after the re-seed")
		ck.EqDeep(before, after, "a re-seed and a re-continue must not change the working path")
	})
}

// TestFundiAssistantNodeCarriesProviderMessageID: an assistant node records the
// upstream message id, which is what reconciles a stored reply with the
// provider's own record. A user row has none.
func TestFundiAssistantNodeCarriesProviderMessageID(t *testing.T) {
	ck := assert.NewCollecting(t)
	pool := convTestPool(t)
	ctx := context.Background()
	sender := &scriptedSender{scripts: []func(anthropic.MessageNewParams) (*anthropic.Message, error){
		func(anthropic.MessageNewParams) (*anthropic.Message, error) {
			return cannedMessage(`{"id":"msg_upstream_1","type":"message","role":"assistant","model":"claude-haiku-4-5",
				"content":[{"type":"text","text":"reply"}],
				"stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":5}}`), nil
		},
	}}
	c := testClient(t, pool, sender)
	conv, err := c.Conversation(ctx, NewConversation("", "test"), Model("claude-haiku-4-5"))
	ck.Require().NoError(err)
	if _, err := conv.Send(ctx, UserText("hi")); err != nil {
		t.Fatalf("Send: %v", err)
	}

	var assistant, user *string
	ck.Require().NoError(pool.QueryRow(ctx, `SELECT
			max(provider_message_id) FILTER (WHERE role = 'assistant'),
			max(provider_message_id) FILTER (WHERE role = 'user')
		FROM conversations.conversation_message WHERE conversation_id=$1::uuid`, conv.ID).
		Scan(&assistant, &user), "read provider_message_id")
	ck.Require().NotNil(assistant, "the assistant node carries no provider_message_id")
	ck.Eq("msg_upstream_1", *assistant, "assistant provider_message_id")
	ck.True(user == nil, "a user row must carry no provider_message_id, got %v", user)
}

// anchorTailTrim is the deterministic trim policy the descriptor tests use: it
// keeps the anchor plus the last two messages, the shape trimDropped
// recognises. The byte-budgeted default policy would depend on fixture sizes.
type anchorTailTrim struct{}

func (anchorTailTrim) Trim(msgs []Message, _ int) ([]Message, bool) {
	if len(msgs) <= 3 {
		return msgs, false
	}
	return []Message{msgs[0], msgs[len(msgs)-2], msgs[len(msgs)-1]}, true
}

// TestTurnRecordsRequestHeadAndNoTrimRange pins a turn row's shape after one
// send: it names the stored row the request ended at and carries no trim range
// when nothing was dropped.
func TestTurnRecordsRequestHeadAndNoTrimRange(t *testing.T) {
	ck := assert.NewCollecting(t)
	pool := convTestPool(t)
	ctx := context.Background()

	sender := &scriptedSender{scripts: []func(anthropic.MessageNewParams) (*anthropic.Message, error){
		respondText("ok"),
	}}
	c := testClient(t, pool, sender)
	conv, err := c.Conversation(ctx, NewConversation("", "test"), Model("claude-haiku-4-5"))
	ck.Require().NoError(err)

	if _, err := conv.Send(ctx, UserText("hello")); err != nil {
		t.Fatalf("Send: %v", err)
	}

	// At send time the working path was the single user row Send appended (the
	// assistant row lands after), so that row is the head the turn must name.
	var wantHead string
	ck.Require().NoError(pool.QueryRow(ctx, `SELECT id::text FROM conversations.conversation_message
		WHERE conversation_id=$1::uuid AND ordinal=0`, conv.ID).Scan(&wantHead), "read the user row id")

	var head *string
	var from, to *int
	ck.Require().NoError(pool.QueryRow(ctx, `SELECT request_head_id::text,
		request_trim_from, request_trim_to FROM conversations.conversation_turn
		WHERE conversation_id=$1::uuid`, conv.ID).Scan(&head, &from, &to), "read the turn row")
	ck.Require().NotNil(head, "the turn names no request head")
	ck.Eq(wantHead, *head, "request_head_id")
	ck.True(from == nil && to == nil, "an untrimmed turn recorded a trim range: from=%v to=%v", from, to)
}

// TestRequestDescriptorTrimRetryRecordsDroppedRange pins that a trimmed retry
// records the range it dropped, in path positions, while the attempt before it
// records none. Both attempts name the same head: a trim changes what was sent,
// never which rows the conversation holds.
func TestRequestDescriptorTrimRetryRecordsDroppedRange(t *testing.T) {
	ck := assert.NewCollecting(t)
	pool := convTestPool(t)
	ctx := context.Background()

	sender := &scriptedSender{scripts: []func(anthropic.MessageNewParams) (*anthropic.Message, error){
		respondErr(promptTooLargeErr()),
		respondText("after trim"),
	}}
	c := testClient(t, pool, sender)
	conv, err := c.Conversation(ctx, NewConversation("", "test"), Model("claude-haiku-4-5"),
		WithTrimPolicy(anchorTailTrim{}))
	ck.Require().NoError(err)

	// Six seeded rows alternating user/assistant, then Send appends ordinal 6:
	// no consecutive user rows, so every wire message is exactly one row.
	roles := []anthropic.MessageParamRole{
		anthropic.MessageParamRoleUser, anthropic.MessageParamRoleAssistant,
		anthropic.MessageParamRoleUser, anthropic.MessageParamRoleAssistant,
		anthropic.MessageParamRoleUser, anthropic.MessageParamRoleAssistant,
	}
	for i, role := range roles {
		appendNode(t, pool, conv.ID, anthropic.MessageParam{
			Role:    role,
			Content: []anthropic.ContentBlockParamUnion{anthropic.NewTextBlock(fmt.Sprintf("seeded %d", i))},
		})
	}
	if _, err := conv.Send(ctx, UserText("question")); err != nil {
		t.Fatalf("Send with trim-retry: %v", err)
	}
	ck.Require().Len(sender.lastReq, 2, "attempts")

	var wantHead string
	ck.Require().NoError(pool.QueryRow(ctx, `SELECT id::text FROM conversations.conversation_message
		WHERE conversation_id=$1::uuid AND ordinal=6`, conv.ID).Scan(&wantHead), "read the tail user row id")

	type turnRow struct {
		head *string
		from *int
		to   *int
	}
	var got []turnRow
	rows, err := pool.Query(ctx, `SELECT request_head_id::text, request_trim_from, request_trim_to
		FROM conversations.conversation_turn WHERE conversation_id=$1::uuid ORDER BY created_at, id`, conv.ID)
	ck.Require().NoError(err, "read turn rows")
	for rows.Next() {
		var r turnRow
		ck.Require().NoError(rows.Scan(&r.head, &r.from, &r.to))
		got = append(got, r)
	}
	rows.Close()
	ck.Require().Len(got, 2, "turn rows (each attempt is its own turn)")

	ck.Require().NotNil(got[0].head, "first attempt names no head")
	ck.Eq(wantHead, *got[0].head, "first attempt head")
	ck.True(got[0].from == nil && got[0].to == nil,
		"the untrimmed attempt recorded a trim range: from=%v to=%v", got[0].from, got[0].to)

	// The anchor (row 0) plus rows 5 and 6 were kept, so rows 1..4 were dropped:
	// anchorEnd 1, one past the last dropped position 5.
	ck.Require().NotNil(got[1].from, "the trimmed attempt recorded no trim range")
	ck.Require().NotNil(got[1].to, "the trimmed attempt recorded no trim end")
	ck.Eq(1, *got[1].from, "request_trim_from")
	ck.Eq(5, *got[1].to, "request_trim_to")
	ck.Require().NotNil(got[1].head, "the trimmed attempt names no head")
	ck.Eq(wantHead, *got[1].head, "the trimmed attempt head")
}

// TestCompactionOverflowRecordsNewHead pins the overflow net's descriptor: the
// retry after a compaction names the head the compaction moved to (a different
// node from the pre-compaction attempt) and records no trim range, because
// compaction is not a trim.
func TestCompactionOverflowRecordsNewHead(t *testing.T) {
	ck := assert.NewCollecting(t)
	pool := convTestPool(t)
	ctx := context.Background()

	sender := &scriptedSender{scripts: []func(anthropic.MessageNewParams) (*anthropic.Message, error){
		respondErr(promptTooLargeErr()),
		respondUsage(10, 5, 0, 0, "<summary>s</summary>"),
		respondUsage(10, 5, 0, 0, "real"),
	}}
	c := testClient(t, pool, sender)
	conv, err := c.Conversation(ctx, NewConversation("", "test"), Model("claude-haiku-4-5"),
		WithCompaction(CompactionPolicy{})) // unknown window: overflow only
	ck.Require().NoError(err)

	roles := []anthropic.MessageParamRole{
		anthropic.MessageParamRoleUser, anthropic.MessageParamRoleAssistant,
		anthropic.MessageParamRoleUser, anthropic.MessageParamRoleAssistant,
	}
	for i, role := range roles {
		appendNode(t, pool, conv.ID, anthropic.MessageParam{
			Role:    role,
			Content: []anthropic.ContentBlockParamUnion{anthropic.NewTextBlock(fmt.Sprintf("seeded %d", i))},
		})
	}

	resp, err := conv.Continue(ctx)
	ck.Require().NoError(err, "the retried call after compaction must succeed")
	ck.Eq("real", resp.Content[0].Text, "response")

	type turnRow struct {
		head   *string
		source *string
		from   *int
		to     *int
	}
	var got []turnRow
	rows, err := pool.Query(ctx, `SELECT request_head_id::text, source, request_trim_from, request_trim_to
		FROM conversations.conversation_turn WHERE conversation_id=$1::uuid ORDER BY created_at, id`, conv.ID)
	ck.Require().NoError(err, "read turn rows")
	for rows.Next() {
		var r turnRow
		ck.Require().NoError(rows.Scan(&r.head, &r.source, &r.from, &r.to))
		got = append(got, r)
	}
	rows.Close()
	// The too-large attempt, the summary call, then the retry.
	ck.Require().Len(got, 3, "turn rows (attempt, summary, retry)")
	ck.Require().NotNil(got[2].source, "the retry turn has no source")
	ck.Eq("compaction", *got[1].source, "the middle turn is the summary call")

	// The post-compaction head is the last tail node the compaction wrote.
	var postHead string
	ck.Require().NoError(pool.QueryRow(ctx, `SELECT id::text FROM conversations.conversation_message
		WHERE conversation_id=$1::uuid AND kind='compaction_tail' ORDER BY ordinal DESC LIMIT 1`,
		conv.ID).Scan(&postHead), "read the post-compaction head")

	ck.Require().NotNil(got[0].head, "the pre-compaction attempt names no head")
	ck.Require().NotNil(got[1].head, "the summary call names no head")
	ck.Require().NotNil(got[2].head, "the retry names no head")
	ck.Eq(*got[0].head, *got[1].head, "the summary call covers the pre-compaction rows")
	ck.Eq(postHead, *got[2].head, "the retry's head after compaction")
	ab := assert.NewAborting(t)
	ab.NotEq(*got[0].head, *got[2].head, "compaction moved the head but the retry kept the old one")
	ck.True(got[2].from == nil && got[2].to == nil,
		"the post-compaction retry recorded a trim range: from=%v to=%v", got[2].from, got[2].to)
}

// TestRequestDescriptorInMemoryConversationHasNoHead pins the in-memory rule:
// an in-memory row id is synthetic, never a conversation_message node, so it
// must stay out of the uuid column. The client's capture store is concrete (no
// stub seam), so this asserts at the head-resolution seam the send consults.
func TestRequestDescriptorInMemoryConversationHasNoHead(t *testing.T) {
	c := assert.NewAborting(t)
	ctx := context.Background()

	sender := &scriptedSender{scripts: []func(anthropic.MessageNewParams) (*anthropic.Message, error){
		respondText("ok"),
	}}
	cl := memClient(t, sender)
	ck := assert.NewCollecting(t)
	conv, err := cl.Conversation(ctx, NewConversation("", "test"), Model("claude-haiku-4-5"))
	ck.Require().NoError(err)
	ck.Require().Nil(cl.capture, "a store-less client must have no capture store")

	seedRows(t, conv, textRow(0, msg.User, "hi"))
	hist, err := conv.History(ctx)
	ck.Require().NoError(err, "History")
	ck.Eq("", conv.requestHeadID(hist), "an in-memory history must resolve no head")

	if _, err := conv.Continue(ctx); err != nil {
		t.Fatalf("Continue: %v", err)
	}
	hist, err = conv.History(ctx)
	ck.Require().NoError(err, "History after the send")
	ck.Eq("", conv.requestHeadID(hist), "the head stays empty after a send")
	c.True(hist[len(hist)-1].ID != "", "the appended reply has no id of its own")
}
