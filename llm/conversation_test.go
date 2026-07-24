package llm

import (
	"context"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/timescale/rafiki/routing"
	"github.com/timescale/rafiki/store"
)

// Integration tests need TimescaleDB (RAFIKI_TEST_DSN, see store/migrate_test.go).
func convTestPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("RAFIKI_TEST_DSN")
	if dsn == "" {
		if os.Getenv("RAFIKI_REQUIRE_DB") != "" {
			t.Fatal("RAFIKI_TEST_DSN not set but RAFIKI_REQUIRE_DB is — the integration job must provide it")
		}
		t.Skip("RAFIKI_TEST_DSN not set; skipping integration test")
	}
	ctx := context.Background()
	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect admin: %v", err)
	}
	t.Cleanup(admin.Close)
	name := fmt.Sprintf("rafiki_llm_%d", time.Now().UnixNano())
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatalf("create scratch db: %v", err)
	}
	t.Cleanup(func() { _, _ = admin.Exec(context.Background(), "DROP DATABASE "+name+" WITH (FORCE)") })
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.Database = name
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if err := store.Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return pool
}

func testClient(t *testing.T, pool *pgxpool.Pool, sender Sender) *Client {
	t.Helper()
	c, err := NewClient(
		WithUpstream(UpstreamAnthropic, sender),
		WithStore(pool),
		WithCatalog(seededCatalog(t)),
		WithDefaultModel("haiku-latest"), // conversations created without Model() intend this default
		WithLogger(testLogger(t)),
	)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestConversationSendPersistsBothGranularities(t *testing.T) {
	pool := convTestPool(t)
	ctx := context.Background()
	sender := &scriptedSender{scripts: []func(anthropic.MessageNewParams) (*anthropic.Message, error){
		respondText("first reply"),
		respondText("second reply"),
	}}
	c := testClient(t, pool, sender)

	conv, err := c.Conversation(ctx,
		NewConversation("brent", "test"),
		Model("sonnet-latest"), // must resolve at creation via the seeded catalog
		SystemText("you are a test"),
	)
	if err != nil {
		t.Fatalf("Conversation: %v", err)
	}

	resp, err := conv.Send(ctx, UserText("hello"))
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if resp.Content[0].Text != "first reply" {
		t.Fatalf("resp = %q", resp.Content[0].Text)
	}

	// Model resolved once at creation.
	if got := string(sender.lastReq[0].Model); got != "claude-sonnet-5" {
		t.Errorf("model = %q, want resolved claude-sonnet-5", got)
	}
	// Default cache policy: one 5m breakpoint (empty TTL) on the last system
	// block. Callers wanting 1h opt in via WithCache (see DefaultCachePolicy).
	sys := sender.lastReq[0].System
	if len(sys) != 1 || sys[0].CacheControl.Type == "" || sys[0].CacheControl.TTL != "" {
		t.Errorf("system breakpoint wrong (want ephemeral/5m-default): %+v", sys)
	}

	// Second send loads history: request must carry 3 messages.
	if _, err := conv.Send(ctx, UserText("and again")); err != nil {
		t.Fatalf("Send 2: %v", err)
	}
	if n := len(sender.lastReq[1].Messages); n != 3 {
		t.Errorf("second request has %d messages, want 3 (history loaded)", n)
	}

	// Message granularity: 4 rows (user, assistant, user, assistant).
	var msgCount int
	var roles []string
	rows, err := pool.Query(ctx, `SELECT role FROM conversations.conversation_message
		WHERE conversation_id=$1::uuid ORDER BY ordinal`, conv.ID)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var r string
		_ = rows.Scan(&r)
		roles = append(roles, r)
		msgCount++
	}
	rows.Close()
	if msgCount != 4 || strings.Join(roles, ",") != "user,assistant,user,assistant" {
		t.Errorf("message rows = %v, want user,assistant,user,assistant", roles)
	}

	// Turn granularity: 2 complete turns, protocol anthropic, prefix_hash set,
	// and prefix_hash IDENTICAL across the conversation (stable prefix).
	var turns, complete, hashes int
	if err := pool.QueryRow(ctx, `SELECT count(*), count(*) FILTER (WHERE status='complete'),
		count(DISTINCT prefix_hash) FROM conversations.conversation_turn
		WHERE conversation_id=$1::uuid AND protocol='anthropic'`, conv.ID).Scan(&turns, &complete, &hashes); err != nil {
		t.Fatal(err)
	}
	if turns != 2 || complete != 2 {
		t.Errorf("turns=%d complete=%d, want 2/2", turns, complete)
	}
	if hashes != 1 {
		t.Errorf("prefix_hash values across the conversation = %d, want 1 (stable prefix)", hashes)
	}

	// Assistant rows carry token counts.
	var inTok int64
	if err := pool.QueryRow(ctx, `SELECT input_tokens FROM conversations.conversation_message
		WHERE conversation_id=$1::uuid AND ordinal=1`, conv.ID).Scan(&inTok); err != nil || inTok != 10 {
		t.Errorf("assistant input_tokens = %d err=%v, want 10", inTok, err)
	}
}

func TestConversationTrimRetryKeepsPrefixAndRows(t *testing.T) {
	pool := convTestPool(t)
	ctx := context.Background()

	// Fail the first attempt as prompt-too-large, succeed the second.
	sender := &scriptedSender{scripts: []func(anthropic.MessageNewParams) (*anthropic.Message, error){
		respondErr(promptTooLargeErr()),
		respondText("after trim"),
	}}
	c := testClient(t, pool, sender)
	conv, err := c.Conversation(ctx, NewConversation("brent", "test"), Model("claude-haiku-4-5"),
		SystemText("sys"))
	if err != nil {
		t.Fatal(err)
	}

	// Seed enough history that the default policy can drop the middle.
	big := strings.Repeat("y", 80*1024)
	for i := range 5 {
		role := anthropic.MessageParamRoleUser
		if i%2 == 1 {
			role = anthropic.MessageParamRoleAssistant
		}
		msg := anthropic.MessageParam{Role: role,
			Content: []anthropic.ContentBlockParamUnion{anthropic.NewTextBlock(big)}}
		if err := store.NewMessages(pool).Append(ctx, conv.ID, i, msg, nil); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := conv.Send(ctx, UserText("question")); err != nil {
		t.Fatalf("Send with trim-retry: %v", err)
	}

	// The retry sent fewer messages than the first attempt.
	if len(sender.lastReq) != 2 {
		t.Fatalf("attempts = %d, want 2", len(sender.lastReq))
	}
	if len(sender.lastReq[1].Messages) >= len(sender.lastReq[0].Messages) {
		t.Errorf("retry not trimmed: %d -> %d messages",
			len(sender.lastReq[0].Messages), len(sender.lastReq[1].Messages))
	}

	// prefix_hash identical across the failed and successful attempts —
	// trimming must not touch the cached tools+system prefix.
	var hashes, turns int
	if err := pool.QueryRow(ctx, `SELECT count(DISTINCT prefix_hash), count(*)
		FROM conversations.conversation_turn WHERE conversation_id=$1::uuid`, conv.ID).Scan(&hashes, &turns); err != nil {
		t.Fatal(err)
	}
	if turns != 2 {
		t.Errorf("turn rows = %d, want 2 (one per attempt)", turns)
	}
	if hashes != 1 {
		t.Errorf("prefix_hash differs across trim-retry attempts (%d distinct)", hashes)
	}
	// One error row (attempt 1) + one complete (attempt 2).
	var errRows, completeRows int
	_ = pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE status='error'),
		count(*) FILTER (WHERE status='complete') FROM conversations.conversation_turn
		WHERE conversation_id=$1::uuid`, conv.ID).Scan(&errRows, &completeRows)
	if errRows != 1 || completeRows != 1 {
		t.Errorf("turn statuses error=%d complete=%d, want 1/1", errRows, completeRows)
	}

	// Trim is request-assembly-time only: all 5 seeded rows + user + assistant
	// still present, untouched.
	var msgRows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM conversations.conversation_message
		WHERE conversation_id=$1::uuid`, conv.ID).Scan(&msgRows); err != nil {
		t.Fatal(err)
	}
	if msgRows != 7 {
		t.Errorf("stored message rows = %d, want 7 (trim must not mutate the store)", msgRows)
	}
}

func TestUnfinishedConversationsAndResumeCounter(t *testing.T) {
	pool := convTestPool(t)
	ctx := context.Background()
	sender := &scriptedSender{scripts: []func(anthropic.MessageNewParams) (*anthropic.Message, error){
		respondText("ok"),
	}}
	c := testClient(t, pool, sender)

	// Conversation A: a pending turn (call never resolved).
	convA, err := c.Conversation(ctx, NewConversation("brent", "test"))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.capture.InsertTurnIntent(ctx, routing.TurnIntent{
		ConversationID: convA.ID, Ordinal: 1, Model: "claude-haiku-4-5",
		Request: []byte(`{"messages":[]}`),
	}); err != nil {
		t.Fatal(err)
	}

	// Conversation B: an assistant tool_use with no matching tool_result.
	convB, err := c.Conversation(ctx, NewConversation("brent", "test"))
	if err != nil {
		t.Fatal(err)
	}
	msgs := store.NewMessages(pool)
	assistant := anthropic.NewAssistantMessage(
		anthropic.NewToolUseBlock("toolu_orphan", map[string]any{"a": 1}, "service_status"))
	if err := msgs.Append(ctx, convB.ID, 0, assistant, nil); err != nil {
		t.Fatal(err)
	}

	// Conversation C: clean (a full Send).
	convC, err := c.Conversation(ctx, NewConversation("brent", "test"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := convC.Send(ctx, UserText("hi")); err != nil {
		t.Fatal(err)
	}

	unfinished, err := store.UnfinishedConversations(ctx, pool, store.DrivenByServer)
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]store.Unfinished{}
	for _, u := range unfinished {
		byID[u.ConversationID] = u
	}
	if len(byID) != 2 {
		t.Fatalf("unfinished = %d conversations, want 2 (got %+v)", len(byID), unfinished)
	}
	if u := byID[convA.ID]; u.PendingTurns != 1 {
		t.Errorf("conv A pending turns = %d, want 1", u.PendingTurns)
	}
	if u := byID[convB.ID]; len(u.OrphanToolUses) != 1 || u.OrphanToolUses[0] != "toolu_orphan" {
		t.Errorf("conv B orphans = %v, want [toolu_orphan]", u.OrphanToolUses)
	}
	if _, ok := byID[convC.ID]; ok {
		t.Error("clean conversation reported as unfinished")
	}

	// Resume counter round-trip.
	if n, err := store.IncrementResumeAttempts(ctx, pool, convB.ID); err != nil || n != 1 {
		t.Errorf("first increment = %d err=%v, want 1", n, err)
	}
	if n, _ := store.IncrementResumeAttempts(ctx, pool, convB.ID); n != 2 {
		t.Errorf("second increment = %d, want 2", n)
	}
}

// TestConversationStoresPrefixOnChange proves the in-process (direct) path
// records turn prefix metadata for parity with the proxy path: prefix_content
// on the first turn, NULL on an unchanged second turn (same static prefix), and
// cache_breakpoints on every turn.
func TestConversationStoresPrefixOnChange(t *testing.T) {
	pool := convTestPool(t)
	ctx := context.Background()
	sender := &scriptedSender{scripts: []func(anthropic.MessageNewParams) (*anthropic.Message, error){
		respondText("one"), respondText("two"),
	}}
	c := testClient(t, pool, sender)

	conv, err := c.Conversation(ctx, NewConversation("brent", "test"),
		Model("sonnet-latest"), SystemText("you are a test"))
	if err != nil {
		t.Fatalf("Conversation: %v", err)
	}
	if _, err := conv.Send(ctx, UserText("hello")); err != nil {
		t.Fatalf("Send 1: %v", err)
	}
	if _, err := conv.Send(ctx, UserText("again")); err != nil {
		t.Fatalf("Send 2: %v", err)
	}

	rows, err := pool.Query(ctx,
		`SELECT prefix_content IS NOT NULL, cache_breakpoints IS NOT NULL
		   FROM conversations.conversation_turn WHERE conversation_id=$1::uuid ORDER BY created_at`, conv.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	type row struct{ hasPrefix, hasBreakpoints bool }
	var got []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.hasPrefix, &r.hasBreakpoints); err != nil {
			t.Fatal(err)
		}
		got = append(got, r)
	}
	if len(got) != 2 {
		t.Fatalf("turns = %d, want 2", len(got))
	}
	if !got[0].hasPrefix {
		t.Error("turn 1 prefix_content is NULL, want the request envelope stored")
	}
	if got[1].hasPrefix {
		t.Error("turn 2 prefix_content is set, want NULL (prefix unchanged from turn 1)")
	}
	for i, r := range got {
		if !r.hasBreakpoints {
			t.Errorf("turn %d cache_breakpoints is NULL, want recorded", i+1)
		}
	}
}

func TestWithCache_EachApplicationGetsItsOwnPolicy(t *testing.T) {
	// One reused ConvOption must not share a CachePolicy pointer across
	// conversations: Conversation() clamps Breakpoints through cfg.cache, and
	// with a shared pointer the first clamp would leak into later uses.
	opt := WithCache(CachePolicy{SystemTTL: Cache1h, MessagesTTL: Cache5m, Breakpoints: 9})
	var cfg1, cfg2 convConfig
	opt(&cfg1)
	opt(&cfg2)
	if cfg1.cache == cfg2.cache {
		t.Fatal("WithCache applications share one CachePolicy pointer")
	}
	cfg1.cache.Breakpoints = 3 // what Conversation()'s clamp does
	if cfg2.cache.Breakpoints != 9 {
		t.Fatalf("clamping one conversation's policy leaked: Breakpoints = %d, want 9", cfg2.cache.Breakpoints)
	}
}

func TestBlockWithCacheControl_CoversEveryCacheableUnionMember(t *testing.T) {
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
		if ptr == nil || ptr.Type != "" || ptr.TTL != "" {
			t.Errorf("%s: blockWithCacheControl did not clear cache_control (got %+v) — add it to the switch", f.Name, ptr)
		}
		if orig := b.GetCacheControl(); orig == nil || orig.Type == "" {
			t.Errorf("%s: input block was mutated; blockWithCacheControl must copy", f.Name)
		}
		tested++
	}
	if tested < 14 { // the cacheable members as of anthropic-sdk-go v1.56
		t.Fatalf("only %d cacheable union members found; reflection walk is broken", tested)
	}
}

func TestConversationWithToolChoice(t *testing.T) {
	pool := convTestPool(t)
	ctx := context.Background()
	sender := &scriptedSender{scripts: []func(anthropic.MessageNewParams) (*anthropic.Message, error){
		respondText("tool choice test"),
		respondText("tool choice test 2"),
	}}
	c := testClient(t, pool, sender)

	conv, err := c.Conversation(ctx,
		NewConversation("brent", "test"),
		Model("sonnet-latest"),
		SystemText("you are a test"))
	if err != nil {
		t.Fatalf("Conversation: %v", err)
	}

	tools := []anthropic.ToolUnionParam{
		anthropic.ToolUnionParamOfTool(
			anthropic.ToolInputSchemaParam{Type: "object"},
			"report_findings",
		),
	}

	resp, err := conv.Send(ctx, UserText("test"), WithTools(tools), WithToolChoice("report_findings"))
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if resp.Content[0].Text != "tool choice test" {
		t.Fatalf("unexpected response: %q", resp.Content[0].Text)
	}

	if sender.lastReq[0].ToolChoice.OfTool == nil {
		t.Fatalf("ToolChoice.OfTool is nil, expected tool choice to be set")
	}
	if sender.lastReq[0].ToolChoice.OfTool.Name != "report_findings" {
		t.Errorf("ToolChoice.OfTool.Name = %q, want report_findings", sender.lastReq[0].ToolChoice.OfTool.Name)
	}

	// Send without WithToolChoice should have zero ToolChoice
	resp2, err := conv.Send(ctx, UserText("test 2"))
	if err != nil {
		t.Fatalf("Send 2: %v", err)
	}
	if resp2.Content[0].Text != "tool choice test 2" {
		t.Fatalf("unexpected response 2: %q", resp2.Content[0].Text)
	}

	if sender.lastReq[1].ToolChoice.OfTool != nil {
		t.Errorf("ToolChoice.OfTool should be nil when WithToolChoice not used, got %v", sender.lastReq[1].ToolChoice.OfTool)
	}
}
