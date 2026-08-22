package executorsdb

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"go.graveland.dev/rafiki/pkg/executors"
)

// Aliases of the sentinels in pkg/executors, which owns them so the pool can
// classify a rejection without linking a database driver. Kept here so every
// existing executorsdb.ErrX reference — and errors.Is against either spelling
// — keeps working.
var (
	ErrTokenUnknown  = executors.ErrTokenUnknown
	ErrTokenConsumed = executors.ErrTokenConsumed
	ErrTokenExpired  = executors.ErrTokenExpired
	ErrDisabled      = executors.ErrDisabled
	ErrNotFound      = executors.ErrNotFound
)

// NewPostgresStore creates an executor Store backed by pg.
func NewPostgresStore(pool *pgxpool.Pool) executors.Store {
	return &pgStore{pool: pool}
}

type pgStore struct {
	pool *pgxpool.Pool
}

func (s *pgStore) MintToken(ctx context.Context, t executors.NewToken) (string, error) {
	plaintext, err := newToken()
	if err != nil {
		return "", err
	}
	labelsJSON := jsonMap(t.Labels)
	roots := t.Roots
	if roots == nil {
		roots = []string{}
	}
	isolation := t.Isolation
	if isolation == "" {
		isolation = "none"
	}
	wmode := t.WorkspaceMode
	if wmode == "" {
		wmode = "pinned"
	}
	_, err = s.pool.Exec(ctx,
		`INSERT INTO conversations.executor_enrollment_token
		   (token_hash, labels, roots, isolation, workspace_mode, admits,
		    minted_by, expires_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		hashToken(plaintext), labelsJSON, roots,
		isolation, wmode, t.Admits,
		t.MintedBy, t.ExpiresAt)
	if err != nil {
		return "", fmt.Errorf("mint token: %w", err)
	}
	return plaintext, nil
}

func (s *pgStore) Enroll(ctx context.Context, token string, self map[string]string) (executors.Executor, string, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return executors.Executor{}, "", err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	hashed := hashToken(token)
	var tr tokenRow
	err = tx.QueryRow(ctx,
		`SELECT labels, roots, isolation, workspace_mode, admits, expires_at, consumed_at
		   FROM conversations.executor_enrollment_token WHERE token_hash = $1`,
		hashed).Scan(&tr.labels, &tr.roots, &tr.isolation, &tr.workspaceMode, &tr.admits, &tr.expiresAt, &tr.consumedAt)
	if err != nil {
		return executors.Executor{}, "", ErrTokenUnknown
	}
	if tr.consumedAt != nil {
		return executors.Executor{}, "", ErrTokenConsumed
	}
	if time.Now().After(tr.expiresAt) {
		return executors.Executor{}, "", ErrTokenExpired
	}

	credential, err := newToken()
	if err != nil {
		return executors.Executor{}, "", err
	}

	selfJSON := jsonMap(self)
	var id string
	isolation := tr.isolation
	if isolation == "" {
		isolation = "none"
	}
	wmode := tr.workspaceMode
	if wmode == "" {
		wmode = "pinned"
	}
	err = tx.QueryRow(ctx,
		`INSERT INTO conversations.executors
		   (credential_hash, labels, self_reported, roots, isolation, workspace_mode, admits)
		 VALUES ($1,$2,$3,$4,$5,$6,$7) RETURNING id`,
		hashToken(credential), tr.labels, selfJSON, tr.roots,
		isolation, wmode, tr.admits).Scan(&id)
	if err != nil {
		return executors.Executor{}, "", fmt.Errorf("insert executor: %w", err)
	}

	tag, err := tx.Exec(ctx,
		`UPDATE conversations.executor_enrollment_token
		    SET consumed_at = now(), executor_id = $2
		  WHERE token_hash = $1 AND consumed_at IS NULL`, hashed, id)
	if err != nil {
		return executors.Executor{}, "", err
	}
	if tag.RowsAffected() == 0 {
		return executors.Executor{}, "", ErrTokenConsumed
	}
	if err := tx.Commit(ctx); err != nil {
		return executors.Executor{}, "", err
	}
	e, err := s.Get(ctx, id)
	return e, credential, err
}

// Create inserts an executor row and returns its credential, skipping the
// enrollment handshake entirely. See the Store interface for when to prefer it.
func (s *pgStore) Create(ctx context.Context, t executors.NewToken) (executors.Executor, string, error) {
	credential, err := newToken()
	if err != nil {
		return executors.Executor{}, "", err
	}

	isolation := t.Isolation
	if isolation == "" {
		isolation = "none"
	}
	wmode := t.WorkspaceMode
	if wmode == "" {
		wmode = "pinned"
	}

	var id string
	err = s.pool.QueryRow(ctx,
		`INSERT INTO conversations.executors
		   (credential_hash, labels, self_reported, roots, isolation, workspace_mode, admits)
		 VALUES ($1,$2,$3,$4,$5,$6,$7) RETURNING id`,
		hashToken(credential), jsonMap(t.Labels), jsonMap(nil), t.Roots,
		isolation, wmode, t.Admits).Scan(&id)
	if err != nil {
		return executors.Executor{}, "", fmt.Errorf("insert executor: %w", err)
	}

	e, err := s.Get(ctx, id)
	return e, credential, err
}

func (s *pgStore) Authenticate(ctx context.Context, credential string) (executors.Executor, error) {
	return s.authenticateByHash(ctx, hashToken(credential))
}

func (s *pgStore) authenticateByHash(ctx context.Context, hashVal string) (executors.Executor, error) {
	var e executors.Executor
	var labelsJSON, selfJSON, annotationsJSON []byte
	var enrolledAt, lastSeenAt *time.Time
	err := s.pool.QueryRow(ctx,
		`SELECT id, display_name, labels, self_reported, annotations,
		        roots, isolation, workspace_mode, admits, enabled,
		        enrolled_at, last_seen_at
		   FROM conversations.executors WHERE credential_hash = $1`,
		hashVal).Scan(
		&e.ID, &e.DisplayName,
		&labelsJSON, &selfJSON, &annotationsJSON,
		&e.Roots, &e.Isolation, &e.WorkspaceMode, &e.Admits, &e.Enabled,
		&enrolledAt, &lastSeenAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return executors.Executor{}, ErrNotFound
	}
	if err != nil {
		// NOT ErrNotFound. "I could not check this credential" is not an
		// answer, and IsTerminalAuthError classifies ErrNotFound as terminal —
		// so collapsing a dead connection into it told every executor that
		// reconnected during a database blip that its credential was
		// permanently invalid, and they all exited. Across a fleet
		// reconnecting together that is the whole fleet, which is exactly the
		// failure CLAUDE.md documents as forbidden. Returning the wrapped
		// error keeps IsTerminalAuthError false, so the executor retries.
		return executors.Executor{}, fmt.Errorf("authenticate executor: %w", err)
	}
	if !e.Enabled {
		return executors.Executor{}, ErrDisabled
	}
	json.Unmarshal(labelsJSON, &e.Labels)           //nolint:errcheck
	json.Unmarshal(selfJSON, &e.SelfReported)       //nolint:errcheck
	json.Unmarshal(annotationsJSON, &e.Annotations) //nolint:errcheck
	if enrolledAt != nil {
		e.EnrolledAt = *enrolledAt
	}
	if lastSeenAt != nil {
		e.LastSeenAt = *lastSeenAt
	}
	return e, nil
}

func (s *pgStore) Get(ctx context.Context, id string) (executors.Executor, error) {
	var e executors.Executor
	var labelsJSON, selfJSON, annotationsJSON []byte
	var enrolledAt, lastSeenAt *time.Time
	err := s.pool.QueryRow(ctx,
		`SELECT id, display_name, labels, self_reported, annotations,
		        roots, isolation, workspace_mode, admits, enabled,
		        enrolled_at, last_seen_at
		   FROM conversations.executors WHERE id = $1`,
		id).Scan(
		&e.ID, &e.DisplayName,
		&labelsJSON, &selfJSON, &annotationsJSON,
		&e.Roots, &e.Isolation, &e.WorkspaceMode, &e.Admits, &e.Enabled,
		&enrolledAt, &lastSeenAt)
	if err != nil {
		return executors.Executor{}, ErrNotFound
	}
	json.Unmarshal(labelsJSON, &e.Labels)           //nolint:errcheck
	json.Unmarshal(selfJSON, &e.SelfReported)       //nolint:errcheck
	json.Unmarshal(annotationsJSON, &e.Annotations) //nolint:errcheck
	if enrolledAt != nil {
		e.EnrolledAt = *enrolledAt
	}
	if lastSeenAt != nil {
		e.LastSeenAt = *lastSeenAt
	}
	return e, nil
}

func (s *pgStore) List(ctx context.Context) ([]executors.Executor, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, display_name, labels, self_reported, annotations,
		        roots, isolation, workspace_mode, admits, enabled,
		        enrolled_at, last_seen_at
		   FROM conversations.executors ORDER BY enrolled_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanExecutors(rows)
}

func (s *pgStore) SetLabels(ctx context.Context, id string, set map[string]string, remove []string) (executors.Executor, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return executors.Executor{}, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	var currentJSON []byte
	err = tx.QueryRow(ctx,
		`SELECT labels FROM conversations.executors WHERE id = $1 FOR UPDATE`, id).Scan(&currentJSON)
	if err != nil {
		return executors.Executor{}, ErrNotFound
	}
	var current map[string]string
	if err := json.Unmarshal(currentJSON, &current); err != nil {
		current = make(map[string]string)
	}
	if current == nil {
		current = make(map[string]string)
	}
	for k, v := range set {
		current[k] = v
	}
	for _, k := range remove {
		delete(current, k)
	}
	newJSON, _ := json.Marshal(current)
	_, err = tx.Exec(ctx,
		`UPDATE conversations.executors SET labels = $1, updated_at = now() WHERE id = $2`,
		newJSON, id)
	if err != nil {
		return executors.Executor{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return executors.Executor{}, err
	}
	return s.Get(ctx, id)
}

func (s *pgStore) SetEnabled(ctx context.Context, id string, enabled bool) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE conversations.executors SET enabled = $1, updated_at = now() WHERE id = $2`,
		enabled, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *pgStore) Delete(ctx context.Context, id string) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM conversations.executors WHERE id = $1`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *pgStore) Annotate(ctx context.Context, id string, set map[string]string, remove []string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	var currentJSON []byte
	err = tx.QueryRow(ctx,
		`SELECT annotations FROM conversations.executors WHERE id = $1 FOR UPDATE`, id).Scan(&currentJSON)
	if err != nil {
		return ErrNotFound
	}
	var current map[string]string
	if err := json.Unmarshal(currentJSON, &current); err != nil {
		current = make(map[string]string)
	}
	if current == nil {
		current = make(map[string]string)
	}
	for k, v := range set {
		current[k] = v
	}
	for _, k := range remove {
		delete(current, k)
	}
	newJSON, _ := json.Marshal(current)
	_, err = tx.Exec(ctx,
		`UPDATE conversations.executors SET annotations = $1, updated_at = now() WHERE id = $2`,
		newJSON, id)
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *pgStore) TouchSeen(ctx context.Context, id string) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE conversations.executors SET last_seen_at = now() WHERE id = $1`, id)
	return err
}

// ─── internal helpers ──────────────────────────────────────────────────────

type tokenRow struct {
	labels        []byte
	roots         []string
	isolation     string
	workspaceMode string
	admits        string
	expiresAt     time.Time
	consumedAt    *time.Time
}

func newToken() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b[:]), nil
}

func hashToken(s string) string {
	h := sha256.Sum256([]byte(s))
	return base64.RawURLEncoding.EncodeToString(h[:])
}

func jsonMap(m map[string]string) []byte {
	if m == nil {
		m = map[string]string{}
	}
	b, _ := json.Marshal(m)
	return b
}

func scanExecutors(rows pgx.Rows) ([]executors.Executor, error) {
	var out []executors.Executor
	for rows.Next() {
		var e executors.Executor
		var labelsJSON, selfJSON, annotationsJSON []byte
		var enrolledAt, lastSeenAt *time.Time
		if err := rows.Scan(
			&e.ID, &e.DisplayName,
			&labelsJSON, &selfJSON, &annotationsJSON,
			&e.Roots, &e.Isolation, &e.WorkspaceMode, &e.Admits, &e.Enabled,
			&enrolledAt, &lastSeenAt); err != nil {
			return nil, err
		}
		json.Unmarshal(labelsJSON, &e.Labels)           //nolint:errcheck
		json.Unmarshal(selfJSON, &e.SelfReported)       //nolint:errcheck
		json.Unmarshal(annotationsJSON, &e.Annotations) //nolint:errcheck
		if enrolledAt != nil {
			e.EnrolledAt = *enrolledAt
		}
		if lastSeenAt != nil {
			e.LastSeenAt = *lastSeenAt
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
