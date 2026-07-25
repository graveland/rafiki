package local

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/jackc/pgx/v5/pgxpool"

	"git.graveland.dev/brent/rafiki/agentcli"
	"git.graveland.dev/brent/rafiki/analyze"
	"git.graveland.dev/brent/rafiki/insights"
	"git.graveland.dev/brent/rafiki/store"
)

// Integration tests need a real TimescaleDB (>= 2.22, PostgreSQL 18 for
// uuidv7()). Set RAFIKI_TEST_DSN to run them, e.g.:
//
//	RAFIKI_TEST_DSN="postgres://postgres@localhost:5432/postgres?sslmode=disable" go test ./agentcli/local/...
//
// Each call to newTestPool runs in its own scratch database, migrated to head.

var scratchSeq atomic.Uint64

func newTestPool(t *testing.T) *pgxpool.Pool {
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

	name := fmt.Sprintf("rafiki_agentcli_local_%d_%d", time.Now().UnixNano(), scratchSeq.Add(1))
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatalf("create scratch db: %v", err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(context.Background(), "DROP DATABASE "+name+" WITH (FORCE)")
	})

	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	cfg.ConnConfig.Database = name
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("connect scratch db: %v", err)
	}
	t.Cleanup(pool.Close)

	if err := store.Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate scratch db: %v", err)
	}
	return pool
}

// insertConversation creates a conversation row and returns its id.
func insertConversation(t *testing.T, pool *pgxpool.Pool, drivenBy, owner string) string {
	t.Helper()
	var id string
	err := pool.QueryRow(context.Background(),
		`INSERT INTO conversations.conversation (owner, persona, model, origin_entrypoint, driven_by)
		 VALUES ($1, 'team-platform', 'claude-fable-5', 'test', $2) RETURNING id::text`,
		owner, drivenBy).Scan(&id)
	if err != nil {
		t.Fatalf("insert conversation: %v", err)
	}
	return id
}

// insertTurn inserts a minimal complete conversation_turn row.
func insertTurn(t *testing.T, pool *pgxpool.Pool, convID string, ordinal int, inTok, outTok int64) {
	t.Helper()
	_, err := pool.Exec(context.Background(),
		`INSERT INTO conversations.conversation_turn
		   (conversation_id, ordinal, status, model, request, response, stop_reason,
		    input_tokens, output_tokens, source, created_at)
		 VALUES ($1, $2, 'complete', 'claude-fable-5', '{}'::jsonb, NULL, 'end_turn',
		         $3, $4, 'claude', now())`,
		convID, ordinal, inTok, outTok)
	if err != nil {
		t.Fatalf("insert turn: %v", err)
	}
}

// insertMessage inserts a conversation_message row.
func insertMessage(t *testing.T, pool *pgxpool.Pool, convID string, ordinal int, role, contentJSON string) {
	t.Helper()
	_, err := pool.Exec(context.Background(),
		`INSERT INTO conversations.conversation_message (conversation_id, ordinal, role, content)
		 VALUES ($1, $2, $3, $4)`,
		convID, ordinal, role, []byte(contentJSON))
	if err != nil {
		t.Fatalf("insert message: %v", err)
	}
}

// seedConversation creates a conversation with two complete turns and a
// user/assistant message pair, owned by alice, driven by the client path.
func seedConversation(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	convID := insertConversation(t, pool, "client", "alice")
	insertTurn(t, pool, convID, 0, 100, 50)
	insertTurn(t, pool, convID, 1, 120, 60)
	insertMessage(t, pool, convID, 0, "user", `[{"type":"text","text":"hello there"}]`)
	insertMessage(t, pool, convID, 1, "assistant", `[{"type":"text","text":"hi"}]`)
	return convID
}

func TestBackendReadPaths(t *testing.T) {
	pool := newTestPool(t)
	convID := seedConversation(t, pool)
	b := New(Options{Pool: pool})
	ctx := context.Background()

	st, err := b.Stats(ctx, insights.StatsFilter{})
	if err != nil || st.Volume.Turns != 2 {
		t.Fatalf("stats = %+v, %v; want 2 turns", st, err)
	}
	one, err := b.ConversationStats(ctx, convID)
	if err != nil || one.Volume.Conversations != 1 {
		t.Fatalf("conversation stats = %+v, %v", one, err)
	}
	rows, err := b.Search(ctx, insights.SearchFilter{})
	if err != nil || len(rows) != 1 || rows[0].ID != convID {
		t.Fatalf("search = %+v, %v", rows, err)
	}
	tr, err := b.Export(ctx, convID)
	if err != nil || tr.ConversationID != convID {
		t.Fatalf("export = %+v, %v", tr, err)
	}
}

func TestBackendFindingsRoundTrip(t *testing.T) {
	pool := newTestPool(t)
	convID := seedConversation(t, pool)
	id, _, err := store.UpsertAnalysis(context.Background(), pool, store.AnalysisRow{
		ConversationID: convID, DetectorVersion: analyze.DetectorVersion, Model: "m", Status: "ok",
		Analysis: []byte(`{"conversation_id":"` + convID + `"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.ReplaceFindings(context.Background(), pool, id, []store.FindingRow{{
		Axis: "grind", TopicKey: "loop", Title: "retry loop", ExpectedSavingsTokens: 100,
	}}, nil); err != nil {
		t.Fatal(err)
	}
	b := New(Options{Pool: pool})
	got, err := b.Findings(context.Background(), store.FindingFilter{})
	if err != nil || len(got) != 1 || got[0].Title != "retry loop" {
		t.Fatalf("findings = %+v, %v", got, err)
	}
	row, err := b.SetFindingStatus(context.Background(), got[0].ID, "dismissed")
	if err != nil || row.Status != "dismissed" {
		t.Fatalf("set status = %+v, %v", row, err)
	}
}

func TestAnalyzeWithoutLLMErrors(t *testing.T) {
	pool := newTestPool(t)
	b := New(Options{Pool: pool})
	if _, err := b.Analyze(context.Background(), agentcli.AnalyzeRequest{ConversationIDs: []string{"x"}}); !errors.Is(err, ErrNoLLM) {
		t.Fatalf("Analyze without an LLM client err = %v, want ErrNoLLM", err)
	}
}

func TestBackendNilPool(t *testing.T) {
	// New() should succeed with nil Pool
	b := New(Options{Pool: nil})
	if b.pool != nil {
		t.Fatal("pool should be nil")
	}

	ctx := context.Background()
	// Read methods should return ErrNoPool
	if _, err := b.Stats(ctx, insights.StatsFilter{}); !errors.Is(err, ErrNoPool) {
		t.Fatalf("Stats err = %v, want ErrNoPool", err)
	}
	if _, err := b.ConversationStats(ctx, "x"); !errors.Is(err, ErrNoPool) {
		t.Fatalf("ConversationStats err = %v, want ErrNoPool", err)
	}
	if _, err := b.Search(ctx, insights.SearchFilter{}); !errors.Is(err, ErrNoPool) {
		t.Fatalf("Search err = %v, want ErrNoPool", err)
	}
	if _, err := b.Export(ctx, "x"); !errors.Is(err, ErrNoPool) {
		t.Fatalf("Export err = %v, want ErrNoPool", err)
	}
	if _, err := b.Findings(ctx, store.FindingFilter{}); !errors.Is(err, ErrNoPool) {
		t.Fatalf("Findings err = %v, want ErrNoPool", err)
	}
	if _, err := b.SetFindingStatus(ctx, "x", "dismissed"); !errors.Is(err, ErrNoPool) {
		t.Fatalf("SetFindingStatus err = %v, want ErrNoPool", err)
	}
}

// TestAnalyzeNilPoolByIDsErrors covers the guard added for the nil-pool
// Analyze panic: a corpus-only backend (nil Pool) given a DB-backed
// population (explicit ConversationIDs here) must return ErrNoPool up
// front, not panic deep inside runAnalyze/storedAnalysesFor on a nil
// *pgxpool.Pool.
func TestAnalyzeNilPoolByIDsErrors(t *testing.T) {
	b := New(Options{LLM: testAnalyzeClient(t, &analyzeFakeSender{})})
	ch, err := b.Analyze(context.Background(), agentcli.AnalyzeRequest{
		ConversationIDs: []string{"11111111-1111-1111-1111-111111111111"},
	})
	if !errors.Is(err, ErrNoPool) {
		t.Fatalf("Analyze by ids on a nil-pool backend err = %v, want ErrNoPool", err)
	}
	if ch != nil {
		t.Error("Analyze: want a nil channel alongside the error")
	}
}

// TestAnalyzeNilPoolByFilterErrors mirrors TestAnalyzeNilPoolByIDsErrors for
// the other DB-backed population path: a search Filter.
func TestAnalyzeNilPoolByFilterErrors(t *testing.T) {
	b := New(Options{LLM: testAnalyzeClient(t, &analyzeFakeSender{})})
	ch, err := b.Analyze(context.Background(), agentcli.AnalyzeRequest{
		Filter: &insights.SearchFilter{Owner: "brent"},
	})
	if !errors.Is(err, ErrNoPool) {
		t.Fatalf("Analyze by filter on a nil-pool backend err = %v, want ErrNoPool", err)
	}
	if ch != nil {
		t.Error("Analyze: want a nil channel alongside the error")
	}
}

// TestAnalyzeNilPoolNoSelectorErrors mirrors TestAnalyzeNilPoolByIDsErrors /
// TestAnalyzeNilPoolByFilterErrors for the request shape the original guard
// missed entirely: no ConversationIDs and no Filter at all. That falls to
// population()'s "default:" branch, which builds an empty insights.SearchFilter
// and calls b.ins.Search — a nil *insights.Insights on a nil-pool backend —
// which SIGSEGVs deep inside the runAnalyze producer goroutine (after
// close(ch) already ran via defer, so the consumer would see zero events and
// no EventError) rather than returning ErrNoPool synchronously like every
// other pool-dependent method does. Keying the guard on
// req.CorpusDir == "" (rather than the old len(ConversationIDs)>0 ||
// Filter != nil) is exactly what closes this gap.
func TestAnalyzeNilPoolNoSelectorErrors(t *testing.T) {
	b := New(Options{LLM: testAnalyzeClient(t, &analyzeFakeSender{})})
	ch, err := b.Analyze(context.Background(), agentcli.AnalyzeRequest{})
	if !errors.Is(err, ErrNoPool) {
		t.Fatalf("Analyze with no selector on a nil-pool backend err = %v, want ErrNoPool", err)
	}
	if ch != nil {
		t.Error("Analyze: want a nil channel alongside the error")
	}
}

// TestAnalyzeNilPoolCorpusStillWorks proves the guard doesn't overreach:
// a corpus-only backend (nil Pool) must still run a --corpus Analyze —
// corpus mode never touches the database.
func TestAnalyzeNilPoolCorpusStillWorks(t *testing.T) {
	dir := t.TempDir()
	tr := insights.Transcript{
		ConversationID: "corpus-conv",
		Owner:          "brent",
		Turns: []insights.TranscriptTurn{
			{Ordinal: 0, Role: "user", Content: json.RawMessage(`[{"type":"text","text":"why is replica X lagging?"}]`)},
		},
	}
	raw, err := json.Marshal(tr)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "conv.json"), raw, 0o644); err != nil {
		t.Fatal(err)
	}

	sender := &analyzeFakeSender{scripts: []func(anthropic.MessageNewParams) (*anthropic.Message, error){
		analyzeRespondToolUse(t, analyzeWellFormedInput),
	}}
	b := New(Options{LLM: testAnalyzeClient(t, sender)})

	ch, err := b.Analyze(context.Background(), agentcli.AnalyzeRequest{CorpusDir: dir})
	if err != nil {
		t.Fatalf("Analyze (corpus, nil pool): %v", err)
	}
	events := drainEvents(ch)
	for _, ev := range events {
		if ev.Kind == agentcli.EventError {
			t.Fatalf("unexpected EventError: %v", ev.Err)
		}
	}
	summary := events[len(events)-1].Summary
	if summary == nil || summary.Analyzed != 1 {
		t.Fatalf("Summary = %+v, want Analyzed=1", summary)
	}
}
