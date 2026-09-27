package capture

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"go.graveland.dev/rafiki/pkg/store"

	"github.com/multigres/testkit/assert"
)

func capturePool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	c := assert.NewAborting(t)
	dsn := os.Getenv("RAFIKI_TEST_DSN")
	if dsn == "" {
		t.Skip("RAFIKI_TEST_DSN not set")
	}
	pool, err := pgxpool.New(context.Background(), dsn)
	c.NoError(err, "pool")
	c.NoError(store.Migrate(context.Background(), pool), "migrate")
	t.Cleanup(pool.Close)
	return pool
}

// TestCaptureRespectsLease proves the capture path is fenced too. Fencing only
// Messages.Append would leave the second writer to conversation_message
// unguarded, which is the same corruption by a different door.
func TestCaptureRespectsLease(t *testing.T) {
	c := assert.NewCollecting(t)
	pool := capturePool(t)
	ctx := context.Background()
	ls := store.NewLeases(pool)

	var conv string
	c.Require().NoError(pool.QueryRow(ctx,
		`INSERT INTO conversations.conversation (origin_entrypoint, driven_by)
		 VALUES ('test','server') RETURNING id::text`).Scan(&conv), "insert conversation")

	stale, ok, err := ls.Acquire(ctx, conv, "daemon-a", -time.Minute)
	c.Require().False(err != nil || !ok, "Acquire: ok=%v err=%v", ok, err)
	if _, ok, err := ls.Acquire(ctx, conv, "daemon-b", 5*time.Minute); err != nil || !ok {
		t.Fatalf("takeover: ok=%v err=%v", ok, err)
	}

	cs := NewCaptureStore(pool).WithLease(stale)
	err = cs.appendMessageForTest(ctx, conv, 0, "user", []byte(`[{"type":"text","text":"nope"}]`))
	c.Require().ErrorIs(err, store.ErrLeaseLost, "append error")

	var n int
	c.Require().NoError(pool.QueryRow(ctx,
		`SELECT count(*) FROM conversations.conversation_message WHERE conversation_id = $1::uuid`,
		conv).Scan(&n), "count")
	c.Eq(0, n, "superseded holder wrote")
}
