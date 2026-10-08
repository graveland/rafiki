// SPDX-License-Identifier: Apache-2.0

// Package routepolicy holds the daemon's runtime routing-policy rows: an
// append-only Postgres log of model_line → routing-spec writes (migration
// 0042), plus the in-memory resolver request paths consult.
//
// The store shape mirrors pkg/ejection: interface and Postgres implementation
// in one package, pgx allowed here, append-only rows, newest row per key wins.
// The daemon keeps the live rows in a Policy (refreshed from Store.Active on
// write and at startup, like the guard's bans) and resolves each request's
// model id against it.
package routepolicy

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"go.graveland.dev/rafiki/pkg/routing"
)

// Row is one live routing-policy row: the model line it governs, the spec
// string (routing.ParseSpec's grammar, e.g. "sort=price,quant=fp8+") and when
// it was written.
type Row struct {
	ModelLine string
	Spec      string
	CreatedAt time.Time
}

// ErrNotFound is returned by Delete when the model line has no live row —
// either it was never set, or its newest row is already a tombstone.
var ErrNotFound = errors.New("routepolicy: no live row for model line")

// Store is the append-only routing-policy log over Postgres. Rows are only
// ever inserted: a Set for an existing line does not update the old row but
// supersedes it, and Delete appends a tombstone — so the table is also the
// history of who routed what, when.
type Store interface {
	// Set appends a live row for modelLine. The spec is validated with
	// routing.ParseSpec first, so the table never holds an unparseable spec;
	// modelLine must be non-empty ("*" is the global line).
	Set(ctx context.Context, modelLine, spec string) error
	// Delete appends a deleted=true row for modelLine. ErrNotFound when the
	// line has no live row: never set, or already deleted — a tombstone is
	// itself the newest row, so the older live row it hides cannot make a
	// second Delete succeed.
	Delete(ctx context.Context, modelLine string) error
	// Active returns the newest row per model_line, excluding lines whose
	// newest row is a tombstone, ordered by model_line.
	Active(ctx context.Context) ([]Row, error)
}

// NewPostgres returns the Postgres-backed Store over pool.
func NewPostgres(pool *pgxpool.Pool) Store { return &postgresStore{pool: pool} }

type postgresStore struct{ pool *pgxpool.Pool }

// validModelLine rejects lines that can never match: modelLineOf reduces
// three-segment ids by stripping their provider segment, so a stored line with
// three or more "/"-separated segments is compared only against reduced ids
// and can never equal or match one — a silently dead row. A line whose glob
// metacharacters form a malformed pattern (e.g. "z-ai/[bad") is also refused:
// it would never match at resolve time.
func validModelLine(line string) error {
	if line == "" {
		return errors.New("routepolicy: model line must not be empty")
	}
	if strings.Count(line, "/") > 1 {
		return fmt.Errorf("routepolicy: model line %q: a line names at most <model>/<id> — "+
			"the provider segment is not part of a policy line", line)
	}
	if strings.ContainsAny(line, "*?[") {
		if _, err := filepath.Match(line, "x"); err != nil {
			return fmt.Errorf("routepolicy: model line %q: malformed glob: %w", line, err)
		}
	}
	return nil
}

func (s *postgresStore) Set(ctx context.Context, modelLine, spec string) error {
	if err := validModelLine(modelLine); err != nil {
		return err
	}
	if _, err := routing.ParseSpec(spec); err != nil {
		return fmt.Errorf("routepolicy: model line %q: %w", modelLine, err)
	}
	_, err := s.pool.Exec(ctx,
		`INSERT INTO openrouter.route_policy (model_line, spec) VALUES ($1, $2)`,
		modelLine, spec)
	return err
}

// Delete checks the line's newest row, then appends the tombstone. The two
// statements are not one transaction: the daemon is the table's single writer,
// and a concurrent Set squeezed between them simply loses to the tombstone's
// later id — the same newest-row-wins rule the reads apply.
func (s *postgresStore) Delete(ctx context.Context, modelLine string) error {
	var deleted bool
	err := s.pool.QueryRow(ctx,
		`SELECT deleted FROM openrouter.route_policy
		  WHERE model_line = $1 ORDER BY id DESC LIMIT 1`,
		modelLine).Scan(&deleted)
	if errors.Is(err, pgx.ErrNoRows) {
		return notFound(modelLine)
	}
	if err != nil {
		return err
	}
	if deleted {
		return notFound(modelLine)
	}
	_, err = s.pool.Exec(ctx,
		`INSERT INTO openrouter.route_policy (model_line, deleted) VALUES ($1, true)`,
		modelLine)
	return err
}

// Active picks the newest row per model_line BEFORE filtering deleted: a
// tombstone must hide the older live row beneath it, exactly as a lift
// supersedes a ban in pkg/ejection (TestEjectionStoreLiftSupersedesBan).
// Filtering first would resurrect the deleted row's predecessor.
func (s *postgresStore) Active(ctx context.Context) ([]Row, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT model_line, spec, created_at
		  FROM (SELECT DISTINCT ON (model_line)
			       model_line, spec, created_at, deleted
			  FROM openrouter.route_policy
			 ORDER BY model_line, id DESC) latest
		 WHERE NOT deleted
		 ORDER BY model_line`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Row
	for rows.Next() {
		var r Row
		if err := rows.Scan(&r.ModelLine, &r.Spec, &r.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func notFound(modelLine string) error {
	return fmt.Errorf("%w: %q", ErrNotFound, modelLine)
}

// Policy is the daemon's in-memory view of the live routing rows, consulted
// per request. Load replaces the whole view; the daemon refreshes it from
// Store.Active on write and at startup.
type Policy struct {
	mu   sync.RWMutex
	rows map[string]routing.Spec // parsed spec by model line, "*" = global
	list []Row                   // as loaded, sorted by ModelLine
}

// NewPolicy returns an empty view: every Resolve is a zero Spec until Load.
func NewPolicy() *Policy { return &Policy{rows: map[string]routing.Spec{}} }

// Load replaces all rows. Every row is parsed before anything is swapped in,
// so a bad spec fails the whole load — naming the offending line — and leaves
// the previous view intact.
func (p *Policy) Load(rows []Row) error {
	specs := make(map[string]routing.Spec, len(rows))
	for _, r := range rows {
		if err := validModelLine(r.ModelLine); err != nil {
			return err
		}
		spec, err := routing.ParseSpec(r.Spec)
		if err != nil {
			return fmt.Errorf("routepolicy: model line %q: %w", r.ModelLine, err)
		}
		specs[r.ModelLine] = spec
	}
	list := make([]Row, len(rows))
	copy(list, rows)
	slices.SortFunc(list, func(a, b Row) int { return strings.Compare(a.ModelLine, b.ModelLine) })
	p.mu.Lock()
	defer p.mu.Unlock()
	p.rows = specs
	p.list = list
	return nil
}

// Resolve returns the spec for a model id: the matching line row merged over
// the global "*" row — line keys win, the global row fills the gaps,
// nodata/zdr are monotone (set at either level they hold). Zero Spec when
// nothing matches.
//
// Model-line matching: the id is first reduced to its line by stripping a
// leading <provider>/ segment when it has exactly three "/"-separated
// segments (openrouter/z-ai/glm-5.3-flash → z-ai/glm-5.3-flash; two-segment
// ids and anything else pass through unchanged). A reduced id matches a row
// when it equals the line, or — if the line contains glob metacharacters (*,
// ? or [) — when filepath.Match succeeds. filepath.Match's * never crosses
// "/", so "z-ai/*" matches "z-ai/glm-5.3" but not "z-ai/x/y". A line with no
// glob metacharacters matches only by exact equality (the old "-" prefix
// family rule has been removed in favour of explicit globs like
// "z-ai/glm-5.3*"). When several line rows match, the LONGEST line wins
// and only that row participates — shorter rows do not fill its gaps.
func (p *Policy) Resolve(modelID string) routing.Spec {
	line := modelLineOf(modelID)
	p.mu.RLock()
	defer p.mu.RUnlock()
	var global routing.Spec
	if g, ok := p.rows[routing.AllModelLines]; ok {
		global = g
	}
	var best routing.Spec
	bestLen := -1
	for l, spec := range p.rows {
		if l == routing.AllModelLines || len(l) <= bestLen {
			continue
		}
		if line == l || (globMatch(l, line)) {
			best, bestLen = spec, len(l)
		}
	}
	return best.Merge(global)
}

// Rows returns the rows as last loaded, sorted by model line — a copy, so
// callers cannot reach into the view. Nil before the first Load.
func (p *Policy) Rows() []Row {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.list == nil {
		return nil
	}
	out := make([]Row, len(p.list))
	copy(out, p.list)
	return out
}

// globMatch reports whether a row line containing glob metacharacters matches
// the reduced id via filepath.Match. A line without metacharacters (or the
// bare global "*", which Resolve skips anyway) is not a glob: exact equality
// is handled by Resolve's first arm, so globMatch can be strict. filepath.Match
// errors are impossible here for lines validModelLine admitted (a malformed
// pattern is refused at Set/Load), so a false is returned if one ever occurs.
func globMatch(pattern, s string) bool {
	if pattern == routing.AllModelLines || !strings.ContainsAny(pattern, "*?[") {
		return false
	}
	ok, err := filepath.Match(pattern, s)
	if err != nil {
		return false
	}
	return ok
}

// modelLineOf reduces a model id to the line policy rows are keyed by: when
// the id has exactly three "/"-separated segments, the leading <provider>/
// segment is stripped (openrouter/z-ai/glm-5.3-flash → z-ai/glm-5.3-flash);
// otherwise the id passes through unchanged. A ":batch" suffix is stripped
// too: a parked call's line is its base id's line, so the row
// z-ai/glm-5.3-flash governs the spawned z-ai/glm-5.3-flash:batch — the
// refusal gate and the policy rows must both see the same line the live
// path sees.
func modelLineOf(modelID string) string {
	modelID = strings.TrimSuffix(modelID, ":batch")
	if segs := strings.Split(modelID, "/"); len(segs) == 3 {
		return segs[1] + "/" + segs[2]
	}
	return modelID
}
