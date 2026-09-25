// SPDX-License-Identifier: Apache-2.0

package ejection

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"go.graveland.dev/rafiki/pkg/routing"
)

// EjectionStore is the append-only ejection log over Postgres. It satisfies
// routing.EjectionSink. Rows are only ever inserted: the table is history as
// much as state, so "when did this provider last go bad" stays answerable.
type EjectionStore struct{ pool *pgxpool.Pool }

func NewEjectionStore(pool *pgxpool.Pool) *EjectionStore { return &EjectionStore{pool: pool} }

// Append records one ejection. A zero ExpiresAt is stored as NULL ("until
// lifted"); an empty Note as NULL.
func (s *EjectionStore) Append(ctx context.Context, e routing.EjectionRecord) error {
	var expires *time.Time
	if !e.ExpiresAt.IsZero() {
		expires = &e.ExpiresAt
	}
	var note *string
	if e.Note != "" {
		note = &e.Note
	}
	_, err := s.pool.Exec(ctx,
		`INSERT INTO openrouter.provider_ejection (provider, model_line, reason, expires_at, evidence, note)
		 VALUES ($1,$2,$3,$4,$5,$6)`,
		e.Provider, e.ModelLine, string(e.Reason), expires, e.Evidence, note)
	return err
}

// Active returns, per (provider, model line), the most recent row — if that
// row is still in force. Latest-row-wins is the point: a lift (or a re-ban
// with a new expiry) supersedes the older ban, so the filter must run AFTER
// picking the latest row, never before, or an expired lift would hide itself
// and resurrect the ban it lifted.
func (s *EjectionStore) Active(ctx context.Context, now time.Time) ([]routing.EjectionRecord, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT provider, model_line, reason, created_at, expires_at, note
		   FROM (SELECT DISTINCT ON (provider, model_line)
		                provider, model_line, reason, created_at, expires_at, note
		           FROM openrouter.provider_ejection
		          ORDER BY provider, model_line, created_at DESC, id DESC) latest
		  WHERE reason <> $2 AND (expires_at IS NULL OR expires_at > $1)`,
		now, string(routing.ReasonLift))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []routing.EjectionRecord
	for rows.Next() {
		var r routing.EjectionRecord
		var reason string
		var expires *time.Time
		var note *string
		if err := rows.Scan(&r.Provider, &r.ModelLine, &reason, &r.CreatedAt, &expires, &note); err != nil {
			return nil, err
		}
		r.Reason = routing.EjectReason(reason)
		if expires != nil {
			r.ExpiresAt = *expires
		}
		if note != nil {
			r.Note = *note
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
