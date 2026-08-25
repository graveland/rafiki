package capture

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"go.graveland.dev/rafiki/pkg/store"
)

func capturePool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	dsn := os.Getenv("RAFIKI_TEST_DSN")
	if dsn == "" {
		t.Skip("RAFIKI_TEST_DSN not set")
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	if err := store.Migrate(context.Background(), pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// TestCaptureRespectsLease proves the capture path is fenced too. Fencing only
// Messages.Append would leave the second writer to conversation_message
// unguarded, which is the same corruption by a different door.
func TestCaptureRespectsLease(t *testing.T) {
	pool := capturePool(t)
	ctx := context.Background()
	ls := store.NewLeases(pool)

	var conv string
	if err := pool.QueryRow(ctx,
		`INSERT INTO conversations.conversation (origin_entrypoint, driven_by)
		 VALUES ('test','server') RETURNING id::text`).Scan(&conv); err != nil {
		t.Fatalf("insert conversation: %v", err)
	}

	stale, ok, err := ls.Acquire(ctx, conv, "daemon-a", -time.Minute)
	if err != nil || !ok {
		t.Fatalf("Acquire: ok=%v err=%v", ok, err)
	}
	if _, ok, err := ls.Acquire(ctx, conv, "daemon-b", 5*time.Minute); err != nil || !ok {
		t.Fatalf("takeover: ok=%v err=%v", ok, err)
	}

	cs := NewCaptureStore(pool).WithLease(stale)
	err = cs.appendMessageForTest(ctx, conv, 0, "user", []byte(`[{"type":"text","text":"nope"}]`))
	if !errors.Is(err, store.ErrLeaseLost) {
		t.Fatalf("append error = %v, want ErrLeaseLost", err)
	}

	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM conversations.conversation_message WHERE conversation_id = $1::uuid`,
		conv).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Errorf("superseded holder wrote %d messages; want 0", n)
	}
}
