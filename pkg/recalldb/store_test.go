package recalldb

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"go.graveland.dev/rafiki/pkg/recall"
	"go.graveland.dev/rafiki/pkg/store"
)

// testStore gives each test its own scratch database, migrated fresh —
// the pattern from pkg/presetsdb/postgres_test.go's testStore, so this never
// touches a developer's real database.
func testStore(t *testing.T) (*Store, *pgxpool.Pool) {
	t.Helper()
	dsn := os.Getenv("RAFIKI_TEST_DSN")
	if dsn == "" {
		if os.Getenv("RAFIKI_REQUIRE_DB") != "" {
			t.Fatal("RAFIKI_TEST_DSN not set but RAFIKI_REQUIRE_DB is")
		}
		t.Skip("RAFIKI_TEST_DSN not set; skipping integration test")
	}
	ctx := context.Background()

	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect admin: %v", err)
	}
	t.Cleanup(admin.Close)

	name := fmt.Sprintf("rafiki_recalldb_%d", time.Now().UnixNano())
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
		t.Fatalf("migrate: %v", err)
	}
	return New(pool), pool
}

// --- fixtures (plain SQL; the migration's DDL is authoritative) ---

func insertUser(t *testing.T, pool *pgxpool.Pool, username string) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO conversations.users (username, token_sha256) VALUES ($1, $2) RETURNING id::text`,
		username, "tok-"+username).Scan(&id); err != nil {
		t.Fatalf("insert user %s: %v", username, err)
	}
	return id
}

type convFixture struct {
	Owner       string // user id; "" = unattributed
	Entrypoint  string // default "claude"
	Name        string
	RepoRoot    string // "" = NULL
	ExternalRef string // "" = NULL
	ClosedAt    *time.Time
}

func insertConversation(t *testing.T, pool *pgxpool.Pool, f convFixture) string {
	t.Helper()
	entrypoint := f.Entrypoint
	if entrypoint == "" {
		entrypoint = "claude"
	}
	var id string
	err := pool.QueryRow(context.Background(), `INSERT INTO conversations.conversation
		(owner_user_id, origin_entrypoint, driven_by, name, external_ref, repo_root, closed_at)
		VALUES ($1::uuid, $2, 'server', $3, nullif($4, ''), nullif($5, ''), $6)
		RETURNING id::text`,
		nullUUID(f.Owner), entrypoint, f.Name, f.ExternalRef, f.RepoRoot, f.ClosedAt).Scan(&id)
	if err != nil {
		t.Fatalf("insert conversation: %v", err)
	}
	return id
}

func insertMessage(t *testing.T, pool *pgxpool.Pool, convID string, ordinal int, role, text string, createdAt *time.Time) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `INSERT INTO conversations.conversation_message
		(conversation_id, ordinal, role, content, created_at)
		VALUES ($1::uuid, $2, $3, $4, coalesce($5, now()))`,
		convID, ordinal, role, fmt.Sprintf(`[{"type":"text","text":%q}]`, text), createdAt); err != nil {
		t.Fatalf("insert message %d: %v", ordinal, err)
	}
}

type childFixture struct {
	ID     string
	Status string
}

func insertChild(t *testing.T, pool *pgxpool.Pool, f childFixture, convID string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `INSERT INTO conversations.child
		(child_id, conversation_id, kind, status, spawned_at)
		VALUES ($1, $2::uuid, 'claude', $3, now())`,
		f.ID, nullUUID(convID), f.Status); err != nil {
		t.Fatalf("insert child %s: %v", f.ID, err)
	}
}

func insertWindow(t *testing.T, pool *pgxpool.Pool, convID, owner string, seq, from, to int, text string, sealed bool) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(context.Background(), `INSERT INTO conversations.conversation_window
		(conversation_id, owner_user_id, seq, ordinal_from, ordinal_to, text, sealed, extractor_version)
		VALUES ($1::uuid, $2::uuid, $3, $4, $5, $6, $7, $8) RETURNING id::text`,
		convID, nullUUID(owner), seq, from, to, text, sealed, recall.ExtractorVersion).Scan(&id); err != nil {
		t.Fatalf("insert window seq %d: %v", seq, err)
	}
	return id
}

func insertSummary(t *testing.T, pool *pgxpool.Pool, convID, owner, level string, seq, from, to int, title, summary, promptVersion string) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(context.Background(), `INSERT INTO conversations.conversation_summary
		(conversation_id, owner_user_id, level, seq, ordinal_from, ordinal_to, title, summary, prompt_version, model)
		VALUES ($1::uuid, $2::uuid, $3, $4, $5, $6, $7, $8, $9, 'sum-model') RETURNING id::text`,
		convID, nullUUID(owner), level, seq, from, to, title, summary, promptVersion).Scan(&id); err != nil {
		t.Fatalf("insert summary: %v", err)
	}
	return id
}

// setEmbedding stamps a row's vector directly, as the embed pass would.
func setEmbedding(t *testing.T, pool *pgxpool.Pool, table, id, model, vec string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`UPDATE `+table+` SET embedding = $2::vector, embedding_model = $3 WHERE id = $1::uuid`,
		id, vec, model); err != nil {
		t.Fatalf("set embedding on %s %s: %v", table, id, err)
	}
}

// ownerScope is the scope every conversation-derived read in these tests uses.
func ownerScope(owner string) recall.Scope { return recall.Scope{OwnerUserID: owner} }

func ago(d time.Duration) *time.Time {
	t := time.Now().UTC().Add(-d)
	return &t
}

// --- scope guard ---

// Every Scope-taking method must refuse recall.Scope{} before any SQL — even
// for ids that would otherwise fail uuid parsing, which pins the guard ahead
// of every other check. The memory source is exempt: memories are scoped by
// their ownerUserID argument, never by a Scope.
func TestStoreScopeZeroValueRefused(t *testing.T) {
	st, _ := testStore(t)
	ctx := context.Background()
	badID := "not-a-uuid"

	methods := []struct {
		name string
		call func() error
	}{
		{"Conversation", func() error { _, err := st.Conversation(ctx, recall.Scope{}, badID); return err }},
		{"Messages", func() error { _, err := st.Messages(ctx, recall.Scope{}, badID, 0, -1); return err }},
		{"Window", func() error { _, err := st.Window(ctx, recall.Scope{}, badID); return err }},
		{"Summary", func() error { _, err := st.Summary(ctx, recall.Scope{}, badID); return err }},
		{"SearchBM25/window", func() error {
			_, err := st.SearchBM25(ctx, recall.SearchQuery{Scope: recall.Scope{}, Text: "x"}, recall.SourceWindow)
			return err
		}},
		{"SearchBM25/summary", func() error {
			_, err := st.SearchBM25(ctx, recall.SearchQuery{Scope: recall.Scope{}, Text: "x"}, recall.SourceSummary)
			return err
		}},
		{"SearchVector/window", func() error {
			_, err := st.SearchVector(ctx, recall.SearchQuery{Scope: recall.Scope{}, Text: "x"}, recall.SourceWindow, "m", []float32{1, 2, 3})
			return err
		}},
		{"SearchVector/summary", func() error {
			_, err := st.SearchVector(ctx, recall.SearchQuery{Scope: recall.Scope{}, Text: "x"}, recall.SourceSummary, "m", []float32{1, 2, 3})
			return err
		}},
	}
	for _, m := range methods {
		if err := m.call(); !errors.Is(err, recall.ErrInvalidScope) {
			t.Errorf("%s with zero Scope = %v, want ErrInvalidScope", m.name, err)
		}
	}

	hits, err := st.SearchBM25(ctx, recall.SearchQuery{Scope: recall.Scope{}, Text: "x"}, recall.SourceMemory)
	if err != nil {
		t.Errorf("SearchBM25(memory) with zero Scope = %v, want nil (memories are never scope-scoped)", err)
	}
	if len(hits) != 0 {
		t.Errorf("SearchBM25(memory) returned %d hits, want 0", len(hits))
	}
}

// --- memories ---

func mustPut(t *testing.T, st *Store, owner, path, name string) string {
	t.Helper()
	m, err := st.PutMemory(context.Background(), owner, recall.Memory{Path: path, Name: name, Body: name + " body"})
	if err != nil {
		t.Fatalf("put %s/%s: %v", path, name, err)
	}
	return m.ID
}

func TestStoreMemoryOwnerIsolation(t *testing.T) {
	st, pool := testStore(t)
	ctx := context.Background()
	a := insertUser(t, pool, "mem-iso-a")
	b := insertUser(t, pool, "mem-iso-b")

	aID := mustPut(t, st, a, "proj.alpha", "notes")

	if _, err := st.GetMemory(ctx, b, "proj.alpha", "notes"); !errors.Is(err, recall.ErrNotFound) {
		t.Errorf("b get a's memory = %v, want ErrNotFound", err)
	}
	tree, err := st.MemoryTree(ctx, b, "proj", 0)
	if err != nil {
		t.Fatalf("b tree: %v", err)
	}
	if len(tree) != 0 {
		t.Errorf("b tree = %+v, want empty", tree)
	}
	hits, err := st.SearchBM25(ctx, recall.SearchQuery{Text: "notes", MemoryOwner: b}, recall.SourceMemory)
	if err != nil {
		t.Fatalf("b search: %v", err)
	}
	if len(hits) != 0 {
		t.Errorf("b search saw %d hits, want 0", len(hits))
	}
	if _, err := st.MemoryByID(ctx, b, aID); !errors.Is(err, recall.ErrNotFound) {
		t.Errorf("b MemoryByID on a's row = %v, want ErrNotFound", err)
	}
	got, err := st.GetMemory(ctx, a, "proj.alpha", "notes")
	if err != nil || got.Body != "notes body" {
		t.Errorf("a get = %+v, %v; want own row", got, err)
	}
}

func TestStoreMemoryRequiresOwnerAndValidPath(t *testing.T) {
	st, pool := testStore(t)
	ctx := context.Background()
	owner := insertUser(t, pool, "mem-validate")

	cases := []struct {
		op   string
		want error
		call func() error
	}{
		{"put", recall.ErrNoOwner, func() error {
			_, err := st.PutMemory(ctx, "", recall.Memory{Path: "a", Name: "n", Body: "b"})
			return err
		}},
		{"get", recall.ErrNoOwner, func() error { _, err := st.GetMemory(ctx, "", "a", "n"); return err }},
		{"tree", recall.ErrNoOwner, func() error { _, err := st.MemoryTree(ctx, "", "a", 0); return err }},
		{"delete", recall.ErrNoOwner, func() error { return st.DeleteMemory(ctx, "", "a", "n") }},
		{"byid", recall.ErrNoOwner, func() error { _, err := st.MemoryByID(ctx, "", "00000000-0000-0000-0000-000000000000"); return err }},
		{"put", recall.ErrInvalidPath, func() error {
			_, err := st.PutMemory(ctx, owner, recall.Memory{Path: "a..b", Name: "n", Body: "b"})
			return err
		}},
		{"get", recall.ErrInvalidPath, func() error { _, err := st.GetMemory(ctx, owner, "a..b", "n"); return err }},
		{"tree", recall.ErrInvalidPath, func() error { _, err := st.MemoryTree(ctx, owner, "a..b", 0); return err }},
		{"delete", recall.ErrInvalidPath, func() error { return st.DeleteMemory(ctx, owner, "a..b", "n") }},
		{"put", recall.ErrInvalidPath, func() error {
			_, err := st.PutMemory(ctx, owner, recall.Memory{Path: "a", Name: "", Body: "b"})
			return err
		}},
		{"get", recall.ErrInvalidPath, func() error { _, err := st.GetMemory(ctx, owner, "a", ""); return err }},
		{"delete", recall.ErrInvalidPath, func() error { return st.DeleteMemory(ctx, owner, "a", "") }},
	}
	seen := map[string]bool{}
	for _, c := range cases {
		if err := c.call(); !errors.Is(err, c.want) {
			t.Errorf("%s = %v, want %v", c.op, err, c.want)
		}
		seen[c.op] = true
	}
	for _, op := range []string{"put", "get", "tree", "delete", "byid"} {
		if !seen[op] {
			t.Errorf("no case exercised %s", op)
		}
	}
}

func TestStoreMemoryPutUpsertsAndClearsEmbedding(t *testing.T) {
	st, pool := testStore(t)
	ctx := context.Background()
	owner := insertUser(t, pool, "mem-upsert")

	first, err := st.PutMemory(ctx, owner, recall.Memory{Path: "proj", Name: "goal", Body: "v1"})
	if err != nil {
		t.Fatalf("put v1: %v", err)
	}
	setEmbedding(t, pool, "conversations.memory", first.ID, "mx", "[1,2,3]")

	second, err := st.PutMemory(ctx, owner, recall.Memory{Path: "proj", Name: "goal", Body: "v2", Meta: json.RawMessage(`{"k":1}`)})
	if err != nil {
		t.Fatalf("put v2: %v", err)
	}
	if second.ID != first.ID {
		t.Fatalf("upsert returned id %s, want the same row id %s", second.ID, first.ID)
	}

	var body string
	var embeddingNull, modelNull bool
	if err := pool.QueryRow(ctx, `SELECT body, embedding IS NULL, embedding_model IS NULL
		FROM conversations.memory WHERE id = $1::uuid`, first.ID).
		Scan(&body, &embeddingNull, &modelNull); err != nil {
		t.Fatalf("read raw row: %v", err)
	}
	if body != "v2" || !embeddingNull || !modelNull {
		t.Fatalf("raw row body=%q embeddingNull=%v modelNull=%v, want v2/true/true", body, embeddingNull, modelNull)
	}

	got, err := st.GetMemory(ctx, owner, "proj", "goal")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	var meta map[string]int
	if err := json.Unmarshal(got.Meta, &meta); err != nil {
		t.Fatalf("meta %s: %v", got.Meta, err)
	}
	if meta["k"] != 1 || got.OwnerUserID != owner {
		t.Fatalf("get = %+v, want meta k=1 and owner set", got)
	}

	// Meta nil means {}.
	if _, err := st.PutMemory(ctx, owner, recall.Memory{Path: "proj", Name: "nometadata", Body: "x"}); err != nil {
		t.Fatalf("put nometadata: %v", err)
	}
	m, err := st.GetMemory(ctx, owner, "proj", "nometadata")
	if err != nil || string(m.Meta) != "{}" {
		t.Fatalf("meta = %s, %v; want {}", m.Meta, err)
	}
}

func TestStoreMemoryDeleteTombstones(t *testing.T) {
	st, pool := testStore(t)
	ctx := context.Background()
	owner := insertUser(t, pool, "mem-tombstone")

	m, err := st.PutMemory(ctx, owner, recall.Memory{Path: "proj", Name: "goal", Body: "v1"})
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	if err := st.DeleteMemory(ctx, owner, "proj", "goal"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	var tombstoned int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM conversations.memory
		WHERE id = $1::uuid AND deleted_at IS NOT NULL`, m.ID).Scan(&tombstoned); err != nil {
		t.Fatalf("read raw row: %v", err)
	}
	if tombstoned != 1 {
		t.Fatalf("tombstone rows = %d, want 1 (delete stamps deleted_at, never a hard delete)", tombstoned)
	}
	if _, err := st.GetMemory(ctx, owner, "proj", "goal"); !errors.Is(err, recall.ErrNotFound) {
		t.Errorf("get after delete = %v, want ErrNotFound", err)
	}
	if err := st.DeleteMemory(ctx, owner, "proj", "goal"); !errors.Is(err, recall.ErrNotFound) {
		t.Errorf("second delete = %v, want ErrNotFound", err)
	}
	if _, err := st.PutMemory(ctx, owner, recall.Memory{Path: "proj", Name: "goal", Body: "v2"}); err != nil {
		t.Fatalf("put after tombstone: %v", err)
	}
	got, err := st.GetMemory(ctx, owner, "proj", "goal")
	if err != nil || got.Body != "v2" || got.ID == m.ID {
		t.Fatalf("get after re-put = %+v, %v; want a fresh row with body v2", got, err)
	}
}

func TestStoreMemoryTreeDepth(t *testing.T) {
	st, pool := testStore(t)
	ctx := context.Background()
	owner := insertUser(t, pool, "mem-tree")

	for _, m := range []recall.Memory{
		{Path: "a", Name: "root"},
		{Path: "a.b", Name: "child"},
		{Path: "a.b.c", Name: "grand"},
		{Path: "a.d", Name: "other"},
	} {
		if _, err := st.PutMemory(ctx, owner, m); err != nil {
			t.Fatalf("put %s/%s: %v", m.Path, m.Name, err)
		}
	}

	paths := func(path string, depth int) []string {
		t.Helper()
		ms, err := st.MemoryTree(ctx, owner, path, depth)
		if err != nil {
			t.Fatalf("tree %s depth %d: %v", path, depth, err)
		}
		out := make([]string, 0, len(ms))
		for _, m := range ms {
			out = append(out, m.Path+"/"+m.Name)
		}
		return out
	}

	// The brief's predicate is path <@ $path, which includes a memory stored
	// AT the root path itself; nlevel bounds only kick in for depth > 0.
	if got := paths("a", 0); strings.Join(got, ",") != "a/root,a.b/child,a.b.c/grand,a.d/other" {
		t.Errorf("tree depth 0 = %v, want all four, ordered by path, name", got)
	}
	if got := paths("a", 1); strings.Join(got, ",") != "a/root,a.b/child,a.d/other" {
		t.Errorf("tree depth 1 = %v, want [a/root a.b/child a.d/other]", got)
	}
	if got := paths("a", 2); strings.Join(got, ",") != "a/root,a.b/child,a.b.c/grand,a.d/other" {
		t.Errorf("tree depth 2 = %v, want [a/root a.b/child a.b.c/grand a.d/other]", got)
	}
}

// --- search ---

func TestStoreSearchBM25FindsWindowSummaryMemory(t *testing.T) {
	st, pool := testStore(t)
	ctx := context.Background()
	owner := insertUser(t, pool, "bm25-owner")
	conv := insertConversation(t, pool, convFixture{Owner: owner, Name: "Alpha", RepoRoot: "/home/me/alpharepo"})
	insertMessage(t, pool, conv, 0, "user", "quartermaster patrolled the north wall", nil)

	// Matching rows plus one decoy each, so rank 1 is earned, not trivial.
	wWant := insertWindow(t, pool, conv, owner, 0, 0, 0, "quartermaster patrolled the north wall", true)
	insertWindow(t, pool, conv, owner, 1, 1, 1, "unrelated weather chatter", true)
	sWant := insertSummary(t, pool, conv, owner, "conversation", 0, 0, 1, "Quartermaster logistics", "the quartermaster kept the ledgers", "1")
	insertSummary(t, pool, conv, owner, "segment", 0, 0, 0, "Weather report", "clouds gathered all afternoon", "1")
	mPut, err := st.PutMemory(ctx, owner, recall.Memory{Path: "proj", Name: "notes", Body: "quartermaster notes for the proj"})
	if err != nil {
		t.Fatalf("put memory: %v", err)
	}
	_ = mPut.ID
	if _, err := st.PutMemory(ctx, owner, recall.Memory{Path: "proj", Name: "weather", Body: "clouds and rain"}); err != nil {
		t.Fatalf("put decoy memory: %v", err)
	}

	q := recall.SearchQuery{Scope: ownerScope(owner), MemoryOwner: owner, Text: "quartermaster", Limit: 10}
	for _, tc := range []struct {
		src   recall.Source
		want  string
		check func(recall.Hit) error
	}{
		{recall.SourceWindow, "w:" + wWant, func(h recall.Hit) error {
			if h.ConversationID != conv || h.ConversationName != "Alpha" || h.Repo != "alpharepo" || h.Kind != "claude" || h.Rank != 1 {
				return fmt.Errorf("window hit fields wrong: %+v", h)
			}
			return nil
		}},
		{recall.SourceSummary, "s:" + sWant, func(h recall.Hit) error {
			if h.Title != "Quartermaster logistics" || h.Rank != 1 || h.Snippet == "" || h.Kind != "claude" {
				return fmt.Errorf("summary hit fields wrong: %+v", h)
			}
			return nil
		}},
		{recall.SourceMemory, "m:" + mPut.ID, func(h recall.Hit) error {
			if h.Path != "proj" || h.Name != "notes" || h.Rank != 1 || h.Snippet == "" {
				return fmt.Errorf("memory hit fields wrong: %+v", h)
			}
			return nil
		}},
	} {
		hits, err := st.SearchBM25(ctx, q, tc.src)
		if err != nil {
			t.Fatalf("search %s: %v", tc.src, err)
		}
		if len(hits) == 0 || hits[0].ID != tc.want {
			t.Fatalf("search %s = %+v, want %q first", tc.src, hits, tc.want)
		}
		if err := tc.check(hits[0]); err != nil {
			t.Fatalf("search %s: %v", tc.src, err)
		}
	}
}

func TestStoreSearchVectorUsesModel(t *testing.T) {
	st, pool := testStore(t)
	ctx := context.Background()
	owner := insertUser(t, pool, "vec-model")
	conv := insertConversation(t, pool, convFixture{Owner: owner, Name: "Vector", RepoRoot: "/srv/vector"})
	insertMessage(t, pool, conv, 0, "user", "vector fixture body", nil)
	wx := insertWindow(t, pool, conv, owner, 0, 0, 0, "window under model x", true)
	wy := insertWindow(t, pool, conv, owner, 1, 1, 1, "window under model y", true)
	mxPut, err := st.PutMemory(ctx, owner, recall.Memory{Path: "v", Name: "x", Body: "memory under model x"})
	if err != nil {
		t.Fatalf("put mx: %v", err)
	}
	myPut, err := st.PutMemory(ctx, owner, recall.Memory{Path: "v", Name: "y", Body: "memory under model y"})
	if err != nil {
		t.Fatalf("put my: %v", err)
	}
	setEmbedding(t, pool, "conversations.conversation_window", wx, "model-x", "[1,0,0]")
	setEmbedding(t, pool, "conversations.conversation_window", wy, "model-y", "[0,1,0]")
	setEmbedding(t, pool, "conversations.memory", mxPut.ID, "model-x", "[1,0,0]")
	setEmbedding(t, pool, "conversations.memory", myPut.ID, "model-y", "[0,1,0]")

	q := recall.SearchQuery{Scope: ownerScope(owner), MemoryOwner: owner, Text: "model", Limit: 10}
	vectorIDs := func(src recall.Source, model string, vec []float32) []string {
		t.Helper()
		hits, err := st.SearchVector(ctx, q, src, model, vec)
		if err != nil {
			t.Fatalf("vector search %s/%s: %v", src, model, err)
		}
		var out []string
		for _, h := range hits {
			out = append(out, h.ID)
		}
		return out
	}

	if got := vectorIDs(recall.SourceWindow, "model-x", []float32{1, 0, 0}); len(got) != 1 || got[0] != "w:"+wx {
		t.Fatalf("window vector hits under model-x = %v, want [w:%s] only", got, wx)
	}
	if got := vectorIDs(recall.SourceMemory, "model-x", []float32{1, 0, 0}); len(got) != 1 || got[0] != "m:"+mxPut.ID {
		t.Fatalf("memory vector hits under model-x = %v, want [m:%s] only", got, mxPut.ID)
	}
	if got := vectorIDs(recall.SourceWindow, "model-y", []float32{0, 1, 0}); len(got) != 1 || got[0] != "w:"+wy {
		t.Fatalf("window vector hits under model-y = %v, want [w:%s] only", got, wy)
	}
}

func TestStoreSearchVectorUsesPartialIndex(t *testing.T) {
	st, pool := testStore(t)
	ctx := context.Background()
	owner := insertUser(t, pool, "vec-index")
	conv := insertConversation(t, pool, convFixture{Owner: owner, Name: "Indexed", RepoRoot: "/srv/indexed"})
	insertMessage(t, pool, conv, 0, "user", "indexed window body", nil)
	wid := insertWindow(t, pool, conv, owner, 0, 0, 0, "indexed window body", true)
	setEmbedding(t, pool, "conversations.conversation_window", wid, "idx-model", "[1,2,3]")

	if err := st.EnsureVectorIndexes(ctx, "idx-model", 3); err != nil {
		t.Fatalf("ensure indexes: %v", err)
	}

	// The generated query must name the model as a literal, or the planner
	// cannot prove the partial HNSW index's predicate; EXPLAIN under
	// enable_seqscan=off dies with a bind parameter there.
	sql, args := searchVectorWindowSQL(recall.SearchQuery{Scope: ownerScope(owner), Text: "body"}, "idx-model", []float32{1, 2, 3}, 10)
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, "SET enable_seqscan = off"); err != nil {
		t.Fatalf("set enable_seqscan: %v", err)
	}
	rows, err := conn.Query(ctx, "EXPLAIN "+sql, args...)
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	defer rows.Close()
	var plan strings.Builder
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatalf("scan plan line: %v", err)
		}
		plan.WriteString(line + "\n")
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("plan rows: %v", err)
	}
	if !strings.Contains(plan.String(), "recall_window_emb_") {
		t.Fatalf("plan does not name the partial HNSW index:\n%s", plan.String())
	}
	if strings.Contains(plan.String(), "Seq Scan") {
		t.Fatalf("plan seq-scans despite enable_seqscan=off:\n%s", plan.String())
	}
}

func TestStoreEnsureVectorIndexesIdempotent(t *testing.T) {
	st, pool := testStore(t)
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		if err := st.EnsureVectorIndexes(ctx, "idem-model", 3); err != nil {
			t.Fatalf("ensure round %d: %v", i, err)
		}
	}
	rows, err := pool.Query(ctx, `SELECT indexname FROM pg_indexes
		WHERE schemaname = 'conversations' ORDER BY 1`)
	if err != nil {
		t.Fatalf("query pg_indexes: %v", err)
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatalf("scan indexname: %v", err)
		}
		if strings.HasPrefix(n, "recall_") && strings.Contains(n, "_emb_") {
			names = append(names, n)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	if len(names) != 3 {
		t.Fatalf("vector index names = %v, want exactly 3", names)
	}
	for _, want := range []string{"recall_memory_emb_", "recall_summary_emb_", "recall_window_emb_"} {
		found := false
		for _, n := range names {
			if strings.HasPrefix(n, want) {
				found = true
			}
		}
		if !found {
			t.Errorf("missing %s<hash> among %v", want, names)
		}
	}
}

// --- indexer ---

func TestStoreExtractCursorsExcludesSummarizerConversations(t *testing.T) {
	st, pool := testStore(t)
	ctx := context.Background()
	owner := insertUser(t, pool, "cursors-owner")
	normal := insertConversation(t, pool, convFixture{Owner: owner, Name: "normal"})
	summarizer := insertConversation(t, pool, convFixture{Owner: owner, Name: "self", Entrypoint: "recall-summary"})
	analyze := insertConversation(t, pool, convFixture{Owner: owner, Name: "analysis", Entrypoint: "analyze"})
	for _, c := range []string{normal, summarizer, analyze} {
		insertMessage(t, pool, c, 0, "user", "fixture text", ago(2*time.Hour))
	}

	cursors, err := st.ExtractCursors(ctx, recall.ExcludedEntrypoints, 10)
	if err != nil {
		t.Fatalf("extract cursors: %v", err)
	}
	seen := map[string]bool{}
	for _, c := range cursors {
		seen[c.Conversation.ID] = true
	}
	if !seen[normal] {
		t.Errorf("normal conversation %s missing from cursors %+v", normal, cursors)
	}
	if seen[summarizer] || seen[analyze] {
		t.Errorf("excluded entrypoints leaked into cursors: %+v", cursors)
	}
}

func TestStoreStoppedRule(t *testing.T) {
	st, pool := testStore(t)
	ctx := context.Background()
	owner := insertUser(t, pool, "stopped-owner")

	cases := []struct {
		name  string
		conv  convFixture
		child *childFixture
		// unlinkedChild inserts the child row with conversation_id NULL, so the
		// conversation links — or fails to link — through external_ref alone:
		// the claude shape, where the thread ref is the only tie.
		unlinkedChild bool
		msgAge        time.Duration
		want          bool
	}{
		{
			name:   "closed_at set",
			conv:   convFixture{Owner: owner, Name: "closed", ClosedAt: ago(time.Minute)},
			msgAge: time.Minute,
			want:   true,
		},
		{
			name:   "linked child exited",
			conv:   convFixture{Owner: owner, Name: "exited", ExternalRef: "c_exited"},
			child:  &childFixture{ID: "c_exited", Status: "exited"},
			msgAge: time.Minute,
			want:   true,
		},
		{
			name:   "linked live child and old messages",
			conv:   convFixture{Owner: owner, Name: "live", ExternalRef: "c_live"},
			child:  &childFixture{ID: "c_live", Status: "running"},
			msgAge: 3 * time.Hour,
			want:   false,
		},
		{
			name:   "no child and old messages",
			conv:   convFixture{Owner: owner, Name: "quiet"},
			msgAge: 3 * time.Hour,
			want:   true,
		},
		{
			name:   "no child and fresh messages",
			conv:   convFixture{Owner: owner, Name: "fresh"},
			msgAge: time.Minute,
			want:   false,
		},
		{
			name:   "claude-style external_ref link counts",
			conv:   convFixture{Owner: owner, Name: "sub", ExternalRef: "c_sub:thread-1"},
			child:  &childFixture{ID: "c_sub", Status: "running"},
			msgAge: 3 * time.Hour,
			want:   false,
		},
		{
			// The child row exists but links to nothing: conversation_id NULL
			// and child_id matching neither the external_ref equality nor the
			// LIKE arm. Unlinked, so the old messages alone decide.
			name:          "different child id does not link",
			conv:          convFixture{Owner: owner, Name: "unlinked", ExternalRef: "c_unlinked:thread-1"},
			child:         &childFixture{ID: "c_other", Status: "running"},
			unlinkedChild: true,
			msgAge:        3 * time.Hour,
			want:          true,
		},
		{
			// The LIKE arm must read the child id's underscore as a literal.
			// Child c_1 ties to its claude-style thread through external_ref
			// alone (conversation_id NULL — the claude shape), and the fresh
			// messages leave the exited-child arm the only thing that can make
			// the conversation stopped: the linkage itself must work.
			name:          "underscore child id links its own thread",
			conv:          convFixture{Owner: owner, Name: "uscore-own", ExternalRef: "c_1:t9"},
			child:         &childFixture{ID: "c_1", Status: "exited"},
			unlinkedChild: true,
			msgAge:        time.Minute,
			want:          true,
		},
		{
			// ...and an external_ref differing ONLY at the underscore position
			// must not link. Unescaped, LIKE 'c_1:%' reads '_' as a wildcard and
			// would stop ca1:t9 through a child it does not belong to; the fresh
			// messages keep every other arm quiet, so the linkage alone decides.
			name:   "same-shaped external_ref without the underscore does not link",
			conv:   convFixture{Owner: owner, Name: "uscore-decoy", ExternalRef: "ca1:t9"},
			msgAge: time.Minute,
			want:   false,
		},
	}
	for _, tc := range cases {
		conv := insertConversation(t, pool, tc.conv)
		if tc.child != nil {
			if tc.unlinkedChild {
				insertChild(t, pool, *tc.child, "")
			} else {
				insertChild(t, pool, *tc.child, conv)
			}
		}
		insertMessage(t, pool, conv, 0, "user", "stopped rule fixture", ago(tc.msgAge))
		got, err := st.Conversation(ctx, recall.Scope{All: true}, conv)
		if err != nil {
			t.Fatalf("%s: conversation: %v", tc.name, err)
		}
		if got.Stopped != tc.want {
			t.Errorf("%s: Stopped = %v, want %v", tc.name, got.Stopped, tc.want)
		}
	}
}

func TestStorePendingEmbedsSkipsLiveTail(t *testing.T) {
	st, pool := testStore(t)
	ctx := context.Background()
	owner := insertUser(t, pool, "pending-owner")
	// Old messages, but a live linked child keeps the conversation unstopped.
	conv := insertConversation(t, pool, convFixture{Owner: owner, Name: "live", ExternalRef: "c_pending"})
	insertChild(t, pool, childFixture{ID: "c_pending", Status: "running"}, conv)
	insertMessage(t, pool, conv, 0, "user", "sealed content", ago(3*time.Hour))
	insertMessage(t, pool, conv, 1, "assistant", "tail content", ago(2*time.Hour))
	sealedID := insertWindow(t, pool, conv, owner, 0, 0, 0, "sealed window text", true)
	tailID := insertWindow(t, pool, conv, owner, 1, 1, 1, "unsealed tail text", false)

	items, err := st.PendingEmbeds(ctx, "pm", 10)
	if err != nil {
		t.Fatalf("pending embeds: %v", err)
	}
	windowIDs := map[string]bool{}
	for _, it := range items {
		if it.Source == recall.SourceWindow {
			windowIDs[it.ID] = true
		}
	}
	if !windowIDs[sealedID] {
		t.Errorf("sealed window %s missing from pending embeds: %v", sealedID, windowIDs)
	}
	if windowIDs[tailID] {
		t.Errorf("unsealed tail of a live conversation must not embed: %v", windowIDs)
	}

	if _, err := pool.Exec(ctx, `UPDATE conversations.conversation SET closed_at = now() WHERE id = $1::uuid`, conv); err != nil {
		t.Fatalf("close conversation: %v", err)
	}
	items, err = st.PendingEmbeds(ctx, "pm", 10)
	if err != nil {
		t.Fatalf("pending embeds after close: %v", err)
	}
	tailFound, headerOK := false, false
	for _, it := range items {
		if it.ID == tailID {
			tailFound = true
			headerOK = strings.HasPrefix(it.Text, "repo: ")
		}
	}
	if !tailFound {
		t.Errorf("tail window %s missing from pending embeds once closed", tailID)
	}
	if !headerOK {
		t.Error("window embed text missing the EmbedHeader prefix")
	}
}

func TestStoreWriteWindowsClearsEmbeddingOnTextChange(t *testing.T) {
	st, pool := testStore(t)
	ctx := context.Background()
	owner := insertUser(t, pool, "write-windows")
	conv := insertConversation(t, pool, convFixture{Owner: owner})

	win := recall.Window{ConversationID: conv, OwnerUserID: owner, Seq: 0, OrdinalFrom: 0, OrdinalTo: 0,
		Text: "one", ExtractorVersion: recall.ExtractorVersion}
	if err := st.WriteWindows(ctx, conv, []recall.Window{win}); err != nil {
		t.Fatalf("write v1: %v", err)
	}
	var winID string
	if err := pool.QueryRow(ctx, `SELECT id::text FROM conversations.conversation_window
		WHERE conversation_id = $1::uuid AND seq = 0`, conv).Scan(&winID); err != nil {
		t.Fatalf("read window id: %v", err)
	}
	setEmbedding(t, pool, "conversations.conversation_window", winID, "wm", "[1,2,3]")

	// Text change clears the embedding.
	win.Text = "two"
	if err := st.WriteWindows(ctx, conv, []recall.Window{win}); err != nil {
		t.Fatalf("write v2: %v", err)
	}
	var embeddingNull, modelNull bool
	if err := pool.QueryRow(ctx, `SELECT embedding IS NULL, embedding_model IS NULL
		FROM conversations.conversation_window WHERE id = $1::uuid`, winID).
		Scan(&embeddingNull, &modelNull); err != nil {
		t.Fatalf("read after change: %v", err)
	}
	if !embeddingNull || !modelNull {
		t.Fatalf("embedding not cleared on text change: embeddingNull=%v modelNull=%v", embeddingNull, modelNull)
	}

	// Same text keeps the embedding: the DISTINCT guard must not fire.
	setEmbedding(t, pool, "conversations.conversation_window", winID, "wm", "[1,2,3]")
	win.Sealed = true
	if err := st.WriteWindows(ctx, conv, []recall.Window{win}); err != nil {
		t.Fatalf("write v3: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT embedding IS NULL FROM conversations.conversation_window
		WHERE id = $1::uuid`, winID).Scan(&embeddingNull); err != nil {
		t.Fatalf("read after no-op: %v", err)
	}
	if embeddingNull {
		t.Error("unchanged text cleared the embedding; the DISTINCT guard is missing")
	}
}

// --- summaries ---

func TestStoreEligibleForSummary(t *testing.T) {
	st, pool := testStore(t)
	ctx := context.Background()
	owner := insertUser(t, pool, "eligible-owner")

	// enabled_at sits 2h back so fixture activity at 90min is both after it
	// and older than QuietPeriod (the quiet clause of the eligibility rule).
	if err := st.SetState(ctx, "summaries_enabled_at", time.Now().UTC().Add(-2*time.Hour).Format(time.RFC3339)); err != nil {
		t.Fatalf("set enabled_at: %v", err)
	}
	opts := recall.EligibleOpts{PromptVersion: recall.SummaryPromptVersion, Model: "sum-model", Limit: 10}

	eligible := insertConversation(t, pool, convFixture{Owner: owner, Name: "eligible"})
	insertMessage(t, pool, eligible, 0, "user", "fresh work", ago(90*time.Minute))

	killed := insertConversation(t, pool, convFixture{Owner: owner, Name: "killed", ExternalRef: "c_killed"})
	insertChild(t, pool, childFixture{ID: "c_killed", Status: "exited"}, killed)
	insertMessage(t, pool, killed, 0, "user", "fresh but killed", ago(90*time.Minute))

	early := insertConversation(t, pool, convFixture{Owner: owner, Name: "early"})
	insertMessage(t, pool, early, 0, "user", "old work", ago(150*time.Minute))

	failed := insertConversation(t, pool, convFixture{Owner: owner, Name: "failed"})
	insertMessage(t, pool, failed, 0, "user", "doomed work", ago(90*time.Minute))

	selfSummary := insertConversation(t, pool, convFixture{Owner: owner, Name: "self", Entrypoint: "recall-summary"})
	insertMessage(t, pool, selfSummary, 0, "assistant", "own summary", ago(90*time.Minute))

	covered := insertConversation(t, pool, convFixture{Owner: owner, Name: "covered"})
	insertMessage(t, pool, covered, 0, "user", "already summarized", ago(90*time.Minute))
	insertSummary(t, pool, covered, owner, "conversation", 0, 0, 0, "t", "whole conversation covered", "1")

	stalePrompt := insertConversation(t, pool, convFixture{Owner: owner, Name: "stale-prompt"})
	insertMessage(t, pool, stalePrompt, 0, "user", "stale summary", ago(90*time.Minute))
	insertSummary(t, pool, stalePrompt, owner, "conversation", 0, 0, 0, "t", "stale prompt_version summary", "0")

	present := func(id string) bool {
		t.Helper()
		got, err := st.EligibleForSummary(ctx, recall.ExcludedEntrypoints, opts)
		if err != nil {
			t.Fatalf("eligible: %v", err)
		}
		for _, c := range got {
			if c.ID == id {
				return true
			}
		}
		return false
	}

	if !present(eligible) {
		t.Error("fresh quiet conversation with no child should be eligible")
	}
	if present(killed) {
		t.Error("killed child (linked row, closed_at NULL) must never be eligible")
	}
	if present(selfSummary) {
		t.Error("origin_entrypoint=recall-summary must never be eligible")
	}
	if present(covered) {
		t.Error("conversation-level summary at the current prompt_version covering all messages blocks eligibility")
	}
	if !present(stalePrompt) {
		t.Error("a stale prompt_version summary must not block eligibility")
	}
	if present(early) {
		t.Error("activity before summaries_enabled_at must not be eligible without backfill")
	}
	if err := st.SetState(ctx, "backfill_since", time.Now().UTC().Add(-4*time.Hour).Format(time.RFC3339)); err != nil {
		t.Fatalf("set backfill: %v", err)
	}
	if !present(early) {
		t.Error("backfill_since covering the activity must make it eligible")
	}
	if err := st.SetState(ctx, "backfill_since", ""); err != nil {
		t.Fatalf("clear backfill: %v", err)
	}
	if present(early) {
		t.Error("backfill_since='' must be treated as unset")
	}

	for i := 0; i < recall.SummaryMaxFailures; i++ {
		if err := st.RecordSummaryFailure(ctx, failed, recall.SummaryPromptVersion, "sum-model", "boom"); err != nil {
			t.Fatalf("record failure %d: %v", i, err)
		}
	}
	if present(failed) {
		t.Errorf("%d failures at the same prompt_version/model must block eligibility", recall.SummaryMaxFailures)
	}
	if err := st.RecordSummaryFailure(ctx, failed, 0, "sum-model", "older prompt"); err != nil {
		t.Fatalf("record old-prompt failure: %v", err)
	}
	if !present(failed) {
		t.Error("failures at an older prompt_version must not block eligibility")
	}
}

func TestStoreUpsertSummaryAccumulatesCost(t *testing.T) {
	st, pool := testStore(t)
	ctx := context.Background()
	owner := insertUser(t, pool, "cost-owner")
	conv := insertConversation(t, pool, convFixture{Owner: owner})

	first := recall.Summary{ConversationID: conv, OwnerUserID: owner, Level: "segment", Seq: 0,
		OrdinalFrom: 0, OrdinalTo: 5, Title: "part one", Summary: "first half",
		PromptVersion: recall.SummaryPromptVersion, Model: "sum-model", CostUSD: 0.01}
	if err := st.UpsertSummary(ctx, first); err != nil {
		t.Fatalf("upsert 1: %v", err)
	}
	second := first
	second.Title = "part one (revised)"
	second.CostUSD = 0.02
	if err := st.UpsertSummary(ctx, second); err != nil {
		t.Fatalf("upsert 2: %v", err)
	}

	var total float64
	if err := pool.QueryRow(ctx, `SELECT total_cost_usd FROM conversations.conversation_summary
		WHERE conversation_id = $1::uuid AND level = 'segment' AND seq = 0`, conv).Scan(&total); err != nil {
		t.Fatalf("read total: %v", err)
	}
	if math.Abs(total-0.03) > 1e-9 {
		t.Fatalf("total_cost_usd = %v, want 0.03", total)
	}
	sums, err := st.Summaries(ctx, conv)
	if err != nil || len(sums) != 1 {
		t.Fatalf("summaries = %+v, %v; want exactly one row", sums, err)
	}
	// CostUSD is this-call-only input; the row read-back carries the
	// accumulated total, so only TotalCostUSD is asserted here.
	if sums[0].Title != "part one (revised)" || math.Abs(sums[0].TotalCostUSD-0.03) > 1e-9 {
		t.Fatalf("summaries[0] = %+v, want revised title and TotalCostUSD 0.03", sums[0])
	}
}

func TestStoreTryLockExclusive(t *testing.T) {
	st, _ := testStore(t)
	ctx := context.Background()

	release1, ok1, err := st.TryLock(ctx)
	if err != nil || !ok1 {
		t.Fatalf("first TryLock: ok=%v err=%v, want true", ok1, err)
	}
	_, ok2, err := st.TryLock(ctx)
	if err != nil {
		t.Fatalf("second TryLock: %v", err)
	}
	if ok2 {
		t.Fatal("second TryLock succeeded while the first lock was held")
	}
	release1()
	release3, ok3, err := st.TryLock(ctx)
	if err != nil || !ok3 {
		t.Fatalf("TryLock after release: ok=%v err=%v, want true", ok3, err)
	}
	release3()
}
