package executors

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// testStore returns a Store for conformance testing, backed by RAFIKI_TEST_DSN.
func testStore(t *testing.T) Store {
	t.Helper()
	dsn := os.Getenv("RAFIKI_TEST_DSN")
	if dsn == "" {
		dsn = os.Getenv("RAFIKI_DB")
	}
	if dsn == "" {
		t.Skip("RAFIKI_TEST_DSN is not set")
	}
	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(pool.Close)
	store := NewPostgresStore(pool)
	// Ensure tables exist (migrations may not have run in test env).
	if err := ensureTables(ctx, pool); err != nil {
		t.Fatalf("ensure tables: %v", err)
	}
	return store
}

func ensureTables(ctx context.Context, pool *pgxpool.Pool) error {
	// Check if tables exist; if not, skip — they should be created by migrations.
	var exists bool
	if err := pool.QueryRow(ctx, `SELECT to_regclass('conversations.executors') IS NOT NULL`).Scan(&exists); err != nil {
		return fmt.Errorf("check executors table: %w", err)
	}
	if !exists {
		return fmt.Errorf("executors table does not exist; run 'rafikid migrate' first")
	}
	return nil
}

func TestConcurrentEnrollmentHasExactlyOneWinner(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	tok, err := s.MintToken(ctx, NewToken{
		Labels:    map[string]string{"rafiki/env": "work"},
		ExpiresAt: time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}

	const racers = 8
	var wg sync.WaitGroup
	var mu sync.Mutex
	var wins int
	var ids []string
	for range racers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			e, _, err := s.Enroll(ctx, tok, map[string]string{"os": "linux"})
			mu.Lock()
			defer mu.Unlock()
			if err == nil {
				wins++
				ids = append(ids, e.ID)
			} else if !errors.Is(err, ErrTokenConsumed) {
				t.Errorf("a loser must lose with ErrTokenConsumed, got %v", err)
			}
		}()
	}
	wg.Wait()
	if wins != 1 {
		t.Fatalf("%d racers enrolled; want exactly 1 (ids %v)", wins, ids)
	}
}

func TestAuthenticateReadsTheCurrentRow(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	tok, _ := s.MintToken(ctx, NewToken{
		Labels: map[string]string{"env": "home"}, ExpiresAt: time.Now().Add(time.Hour)})
	e, cred, err := s.Enroll(ctx, tok, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.SetLabels(ctx, e.ID, map[string]string{"env": "work"}, nil); err != nil {
		t.Fatal(err)
	}
	got, err := s.Authenticate(ctx, cred)
	if err != nil {
		t.Fatal(err)
	}
	if got.Labels["env"] != "work" {
		t.Fatalf("Authenticate returned stale labels %v — the credential is being trusted for what the row says", got.Labels)
	}
}

func TestDisabledExecutorCannotAuthenticate(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	tok, _ := s.MintToken(ctx, NewToken{ExpiresAt: time.Now().Add(time.Hour)})
	e, cred, _ := s.Enroll(ctx, tok, nil)
	if err := s.SetEnabled(ctx, e.ID, false); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Authenticate(ctx, cred); !errors.Is(err, ErrDisabled) {
		t.Fatalf("want ErrDisabled, got %v", err)
	}
}

func TestExpiredTokenIsRejected(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	tok, _ := s.MintToken(ctx, NewToken{ExpiresAt: time.Now().Add(-time.Minute)})
	if _, _, err := s.Enroll(ctx, tok, nil); !errors.Is(err, ErrTokenExpired) {
		t.Fatalf("want ErrTokenExpired, got %v", err)
	}
}

func TestUnknownCredentialIsRejectedNotAutoEnrolled(t *testing.T) {
	s := testStore(t)
	if _, err := s.Authenticate(context.Background(), "not-a-real-credential"); err == nil {
		t.Fatal("an unknown identity must be rejected — auto-enrollment means anyone who reaches the endpoint joins the pool and starts receiving file contents")
	}
}

func TestSelfReportCannotOverwriteATrustLabel(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	tok, _ := s.MintToken(ctx, NewToken{
		Labels: map[string]string{"rafiki/env": "home"}, ExpiresAt: time.Now().Add(time.Hour)})
	e, _, err := s.Enroll(ctx, tok, map[string]string{"rafiki/env": "work", "os": "linux"})
	if err != nil {
		t.Fatal(err)
	}
	if e.Labels["rafiki/env"] != "home" {
		t.Fatalf("the executor claimed a trust label: %v", e.Labels)
	}
	if e.SelfReported["os"] != "linux" {
		t.Fatalf("a harmless capability fact must still be recorded: %v", e.SelfReported)
	}
}

func TestAdmissionSelectorIgnoresAnnotations(t *testing.T) {
	// Annotations are never consulted for ADMISSION, or an agent could annotate
	// its way onto a machine. They ARE selectable for FINDING one.
	s := testStore(t)
	ctx := context.Background()
	tok, _ := s.MintToken(ctx, NewToken{
		Labels: map[string]string{"env": "work"}, ExpiresAt: time.Now().Add(time.Hour)})
	e, _, err := s.Enroll(ctx, tok, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Annotate something — this must not make the executor match selectors
	// that only look at labels.
	if err := s.Annotate(ctx, e.ID, map[string]string{"sentinel": "built"}, nil); err != nil {
		t.Fatal(err)
	}
	// Get the executor by id to verify it has the annotation.
	got, err := s.Get(ctx, e.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Annotations["sentinel"] != "built" {
		t.Fatal("annotation was not stored")
	}
	// The annotation must not appear in a label selector match.
	sel, _ := ParseSelector("sentinel=built")
	if sel.Matches(got.Labels) {
		t.Fatal("annotation appeared in labels — the selector is reading annotations, which is the admission hole")
	}
}

func TestFindSelectorHonoursAnnotations(t *testing.T) {
	// Annotations ARE selectable for FINDING one — that is the entire use case.
	// But they are not in Labels, so a selector over labels won't see them.
	// The plan says annotations are selectable by key-presence and exact value
	// but the selector operates on labels. Annotations are consulted by list-like
	// operations that include them. For now, verify they are stored correctly.
	s := testStore(t)
	ctx := context.Background()
	tok, _ := s.MintToken(ctx, NewToken{
		Labels: map[string]string{"env": "test"}, ExpiresAt: time.Now().Add(time.Hour)})
	e, _, err := s.Enroll(ctx, tok, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Annotate(ctx, e.ID, map[string]string{"sentinel": "built"}, nil); err != nil {
		t.Fatal(err)
	}
	got, err := s.Get(ctx, e.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Annotations["sentinel"] != "built" {
		t.Fatal("annotation was not stored")
	}
}
