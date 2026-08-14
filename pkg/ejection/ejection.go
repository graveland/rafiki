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

// Append records one ejection.
func (s *EjectionStore) Append(ctx context.Context, e routing.EjectionRecord) error {
	_, err := s.pool.Exec(ctx,
		`INSERT INTO openrouter.provider_ejection (provider, model_line, reason, expires_at, evidence)
		 VALUES ($1,$2,$3,$4,$5)`,
		e.Provider, e.ModelLine, string(e.Reason), e.ExpiresAt, e.Evidence)
	return err
}

// Active returns the most recent unexpired ejection per (provider, model line),
// which is what the guard seeds its in-memory blacklist from at startup.
func (s *EjectionStore) Active(ctx context.Context, now time.Time) ([]routing.EjectionRecord, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT DISTINCT ON (provider, model_line) provider, model_line, reason, expires_at
		   FROM openrouter.provider_ejection
		  WHERE expires_at > $1
		  ORDER BY provider, model_line, created_at DESC`, now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []routing.EjectionRecord
	for rows.Next() {
		var r routing.EjectionRecord
		var reason string
		if err := rows.Scan(&r.Provider, &r.ModelLine, &reason, &r.ExpiresAt); err != nil {
			return nil, err
		}
		r.Reason = routing.EjectReason(reason)
		out = append(out, r)
	}
	return out, rows.Err()
}
