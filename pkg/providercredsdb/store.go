// SPDX-License-Identifier: Apache-2.0

// Package providercredsdb is the Postgres implementation of
// providercreds.Store. It is the only package that may touch pgx on the
// provider-credential path -- see pkg/providercreds.
//
// Two properties are the whole point of this package:
//
//   - A token is never stored usable. Every token column is AES-256-GCM
//     ciphertext under RAFIKI_SECRETS_KEY (pkg/secretbox), bound by associated
//     data to its (owner, provider, field) position, so a row copied to
//     another owner, provider or column fails to open instead of yielding
//     someone else's credential.
//   - A refresh happens under SELECT ... FOR UPDATE on the owner's row. Refresh
//     tokens are single-use, so two goroutines refreshing at once must not
//     both call upstream: the second waits for the first to commit and then
//     sees the already-rotated token.
package providercredsdb

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"go.graveland.dev/rafiki/pkg/providercreds"
	"go.graveland.dev/rafiki/pkg/secretbox"
)

// Store is the Postgres-backed providercreds.Store.
type Store struct {
	pool *pgxpool.Pool
	box  *secretbox.Box
}

var _ providercreds.Store = (*Store)(nil)

// New creates a providercreds.Store over pool, sealing and opening token
// columns with box. Both are required: a nil pool or box is a wiring error,
// and it is reported here rather than as a nil dereference on the first login.
func New(pool *pgxpool.Pool, box *secretbox.Box) providercreds.Store {
	if pool == nil {
		panic("providercredsdb: nil pool")
	}
	if box == nil {
		panic("providercredsdb: nil secretbox (RAFIKI_SECRETS_KEY is required for provider credentials)")
	}
	return &Store{pool: pool, box: box}
}

// The token columns of the credential table. AAD names the position of a
// ciphertext in the schema; the field is the column it lives in. It is built
// HERE, once, and used by both Seal and Open at every call site, so the two
// can never disagree about what a ciphertext was bound to.
const (
	fieldRefresh = "refresh"
	fieldAccess  = "access"
)

// aad is the associated data a token ciphertext is bound to:
// owner|provider|field. It is authenticated but not encrypted.
func aad(owner, provider, field string) []byte {
	return []byte(owner + "|" + provider + "|" + field)
}

// errUndecryptable is the one report for a ciphertext this box cannot open.
// It never carries token bytes, and it names the fix: the key changed.
func errUndecryptable() error {
	return fmt.Errorf("%w: stored credential cannot be decrypted with the current RAFIKI_SECRETS_KEY", providercreds.ErrDead)
}

// credentialCols is every column a Credential carries, in scan order.
// owner_user_id is read as text so the caller sees the same string it passes
// in, rather than a 16-byte pgtype.UUID it would have to render.
const credentialCols = `owner_user_id::text, provider, key_id, refresh_sealed, access_sealed,
	access_expiry, account_id, models_json, models_etag, dead_at, last_refresh_at`

const (
	selectCredential          = `SELECT ` + credentialCols + ` FROM conversations.provider_credential WHERE owner_user_id = $1::uuid AND provider = $2`
	selectCredentialForUpdate = selectCredential + ` FOR UPDATE`
)

// storedRow is one row as it sits in the table: tokens still sealed.
type storedRow struct {
	owner         string
	provider      string
	keyID         string
	refreshSealed []byte
	accessSealed  []byte
	accessExpiry  time.Time
	accountID     string
	modelsJSON    []byte
	modelsETag    string
	deadAt        *time.Time
	lastRefresh   time.Time
}

func scanStored(row pgx.Row) (storedRow, error) {
	var r storedRow
	err := row.Scan(&r.owner, &r.provider, &r.keyID, &r.refreshSealed, &r.accessSealed,
		&r.accessExpiry, &r.accountID, &r.modelsJSON, &r.modelsETag, &r.deadAt, &r.lastRefresh)
	return r, err
}

// querier is the read half pool and transaction share.
type querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// read loads one row, translating "no row" into ErrNotFound. Every other
// failure stays an internal error: a store outage has not learned that the
// owner has no credential.
func read(ctx context.Context, q querier, sql, owner, provider string) (storedRow, error) {
	r, err := scanStored(q.QueryRow(ctx, sql, owner, provider))
	if errors.Is(err, pgx.ErrNoRows) {
		return storedRow{}, providercreds.ErrNotFound
	}
	if err != nil {
		return storedRow{}, fmt.Errorf("providercredsdb: read credential: %w", err)
	}
	return r, nil
}

// openStored decrypts a row's token columns. On a decryption failure it
// returns the row's non-secret metadata alongside ErrDead wrapped in
// errUndecryptable: the caller may still want owner/provider/expiry, and it
// must never be handed a partially decrypted token.
func (s *Store) openStored(r storedRow) (providercreds.Credential, error) {
	c := providercreds.Credential{
		OwnerUserID:  r.owner,
		Provider:     r.provider,
		AccountID:    r.accountID,
		AccessExpiry: r.accessExpiry,
		ModelsJSON:   r.modelsJSON,
		ModelsETag:   r.modelsETag,
		Dead:         r.deadAt != nil,
		LastRefresh:  r.lastRefresh,
	}
	refresh, err := s.box.Open(r.keyID, r.refreshSealed, aad(r.owner, r.provider, fieldRefresh))
	if err != nil {
		return c, errUndecryptable()
	}
	access, err := s.box.Open(r.keyID, r.accessSealed, aad(r.owner, r.provider, fieldAccess))
	if err != nil {
		return c, errUndecryptable()
	}
	c.RefreshToken = string(refresh)
	c.AccessToken = string(access)
	return c, nil
}

// Get returns the stored credential, or ErrNotFound / ErrDead.
func (s *Store) Get(ctx context.Context, owner, provider string) (providercreds.Credential, error) {
	r, err := read(ctx, s.pool, selectCredential, owner, provider)
	if err != nil {
		return providercreds.Credential{}, err
	}
	c, err := s.openStored(r)
	if err != nil {
		return c, err
	}
	if c.Dead {
		return c, providercreds.ErrDead
	}
	return c, nil
}

// Put upserts one credential and clears dead_at: a Put is a login in hand, so
// whatever was stored before (including a tombstoned login) is replaced whole.
// The model cache is replaced too, not merged, because a second login may be a
// different upstream account.
func (s *Store) Put(ctx context.Context, c providercreds.Credential) error {
	lastRefresh := c.LastRefresh
	if lastRefresh.IsZero() {
		lastRefresh = time.Now()
	}
	_, err := s.pool.Exec(ctx, `INSERT INTO conversations.provider_credential
			(owner_user_id, provider, key_id, refresh_sealed, access_sealed, access_expiry,
			 account_id, models_json, models_etag, dead_at, last_refresh_at)
		VALUES ($1::uuid, $2, $3, $4, $5, $6, $7, $8, $9, NULL, $10)
		ON CONFLICT (owner_user_id, provider) DO UPDATE SET
			key_id          = EXCLUDED.key_id,
			refresh_sealed  = EXCLUDED.refresh_sealed,
			access_sealed   = EXCLUDED.access_sealed,
			access_expiry   = EXCLUDED.access_expiry,
			account_id      = EXCLUDED.account_id,
			models_json     = EXCLUDED.models_json,
			models_etag     = EXCLUDED.models_etag,
			dead_at         = NULL,
			last_refresh_at = EXCLUDED.last_refresh_at,
			updated_at      = now()`,
		c.OwnerUserID, c.Provider, s.box.KeyID(),
		s.box.Seal([]byte(c.RefreshToken), aad(c.OwnerUserID, c.Provider, fieldRefresh)),
		s.box.Seal([]byte(c.AccessToken), aad(c.OwnerUserID, c.Provider, fieldAccess)),
		c.AccessExpiry, c.AccountID, jsonArg(c.ModelsJSON), c.ModelsETag, lastRefresh)
	if err != nil {
		return fmt.Errorf("providercredsdb: put credential: %w", err)
	}
	return nil
}

// Delete removes the row and hands back what it held, so a caller can revoke
// the tokens upstream. A dead credential is deleted rather than refused:
// deleting one is how an owner clears it.
func (s *Store) Delete(ctx context.Context, owner, provider string) (providercreds.Credential, error) {
	r, err := scanStored(s.pool.QueryRow(ctx, `DELETE FROM conversations.provider_credential
		WHERE owner_user_id = $1::uuid AND provider = $2
		RETURNING `+credentialCols, owner, provider))
	if errors.Is(err, pgx.ErrNoRows) {
		return providercreds.Credential{}, providercreds.ErrNotFound
	}
	if err != nil {
		return providercreds.Credential{}, fmt.Errorf("providercredsdb: delete credential: %w", err)
	}
	return s.openStored(r)
}

// Refresh runs fn while holding the owner's row lock, then writes what fn
// returned, all in one transaction.
//
// The lock is what makes a single-use refresh token safe: a second caller
// blocks until the first commits, then reads the rotated token and fn can
// return it unchanged instead of spending the old one upstream.
//
// fn is not called for a missing row (ErrNotFound) or a dead one (ErrDead).
// If fn reports ErrDead, dead_at is set and that error returned. Any other fn
// error rolls back and is returned with the still-stored credential. If fn
// succeeds but the write or commit fails, the ROTATED credential is returned
// with ErrNotPersisted: upstream has already spent the old refresh token, so
// the new one must not be dropped.
//
// Refresh writes the token columns, the expiry, the account id and dead_at. It
// does NOT write models_json/models_etag (SetModels owns those): the returned
// credential carries the stored ones, and an account id fn returned empty is
// filled from the stored row, since a refresh response need not repeat it. An
// fn that returns a credential marked Dead (with no error) tombstones the row
// on the way out, the same way ErrDead does.
func (s *Store) Refresh(ctx context.Context, owner, provider string, fn func(cur providercreds.Credential) (providercreds.Credential, error)) (providercreds.Credential, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return providercreds.Credential{}, fmt.Errorf("providercredsdb: begin refresh: %w", err)
	}
	// WithoutCancel: the caller's context may be the thing that failed, and a
	// transaction left open holds the row lock until the connection dies.
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	r, err := read(ctx, tx, selectCredentialForUpdate, owner, provider)
	if err != nil {
		return providercreds.Credential{}, err
	}
	cur, err := s.openStored(r)
	if err != nil {
		return cur, err
	}
	if cur.Dead {
		return cur, providercreds.ErrDead
	}

	next, fnErr := fn(cur)
	if fnErr != nil {
		if errors.Is(fnErr, providercreds.ErrDead) {
			if _, err := tx.Exec(ctx, `UPDATE conversations.provider_credential
				SET dead_at = now(), updated_at = now()
				WHERE owner_user_id = $1::uuid AND provider = $2`, owner, provider); err != nil {
				return cur, fmt.Errorf("providercredsdb: mark credential dead: %w", err)
			}
			if err := tx.Commit(ctx); err != nil {
				return cur, fmt.Errorf("providercredsdb: mark credential dead: %w", err)
			}
			cur.Dead = true
			return cur, fnErr
		}
		return cur, fnErr
	}

	// The returned credential must describe what the row will hold, not just
	// what fn produced.
	next.OwnerUserID, next.Provider = owner, provider
	next.ModelsJSON, next.ModelsETag = cur.ModelsJSON, cur.ModelsETag
	if next.AccountID == "" {
		next.AccountID = cur.AccountID
	}
	if next.LastRefresh.IsZero() {
		next.LastRefresh = cur.LastRefresh
	}
	bumped := next.RefreshToken != cur.RefreshToken || next.AccessToken != cur.AccessToken
	var deadAt any
	if next.Dead {
		deadAt = time.Now()
	}
	if err := tx.QueryRow(ctx, `UPDATE conversations.provider_credential
		SET key_id = $3, refresh_sealed = $4, access_sealed = $5, access_expiry = $6,
			account_id = $7, dead_at = $8,
			last_refresh_at = CASE WHEN $9 THEN now() ELSE last_refresh_at END,
			updated_at = now()
		WHERE owner_user_id = $1::uuid AND provider = $2
		RETURNING last_refresh_at`,
		owner, provider, s.box.KeyID(),
		s.box.Seal([]byte(next.RefreshToken), aad(owner, provider, fieldRefresh)),
		s.box.Seal([]byte(next.AccessToken), aad(owner, provider, fieldAccess)),
		next.AccessExpiry, next.AccountID, deadAt, bumped).Scan(&next.LastRefresh); err != nil {
		// The rotated token exists only in memory: upstream has already spent
		// the old one, so the new one must reach the caller even though the
		// row still holds the old one.
		return next, fmt.Errorf("%w: %v", providercreds.ErrNotPersisted, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return next, fmt.Errorf("%w: %v", providercreds.ErrNotPersisted, err)
	}
	return next, nil
}

// SetModels caches the provider's model directory, and the ETag to revalidate
// it with. A nil modelsJSON stores SQL NULL, which reads back as nil: "never
// fetched", not "an empty directory".
//
// The column is JSONB, so what reads back is Postgres's canonical rendering of
// the document. Callers parse it; they must not compare it byte for byte.
func (s *Store) SetModels(ctx context.Context, owner, provider string, modelsJSON []byte, etag string) error {
	tag, err := s.pool.Exec(ctx, `UPDATE conversations.provider_credential
		SET models_json = $3, models_etag = $4, updated_at = now()
		WHERE owner_user_id = $1::uuid AND provider = $2`,
		owner, provider, jsonArg(modelsJSON), etag)
	if err != nil {
		return fmt.Errorf("providercredsdb: set models: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return providercreds.ErrNotFound
	}
	return nil
}

// ListStale lists live credentials whose last refresh is older than age,
// oldest first. Dead rows are excluded: they need a login, not a refresh.
func (s *Store) ListStale(ctx context.Context, age time.Duration) ([]providercreds.Key, error) {
	rows, err := s.pool.Query(ctx, `SELECT owner_user_id::text, provider
		FROM conversations.provider_credential
		WHERE dead_at IS NULL AND last_refresh_at < $1
		ORDER BY last_refresh_at, owner_user_id, provider`, time.Now().Add(-age))
	if err != nil {
		return nil, fmt.Errorf("providercredsdb: list stale: %w", err)
	}
	defer rows.Close()
	var out []providercreds.Key
	for rows.Next() {
		var k providercreds.Key
		if err := rows.Scan(&k.Owner, &k.Provider); err != nil {
			return nil, fmt.Errorf("providercredsdb: scan stale: %w", err)
		}
		out = append(out, k)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("providercredsdb: list stale: %w", err)
	}
	return out, nil
}

// jsonArg turns a nil or empty model document into a SQL NULL. An empty
// []byte is not valid JSON, and "no directory" is one state, not two.
func jsonArg(b []byte) any {
	if len(b) == 0 {
		return nil
	}
	return b
}
