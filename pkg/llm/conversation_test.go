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
	"go.graveland.dev/rafiki/pkg/routing"
	"go.graveland.dev/rafiki/pkg/store"

	"github.com/multigres/testkit/assert"
)

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
		ck.Require().NoError(store.NewMessages(pool).Append(ctx, conv.ID, i, msg, nil))
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
		Request: []byte(`{"messages":[]}`),
	}); err != nil {
		t.Fatal(err)
	}

	// Conversation B: an assistant tool_use with no matching tool_result.
	convB, err := c.Conversation(ctx, NewConversation("", "test"))
	ck.Require().NoError(err)
	msgs := store.NewMessages(pool)
	assistant := anthropic.NewAssistantMessage(
		anthropic.NewToolUseBlock("toolu_orphan", map[string]any{"a": 1}, "service_status"))
	ck.Require().NoError(msgs.Append(ctx, convB.ID, 0, assistant, nil))

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
