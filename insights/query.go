package insights

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// Query bounds for the read-only executor.
const (
	defaultQueryLimit = 1000
	maxQueryLimit     = 10000
	// maxResultBytes caps the approximate serialized size of the returned rows,
	// so a query for a few very large rows can't blow past gRPC's 4MB message
	// cap or an LLM reader's context. Measured as the summed length of column
	// names plus stringified values as rows are built (approximate, not the
	// exact JSON size). At least one row is always returned even if it alone
	// exceeds the budget.
	maxResultBytes = 3 << 20 // 3 MiB
)

// clampQueryLimit maps a requested row limit into [1, maxQueryLimit]: a
// non-positive limit becomes the default, an over-max limit is clamped DOWN to
// the max (not reset to the smaller default).
func clampQueryLimit(limit int) int {
	switch {
	case limit <= 0:
		return defaultQueryLimit
	case limit > maxQueryLimit:
		return maxQueryLimit
	default:
		return limit
	}
}

// Query runs a caller-validated read-only SELECT and returns up to limit rows.
//
// Query does NOT parse or sanitize sqlText itself — the caller MUST pass a
// validate func that guarantees the statement is a read-only SELECT constrained
// to the conversations schema (the server injects a pg_query-backed validator so
// this package carries no cgo dependency). A nil validate is refused: this
// runs caller-supplied SQL, so it fails closed rather than trusting that
// out-of-band validation happened.
//
// Defence in depth: the statement runs inside a read-only transaction with a
// 30s statement_timeout, so a statement that slips past the validator still
// cannot write and cannot run unbounded. truncated reports that the row cap or
// the byte budget (maxResultBytes) stopped the scan before all rows were read.
func (i *Insights) Query(ctx context.Context, sqlText string, limit int, validate func(string) error) (rows []map[string]any, truncated bool, err error) {
	if validate == nil {
		return nil, false, fmt.Errorf("insights: Query requires a validator")
	}
	if err := validate(sqlText); err != nil {
		return nil, false, fmt.Errorf("query rejected: %w", err)
	}
	limit = clampQueryLimit(limit)

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
	var bytesSoFar int
	for rs.Next() {
		if len(out) >= limit {
			truncated = true // one row past the cap proves truncation
			break
		}
		vals, err := rs.Values()
		if err != nil {
			return nil, false, fmt.Errorf("query: read row: %w", err)
		}
		row := make(map[string]any, len(fds))
		rowBytes := 0
		for j, fd := range fds {
			v := canonicalValue(vals[j])
			row[fd.Name] = v
			rowBytes += len(fd.Name) + valueSize(v)
		}
		// Enforce the byte budget, but always return at least one row.
		if len(out) > 0 && bytesSoFar+rowBytes > maxResultBytes {
			truncated = true
			break
		}
		out = append(out, row)
		bytesSoFar += rowBytes
	}
	if err := rs.Err(); err != nil {
		return nil, false, fmt.Errorf("query: rows: %w", err)
	}
	return out, truncated, nil
}

// canonicalValue normalizes pgx-decoded values for JSON output. Without a
// registered data type, pgx yields a uuid column as a raw [16]byte, which
// marshals as a JSON array of ints; render it as the canonical dashed string
// instead so `SELECT id` matches `SELECT id::text`.
func canonicalValue(v any) any {
	if b, ok := v.([16]byte); ok {
		return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
	}
	return v
}

// valueSize approximates the serialized byte size of a column value for the
// result byte budget.
func valueSize(v any) int {
	switch t := v.(type) {
	case nil:
		return 0
	case string:
		return len(t)
	case []byte:
		return len(t)
	default:
		return len(fmt.Sprint(t))
	}
}
