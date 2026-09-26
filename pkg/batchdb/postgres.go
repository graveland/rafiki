// SPDX-License-Identifier: Apache-2.0

// Package batchdb is the Postgres implementation of batch.Store (pkg/batch),
// backed by the conversations.batch_call table (migration 0041). The Batcher
// (pkg/batch) is the only writer: it reads a snapshot with Live/InState, acts,
// and writes back — so transitions are conditional, exactly like
// pkg/batch.MemStore: each Mark*/Requeue/Complete/Fail applies only to a live
// row in the state it moves from, and is a silent no-op otherwise. A stale
// sweep's write after an outcome already landed therefore clobbers nothing.
//
// Store errors: the adopt-boundary decision for task 1.3's review is to make
// these PLAIN errors, not non-retryable ones. Park (the only path that turns
// a store error into a send result) never reaches the database — the caller
// pre-flights with Live before Insert — and a mid-flight store failure fails
// the turn as a retryable transport-class error, which is correct: the row may
// or may not exist. The non-retryable contract is reserved for terminal batch
// outcomes (batch.Error).
package batchdb

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"go.graveland.dev/rafiki/pkg/batch"
)

// Store is the Postgres batch.Store.
type Store struct {
	pool *pgxpool.Pool
}

// New returns a Store backed by pool. The pool's target schema must already
// contain conversations.batch_call (store.Migrate).
func New(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

// uniqueViolation is the pg error code Insert surfaces on a live duplicate
// custom_id (the partial index batch_call_custom_id_live enforces it).
const uniqueViolation = "23505"

// validState reports whether st is one of the five known states, mirroring
// pkg/batch's unexported validState.
func validState(st batch.State) bool {
	switch st {
	case batch.StateQueued, batch.StateSubmitting, batch.StateSubmitted,
		batch.StateCompleted, batch.StateFailed:
		return true
	}
	return false
}

// liveCols is the column list shared by every SELECT and RETURNING of a row,
// in scanRow's order.
const liveCols = `id, custom_id, model, state, provider_batch_id, request, response, error, created_at, updated_at`

// Live returns the non-tombstoned row for customID.
func (s *Store) Live(ctx context.Context, customID string) (batch.Row, bool, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT `+liveCols+`
		FROM conversations.batch_call
		WHERE custom_id = $1 AND deleted_at IS NULL`, customID)
	var got batch.Row
	if err := scanRow(row, &got); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return batch.Row{}, false, nil
		}
		return batch.Row{}, false, fmt.Errorf("batchdb: live %q: %w", customID, err)
	}
	return got, true, nil
}

// Insert stores a new queued row. It refuses a State other than StateQueued
// and an empty CustomID before touching the database; a live-duplicate
// custom_id surfaces the underlying unique violation.
//
// Deliberate divergences from MemStore (see batch.Store.Insert): the caller's
// CreatedAt/UpdatedAt are replaced by the database clock (the Batcher passes
// its own now() for both, so the two are the same instant in prod), and a
// caller-provided ProviderBatchID is ignored — provider_batch_id is written
// only by MarkSubmitted.
func (s *Store) Insert(ctx context.Context, r batch.Row) (batch.Row, error) {
	if r.State != batch.StateQueued {
		return batch.Row{}, fmt.Errorf("batch: insert requires state %q, got %q", batch.StateQueued, r.State)
	}
	if r.CustomID == "" {
		return batch.Row{}, fmt.Errorf("batch: insert requires a custom_id")
	}
	var out batch.Row
	var request, response []byte
	var providerBatchID, errText *string
	err := s.pool.QueryRow(ctx, `
		INSERT INTO conversations.batch_call (custom_id, model, state, request, created_at, updated_at)
		VALUES ($1, $2, $3, $4, now(), now())
		RETURNING `+liveCols,
		r.CustomID, r.Model, string(batch.StateQueued), jsonbArg(r.Request)).Scan(
		&out.ID, &out.CustomID, &out.Model, (*stateScanner)(&out.State), &providerBatchID,
		&request, &response, &errText, &out.CreatedAt, &out.UpdatedAt)
	if err == nil {
		out.ProviderBatchID, out.Error = deref(providerBatchID), deref(errText)
		setRaw(&out, request, response)
	}
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == uniqueViolation {
			return batch.Row{}, fmt.Errorf("batch: custom_id %q already live: %w", r.CustomID, err)
		}
		return batch.Row{}, fmt.Errorf("batchdb: insert %q: %w", r.CustomID, err)
	}
	return out, nil
}

// Tombstone marks the row deleted. It never removes the row and is a no-op
// for an unknown or already-tombstoned id.
func (s *Store) Tombstone(ctx context.Context, id int64) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE conversations.batch_call
		SET deleted_at = now(), updated_at = now()
		WHERE id = $1 AND deleted_at IS NULL`, id)
	if err != nil {
		return fmt.Errorf("batchdb: tombstone %d: %w", id, err)
	}
	return nil
}

// MarkSubmitting moves queued rows to StateSubmitting.
func (s *Store) MarkSubmitting(ctx context.Context, ids []int64) error {
	return s.transition(ctx, ids, batch.StateQueued, batch.StateSubmitting,
		`provider_batch_id = NULL`, nil)
}

// MarkSubmitted records the provider batch id on submitting rows.
func (s *Store) MarkSubmitted(ctx context.Context, ids []int64, providerBatchID string) error {
	return s.transition(ctx, ids, batch.StateSubmitting, batch.StateSubmitted,
		`provider_batch_id = $4`,
		func(args []any) []any { return append(args, providerBatchID) })
}

// Requeue returns submitting rows to StateQueued.
func (s *Store) Requeue(ctx context.Context, ids []int64) error {
	return s.transition(ctx, ids, batch.StateSubmitting, batch.StateQueued,
		`provider_batch_id = NULL`, nil)
}

// Complete stores the response on a submitted row.
func (s *Store) Complete(ctx context.Context, id int64, response json.RawMessage) error {
	return s.transition(ctx, []int64{id}, batch.StateSubmitted, batch.StateCompleted,
		`response = $4, error = NULL`,
		func(args []any) []any { return append(args, jsonbArg(response)) })
}

// Fail stores the error text on a submitting or submitted row.
func (s *Store) Fail(ctx context.Context, id int64, msg string) error {
	fail := func(from batch.State) error {
		return s.transition(ctx, []int64{id}, from, batch.StateFailed,
			`error = $4`, func(args []any) []any { return append(args, msg) })
	}
	if err := fail(batch.StateSubmitting); err != nil {
		return err
	}
	return fail(batch.StateSubmitted)
}

// InState returns the live rows in state st, oldest first. An empty or unknown
// State is refused.
func (s *Store) InState(ctx context.Context, st batch.State) ([]batch.Row, error) {
	if !validState(st) {
		return nil, fmt.Errorf("batch: unknown state %q", string(st))
	}
	rows, err := s.pool.Query(ctx, `
		SELECT `+liveCols+`
		FROM conversations.batch_call
		WHERE deleted_at IS NULL AND state = $1
		ORDER BY id`, string(st))
	if err != nil {
		return nil, fmt.Errorf("batchdb: in state %q: %w", st, err)
	}
	defer rows.Close()
	var out []batch.Row
	for rows.Next() {
		var r batch.Row
		if err := scanRow(rows, &r); err != nil {
			return nil, fmt.Errorf("batchdb: in state %q: %w", st, err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("batchdb: in state %q: %w", st, err)
	}
	return out, nil
}

// transition applies one conditional state change: live rows among ids whose
// state is `from` move to the SET clause (state = $2, plus any extra column
// setter bound to $3) with updated_at = now(); every other row — wrong state,
// tombstoned, unknown id — is left untouched, the stale-clobber no-op. The
// caller works from a snapshot it read earlier, so a row that changed since is
// skipped, never clobbered.
func (s *Store) transition(ctx context.Context, ids []int64, from, to batch.State,
	set string, extra func([]any) []any) error {
	args := []any{ids, string(from), string(to)}
	if extra != nil {
		args = extra(args)
	}
	_, err := s.pool.Exec(ctx, `
		UPDATE conversations.batch_call
		SET state = $3`+setClause(set)+`, updated_at = now()
		WHERE id = ANY($1) AND deleted_at IS NULL AND state = $2`, args...)
	if err != nil {
		return fmt.Errorf("batchdb: transition %s to %s: %w", from, to, err)
	}
	return nil
}

// setClause prepends the extra SET terms ("" when the transition only changes
// the state). The first extra parameter is $4: $1 is the id list, $2 the
// source state, $3 the target state.
func setClause(set string) string {
	if set == "" {
		return ""
	}
	return ", " + set
}

// jsonbArg converts a nil Request/Response to a JSON null so the NOT NULL
// request column accepts it; scanRow turns a stored "null" (or SQL NULL)
// back into nil on read, so a nil Request/Response round-trips.
func jsonbArg(raw json.RawMessage) []byte {
	if len(raw) == 0 {
		return []byte("null")
	}
	return raw
}

type rowScanner interface{ Scan(dest ...any) error }

func scanRow(rs rowScanner, r *batch.Row) error {
	var request, response []byte
	var providerBatchID, errText *string
	if err := rs.Scan(
		&r.ID, &r.CustomID, &r.Model, (*stateScanner)(&r.State), &providerBatchID,
		&request, &response, &errText, &r.CreatedAt, &r.UpdatedAt); err != nil {
		return err
	}
	r.ProviderBatchID, r.Error = deref(providerBatchID), deref(errText)
	setRaw(r, request, response)
	return nil
}

// deref maps a nullable text column to the empty string.
func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// setRaw stores the scanned jsonb bytes: a SQL NULL or a stored JSON null
// becomes nil, so a nil Request/Response round trips (see jsonbArg).
func setRaw(r *batch.Row, request, response []byte) {
	r.Request = jsonRaw(request)
	r.Response = jsonRaw(response)
}

// jsonRaw maps the empty and "null" byte slices to nil.
func jsonRaw(b []byte) json.RawMessage {
	if len(b) == 0 || string(b) == "null" {
		return nil
	}
	return json.RawMessage(b)
}

// stateScanner adapts batch.State to pgx's text scanning, refusing NULL and
// unknown values (the CHECK constraint makes unknown unreachable in practice).
type stateScanner batch.State

// Scan implements sql.Scanner.
func (s *stateScanner) Scan(src any) error {
	if src == nil {
		return fmt.Errorf("batchdb: NULL state")
	}
	var text string
	switch b := src.(type) {
	case []byte:
		text = string(b)
	case string:
		text = b
	default:
		return fmt.Errorf("batchdb: state column is %T, not text", src)
	}
	b := text
	st := batch.State(b)
	if !validState(st) {
		return fmt.Errorf("batchdb: unknown state %q in row", string(b))
	}
	*(*batch.State)(s) = st
	return nil
}
