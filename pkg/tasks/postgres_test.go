package tasks_test

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"go.graveland.dev/rafiki/pkg/tasks"
)

func TestPostgresStoreConformance(t *testing.T) {
	pool := testPool(t)

	RunConformance(t, func(t *testing.T) (tasks.Store, string) {
		// Create a fresh conversation row per subtest.
		convID := newTestConversation(t, pool)
		return tasks.NewPostgresStore(pool), convID
	})
}

// testPool connects to RAFIKI_TEST_DSN, skipping the test when it is unset.
func testPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("RAFIKI_TEST_DSN")
	if dsn == "" {
		t.Skip("RAFIKI_TEST_DSN not set")
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// newTestConversation inserts a fresh conversation row and registers cleanup
// for it and any tasks that get attached to it.
func newTestConversation(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	var convID string
	err := pool.QueryRow(context.Background(),
		`INSERT INTO conversations.conversation
			(origin_entrypoint, driven_by)
		 VALUES ('test', 'postgres_store_conformance')
		 RETURNING id`,
	).Scan(&convID)
	if err != nil {
		t.Fatalf("create conversation: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM conversations.tasks WHERE conversation_id = $1`, convID)
		_, _ = pool.Exec(context.Background(),
			`DELETE FROM conversations.conversation WHERE id = $1`, convID)
	})
	return convID
}

// The ctrl_task_list path passes no conversation id: a human asking "what is
// every agent doing" has no single conversation to scope to. Before this was
// fixed, loadAll sent "" to a UUID column and every call to the verb failed
// with SQLSTATE 22P02.
func TestPostgresListWithoutConversationScope(t *testing.T) {
	pool := testPool(t)
	st := tasks.NewPostgresStore(pool)
	ctx := context.Background()

	convA := newTestConversation(t, pool)
	convB := newTestConversation(t, pool)
	if _, err := st.Add(ctx, convA, "", []tasks.NewTask{{Content: "a", ActiveForm: "a-ing"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Add(ctx, convB, "", []tasks.NewTask{{Content: "b", ActiveForm: "b-ing"}}); err != nil {
		t.Fatal(err)
	}

	rows, err := st.List(ctx, tasks.ListFilter{})
	if err != nil {
		t.Fatalf("unscoped List failed: %v", err)
	}
	var sawA, sawB bool
	for _, r := range rows {
		if r.ConversationID == convA {
			sawA = true
		}
		if r.ConversationID == convB {
			sawB = true
		}
	}
	if !sawA || !sawB {
		t.Fatalf("unscoped List must span conversations; sawA=%v sawB=%v", sawA, sawB)
	}

	limited, err := st.List(ctx, tasks.ListFilter{Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(limited) != 1 {
		t.Fatalf("Limit 1 returned %d rows", len(limited))
	}
}
