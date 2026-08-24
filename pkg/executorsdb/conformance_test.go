package executorsdb

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"go.graveland.dev/rafiki/pkg/executors"
)

// testStore returns a Store for conformance testing, backed by RAFIKI_TEST_DSN.
func testStore(t *testing.T) executors.Store {
	t.Helper()
	dsn := os.Getenv("RAFIKI_TEST_DSN")
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
	tok, err := s.MintToken(ctx, executors.NewToken{
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
	tok, _ := s.MintToken(ctx, executors.NewToken{
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
	tok, _ := s.MintToken(ctx, executors.NewToken{ExpiresAt: time.Now().Add(time.Hour)})
	e, cred, _ := s.Enroll(ctx, tok, nil)
	if err := s.SetEnabled(ctx, e.ID, false); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Authenticate(ctx, cred); !errors.Is(err, ErrDisabled) {
		t.Fatalf("want ErrDisabled, got %v", err)
	}
}

func TestDeleteRemovesTheRow(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	tok, _ := s.MintToken(ctx, executors.NewToken{ExpiresAt: time.Now().Add(time.Hour)})
	e, _, err := s.Enroll(ctx, tok, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Delete(ctx, e.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := s.Get(ctx, e.ID); !errors.Is(err, executors.ErrNotFound) {
		t.Fatalf("Get after delete: want ErrNotFound, got %v", err)
	}
}

func TestDeleteUnknownIDIsNotFound(t *testing.T) {
	s := testStore(t)
	if err := s.Delete(context.Background(), "00000000-0000-0000-0000-000000000000"); !errors.Is(err, executors.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestExpiredTokenIsRejected(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	tok, _ := s.MintToken(ctx, executors.NewToken{ExpiresAt: time.Now().Add(-time.Minute)})
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
	tok, _ := s.MintToken(ctx, executors.NewToken{
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
	tok, _ := s.MintToken(ctx, executors.NewToken{
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
	sel, _ := executors.ParseSelector("sentinel=built")
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
	tok, _ := s.MintToken(ctx, executors.NewToken{
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

// machineName returns a machine label unique to this test run. The conformance
// tests share one long-lived database and never clean up their rows, so a
// fixed name would collide with the previous run's row rather than with the
// row the test itself created — passing for the wrong reason once and failing
// forever after.
func machineName(t *testing.T) string {
	t.Helper()
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return "laptop-" + hex.EncodeToString(b[:])
}

// createExecutor mints a row with the given labels and deletes it afterwards,
// so a run does not leave a name claimed for the next one.
//
// Returns the row as well as the error because the relabel test needs an id to
// aim SetLabels at; callers that only care whether the insert was refused
// discard it.
func createExecutor(t *testing.T, s executors.Store, labels map[string]string) (executors.Executor, error) {
	t.Helper()
	e, _, err := s.Create(context.Background(), executors.NewToken{
		Labels: labels,
		// Not decoration: Create passes Roots straight through to a NOT NULL
		// TEXT[] column, so a nil slice inserts NULL and the row is rejected
		// before the unique index is ever consulted -- which would make these
		// tests "pass" on the wrong error.
		Roots:     []string{},
		ExpiresAt: time.Now().Add(time.Hour),
	})
	if err == nil {
		t.Cleanup(func() { _ = s.Delete(context.Background(), e.ID) })
	}
	return e, err
}

// mustCreateExecutor is createExecutor for the seeding steps, where a failure
// is the fixture breaking rather than the thing under test.
func mustCreateExecutor(t *testing.T, s executors.Store, labels map[string]string) executors.Executor {
	t.Helper()
	e, err := createExecutor(t, s, labels)
	if err != nil {
		t.Fatalf("seed executor %v: %v", labels, err)
	}
	return e
}

func TestTwoExecutorsCannotShareAnOwnerAndMachine(t *testing.T) {
	s := testStore(t)
	machine := machineName(t)

	mustCreateExecutor(t, s, map[string]string{"owner": "brent", "machine": machine})
	_, err := createExecutor(t, s, map[string]string{"owner": "brent", "machine": machine})
	if err == nil {
		t.Fatal("a second executor claiming the same owner+machine must be " +
			"refused: an interactive client picks the durable executor for its " +
			"box by exactly that pair, and two matches is a coin flip over which " +
			"filesystem a child lands on")
	}

	// WHICH failure matters as much as that there was one. This insert path
	// also carries a live nil-Roots defect that rejects the row with 23502
	// (not-null violation) BEFORE the unique index is ever consulted, so a
	// bare err != nil would keep passing with the index dropped -- the exact
	// false green a well-meaning tidy-up of the Roots argument would cause.
	//
	// ErrMachineNameTaken is the assertion that pins it, and a stronger one
	// than the SQLSTATE check it replaces: duplicateMachineName produces this
	// sentinel ONLY for 23505 on executors_owner_machine_unique by name, so a
	// not-null violation cannot satisfy it and neither can another unique
	// index added to this table later. The store no longer returns the raw
	// pgconn error here -- it must not, since this same translation travels to
	// an unauthenticated peer on the enrollment path.
	if !errors.Is(err, executors.ErrMachineNameTaken) {
		t.Fatalf("want executors.ErrMachineNameTaken, got %T: %v — the row was "+
			"rejected by something other than the (owner, machine) index, so "+
			"this test is no longer evidence that the index exists", err, err)
	}
}

func TestTwoOwnersMayEachHaveALaptop(t *testing.T) {
	s := testStore(t)
	machine := machineName(t)
	for _, owner := range []string{"brent", "sam"} {
		if _, err := createExecutor(t, s, map[string]string{"owner": owner, "machine": machine}); err != nil {
			t.Fatalf("owner %s: %v", owner, err)
		}
	}
}

func TestExecutorsWithNoMachineLabelAreUnconstrained(t *testing.T) {
	s := testStore(t)
	for range 3 {
		if _, err := createExecutor(t, s, map[string]string{"owner": "brent", "env": "prod"}); err != nil {
			t.Fatalf("a fleet executor needs no machine name: %v", err)
		}
	}
}

// The (owner, machine) index does not fire when a token is MINTED -- a token
// lives in executor_enrollment_token, not in executors -- so two tokens with
// the same --name both mint successfully and the collision surfaces here, at
// redemption, on the executor's own connection.
//
// That path never reaches translateExecutorErr. It reaches writeAuthFailure,
// which asks IsTerminalAuthError; a raw 23505 matches no sentinel, falls
// through to Retryable, and pkg/execpool's dial loop reconnects. The loser then
// retries forever against a name it can never claim while the daemon logs
// "could not verify an executor credential" every attempt, so the operator sees
// an executor that simply never appears. The sentinel is what stops the loop.
func TestEnrollRefusesATokenWhoseMachineNameIsTaken(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	machine := machineName(t)
	labels := map[string]string{"owner": "brent", "machine": machine}

	mustCreateExecutor(t, s, labels)

	tok, err := s.MintToken(ctx, executors.NewToken{
		Labels: labels,
		// Roots explicitly empty for the same reason createExecutor does it:
		// a nil slice inserts NULL into a NOT NULL column and the row is
		// rejected with 23502 BEFORE the unique index is consulted, which
		// would make this test pass on entirely the wrong error.
		Roots:     []string{},
		ExpiresAt: time.Now().Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("minting must still succeed -- the index does not cover tokens: %v", err)
	}

	e, _, err := s.Enroll(ctx, tok, nil)
	if err == nil {
		t.Cleanup(func() { _ = s.Delete(context.Background(), e.ID) })
		t.Fatal("redeeming a token for an already-claimed (owner, machine) must fail")
	}
	if !errors.Is(err, executors.ErrMachineNameTaken) {
		t.Fatalf("Enroll returned %#v (%v); want an error satisfying "+
			"errors.Is(err, executors.ErrMachineNameTaken) -- anything else is "+
			"classified retryable and loops the executor forever", err, err)
	}
	// The peer receives this text verbatim, so it must be the sentinel's own
	// message and not a pgx one: a pgconn error carries the DSN to a peer that
	// has not proved who it is.
	if strings.Contains(err.Error(), "SQLSTATE") || strings.Contains(err.Error(), "23505") {
		t.Fatalf("the store's raw text must not travel to the peer: %q", err.Error())
	}
}

// Only THIS index is a name collision. Another unique index on the executors
// table later -- on the credential hash, say -- must inherit neither the rename
// advice nor, more importantly, the terminal classification that stops an
// executor retrying.
func TestOnlyTheOwnerMachineIndexMeansTheNameIsTaken(t *testing.T) {
	other := &pgconn.PgError{Code: uniqueViolation, ConstraintName: "executors_credential_hash_key"}
	if got := duplicateMachineName(other); got != nil {
		t.Fatalf("an unrelated unique index reported %v, want nil", got)
	}
	notUnique := &pgconn.PgError{Code: "23502", ConstraintName: ownerMachineIndex}
	if got := duplicateMachineName(notUnique); got != nil {
		t.Fatalf("a not-null violation reported %v, want nil", got)
	}
	if got := duplicateMachineName(errors.New("dial tcp: connection refused")); got != nil {
		t.Fatalf("a non-pg error reported %v, want nil", got)
	}
	match := &pgconn.PgError{Code: uniqueViolation, ConstraintName: ownerMachineIndex}
	if !errors.Is(duplicateMachineName(match), executors.ErrMachineNameTaken) {
		t.Fatal("the (owner, machine) index must translate to ErrMachineNameTaken")
	}
}

// The remedy the collision message recommends must not itself collide opaquely.
// SetLabels rewrites the whole labels map, so relabelling one executor onto a
// name another already holds trips the same index -- and between the two
// commits that introduced the sentinel this path lost its mapping and answered
// ERR_INTERNAL ("the daemon is broken") to an operator following the advice the
// daemon had just given them.
func TestRelabellingOntoATakenMachineNameIsRefused(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	taken, mine := machineName(t), machineName(t)

	mustCreateExecutor(t, s, map[string]string{"owner": "brent", "machine": taken})
	other := mustCreateExecutor(t, s, map[string]string{"owner": "brent", "machine": mine})

	_, err := s.SetLabels(ctx, other.ID, map[string]string{"machine": taken}, nil)
	if err == nil {
		t.Fatal("relabelling onto a name this owner already uses must be refused: " +
			"an interactive client picks its durable executor by (owner, machine), " +
			"and two matches is a coin flip over which filesystem a child lands on")
	}
	if !errors.Is(err, executors.ErrMachineNameTaken) {
		t.Fatalf("SetLabels returned %T: %v; want an error satisfying "+
			"errors.Is(err, executors.ErrMachineNameTaken) -- anything else "+
			"reaches the operator as ERR_INTERNAL", err, err)
	}
	if strings.Contains(err.Error(), "SQLSTATE") || strings.Contains(err.Error(), "23505") {
		t.Fatalf("the store's raw text must not travel: %q", err.Error())
	}

	// The refused UPDATE must leave the row alone -- a partially applied
	// relabel would be worse than the refusal.
	after, err := s.Get(ctx, other.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Labels["machine"] != mine {
		t.Fatalf("machine = %q after a refused relabel, want %q unchanged",
			after.Labels["machine"], mine)
	}
}

// Relabelling onto a name a DIFFERENT owner holds is fine -- the key is the
// pair, and two operators may each have a laptop.
func TestRelabellingOntoAnotherOwnersMachineNameIsAllowed(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	shared, mine := machineName(t), machineName(t)

	mustCreateExecutor(t, s, map[string]string{"owner": "sam", "machine": shared})
	mine1 := mustCreateExecutor(t, s, map[string]string{"owner": "brent", "machine": mine})

	e, err := s.SetLabels(ctx, mine1.ID, map[string]string{"machine": shared}, nil)
	if err != nil {
		t.Fatalf("sam holding %q must not stop brent using it: %v", shared, err)
	}
	if e.Labels["machine"] != shared {
		t.Fatalf("machine = %q, want %q", e.Labels["machine"], shared)
	}
}
