package tasks_test

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"go.graveland.dev/rafiki/pkg/tasks"
)

func TestPostgresStoreConformance(t *testing.T) {
	dsn := os.Getenv("RAFIKI_TEST_DSN")
	if dsn == "" {
		t.Skip("RAFIKI_TEST_DSN not set")
	}

	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)

	RunConformance(t, func(t *testing.T) (tasks.Store, string) {
		// Create a fresh conversation row per subtest.
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
		return tasks.NewPostgresStore(pool), convID
	})
}
