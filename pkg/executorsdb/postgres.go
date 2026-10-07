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

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
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

	ErrMachineNameTaken = executors.ErrMachineNameTaken
)

// uniqueViolation is SQLSTATE 23505.
const uniqueViolation = "23505"

// ownerMachineIndex is the partial unique index over
// (labels->>'owner', labels->>'machine') added by migration 0020.
const ownerMachineIndex = "executors_owner_machine_unique"

// duplicateMachineName translates a rejected INSERT into conversations.executors
// when — and only when — the (owner, machine) unique index is what rejected it.
//
// It returns executors.ErrMachineNameTaken BARE, discarding the pgconn error
// rather than wrapping it. That is not tidiness: this error travels to a peer
// that has not yet proved who it is (writeAuthFailure forwards a terminal
// error's text verbatim), and a pgx message carries the DSN.
//
// Matched by CONSTRAINT NAME as well as by SQLSTATE. Another unique index on
// this table later — on the credential hash, say — must not inherit advice to
// rename a machine over a collision that has nothing to do with names, nor
// inherit the terminal classification that stops an executor from retrying.
func duplicateMachineName(err error) error {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return nil
	}
	if pgErr.Code != uniqueViolation || pgErr.ConstraintName != ownerMachineIndex {
		return nil
	}
	return executors.ErrMachineNameTaken
}

// NewPostgresStore creates an executor Store backed by pg.
func NewPostgresStore(pool *pgxpool.Pool) executors.Store {
	return &pgStore{pool: pool}
}

type pgStore struct {
	pool *pgxpool.Pool
}

// checkWriteErr translates a Write error on conversations.executors through
// duplicateMachineName. Every INSERT/UPDATE against that table must route its
// error through this — not because any one path is special, but because a
// fourth path that forgets is exactly how R15 shipped, and the unique index
// is what made the forgetting invisible (the raw 23505 looked like success
// until the operator followed the daemon's own relabel advice and got an
// opaque 503). One helper, three call sites, no fourth to forget.
func (s *pgStore) checkWriteErr(err error) error {
	if dup := duplicateMachineName(err); dup != nil {
		return dup
	}
	return err
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
		    minted_by, expires_at, owner_user_id)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, NULLIF($9,'')::uuid)`,
		hashToken(plaintext), labelsJSON, roots,
		isolation, wmode, t.Admits,
		t.MintedBy, t.ExpiresAt, t.OwnerUserID)
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
		`SELECT t.labels, t.roots, t.isolation, t.workspace_mode, t.admits, t.expires_at, t.consumed_at,
		        COALESCE(t.owner_user_id::text, ''), COALESCE(u.deleted_at IS NOT NULL, false)
		   FROM conversations.executor_enrollment_token t
		   LEFT JOIN conversations.users u ON u.id = t.owner_user_id
		  WHERE t.token_hash = $1`,
		hashed).Scan(&tr.labels, &tr.roots, &tr.isolation, &tr.workspaceMode, &tr.admits, &tr.expiresAt, &tr.consumedAt, &tr.ownerUserID, &tr.ownerDeleted)
	if errors.Is(err, pgx.ErrNoRows) {
		return executors.Executor{}, "", ErrTokenUnknown
	}
	if err != nil {
		// NOT ErrTokenUnknown. See authenticateByHash's comment: "I could not
		// check this token" is not "this token is unknown", and
		// IsTerminalAuthError treats ErrTokenUnknown as terminal — collapsing
		// a dead connection into it told every executor enrolling during a
		// database blip that its token was permanently invalid.
		return executors.Executor{}, "", fmt.Errorf("look up enrollment token: %w", err)
	}
	if tr.ownerDeleted {
		// The token was minted for a user whose row is now tombstoned. Not
		// ErrTokenUnknown — the token exists; this is the enrollment-side twin
		// of authenticateByHash's tombstoned-owner refusal, and it is
		// ErrNotFound for the same reason: an ANSWER (the owner is gone,
		// retrying cannot change that), while a store failure is wrapped above.
		return executors.Executor{}, "", ErrNotFound
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
		   (credential_hash, labels, self_reported, roots, isolation, workspace_mode, admits, owner_user_id)
		 VALUES ($1,$2,$3,$4,$5,$6,$7, NULLIF($8,'')::uuid) RETURNING id`,
		hashToken(credential), tr.labels, selfJSON, tr.roots,
		isolation, wmode, tr.admits, tr.ownerUserID).Scan(&id)
	if err != nil {
		if werr := s.checkWriteErr(err); werr != err {
			return executors.Executor{}, "", werr
		}
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

	var id string
	err = s.pool.QueryRow(ctx,
		`INSERT INTO conversations.executors
		   (credential_hash, labels, self_reported, roots, isolation, workspace_mode, admits, owner_user_id)
		 VALUES ($1,$2,$3,$4,$5,$6,$7, NULLIF($8,'')::uuid) RETURNING id`,
		hashToken(credential), jsonMap(t.Labels), jsonMap(nil), roots,
		isolation, wmode, t.Admits, t.OwnerUserID).Scan(&id)
	if err != nil {
		if werr := s.checkWriteErr(err); werr != err {
			return executors.Executor{}, "", werr
		}
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
		`SELECT e.id, e.labels, e.self_reported, e.annotations,
		        e.roots, e.isolation, e.workspace_mode, e.admits, e.enabled,
		        e.enrolled_at, e.last_seen_at, COALESCE(e.owner_user_id::text, '')
		   FROM conversations.executors e
		   LEFT JOIN conversations.users u ON u.id = e.owner_user_id
		  WHERE e.credential_hash = $1
		    AND (e.owner_user_id IS NULL OR u.deleted_at IS NULL)`,
		hashVal).Scan(
		&e.ID,
		&labelsJSON, &selfJSON, &annotationsJSON,
		&e.Roots, &e.Isolation, &e.WorkspaceMode, &e.Admits, &e.Enabled,
		&enrolledAt, &lastSeenAt, &e.OwnerUserID)
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

// malformedID reports whether id cannot name an executor row at all, as far as
// Go can tell. Postgres is the authority — executorReadErr maps the cast error
// it returns — so this only short-circuits the common case (a short SUFFIX
// ref) before the round trip.
//
// A malformed id is an ANSWER, not a read failure: the same rule usersdb
// states for a token id ("a malformed id is no token: an answer, not a 22P02
// outage"). Callers depend on it — executor ref resolution accepts a
// user-supplied ref that may be a short SUFFIX and falls through to its suffix
// search only on ErrNotFound, so letting the cast error through would break
// `rafiki executor <suffix>` outright.
func malformedID(id string) bool {
	_, err := uuid.Parse(id)
	return err != nil
}

// invalidTextRepresentation is SQLSTATE 22P02: Postgres could not read the id
// as a uuid, so it cannot name a row. That is an ANSWER (ErrNotFound), not a
// read failure — the same rule malformedID states, but decided by the database
// itself, so the two cannot disagree.
const invalidTextRepresentation = "22P02"

// executorReadErr classifies a read of the executor table: ErrNotFound when the
// row cannot exist (no rows, or an id the database could not read as a uuid),
// and a wrapped error otherwise.
//
// The distinction is load-bearing for every caller. execpool.refreshRow REVOKES
// a connected executor on ErrNotFound but keeps the last known row on any other
// error; executor ref resolution falls through to its suffix search on
// ErrNotFound but surfaces a real error. Collapsing a dead connection into
// ErrNotFound therefore told a connected executor its row was gone during a
// database blip, and it exited. Authenticate, Enroll and the whole usersdb
// store already checked pgx.ErrNoRows first; Get, SetLabels and Annotate did
// not.
func executorReadErr(what, id string, err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == invalidTextRepresentation {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("%s for executor %s: %w", what, id, err)
	}
	return nil
}

func (s *pgStore) Get(ctx context.Context, id string) (executors.Executor, error) {
	if malformedID(id) {
		return executors.Executor{}, ErrNotFound
	}
	var e executors.Executor
	var labelsJSON, selfJSON, annotationsJSON []byte
	var enrolledAt, lastSeenAt *time.Time
	err := s.pool.QueryRow(ctx,
		`SELECT id, labels, self_reported, annotations,
		        roots, isolation, workspace_mode, admits, enabled,
		        enrolled_at, last_seen_at, COALESCE(owner_user_id::text, '')
		   FROM conversations.executors WHERE id = $1`,
		id).Scan(
		&e.ID,
		&labelsJSON, &selfJSON, &annotationsJSON,
		&e.Roots, &e.Isolation, &e.WorkspaceMode, &e.Admits, &e.Enabled,
		&enrolledAt, &lastSeenAt, &e.OwnerUserID)
	if rerr := executorReadErr("read", id, err); rerr != nil {
		return executors.Executor{}, rerr
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
		`SELECT id, labels, self_reported, annotations,
		        roots, isolation, workspace_mode, admits, enabled,
		        enrolled_at, last_seen_at, COALESCE(owner_user_id::text, '')
		   FROM conversations.executors ORDER BY enrolled_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanExecutors(rows)
}

func (s *pgStore) SetLabels(ctx context.Context, id string, set map[string]string, remove []string) (executors.Executor, error) {
	if malformedID(id) {
		return executors.Executor{}, ErrNotFound
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return executors.Executor{}, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	var currentJSON []byte
	err = tx.QueryRow(ctx,
		`SELECT labels FROM conversations.executors WHERE id = $1 FOR UPDATE`, id).Scan(&currentJSON)
	if rerr := executorReadErr("read labels", id, err); rerr != nil {
		return executors.Executor{}, rerr
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
		// Same index, third path. A relabel rewrites the whole map, so moving
		// one executor onto a name another already holds for this owner trips
		// it exactly as an insert would -- and this is the path the collision
		// message RECOMMENDS, so leaving it untranslated answers "the daemon
		// is broken" to an operator following the daemon's own advice.
		if werr := s.checkWriteErr(err); werr != err {
			return executors.Executor{}, werr
		}
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
	if malformedID(id) {
		return ErrNotFound
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	var currentJSON []byte
	err = tx.QueryRow(ctx,
		`SELECT annotations FROM conversations.executors WHERE id = $1 FOR UPDATE`, id).Scan(&currentJSON)
	if rerr := executorReadErr("read annotations", id, err); rerr != nil {
		return rerr
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
	ownerUserID   string
	ownerDeleted  bool
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
			&e.ID,
			&labelsJSON, &selfJSON, &annotationsJSON,
			&e.Roots, &e.Isolation, &e.WorkspaceMode, &e.Admits, &e.Enabled,
			&enrolledAt, &lastSeenAt, &e.OwnerUserID); err != nil {
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
