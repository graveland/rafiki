package insights

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// Query defaults and hard cap for the read-only executor.
const (
	defaultQueryLimit = 1000
	maxQueryLimit     = 10000
)

// Query runs a caller-validated read-only SELECT and returns up to limit rows.
//
// Query does NOT parse or sanitize sqlText itself — the caller MUST pass a
// validate func that guarantees the statement is a read-only SELECT constrained
// to the conversations schema (the server injects a pg_query-backed validator so
// this package carries no cgo dependency). validate may be nil only when the
// caller has already validated sqlText out of band.
//
// Defence in depth: the statement runs inside a read-only transaction with a
// 30s statement_timeout, so a statement that slips past the validator still
// cannot write and cannot run unbounded. limit is clamped to [1, maxQueryLimit]
// (default defaultQueryLimit when <= 0); truncated reports that more rows were
// available than returned.
func (i *Insights) Query(ctx context.Context, sqlText string, limit int, validate func(string) error) (rows []map[string]any, truncated bool, err error) {
	if validate != nil {
		if err := validate(sqlText); err != nil {
			return nil, false, fmt.Errorf("query rejected: %w", err)
		}
	}
	if limit <= 0 || limit > maxQueryLimit {
		limit = defaultQueryLimit
	}

	tx, err := i.pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, false, fmt.Errorf("query: begin read-only tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, "SET LOCAL statement_timeout = '30s'"); err != nil {
		return nil, false, fmt.Errorf("query: set statement_timeout: %w", err)
	}

	rs, err := tx.Query(ctx, sqlText)
	if err != nil {
		return nil, false, fmt.Errorf("query: %w", err)
	}
	defer rs.Close()

	fds := rs.FieldDescriptions()
	out := make([]map[string]any, 0, limit)
	for rs.Next() {
		vals, err := rs.Values()
		if err != nil {
			return nil, false, fmt.Errorf("query: read row: %w", err)
		}
		if len(out) >= limit {
			// One row past the cap proves truncation; stop without keeping it.
			truncated = true
			break
		}
		row := make(map[string]any, len(fds))
		for j, fd := range fds {
			row[fd.Name] = vals[j]
		}
		out = append(out, row)
	}
	if err := rs.Err(); err != nil {
		return nil, false, fmt.Errorf("query: rows: %w", err)
	}
	return out, truncated, nil
}
