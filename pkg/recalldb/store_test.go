package recalldb

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"go.graveland.dev/rafiki/pkg/recall"
	"go.graveland.dev/rafiki/pkg/store"

	"github.com/multigres/testkit/assert"
)

// testStore gives each test its own scratch database, migrated fresh —
// the pattern from pkg/presetsdb/postgres_test.go's testStore, so this never
// touches a developer's real database.
func testStore(t *testing.T) (*Store, *pgxpool.Pool) {
	t.Helper()
	c := assert.NewAborting(t)
	dsn := os.Getenv("RAFIKI_TEST_DSN")
	if dsn == "" {
		c.Eq("", os.Getenv("RAFIKI_REQUIRE_DB"), "RAFIKI_TEST_DSN not set but RAFIKI_REQUIRE_DB is")
		t.Skip("RAFIKI_TEST_DSN not set; skipping integration test")
	}
	ctx := context.Background()

	admin, err := pgxpool.New(ctx, dsn)
	c.NoError(err, "connect admin")
	t.Cleanup(admin.Close)

	name := fmt.Sprintf("rafiki_recalldb_%d", time.Now().UnixNano())
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatalf("create scratch db: %v", err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(context.Background(), "DROP DATABASE "+name+" WITH (FORCE)")
	})

	cfg, err := pgxpool.ParseConfig(dsn)
	c.NoError(err, "parse dsn")
	cfg.ConnConfig.Database = name
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	c.NoError(err, "connect scratch db")
	t.Cleanup(pool.Close)

	c.NoError(store.Migrate(ctx, pool), "migrate")
	return New(pool), pool
}

// --- fixtures (plain SQL; the migration's DDL is authoritative) ---

func insertUser(t *testing.T, pool *pgxpool.Pool, username string) string {
	t.Helper()
	var id string
	assert.NewAborting(t).NoError(pool.QueryRow(context.Background(),
		`INSERT INTO conversations.users (username, token_sha256) VALUES ($1, $2) RETURNING id::text`,
		username, "tok-"+username).Scan(&id), "insert user %s", username)
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
	assert.NewAborting(t).NoError(err, "insert conversation")
	return id
}

func insertMessage(t *testing.T, pool *pgxpool.Pool, convID string, ordinal int, role, text string, createdAt *time.Time) {
	t.Helper()
	_, err := pool.Exec(context.Background(), `INSERT INTO conversations.conversation_message
		(conversation_id, ordinal, role, content, created_at)
		VALUES ($1::uuid, $2, $3, $4, coalesce($5, now()))`,
		convID, ordinal, role, fmt.Sprintf(`[{"type":"text","text":%q}]`, text), createdAt)
	assert.NewAborting(t).NoError(err, "insert message %d", ordinal)
}

type childFixture struct {
	ID               string
	Status           string
	SkipDerivedIndex bool
}

func insertChild(t *testing.T, pool *pgxpool.Pool, f childFixture, convID string) {
	t.Helper()
	_, err := pool.Exec(context.Background(), `INSERT INTO conversations.child
		(child_id, conversation_id, kind, status, spawned_at, skip_derived_index)
		VALUES ($1, $2::uuid, 'claude', $3, now(), $4)`,
		f.ID, nullUUID(convID), f.Status, f.SkipDerivedIndex)
	assert.NewAborting(t).NoError(err, "insert child %s", f.ID)
}

func insertWindow(t *testing.T, pool *pgxpool.Pool, convID, owner string, seq, from, to int, text string, sealed bool) string {
	t.Helper()
	var id string
	assert.NewAborting(t).NoError(pool.QueryRow(context.Background(), `INSERT INTO conversations.conversation_window
		(conversation_id, owner_user_id, seq, ordinal_from, ordinal_to, text, sealed, extractor_version)
		VALUES ($1::uuid, $2::uuid, $3, $4, $5, $6, $7, $8) RETURNING id::text`,
		convID, nullUUID(owner), seq, from, to, text, sealed, recall.ExtractorVersion).Scan(&id), "insert window seq %d", seq)
	return id
}

func insertSummary(t *testing.T, pool *pgxpool.Pool, convID, owner, level string, seq, from, to int, title, summary, promptVersion string) string {
	t.Helper()
	var id string
	assert.NewAborting(t).NoError(pool.QueryRow(context.Background(), `INSERT INTO conversations.conversation_summary
		(conversation_id, owner_user_id, level, seq, ordinal_from, ordinal_to, title, summary, prompt_version, model)
		VALUES ($1::uuid, $2::uuid, $3, $4, $5, $6, $7, $8, $9, 'sum-model') RETURNING id::text`,
		convID, nullUUID(owner), level, seq, from, to, title, summary, promptVersion).Scan(&id), "insert summary")
	return id
}

// setEmbedding stamps a row's vector directly, as the embed pass would.
func setEmbedding(t *testing.T, pool *pgxpool.Pool, table, id, model, vec string) {
	t.Helper()
	_, err := pool.Exec(context.Background(),
		`UPDATE `+table+` SET embedding = $2::vector, embedding_model = $3 WHERE id = $1::uuid`,
		id, vec, model)
	assert.NewAborting(t).NoError(err, "set embedding on %s %s", table, id)
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
	c := assert.NewCollecting(t)
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
	c.NoError(err, "SearchBM25(memory) with zero Scope")
	c.Empty(hits, "SearchBM25(memory) returned %d hits, want 0", len(hits))
}

// --- memories ---

func mustPut(t *testing.T, st *Store, owner, path, name string) string {
	t.Helper()
	m, err := st.PutMemory(context.Background(), owner, recall.Memory{Path: path, Name: name, Body: name + " body"})
	assert.NewAborting(t).NoError(err, "put %s/%s", path, name)
	return m.ID
}

func TestStoreMemoryOwnerIsolation(t *testing.T) {
	c := assert.NewCollecting(t)
	st, pool := testStore(t)
	ctx := context.Background()
	a := insertUser(t, pool, "mem-iso-a")
	b := insertUser(t, pool, "mem-iso-b")

	aID := mustPut(t, st, a, "proj.alpha", "notes")

	if _, err := st.GetMemory(ctx, b, "proj.alpha", "notes"); !errors.Is(err, recall.ErrNotFound) {
		t.Errorf("b get a's memory = %v, want ErrNotFound", err)
	}
	tree, err := st.MemoryTree(ctx, b, "proj", 0)
	c.Require().NoError(err, "b tree")
	c.Empty(tree, "b tree")
	hits, err := st.SearchBM25(ctx, recall.SearchQuery{Text: "notes", MemoryOwner: b}, recall.SourceMemory)
	c.Require().NoError(err, "b search")
	c.Empty(hits, "b search saw %d hits, want 0", len(hits))
	if _, err := st.MemoryByID(ctx, b, aID); !errors.Is(err, recall.ErrNotFound) {
		t.Errorf("b MemoryByID on a's row = %v, want ErrNotFound", err)
	}
	got, err := st.GetMemory(ctx, a, "proj.alpha", "notes")
	c.False(err != nil || got.Body != "notes body", "a get = %+v, %v; want own row", got, err)
}

func TestStoreMemoryRequiresOwnerAndValidPath(t *testing.T) {
	ck := assert.NewCollecting(t)
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
		err := c.call()
		ck.ErrorIs(err, c.want, "%s = %v, want", c.op, err)
		seen[c.op] = true
	}
	for _, op := range []string{"put", "get", "tree", "delete", "byid"} {
		ck.False(!seen[op], "no case exercised %s", op)
	}
}

func TestStoreMemoryPutUpsertsAndClearsEmbedding(t *testing.T) {
	c := assert.NewAborting(t)
	st, pool := testStore(t)
	ctx := context.Background()
	owner := insertUser(t, pool, "mem-upsert")

	first, err := st.PutMemory(ctx, owner, recall.Memory{Path: "proj", Name: "goal", Body: "v1"})
	c.NoError(err, "put v1")
	setEmbedding(t, pool, "conversations.memory", first.ID, "mx", "[1,2,3]")

	second, err := st.PutMemory(ctx, owner, recall.Memory{Path: "proj", Name: "goal", Body: "v2", Meta: json.RawMessage(`{"k":1}`)})
	c.NoError(err, "put v2")
	c.Eq(first.ID, second.ID, "upsert returned id")

	var body string
	var embeddingNull, modelNull bool
	c.NoError(pool.QueryRow(ctx, `SELECT body, embedding IS NULL, embedding_model IS NULL
		FROM conversations.memory WHERE id = $1::uuid`, first.ID).
		Scan(&body, &embeddingNull, &modelNull), "read raw row")
	c.False(body != "v2" || !embeddingNull || !modelNull, "raw row body=%q embeddingNull=%v modelNull=%v, want v2/true/true", body, embeddingNull, modelNull)

	got, err := st.GetMemory(ctx, owner, "proj", "goal")
	c.NoError(err, "get")
	var meta map[string]int
	c.NoError(json.Unmarshal(got.Meta, &meta), "meta %s", got.Meta)
	c.False(meta["k"] != 1 || got.OwnerUserID != owner, "get = %+v, want meta k=1 and owner set", got)

	// Meta nil means {}.
	if _, err := st.PutMemory(ctx, owner, recall.Memory{Path: "proj", Name: "nometadata", Body: "x"}); err != nil {
		t.Fatalf("put nometadata: %v", err)
	}
	m, err := st.GetMemory(ctx, owner, "proj", "nometadata")
	c.False(err != nil || string(m.Meta) != "{}", "meta = %s, %v; want {}", m.Meta, err)
}

func TestStoreMemoryDeleteTombstones(t *testing.T) {
	c := assert.NewCollecting(t)
	st, pool := testStore(t)
	ctx := context.Background()
	owner := insertUser(t, pool, "mem-tombstone")

	m, err := st.PutMemory(ctx, owner, recall.Memory{Path: "proj", Name: "goal", Body: "v1"})
	c.Require().NoError(err, "put")
	c.Require().NoError(st.DeleteMemory(ctx, owner, "proj", "goal"), "delete")
	var tombstoned int
	c.Require().NoError(pool.QueryRow(ctx, `SELECT count(*) FROM conversations.memory
		WHERE id = $1::uuid AND deleted_at IS NOT NULL`, m.ID).Scan(&tombstoned), "read raw row")
	c.Require().Eq(1, tombstoned, "tombstone rows")
	if _, err := st.GetMemory(ctx, owner, "proj", "goal"); !errors.Is(err, recall.ErrNotFound) {
		t.Errorf("get after delete = %v, want ErrNotFound", err)
	}
	c.ErrorIs(st.DeleteMemory(ctx, owner, "proj", "goal"), recall.ErrNotFound, "second delete")
	if _, err := st.PutMemory(ctx, owner, recall.Memory{Path: "proj", Name: "goal", Body: "v2"}); err != nil {
		t.Fatalf("put after tombstone: %v", err)
	}
	got, err := st.GetMemory(ctx, owner, "proj", "goal")
	c.Require().False(err != nil || got.Body != "v2" || got.ID == m.ID, "get after re-put = %+v, %v; want a fresh row with body v2", got, err)
}

func TestStoreMemoryTreeDepth(t *testing.T) {
	c := assert.NewCollecting(t)
	st, pool := testStore(t)
	ctx := context.Background()
	owner := insertUser(t, pool, "mem-tree")

	for _, m := range []recall.Memory{
		{Path: "a", Name: "root"},
		{Path: "a.b", Name: "child"},
		{Path: "a.b.c", Name: "grand"},
		{Path: "a.d", Name: "other"},
	} {
		_, err := st.PutMemory(ctx, owner, m)
		c.Require().NoError(err, "put %s/%s", m.Path, m.Name)
	}

	paths := func(path string, depth int) []string {
		t.Helper()
		ms, err := st.MemoryTree(ctx, owner, path, depth)
		c.Require().NoError(err, "tree %s depth %d", path, depth)
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
	got := paths("a", 2)
	c.Eq("a/root,a.b/child,a.b.c/grand,a.d/other", strings.Join(got, ","), "tree depth 2 = %v, want [a/root a.b/child a.b.c/grand a.d/other]", got)
}

// --- search ---

func TestStoreSearchBM25FindsWindowSummaryMemory(t *testing.T) {
	c := assert.NewAborting(t)
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
	c.NoError(err, "put memory")
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
		c.NoError(err, "search %s", tc.src)
		c.False(len(hits) == 0 || hits[0].ID != tc.want, "search %s = %+v, want %q first", tc.src, hits, tc.want)
		c.NoError(tc.check(hits[0]), "search %s", tc.src)
	}
}

func TestStoreSearchVectorUsesModel(t *testing.T) {
	c := assert.NewAborting(t)
	st, pool := testStore(t)
	ctx := context.Background()
	owner := insertUser(t, pool, "vec-model")
	conv := insertConversation(t, pool, convFixture{Owner: owner, Name: "Vector", RepoRoot: "/srv/vector"})
	insertMessage(t, pool, conv, 0, "user", "vector fixture body", nil)
	wx := insertWindow(t, pool, conv, owner, 0, 0, 0, "window under model x", true)
	wy := insertWindow(t, pool, conv, owner, 1, 1, 1, "window under model y", true)
	mxPut, err := st.PutMemory(ctx, owner, recall.Memory{Path: "v", Name: "x", Body: "memory under model x"})
	c.NoError(err, "put mx")
	myPut, err := st.PutMemory(ctx, owner, recall.Memory{Path: "v", Name: "y", Body: "memory under model y"})
	c.NoError(err, "put my")
	setEmbedding(t, pool, "conversations.conversation_window", wx, "model-x", "[1,0,0]")
	setEmbedding(t, pool, "conversations.conversation_window", wy, "model-y", "[0,1,0]")
	setEmbedding(t, pool, "conversations.memory", mxPut.ID, "model-x", "[1,0,0]")
	setEmbedding(t, pool, "conversations.memory", myPut.ID, "model-y", "[0,1,0]")

	q := recall.SearchQuery{Scope: ownerScope(owner), MemoryOwner: owner, Text: "model", Limit: 10}
	vectorIDs := func(src recall.Source, model string, vec []float32) []string {
		t.Helper()
		hits, err := st.SearchVector(ctx, q, src, model, vec)
		c.NoError(err, "vector search %s/%s", src, model)
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
	c := assert.NewAborting(t)
	st, pool := testStore(t)
	ctx := context.Background()
	owner := insertUser(t, pool, "vec-index")
	conv := insertConversation(t, pool, convFixture{Owner: owner, Name: "Indexed", RepoRoot: "/srv/indexed"})
	insertMessage(t, pool, conv, 0, "user", "indexed window body", nil)
	wid := insertWindow(t, pool, conv, owner, 0, 0, 0, "indexed window body", true)
	setEmbedding(t, pool, "conversations.conversation_window", wid, "idx-model", "[1,2,3]")

	c.NoError(st.EnsureVectorIndexes(ctx, "idx-model", 3), "ensure indexes")

	// The generated query must name the model as a literal, or the planner
	// cannot prove the partial HNSW index's predicate; EXPLAIN under
	// enable_seqscan=off dies with a bind parameter there.
	sql, args := searchVectorWindowSQL(recall.SearchQuery{Scope: ownerScope(owner), Text: "body"}, "idx-model", []float32{1, 2, 3}, 10)
	conn, err := pool.Acquire(ctx)
	c.NoError(err, "acquire")
	defer conn.Release()
	if _, err := conn.Exec(ctx, "SET enable_seqscan = off"); err != nil {
		t.Fatalf("set enable_seqscan: %v", err)
	}
	rows, err := conn.Query(ctx, "EXPLAIN "+sql, args...)
	c.NoError(err, "explain")
	defer rows.Close()
	var plan strings.Builder
	for rows.Next() {
		var line string
		c.NoError(rows.Scan(&line), "scan plan line")
		plan.WriteString(line + "\n")
	}
	c.NoError(rows.Err(), "plan rows")
	c.StrContains(plan.String(), "recall_window_emb_", "plan does not name the partial HNSW index:\n")
	c.NotStrContains(plan.String(), "Seq Scan", "plan seq-scans despite enable_seqscan=off:\n")
}

func TestStoreEnsureVectorIndexesIdempotent(t *testing.T) {
	c := assert.NewCollecting(t)
	st, pool := testStore(t)
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		c.Require().NoError(st.EnsureVectorIndexes(ctx, "idem-model", 3), "ensure round %d", i)
	}
	rows, err := pool.Query(ctx, `SELECT indexname FROM pg_indexes
		WHERE schemaname = 'conversations' ORDER BY 1`)
	c.Require().NoError(err, "query pg_indexes")
	defer rows.Close()
	var names []string
	for rows.Next() {
		var n string
		c.Require().NoError(rows.Scan(&n), "scan indexname")
		if strings.HasPrefix(n, "recall_") && strings.Contains(n, "_emb_") {
			names = append(names, n)
		}
	}
	c.Require().NoError(rows.Err(), "rows")
	c.Require().Len(names, 3, "vector index names")
	for _, want := range []string{"recall_memory_emb_", "recall_summary_emb_", "recall_window_emb_"} {
		found := false
		for _, n := range names {
			if strings.HasPrefix(n, want) {
				found = true
			}
		}
		c.True(found, "missing %s<hash> among %v", want, names)
	}
}

// --- indexer ---

func TestStoreExtractCursorsExcludesSummarizerConversations(t *testing.T) {
	ck := assert.NewCollecting(t)
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
	ck.Require().NoError(err, "extract cursors")
	seen := map[string]bool{}
	for _, c := range cursors {
		seen[c.Conversation.ID] = true
	}
	ck.False(!seen[normal], "normal conversation %s missing from cursors %+v", normal, cursors)
	ck.False(seen[summarizer] || seen[analyze], "excluded entrypoints leaked into cursors: %+v", cursors)
}

func TestStoreStoppedRule(t *testing.T) {
	c := assert.NewCollecting(t)
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
			// This case only falsifies because the cases above share this scratch
			// database: the exited child c_1 they insert must still be present
			// when this conversation is evaluated.
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
		c.Require().NoError(err, "%s: conversation", tc.name)
		c.Eq(tc.want, got.Stopped, "%s: Stopped = %v, want", tc.name, got.Stopped)
	}
}

func TestStorePendingEmbedsSkipsLiveTail(t *testing.T) {
	c := assert.NewCollecting(t)
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
	c.Require().NoError(err, "pending embeds")
	windowIDs := map[string]bool{}
	for _, it := range items {
		if it.Source == recall.SourceWindow {
			windowIDs[it.ID] = true
		}
	}
	c.False(!windowIDs[sealedID], "sealed window %s missing from pending embeds: %v", sealedID, windowIDs)
	c.False(windowIDs[tailID], "unsealed tail of a live conversation must not embed: %v", windowIDs)

	if _, err := pool.Exec(ctx, `UPDATE conversations.conversation SET closed_at = now() WHERE id = $1::uuid`, conv); err != nil {
		t.Fatalf("close conversation: %v", err)
	}
	items, err = st.PendingEmbeds(ctx, "pm", 10)
	c.Require().NoError(err, "pending embeds after close")
	tailFound, headerOK := false, false
	for _, it := range items {
		if it.ID == tailID {
			tailFound = true
			headerOK = strings.HasPrefix(it.Text, "repo: ")
		}
	}
	c.True(tailFound, "tail window %s missing from pending embeds once closed", tailID)
	c.True(headerOK, "window embed text missing the EmbedHeader prefix")
}

func TestStorePendingEmbedsNullNameConversation(t *testing.T) {
	c := assert.NewAborting(t)
	st, pool := testStore(t)
	ctx := context.Background()
	owner := insertUser(t, pool, "pending-null-name")
	// Prod shape the wave-2 fixtures never had: a conversation with a NULL
	// name (and NULL repo_root). PendingEmbeds' summary/window queries join
	// the conversation for the EmbedHeader meta — an uncoalesced c.name scan
	// fails the whole embed pass with "cannot scan NULL into *string" the
	// first time one of these rows is pending (observed live on prod).
	conv := insertConversation(t, pool, convFixture{Owner: owner, RepoRoot: ""})
	if _, err := pool.Exec(ctx, `UPDATE conversations.conversation SET name = NULL, repo_root = NULL WHERE id = $1::uuid`, conv); err != nil {
		t.Fatalf("null out name/repo: %v", err)
	}
	insertMessage(t, pool, conv, 0, "user", "sealed content", ago(3*time.Hour))
	sealedID := insertWindow(t, pool, conv, owner, 0, 0, 0, "sealed window text", true)

	items, err := st.PendingEmbeds(ctx, "pn", 10)
	c.NoError(err, "pending embeds with a NULL-name conversation")
	var found *recall.EmbedItem
	for i := range items {
		if items[i].ID == sealedID {
			found = &items[i]
		}
	}
	c.NotNil(found, "sealed window %s missing from pending embeds: %v", sealedID, items)
	// EmbedHeader renders the empty name as an empty quoted string and the
	// missing repo as "-"; either way the header, not a scan error.
	wantPrefix := "repo: - · conversation: \"\" · "
	if !strings.HasPrefix(found.Text, wantPrefix) {
		t.Errorf("embed text %q, want prefix %q", found.Text, wantPrefix)
	}
}

func TestStoreWriteWindowsClearsEmbeddingOnTextChange(t *testing.T) {
	c := assert.NewCollecting(t)
	st, pool := testStore(t)
	ctx := context.Background()
	owner := insertUser(t, pool, "write-windows")
	conv := insertConversation(t, pool, convFixture{Owner: owner})

	win := recall.Window{ConversationID: conv, OwnerUserID: owner, Seq: 0, OrdinalFrom: 0, OrdinalTo: 0,
		Text: "one", ExtractorVersion: recall.ExtractorVersion}
	c.Require().NoError(st.WriteWindows(ctx, conv, []recall.Window{win}), "write v1")
	var winID string
	c.Require().NoError(pool.QueryRow(ctx, `SELECT id::text FROM conversations.conversation_window
		WHERE conversation_id = $1::uuid AND seq = 0`, conv).Scan(&winID), "read window id")
	setEmbedding(t, pool, "conversations.conversation_window", winID, "wm", "[1,2,3]")

	// Text change clears the embedding.
	win.Text = "two"
	c.Require().NoError(st.WriteWindows(ctx, conv, []recall.Window{win}), "write v2")
	var embeddingNull, modelNull bool
	c.Require().NoError(pool.QueryRow(ctx, `SELECT embedding IS NULL, embedding_model IS NULL
		FROM conversations.conversation_window WHERE id = $1::uuid`, winID).
		Scan(&embeddingNull, &modelNull), "read after change")
	c.Require().False(!embeddingNull || !modelNull, "embedding not cleared on text change: embeddingNull=%v modelNull=%v", embeddingNull, modelNull)

	// Same text keeps the embedding: the DISTINCT guard must not fire.
	setEmbedding(t, pool, "conversations.conversation_window", winID, "wm", "[1,2,3]")
	win.Sealed = true
	c.Require().NoError(st.WriteWindows(ctx, conv, []recall.Window{win}), "write v3")
	c.Require().NoError(pool.QueryRow(ctx, `SELECT embedding IS NULL FROM conversations.conversation_window
		WHERE id = $1::uuid`, winID).Scan(&embeddingNull), "read after no-op")
	c.False(embeddingNull, "unchanged text cleared the embedding; the DISTINCT guard is missing")
}

// --- summaries ---

func TestStoreEligibleForSummary(t *testing.T) {
	ck := assert.NewCollecting(t)
	st, pool := testStore(t)
	ctx := context.Background()
	owner := insertUser(t, pool, "eligible-owner")

	// enabled_at sits 2h back so fixture activity at 90min is both after it
	// and older than QuietPeriod (the quiet clause of the eligibility rule).
	ck.Require().NoError(st.SetState(ctx, "summaries_enabled_at", time.Now().UTC().Add(-2*time.Hour).Format(time.RFC3339)), "set enabled_at")
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
		ck.Require().NoError(err, "eligible")
		for _, c := range got {
			if c.ID == id {
				return true
			}
		}
		return false
	}

	ck.True(present(eligible), "fresh quiet conversation with no child should be eligible")
	ck.False(present(killed), "killed child (linked row, closed_at NULL) must never be eligible")
	ck.False(present(selfSummary), "origin_entrypoint=recall-summary must never be eligible")
	ck.False(present(covered), "conversation-level summary at the current prompt_version covering all messages blocks eligibility")
	ck.True(present(stalePrompt), "a stale prompt_version summary must not block eligibility")
	ck.False(present(early), "activity before summaries_enabled_at must not be eligible without backfill")
	ck.Require().NoError(st.SetState(ctx, "backfill_since", time.Now().UTC().Add(-4*time.Hour).Format(time.RFC3339)), "set backfill")
	ck.True(present(early), "backfill_since covering the activity must make it eligible")
	ck.Require().NoError(st.SetState(ctx, "backfill_since", ""), "clear backfill")
	ck.False(present(early), "backfill_since='' must be treated as unset")

	for i := 0; i < recall.SummaryMaxFailures; i++ {
		ck.Require().NoError(st.RecordSummaryFailure(ctx, failed, recall.SummaryPromptVersion, "sum-model", "boom"), "record failure %d", i)
	}
	if present(failed) {
		t.Errorf("%d failures at the same prompt_version/model must block eligibility", recall.SummaryMaxFailures)
	}
	ck.Require().NoError(st.RecordSummaryFailure(ctx, failed, 0, "sum-model", "older prompt"), "record old-prompt failure")
	ck.True(present(failed), "failures at an older prompt_version must not block eligibility")
}

func TestStoreUpsertSummaryAccumulatesCost(t *testing.T) {
	c := assert.NewAborting(t)
	st, pool := testStore(t)
	ctx := context.Background()
	owner := insertUser(t, pool, "cost-owner")
	conv := insertConversation(t, pool, convFixture{Owner: owner})

	first := recall.Summary{ConversationID: conv, OwnerUserID: owner, Level: "segment", Seq: 0,
		OrdinalFrom: 0, OrdinalTo: 5, Title: "part one", Summary: "first half",
		PromptVersion: recall.SummaryPromptVersion, Model: "sum-model", CostUSD: 0.01}
	c.NoError(st.UpsertSummary(ctx, first), "upsert 1")
	second := first
	second.Title = "part one (revised)"
	second.CostUSD = 0.02
	c.NoError(st.UpsertSummary(ctx, second), "upsert 2")

	var total float64
	c.NoError(pool.QueryRow(ctx, `SELECT total_cost_usd FROM conversations.conversation_summary
		WHERE conversation_id = $1::uuid AND level = 'segment' AND seq = 0`, conv).Scan(&total), "read total")
	c.LessOrEqual(1e-9, math.Abs(total-0.03), "total_cost_usd = %v, want 0.03", total)
	sums, err := st.Summaries(ctx, conv)
	c.False(err != nil || len(sums) != 1, "summaries = %+v, %v; want exactly one row", sums, err)
	// CostUSD is this-call-only input; the row read-back carries the
	// accumulated total, so only TotalCostUSD is asserted here.
	if sums[0].Title != "part one (revised)" || math.Abs(sums[0].TotalCostUSD-0.03) > 1e-9 {
		t.Fatalf("summaries[0] = %+v, want revised title and TotalCostUSD 0.03", sums[0])
	}
}

func TestStoreTryLockExclusive(t *testing.T) {
	c := assert.NewAborting(t)
	st, _ := testStore(t)
	ctx := context.Background()

	release1, ok1, err := st.TryLock(ctx)
	c.False(err != nil || !ok1, "first TryLock: ok=%v err=%v, want true", ok1, err)
	_, ok2, err := st.TryLock(ctx)
	c.NoError(err, "second TryLock")
	c.False(ok2, "second TryLock succeeded while the first lock was held")
	release1()
	release3, ok3, err := st.TryLock(ctx)
	c.False(err != nil || !ok3, "TryLock after release: ok=%v err=%v, want true", ok3, err)
	release3()
}

// --- skip_derived_index gates ---

// markDerivedSkip sets a conversation's derived_skip through WriteWindows, the
// production mark path, without writing any window rows.
func markDerivedSkip(t *testing.T, st *Store, convID string) {
	t.Helper()
	assert.NewAborting(t).NoError(st.WriteWindows(context.Background(), convID, nil), "mark derived_skip")
}

func derivedSkipOf(t *testing.T, pool *pgxpool.Pool, convID string) bool {
	t.Helper()
	var skip bool
	assert.NewAborting(t).NoError(pool.QueryRow(context.Background(),
		`SELECT derived_skip FROM conversations.conversation WHERE id = $1::uuid`, convID).Scan(&skip), "read derived_skip")
	return skip
}

// skipFakeEmbedder records the embed calls the indexer makes.
type skipFakeEmbedder struct {
	model string
	calls int
	texts []string
}

func (f *skipFakeEmbedder) Model() string   { return f.model }
func (f *skipFakeEmbedder) Dimensions() int { return 3 }
func (f *skipFakeEmbedder) Embed(ctx context.Context, in []string) ([][]float32, error) {
	f.calls++
	f.texts = append(f.texts, in...)
	out := make([][]float32, len(in))
	for i := range in {
		out[i] = []float32{1, 2, 3}
	}
	return out, nil
}

// skipFakeSummaryPass records which conversations a summary pass would take.
type skipFakeSummaryPass struct {
	store    *Store
	passes   int
	eligible []string
}

func (f *skipFakeSummaryPass) Pass(ctx context.Context) error {
	f.passes++
	rows, err := f.store.EligibleForSummary(ctx, recall.ExcludedEntrypoints,
		recall.EligibleOpts{PromptVersion: recall.SummaryPromptVersion, Model: "skip-model", Limit: 50})
	if err != nil {
		return err
	}
	for _, c := range rows {
		f.eligible = append(f.eligible, c.ID)
	}
	return nil
}

func TestSkipDerivedIndexGatesEmbedsAndSummaries(t *testing.T) {
	ck := assert.NewCollecting(t)
	st, pool := testStore(t)
	ctx := context.Background()
	owner := insertUser(t, pool, "skip-gates")
	ck.Require().NoError(st.SetState(ctx, "summaries_enabled_at", time.Now().UTC().Add(-2*time.Hour).Format(time.RFC3339)), "set enabled_at")
	promptVersion := fmt.Sprint(recall.SummaryPromptVersion)

	flagged := insertConversation(t, pool, convFixture{Owner: owner, Name: "flagged", ClosedAt: ago(90 * time.Minute)})
	insertMessage(t, pool, flagged, 0, "user", "flagged work", ago(90*time.Minute))
	flaggedWin := insertWindow(t, pool, flagged, owner, 0, 0, 0, "flagged window text", true)
	flaggedSum := insertSummary(t, pool, flagged, owner, "conversation", 0, 0, 0, "flagged title", "flagged summary", promptVersion)
	insertChild(t, pool, childFixture{ID: fmt.Sprintf("c_skipidx%d", time.Now().UnixNano()), Status: "running", SkipDerivedIndex: true}, flagged)
	markDerivedSkip(t, st, flagged)

	// The control has two messages but a summary covering only the first, so it
	// is both pending (embedding NULL) and still eligible for a fresh summary.
	control := insertConversation(t, pool, convFixture{Owner: owner, Name: "control", ClosedAt: ago(90 * time.Minute)})
	insertMessage(t, pool, control, 0, "user", "control work", ago(90*time.Minute))
	insertMessage(t, pool, control, 1, "assistant", "more control work", ago(89*time.Minute))
	controlWin := insertWindow(t, pool, control, owner, 0, 0, 0, "control window text", true)
	controlSum := insertSummary(t, pool, control, owner, "conversation", 0, 0, 0, "control title", "control summary", promptVersion)

	winRows, err := st.pendingWindowEmbeds(ctx, "skip-model", 50)
	ck.Require().NoError(err, "pending window embeds")
	winSeen := map[string]bool{}
	for _, it := range winRows {
		winSeen[it.ID] = true
	}
	ck.True(winSeen[controlWin], "control window must still embed")
	ck.False(winSeen[flaggedWin], "window of a derived-skip conversation must not embed")

	sumRows, err := st.pendingSummaryEmbeds(ctx, "skip-model", 50)
	ck.Require().NoError(err, "pending summary embeds")
	sumSeen := map[string]bool{}
	for _, it := range sumRows {
		sumSeen[it.ID] = true
	}
	ck.True(sumSeen[controlSum], "control summary must still embed")
	ck.False(sumSeen[flaggedSum], "summary of a derived-skip conversation must not embed")

	opts := recall.EligibleOpts{PromptVersion: recall.SummaryPromptVersion, Model: "sum-model", Limit: 50}
	eligible, err := st.EligibleForSummary(ctx, recall.ExcludedEntrypoints, opts)
	ck.Require().NoError(err, "eligible")
	present := map[string]bool{}
	for _, c := range eligible {
		present[c.ID] = true
	}
	ck.True(present[control], "control conversation must still be summarizable")
	ck.False(present[flagged], "conversation marked derived_skip must not be summarizable")
}

func TestSkipDerivedIndexCoversClaudeLinkage(t *testing.T) {
	ck := assert.NewCollecting(t)
	st, pool := testStore(t)
	ctx := context.Background()
	owner := insertUser(t, pool, "skip-claude")

	// A claude child has no conversation_id; its conversations link by
	// external_ref = child_id and by external_ref = child_id:<thread>. The
	// child id carries an underscore so the LIKE escape is exercised.
	childID := fmt.Sprintf("c_skipidx%d", time.Now().UnixNano())
	plain := insertConversation(t, pool, convFixture{Owner: owner, Name: "plain", ExternalRef: childID})
	plainWin := insertWindow(t, pool, plain, owner, 0, 0, 0, "plain window", true)

	threaded := insertConversation(t, pool, convFixture{Owner: owner, Name: "threaded", ExternalRef: childID + ":sub"})
	threadedWin := insertWindow(t, pool, threaded, owner, 0, 0, 0, "threaded window", true)

	insertChild(t, pool, childFixture{ID: childID, Status: "running", SkipDerivedIndex: true}, "")
	markDerivedSkip(t, st, plain)
	markDerivedSkip(t, st, threaded)

	control := insertConversation(t, pool, convFixture{Owner: owner, Name: "control"})
	controlWin := insertWindow(t, pool, control, owner, 0, 0, 0, "control window", true)

	rows, err := st.pendingWindowEmbeds(ctx, "skip-model", 50)
	ck.Require().NoError(err, "pending window embeds")
	seen := map[string]bool{}
	for _, it := range rows {
		seen[it.ID] = true
	}
	ck.True(seen[controlWin], "control window must still embed")
	ck.False(seen[plainWin], "conversation linked by external_ref = child_id must not embed")
	ck.False(seen[threadedWin], "conversation linked by external_ref = child_id:<thread> must not embed")
}

func TestSkipDerivedIndexStillExtractsWindows(t *testing.T) {
	ck := assert.NewCollecting(t)
	st, pool := testStore(t)
	ctx := context.Background()
	owner := insertUser(t, pool, "skip-extract")

	conv := insertConversation(t, pool, convFixture{Owner: owner, Name: "flagged"})
	insertMessage(t, pool, conv, 0, "user", "needs a window", ago(time.Minute))
	insertChild(t, pool, childFixture{ID: fmt.Sprintf("c_skipidx%d", time.Now().UnixNano()), Status: "running", SkipDerivedIndex: true}, conv)
	markDerivedSkip(t, st, conv)

	// Windows are still built for a marked conversation: the gate lives only on
	// the embed and summary queries.
	cursors, err := st.ExtractCursors(ctx, recall.ExcludedEntrypoints, 50)
	ck.Require().NoError(err, "extract cursors")
	found := false
	for _, cur := range cursors {
		if cur.Conversation.ID == conv {
			found = true
		}
	}
	ck.True(found, "a derived-skip conversation must still be extracted")
}

func TestMarkerDoesNotDeleteExistingEmbeddings(t *testing.T) {
	ck := assert.NewCollecting(t)
	st, pool := testStore(t)
	ctx := context.Background()
	owner := insertUser(t, pool, "skip-existing")

	conv := insertConversation(t, pool, convFixture{Owner: owner, Name: "flagged", ClosedAt: ago(90 * time.Minute)})
	win := insertWindow(t, pool, conv, owner, 0, 0, 0, "existing window", true)
	setEmbedding(t, pool, "conversations.conversation_window", win, "skip-model", "[1,2,3]")
	insertChild(t, pool, childFixture{ID: fmt.Sprintf("c_skipidx%d", time.Now().UnixNano()), Status: "running", SkipDerivedIndex: true}, conv)
	markDerivedSkip(t, st, conv)

	rows, err := st.pendingWindowEmbeds(ctx, "skip-model", 50)
	ck.Require().NoError(err, "pending window embeds")
	for _, it := range rows {
		ck.False(it.ID == win, "an already-embedded window of a marked conversation must not reappear as pending")
	}

	var embeddingNull bool
	var model string
	ck.Require().NoError(pool.QueryRow(ctx, `SELECT embedding IS NULL, coalesce(embedding_model, '')
		FROM conversations.conversation_window WHERE id = $1::uuid`, win).Scan(&embeddingNull, &model), "read embedding")
	ck.False(embeddingNull, "the marker must not delete an existing embedding")
	ck.Eq("skip-model", model, "existing embedding_model must be unchanged")
}

func TestSkipDerivedIndexDoesNotGateMemoryEmbeds(t *testing.T) {
	ck := assert.NewCollecting(t)
	st, pool := testStore(t)
	ctx := context.Background()
	owner := insertUser(t, pool, "skip-memory")

	// Memory is source data, not conversation-derived: a marked conversation
	// must not stop a pending memory from embedding.
	conv := insertConversation(t, pool, convFixture{Owner: owner, Name: "flagged"})
	insertChild(t, pool, childFixture{ID: fmt.Sprintf("c_skipidx%d", time.Now().UnixNano()), Status: "running", SkipDerivedIndex: true}, conv)
	markDerivedSkip(t, st, conv)

	memID := mustPut(t, st, owner, "proj.skipmem", "notes")
	rows, err := st.pendingMemoryEmbeds(ctx, "skip-model", 50)
	ck.Require().NoError(err, "pending memory embeds")
	found := false
	for _, it := range rows {
		if it.ID == memID {
			found = true
		}
	}
	ck.True(found, "a pending memory must still embed while a marked conversation exists")
}

func TestWriteWindowsMarksFlaggedConversation(t *testing.T) {
	ck := assert.NewCollecting(t)
	st, pool := testStore(t)
	ctx := context.Background()
	owner := insertUser(t, pool, "skip-mark")
	uniq := time.Now().UnixNano()

	// conversation_id link.
	byConv := insertConversation(t, pool, convFixture{Owner: owner, Name: "by-conv"})
	insertChild(t, pool, childFixture{ID: fmt.Sprintf("c_skipmark%d_a", uniq), Status: "running", SkipDerivedIndex: true}, byConv)

	// external_ref = child_id link.
	refID := fmt.Sprintf("c_skipmark%d_b", uniq)
	byRef := insertConversation(t, pool, convFixture{Owner: owner, Name: "by-ref", ExternalRef: refID})
	insertChild(t, pool, childFixture{ID: refID, Status: "running", SkipDerivedIndex: true}, "")

	// external_ref = child_id:<thread> link.
	threadID := fmt.Sprintf("c_skipmark%d_c", uniq)
	byThread := insertConversation(t, pool, convFixture{Owner: owner, Name: "by-thread", ExternalRef: threadID + ":sub"})
	insertChild(t, pool, childFixture{ID: threadID, Status: "running", SkipDerivedIndex: true}, "")

	// control: an unflagged child links to it.
	control := insertConversation(t, pool, convFixture{Owner: owner, Name: "control-mark"})
	insertChild(t, pool, childFixture{ID: fmt.Sprintf("c_skipmark%d_ok", uniq), Status: "running", SkipDerivedIndex: false}, control)

	window := func(conv string) []recall.Window {
		return []recall.Window{{ConversationID: conv, OwnerUserID: owner, Seq: 0, OrdinalFrom: 0, OrdinalTo: 0,
			Text: "mark window", Sealed: true, ExtractorVersion: recall.ExtractorVersion}}
	}
	for _, conv := range []string{byConv, byRef, byThread, control} {
		ck.Require().NoError(st.WriteWindows(ctx, conv, window(conv)), "WriteWindows %s", conv)
	}

	ck.True(derivedSkipOf(t, pool, byConv), "conversation_id link must mark")
	ck.True(derivedSkipOf(t, pool, byRef), "external_ref = child_id link must mark")
	ck.True(derivedSkipOf(t, pool, byThread), "external_ref = child_id:<thread> link must mark")
	ck.False(derivedSkipOf(t, pool, control), "an unflagged child must leave derived_skip false")
}

func TestMarkingHappensBeforeEmbedAndSummaryInTheSameTick(t *testing.T) {
	ck := assert.NewCollecting(t)
	st, pool := testStore(t)
	ctx := context.Background()
	owner := insertUser(t, pool, "skip-tick")
	ck.Require().NoError(st.SetState(ctx, "summaries_enabled_at", time.Now().UTC().Add(-2*time.Hour).Format(time.RFC3339)), "set enabled_at")

	// flagged: closed, has messages, no windows yet, linked to a flagged child.
	flagged := insertConversation(t, pool, convFixture{Owner: owner, Name: "tick-flagged", ClosedAt: ago(90 * time.Minute)})
	insertMessage(t, pool, flagged, 0, "user", "flaggedticktoken", ago(90*time.Minute))
	insertChild(t, pool, childFixture{ID: fmt.Sprintf("c_skipidx%d", time.Now().UnixNano()), Status: "running", SkipDerivedIndex: true}, flagged)

	// control: the same shape, no child.
	control := insertConversation(t, pool, convFixture{Owner: owner, Name: "tick-control", ClosedAt: ago(90 * time.Minute)})
	insertMessage(t, pool, control, 0, "user", "controlticktoken", ago(90*time.Minute))

	emb := &skipFakeEmbedder{model: "skip-model"}
	sum := &skipFakeSummaryPass{store: st}
	ix := recall.NewIndexer(recall.IndexerOptions{
		Store: st, Embedder: emb, Summaries: sum,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	ck.Require().NoError(ix.Tick(ctx), "tick")

	// Extract ran first and marked the flagged conversation before the embed
	// and summary passes read it.
	ck.True(derivedSkipOf(t, pool, flagged), "extract must mark the flagged conversation")
	ck.False(derivedSkipOf(t, pool, control), "the control stays unmarked")

	ck.True(emb.calls > 0, "the control window must still be embedded")
	for _, tx := range emb.texts {
		ck.False(strings.Contains(tx, "flaggedticktoken"), "the flagged window must never be embedded")
	}
	present := map[string]bool{}
	for _, id := range sum.eligible {
		present[id] = true
	}
	ck.True(present[control], "the control must be summarizable in the same tick")
	ck.False(present[flagged], "the flagged conversation must not be summarizable")
}

func TestStatusDoesNotCountFlaggedWindowsAsUnembedded(t *testing.T) {
	ck := assert.NewCollecting(t)
	st, pool := testStore(t)
	ctx := context.Background()
	owner := insertUser(t, pool, "skip-status")
	promptVersion := fmt.Sprint(recall.SummaryPromptVersion)

	flagged := insertConversation(t, pool, convFixture{Owner: owner, Name: "status-flagged", ClosedAt: ago(90 * time.Minute)})
	insertWindow(t, pool, flagged, owner, 0, 0, 0, "flagged status window", true)
	insertSummary(t, pool, flagged, owner, "conversation", 0, 0, 0, "flagged", "flagged", promptVersion)
	insertChild(t, pool, childFixture{ID: fmt.Sprintf("c_skipidx%d", time.Now().UnixNano()), Status: "running", SkipDerivedIndex: true}, flagged)
	markDerivedSkip(t, st, flagged)

	control := insertConversation(t, pool, convFixture{Owner: owner, Name: "status-control", ClosedAt: ago(90 * time.Minute)})
	insertWindow(t, pool, control, owner, 0, 0, 0, "control status window", true)
	insertSummary(t, pool, control, owner, "conversation", 0, 0, 0, "control", "control", promptVersion)

	stt, err := st.Status(ctx)
	ck.Require().NoError(err, "status")
	ck.Eq(int64(2), stt.Windows, "total windows is untouched")
	ck.Eq(int64(2), stt.Summaries, "total summaries is untouched")
	ck.Eq(int64(1), stt.WindowsUnembedded, "only the control window counts as unembedded")
	ck.Eq(int64(1), stt.SummariesPending, "only the control summary counts as pending")
}

func TestSkipDerivedIndexPendingQueryIsFast(t *testing.T) {
	ck := assert.NewCollecting(t)
	st, pool := testStore(t)
	ctx := context.Background()
	owner := insertUser(t, pool, "skip-perf")
	uniq := time.Now().UnixNano()

	flagged := insertConversation(t, pool, convFixture{Owner: owner, Name: "perf-flagged"})
	_, err := pool.Exec(ctx, `INSERT INTO conversations.conversation_window
			(conversation_id, owner_user_id, seq, ordinal_from, ordinal_to, text, sealed, extractor_version)
		SELECT $1::uuid, $2::uuid, g, g, g, 'perf window ' || g::text, true, $3
		FROM generate_series(0, 1999) g`, flagged, nullUUID(owner), recall.ExtractorVersion)
	ck.Require().NoError(err, "seed flagged windows")

	_, err = pool.Exec(ctx, `INSERT INTO conversations.child
			(child_id, conversation_id, kind, status, spawned_at, skip_derived_index)
		SELECT 'c_skipmark' || $2::bigint || '_' || g, $1::uuid, 'claude', 'running', now(), true
		FROM generate_series(0, 499) g`, flagged, uniq)
	ck.Require().NoError(err, "seed flagged children")

	_, err = pool.Exec(ctx, `UPDATE conversations.conversation SET derived_skip = true WHERE id = $1::uuid`, flagged)
	ck.Require().NoError(err, "mark flagged conversation")

	control := insertConversation(t, pool, convFixture{Owner: owner, Name: "perf-control"})
	insertWindow(t, pool, control, owner, 0, 0, 0, "perf control window", true)

	start := time.Now()
	rows, err := st.pendingWindowEmbeds(ctx, "skip-model", 50)
	elapsed := time.Since(start)
	ck.Require().NoError(err, "pending window embeds")
	ck.True(elapsed < time.Second, "pending-window query took %s over 2000 flagged windows", elapsed)
	ck.Eq(1, len(rows), "only the control window must be pending")
	ck.True(strings.Contains(rows[0].Text, "perf control window"), "control window must be the pending row")
}
