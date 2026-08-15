package tasks_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
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

// Drop must refuse when ANY row in the subtree has a live assignee, and that
// must hold against a CONCURRENT Assign, not just a sequential one. The
// conformance suite structurally cannot express this: memoryStore.Drop holds
// its own mutex across the whole check-and-mutate and is genuinely atomic,
// so the shared suite passes on both stores throughout.
//
// The barrier is a real row lock rather than a sleep, which is what makes the
// outcome deterministic in both directions. An uncommitted UPDATE in tx B
// locks the child row, so BOTH versions of Drop block — the fixed one on its
// `FOR UPDATE`, the unfixed one later on its own UPDATE of the same row. Once
// B commits, READ COMMITTED re-reads the row for whichever statement was
// waiting:
//
//	fixed:   the SELECT ... FOR UPDATE now sees assignee='c_live' -> ErrAssigned
//	unfixed: the UPDATE re-checks only id and status, both still matching,
//	         and drops a task a live agent is holding.
func TestDropRefusesAgainstAConcurrentAssign(t *testing.T) {
	pool := testPool(t)
	st := tasks.NewPostgresStore(pool)
	ctx := context.Background()
	conv := newTestConversation(t, pool)

	if _, err := st.Add(ctx, conv, "", []tasks.NewTask{{Content: "parent"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Add(ctx, conv, "1", []tasks.NewTask{{Content: "child"}}); err != nil {
		t.Fatal(err)
	}
	rows, err := st.List(ctx, tasks.ListFilter{ConversationID: conv})
	if err != nil {
		t.Fatal(err)
	}
	var childID string
	for _, r := range rows {
		if r.Handle == "1.1" {
			childID = r.ID
		}
	}
	if childID == "" {
		t.Fatalf("fixture: no child task; handles = %v", handlesOf(rows))
	}

	// tx B assigns the CHILD and holds the row lock without committing.
	txB, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = txB.Rollback(ctx) }()
	if _, err := txB.Exec(ctx,
		`UPDATE conversations.tasks SET assignee = 'c_live' WHERE id = $1`, childID,
	); err != nil {
		t.Fatal(err)
	}

	// Drop the PARENT — the child is only reachable through the subtree walk,
	// which is the half a fix that locked "rows with an assignee today" would
	// miss.
	dropCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	dropErr := make(chan error, 1)
	go func() {
		_, err := st.Drop(dropCtx, conv, "1", "no longer needed")
		dropErr <- err
	}()

	select {
	case err := <-dropErr:
		t.Fatalf("Drop returned before the competing assign committed (%v); "+
			"the row lock barrier did not hold, so this run proves nothing", err)
	case <-time.After(500 * time.Millisecond):
		// Blocked on the row lock, as both versions must be.
	}

	if err := txB.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	if err := <-dropErr; !errors.Is(err, tasks.ErrAssigned) {
		t.Fatalf("Drop returned %v; want ErrAssigned — a task held by a live agent was dropped", err)
	}

	after, err := st.List(ctx, tasks.ListFilter{ConversationID: conv, IncludeDropped: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range after {
		if r.Status == tasks.StatusDropped {
			t.Errorf("task %q was dropped despite the refusal; Drop is not atomic", r.Content)
		}
	}
}

// Drop now holds row locks across its whole subtree, which is exactly the
// shape that introduces deadlocks. This hammers the three mutating paths
// against one conversation and fails on SQLSTATE 40P01 specifically — an
// ErrAssigned or ErrNotFound from a losing racer is a correct outcome and
// must not be mistaken for one.
func TestConcurrentAddAssignDropDoNotDeadlock(t *testing.T) {
	pool := testPool(t)
	st := tasks.NewPostgresStore(pool)
	ctx := context.Background()
	conv := newTestConversation(t, pool)

	if _, err := st.Add(ctx, conv, "", []tasks.NewTask{
		{Content: "a"}, {Content: "b"}, {Content: "c"},
	}); err != nil {
		t.Fatal(err)
	}
	for _, h := range []string{"1", "2", "3"} {
		if _, err := st.Add(ctx, conv, h, []tasks.NewTask{{Content: "child of " + h}}); err != nil {
			t.Fatal(err)
		}
	}

	const rounds = 12
	var wg sync.WaitGroup
	errs := make(chan error, rounds*3)
	start := make(chan struct{})
	for i := range rounds {
		h := []string{"1", "2", "3"}[i%3]
		for _, op := range []func() error{
			func() error { _, err := st.Drop(ctx, conv, h, "racing"); return err },
			func() error { _, err := st.Assign(ctx, conv, h+".1", "c_racer"); return err },
			func() error {
				_, err := st.Add(ctx, conv, h, []tasks.NewTask{{Content: "late"}})
				return err
			},
		} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				errs <- op()
			}()
		}
	}
	close(start)
	wg.Wait()
	close(errs)

	for err := range errs {
		if err == nil {
			continue
		}
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "40P01" {
			t.Fatalf("deadlock between Add, Assign and Drop: %v", err)
		}
		// Anything else is a legitimate refusal from a losing racer.
	}
}

func handlesOf(rows []tasks.Task) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.Handle)
	}
	return out
}

// Two agents calling task_add at the same instant in one conversation must
// not both get handle "1". The row lock in Add cannot protect an empty
// partition, so this is enforced by an advisory lock plus a unique
// constraint that treats NULL parent_id as equal.
func TestPostgresConcurrentFirstAddDoesNotDuplicateOrdinal(t *testing.T) {
	pool := testPool(t)
	st := tasks.NewPostgresStore(pool)
	ctx := context.Background()
	conv := newTestConversation(t, pool)

	const n = 8
	var wg sync.WaitGroup
	errs := make([]error, n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, errs[i] = st.Add(ctx, conv, "", []tasks.NewTask{
				{Content: fmt.Sprintf("task %d", i), ActiveForm: "doing"},
			})
		}(i)
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("add %d failed: %v", i, err)
		}
	}

	rows, err := st.List(ctx, tasks.ListFilter{ConversationID: conv})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != n {
		t.Fatalf("got %d tasks, want %d", len(rows), n)
	}
	seen := map[string]bool{}
	for _, r := range rows {
		if seen[r.Handle] {
			t.Fatalf("handle %q assigned twice — two tasks share one handle", r.Handle)
		}
		seen[r.Handle] = true
	}
}
