// SPDX-License-Identifier: Apache-2.0

// Package mcpauthdb is the Postgres implementation of mcpoauth.Store. It is
// the only package that may touch pgx on the MCP OAuth credential path -- see
// pkg/mcpoauth/store.go for the contract this implements.
//
// Three properties are the whole point of this package:
//
//   - A token is never stored usable. The client secret and the access and
//     refresh tokens live in ONE AES-256-GCM ciphertext under
//     RAFIKI_SECRETS_KEY (pkg/secretbox), bound by associated data to
//     owner|mcp|resource|credential, so a row copied to another owner or
//     another resource fails to open instead of yielding someone else's
//     credential. No secret has a plaintext column.
//   - Every mutation for an owner serializes on that owner's
//     conversations.users row BEFORE any endpoint or grant row, using the same
//     row lock that conflicts with user tombstoning. Users do not block each
//     other. No Go mutex is an authority and none is held across I/O.
//   - A logout and a newer login are fenced by persisted generations, and a
//     refresh is fenced by a durable intent row state committed before the
//     upstream request. Neither an in-memory cache nor a transaction that has
//     already spent a single-use refresh token can outlive them.
package mcpauthdb

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"go.graveland.dev/rafiki/pkg/mcpoauth"
	"go.graveland.dev/rafiki/pkg/secretbox"
)

// Store is the Postgres-backed mcpoauth.Store.
type Store struct {
	pool *pgxpool.Pool
	box  *secretbox.Box
}

var _ mcpoauth.Store = (*Store)(nil)

// New creates an mcpoauth.Store over pool, sealing credential blobs with box.
// Both are required: a nil pool or box is a wiring error, and it is reported
// here rather than as a nil dereference on the first login.
func New(pool *pgxpool.Pool, box *secretbox.Box) mcpoauth.Store {
	if pool == nil {
		panic("mcpauthdb: nil pool")
	}
	if box == nil {
		panic("mcpauthdb: nil secretbox (RAFIKI_SECRETS_KEY is required for MCP credentials)")
	}
	return &Store{pool: pool, box: box}
}

// The associated data a credential blob is bound to:
// lowercase-owner|mcp|canonical-resource|credential. It is authenticated but
// not encrypted.
//
// This is the ONE place that shape is built, used by both Seal and Open at
// every call site, so the two can never disagree about what a ciphertext was
// bound to. It also NORMALISES the owner, because the two sides get it from
// different places: a write gets whatever the caller was authenticated as, and
// a read gets the row's canonical owner_user_id::text, which Postgres renders
// in lowercase. A caller who spells the id in uppercase would otherwise seal a
// row nothing can ever open. The column is a UUID, so lowercasing loses
// nothing. The resource segment is the CANONICAL resource, never the endpoint
// URL, so the same credential cannot be transplanted between the two.
func aad(owner, resource string) []byte {
	return []byte(strings.ToLower(owner) + "|mcp|" + resource + "|credential")
}

// sealedCredential is the whole secret half of a row. It is JSON so a later
// field can be added without a schema change, and it is ALWAYS sealed: no part
// of it may become a column.
type sealedCredential struct {
	ClientSecret string `json:"client_secret,omitempty"`
	AccessToken  string `json:"access_token,omitempty"`
	RefreshToken string `json:"refresh_token,omitempty"`
}

// unavailable reports a store failure. The sentinel comes first so
// errors.Is(err, mcpoauth.ErrUnavailable) works, and the cause stays attached
// so a caller can still recognise context cancellation or a driver error. It
// never wraps ErrMissing or ErrLoginRequired: a database failure has not
// learned that the owner has no credential.
func unavailable(op string, err error) error {
	return fmt.Errorf("%w: %s: %w", mcpoauth.ErrUnavailable, op, err)
}

// unusable reports a stored credential this box cannot open. It names the fix
// (the key changed) and never carries token bytes, the blob or the blob length.
func unusable(err error) error {
	return fmt.Errorf("%w: stored credential cannot be decrypted with the current RAFIKI_SECRETS_KEY: %v",
		mcpoauth.ErrLoginRequired, err)
}

// credentialCols is every column a credential row carries, in scan order.
// owner_user_id is read as text so the caller sees the same string it passes
// in, rather than a 16-byte pgtype.UUID it would have to render.
const credentialCols = `owner_user_id::text, resource, issuer, generation, key_id,
	credential_sealed, client_id, auth_style, authorization_endpoint, token_endpoint,
	revocation_endpoint, scopes, redirect_url, token_type, access_expiry,
	refresh_state, refresh_version, invalidated_at IS NOT NULL`

const (
	selectCredential          = `SELECT ` + credentialCols + ` FROM conversations.mcp_credential WHERE owner_user_id = $1::uuid AND resource = $2`
	selectCredentialForUpdate = selectCredential + ` FOR UPDATE`
)

// credentialRow is one row as it sits in the table: secrets still sealed.
type credentialRow struct {
	owner                 string
	resource              string
	issuer                string
	generation            int64
	keyID                 string
	sealed                []byte
	clientID              string
	authStyle             string
	authorizationEndpoint string
	tokenEndpoint         string
	revocationEndpoint    string
	scopes                []string
	redirectURL           string
	tokenType             string
	accessExpiry          *time.Time
	refreshState          string
	refreshVersion        int64
	invalidated           bool
}

func scanCredential(row pgx.Row) (credentialRow, error) {
	var r credentialRow
	err := row.Scan(&r.owner, &r.resource, &r.issuer, &r.generation, &r.keyID,
		&r.sealed, &r.clientID, &r.authStyle, &r.authorizationEndpoint, &r.tokenEndpoint,
		&r.revocationEndpoint, &r.scopes, &r.redirectURL, &r.tokenType, &r.accessExpiry,
		&r.refreshState, &r.refreshVersion, &r.invalidated)
	return r, err
}

// querier is the read half pool and transaction share.
type querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// readCredential loads one row, translating "no row" into ErrMissing. Every
// other failure stays an internal error: a store outage has not learned that
// the owner has no grant.
func readCredential(ctx context.Context, q querier, owner, resource string, forUpdate bool) (credentialRow, error) {
	sql := selectCredential
	if forUpdate {
		sql = selectCredentialForUpdate
	}
	r, err := scanCredential(q.QueryRow(ctx, sql, owner, resource))
	if errors.Is(err, pgx.ErrNoRows) {
		return credentialRow{}, fmt.Errorf("%w: no grant for resource %q", mcpoauth.ErrMissing, resource)
	}
	if err != nil {
		return credentialRow{}, unavailable("read credential", err)
	}
	return r, nil
}

// openRow decrypts a row's secret blob into a Grant. On a decryption failure it
// returns the row's non-secret metadata alongside ErrLoginRequired: the caller
// may still want owner/resource/generation, and it must never be handed a
// partially decrypted token.
func (s *Store) openRow(r credentialRow) (mcpoauth.Grant, error) {
	g := mcpoauth.Grant{
		OwnerUserID: r.owner,
		Resource:    r.resource,
		Issuer:      r.issuer,
		Generation:  r.generation,
		Registration: mcpoauth.Registration{
			ClientID:              r.clientID,
			AuthStyle:             mcpoauth.AuthStyle(r.authStyle),
			AuthorizationEndpoint: r.authorizationEndpoint,
			TokenEndpoint:         r.tokenEndpoint,
			RevocationEndpoint:    r.revocationEndpoint,
		},
		Scopes:         r.scopes,
		RedirectURL:    r.redirectURL,
		RefreshState:   mcpoauth.RefreshState(r.refreshState),
		RefreshVersion: r.refreshVersion,
	}
	g.Token.TokenType = r.tokenType
	if r.accessExpiry != nil {
		g.Token.Expiry = *r.accessExpiry
	}
	plain, err := s.box.Open(r.keyID, r.sealed, aad(r.owner, r.resource))
	if err != nil {
		return g, unusable(err)
	}
	var blob sealedCredential
	if err := json.Unmarshal(plain, &blob); err != nil {
		return g, unusable(err)
	}
	g.Registration.ClientSecret = blob.ClientSecret
	g.Token.AccessToken = blob.AccessToken
	g.Token.RefreshToken = blob.RefreshToken
	return g, nil
}

// seal encodes and encrypts a grant's secret half. Owner and resource must
// already be canonical: the AAD is a wire format, not a suggestion.
func (s *Store) seal(g mcpoauth.Grant) ([]byte, error) {
	plain, err := json.Marshal(sealedCredential{
		ClientSecret: g.Registration.ClientSecret,
		AccessToken:  g.Token.AccessToken,
		RefreshToken: g.Token.RefreshToken,
	})
	if err != nil {
		return nil, unavailable("seal credential", err)
	}
	return s.box.Seal(plain, aad(g.OwnerUserID, g.Resource)), nil
}

// normalizeAuthStyle folds the zero value into the explicit auto-detect name
// and refuses anything unmodelled: the column is keyed by NAME, so an
// unreadable value is a bug to fix, not a row to reinterpret.
func normalizeAuthStyle(s mcpoauth.AuthStyle) (mcpoauth.AuthStyle, error) {
	switch s {
	case "", mcpoauth.AuthStyleAuto:
		return mcpoauth.AuthStyleAuto, nil
	case mcpoauth.AuthStyleInParams, mcpoauth.AuthStyleInHeader:
		return s, nil
	}
	return "", fmt.Errorf("mcpauthdb: unknown auth style %q", s)
}

// scopesArg keeps a nil slice from becoming SQL NULL in a NOT NULL column.
func scopesArg(scopes []string) []string {
	if scopes == nil {
		return []string{}
	}
	return scopes
}

// expiryArg stores the zero time as SQL NULL: "the issuer reported no expiry"
// is a state, not the year 1.
func expiryArg(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t
}

// expired reports whether an access token's expiry has passed. A zero expiry is
// the issuer's "no expiry" and never expires.
func expired(expiry, now time.Time) bool {
	return !expiry.IsZero() && !expiry.After(now)
}

// lockLiveOwner takes the owner's users row lock and requires a live user.
// This is the row a usersdb tombstone UPDATE conflicts with, and it is always
// taken BEFORE any endpoint or grant row so mutations for one owner serialize
// and a concurrent tombstone cannot slip between the check and the write.
func lockLiveOwner(ctx context.Context, tx pgx.Tx, owner string) error {
	var live bool
	// FOR NO KEY UPDATE, not FOR UPDATE: it conflicts with the tombstone UPDATE
	// and with itself (so this owner's mutations still serialize on the row),
	// but not with the FOR KEY SHARE that FK inserts elsewhere in the schema
	// take, so a token acquisition does not block that owner's inserts.
	err := tx.QueryRow(ctx,
		`SELECT deleted_at IS NULL FROM conversations.users WHERE id = $1::uuid FOR NO KEY UPDATE`, owner).Scan(&live)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: owner %s does not exist", mcpoauth.ErrLoginRequired, owner)
	}
	if err != nil {
		return unavailable("lock owner", err)
	}
	if !live {
		return fmt.Errorf("%w: owner %s is deleted", mcpoauth.ErrLoginRequired, owner)
	}
	return nil
}

// lockOwner takes the owner's users row lock without requiring a live user, for
// destructive cleanup: removing a user tombstones the row, and the cleanup that
// follows must still run. It reports whether the owner row exists at all; a
// nonexistent owner can own no endpoint or grant rows, so cleanup is a no-op.
func lockOwner(ctx context.Context, tx pgx.Tx, owner string) (bool, error) {
	var id string
	// FOR NO KEY UPDATE for the same reason as lockLiveOwner: serialization on
	// this row without blocking foreign-key inserts elsewhere.
	err := tx.QueryRow(ctx,
		`SELECT id::text FROM conversations.users WHERE id = $1::uuid FOR NO KEY UPDATE`, owner).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, unavailable("lock owner", err)
	}
	return true, nil
}

// BeginFence starts a login: it locks the owner, requires a live user and
// returns the endpoint's current generation, creating the endpoint state row
// (generation 1) when this is the endpoint's first login.
//
// The upsert deliberately leaves bound_* alone -- beginning a login does not
// disturb a live binding -- and clears pending_*, because a second Begin on the
// same endpoint supersedes any older pending flow there: the older fence's
// pending_resource no longer matches, so its CommitLogin is refused.
func (s *Store) BeginFence(ctx context.Context, owner, endpoint string) (mcpoauth.LoginFence, error) {
	if endpoint == "" {
		return mcpoauth.LoginFence{}, errors.New("mcpauthdb: begin fence: endpoint is required")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return mcpoauth.LoginFence{}, unavailable("begin fence", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	if err := lockLiveOwner(ctx, tx, owner); err != nil {
		return mcpoauth.LoginFence{}, err
	}
	var generation int64
	err = tx.QueryRow(ctx, `
		INSERT INTO conversations.mcp_endpoint (owner_user_id, endpoint)
		VALUES ($1::uuid, $2)
		ON CONFLICT (owner_user_id, endpoint) DO UPDATE
		   SET updated_at = now(),
		       pending_resource = NULL,
		       pending_issuer = NULL,
		       pending_grant_generation = NULL
		RETURNING generation`, owner, endpoint).Scan(&generation)
	if err != nil {
		return mcpoauth.LoginFence{}, unavailable("begin fence", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return mcpoauth.LoginFence{}, unavailable("commit begin fence", err)
	}
	return mcpoauth.LoginFence{
		OwnerUserID:        owner,
		Endpoint:           endpoint,
		EndpointGeneration: generation,
		OwnerLive:          true,
	}, nil
}

// BindFence records the validated resource and exact issuer, and the grant
// generation observed under the owner lock, then returns the fence CommitLogin
// accepts. The endpoint row must exist and its generation must still be the one
// BeginFence captured: a logout in between advances it and refuses this bind.
func (s *Store) BindFence(ctx context.Context, fence mcpoauth.LoginFence, resource, issuer string) (mcpoauth.LoginFence, error) {
	if fence.Endpoint == "" || resource == "" || issuer == "" {
		return fence, errors.New("mcpauthdb: bind fence: endpoint, resource and issuer are required")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fence, unavailable("bind fence", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	if err := lockLiveOwner(ctx, tx, fence.OwnerUserID); err != nil {
		return fence, err
	}
	var generation int64
	err = tx.QueryRow(ctx, `
		SELECT generation FROM conversations.mcp_endpoint
		 WHERE owner_user_id = $1::uuid AND endpoint = $2 FOR UPDATE`,
		fence.OwnerUserID, fence.Endpoint).Scan(&generation)
	if errors.Is(err, pgx.ErrNoRows) {
		return fence, fmt.Errorf("%w: endpoint %q has no login fence", mcpoauth.ErrStaleFence, fence.Endpoint)
	}
	if err != nil {
		return fence, unavailable("bind fence", err)
	}
	if generation != fence.EndpointGeneration {
		return fence, fmt.Errorf("%w: endpoint generation %d, fence %d",
			mcpoauth.ErrStaleFence, generation, fence.EndpointGeneration)
	}

	// The grant may not exist yet (a first login), so its generation is 0; an
	// existing row contributes its live or tombstone generation, which is what
	// the commit must still see.
	var grantGeneration int64
	err = tx.QueryRow(ctx, `
		SELECT generation FROM conversations.mcp_credential
		 WHERE owner_user_id = $1::uuid AND resource = $2 FOR UPDATE`,
		fence.OwnerUserID, resource).Scan(&grantGeneration)
	if errors.Is(err, pgx.ErrNoRows) {
		grantGeneration = 0
	} else if err != nil {
		return fence, unavailable("bind fence", err)
	}

	if _, err := tx.Exec(ctx, `
		UPDATE conversations.mcp_endpoint
		   SET pending_resource = $3, pending_issuer = $4,
		       pending_grant_generation = $5, updated_at = now()
		 WHERE owner_user_id = $1::uuid AND endpoint = $2`,
		fence.OwnerUserID, fence.Endpoint, resource, issuer, grantGeneration); err != nil {
		return fence, unavailable("bind fence", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fence, unavailable("commit bind fence", err)
	}

	out := fence
	out.OwnerLive = true
	out.Resource = resource
	out.Issuer = issuer
	out.GrantGeneration = grantGeneration
	return out, nil
}

// CommitLogin installs grant and binds fence.Endpoint to it in one transaction
// that also proves the owner is still live. It requires BOTH captured
// generations to still be current and the endpoint's pending fence to be this
// one, so a logout or another login in between refuses it instead of being
// overwritten. A success advances the grant generation by one.
func (s *Store) CommitLogin(ctx context.Context, fence mcpoauth.LoginFence, grant mcpoauth.Grant) (mcpoauth.Grant, error) {
	if fence.OwnerUserID == "" || fence.Endpoint == "" {
		return mcpoauth.Grant{}, errors.New("mcpauthdb: commit login: fence has no owner or endpoint")
	}
	if fence.Resource == "" {
		return mcpoauth.Grant{}, fmt.Errorf("%w: login fence was never bound to a resource", mcpoauth.ErrStaleFence)
	}
	if grant.Resource != fence.Resource || grant.Issuer != fence.Issuer {
		return mcpoauth.Grant{}, fmt.Errorf("%w: grant is for resource %q issuer %q, fence for %q issuer %q",
			mcpoauth.ErrStaleFence, grant.Resource, grant.Issuer, fence.Resource, fence.Issuer)
	}
	authStyle, err := normalizeAuthStyle(grant.Registration.AuthStyle)
	if err != nil {
		return mcpoauth.Grant{}, err
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return mcpoauth.Grant{}, unavailable("commit login", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	if err := lockLiveOwner(ctx, tx, fence.OwnerUserID); err != nil {
		return mcpoauth.Grant{}, err
	}

	// The endpoint fence this login was begun under.
	var generation int64
	var pendingResource, pendingIssuer *string
	var pendingGeneration *int64
	err = tx.QueryRow(ctx, `
		SELECT generation, pending_resource, pending_issuer, pending_grant_generation
		  FROM conversations.mcp_endpoint
		 WHERE owner_user_id = $1::uuid AND endpoint = $2 FOR UPDATE`,
		fence.OwnerUserID, fence.Endpoint).
		Scan(&generation, &pendingResource, &pendingIssuer, &pendingGeneration)
	if errors.Is(err, pgx.ErrNoRows) {
		return mcpoauth.Grant{}, fmt.Errorf("%w: endpoint %q has no login fence", mcpoauth.ErrStaleFence, fence.Endpoint)
	}
	if err != nil {
		return mcpoauth.Grant{}, unavailable("commit login", err)
	}
	if generation != fence.EndpointGeneration {
		return mcpoauth.Grant{}, fmt.Errorf("%w: endpoint generation %d, fence %d",
			mcpoauth.ErrStaleFence, generation, fence.EndpointGeneration)
	}
	if pendingResource == nil || *pendingResource != fence.Resource ||
		pendingIssuer == nil || *pendingIssuer != fence.Issuer ||
		pendingGeneration == nil || *pendingGeneration != fence.GrantGeneration {
		return mcpoauth.Grant{}, fmt.Errorf("%w: endpoint %q pending fence is not this login", mcpoauth.ErrStaleFence, fence.Endpoint)
	}

	// The grant CAS: another explicit login that committed first advanced this.
	var current int64
	err = tx.QueryRow(ctx, `
		SELECT generation FROM conversations.mcp_credential
		 WHERE owner_user_id = $1::uuid AND resource = $2 FOR UPDATE`,
		fence.OwnerUserID, fence.Resource).Scan(&current)
	if errors.Is(err, pgx.ErrNoRows) {
		current = 0
	} else if err != nil {
		return mcpoauth.Grant{}, unavailable("commit login", err)
	}
	if current != fence.GrantGeneration {
		return mcpoauth.Grant{}, fmt.Errorf("%w: grant generation %d, fence %d",
			mcpoauth.ErrStaleFence, current, fence.GrantGeneration)
	}

	next := grant
	next.OwnerUserID = strings.ToLower(fence.OwnerUserID)
	next.Resource = fence.Resource
	next.Issuer = fence.Issuer
	next.Generation = current + 1
	next.Registration.AuthStyle = authStyle
	next.RefreshState = mcpoauth.RefreshIdle
	next.RefreshVersion = 0

	sealed, err := s.seal(next)
	if err != nil {
		return mcpoauth.Grant{}, err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO conversations.mcp_credential
		   (owner_user_id, resource, issuer, generation, key_id, credential_sealed,
		    client_id, auth_style, authorization_endpoint, token_endpoint,
		    revocation_endpoint, scopes, redirect_url, token_type, access_expiry,
		    refresh_state, refresh_version, invalidated_at)
		VALUES ($1::uuid, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15,
		        'idle', 0, NULL)
		ON CONFLICT (owner_user_id, resource) DO UPDATE SET
		    issuer                 = EXCLUDED.issuer,
		    generation             = EXCLUDED.generation,
		    key_id                 = EXCLUDED.key_id,
		    credential_sealed      = EXCLUDED.credential_sealed,
		    client_id              = EXCLUDED.client_id,
		    auth_style             = EXCLUDED.auth_style,
		    authorization_endpoint = EXCLUDED.authorization_endpoint,
		    token_endpoint         = EXCLUDED.token_endpoint,
		    revocation_endpoint    = EXCLUDED.revocation_endpoint,
		    scopes                 = EXCLUDED.scopes,
		    redirect_url           = EXCLUDED.redirect_url,
		    token_type             = EXCLUDED.token_type,
		    access_expiry          = EXCLUDED.access_expiry,
		    refresh_state          = 'idle',
		    refresh_version        = 0,
		    invalidated_at         = NULL,
		    updated_at             = now()`,
		next.OwnerUserID, next.Resource, next.Issuer, next.Generation, s.box.KeyID(), sealed,
		next.Registration.ClientID, string(authStyle), next.Registration.AuthorizationEndpoint,
		next.Registration.TokenEndpoint, next.Registration.RevocationEndpoint,
		scopesArg(next.Scopes), next.RedirectURL, next.Token.TokenType,
		expiryArg(next.Token.Expiry)); err != nil {
		return mcpoauth.Grant{}, unavailable("commit login", err)
	}

	// Bind the exact canonical endpoint to the installed grant generation and
	// retire the pending fence.
	if _, err := tx.Exec(ctx, `
		UPDATE conversations.mcp_endpoint
		   SET bound_resource = $3, bound_issuer = $4, bound_grant_generation = $5,
		       pending_resource = NULL, pending_issuer = NULL,
		       pending_grant_generation = NULL, updated_at = now()
		 WHERE owner_user_id = $1::uuid AND endpoint = $2`,
		fence.OwnerUserID, fence.Endpoint, next.Resource, next.Issuer, next.Generation); err != nil {
		return mcpoauth.Grant{}, unavailable("commit login", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return mcpoauth.Grant{}, unavailable("commit login", err)
	}
	return next, nil
}

// LookupBinding resolves one exact owner+endpoint to its live binding, with no
// discovery and no guessing from the endpoint's origin. A tombstoned owner is
// refused before the binding is even consulted, so cleanup that failed cannot
// make removed credentials readable.
func (s *Store) LookupBinding(ctx context.Context, owner, endpoint string) (mcpoauth.Binding, error) {
	var canonicalOwner string
	var live bool
	err := s.pool.QueryRow(ctx,
		`SELECT id::text, deleted_at IS NULL FROM conversations.users WHERE id = $1::uuid`, owner).
		Scan(&canonicalOwner, &live)
	if errors.Is(err, pgx.ErrNoRows) {
		return mcpoauth.Binding{}, fmt.Errorf("%w: owner %s does not exist", mcpoauth.ErrLoginRequired, owner)
	}
	if err != nil {
		return mcpoauth.Binding{}, unavailable("lookup binding", err)
	}
	if !live {
		return mcpoauth.Binding{}, fmt.Errorf("%w: owner %s is deleted", mcpoauth.ErrLoginRequired, owner)
	}

	var resource, issuer *string
	var boundGeneration *int64
	err = s.pool.QueryRow(ctx, `
		SELECT bound_resource, bound_issuer, bound_grant_generation
		  FROM conversations.mcp_endpoint
		 WHERE owner_user_id = $1::uuid AND endpoint = $2`, owner, endpoint).
		Scan(&resource, &issuer, &boundGeneration)
	if errors.Is(err, pgx.ErrNoRows) {
		return mcpoauth.Binding{}, fmt.Errorf("%w: endpoint %q was never logged in", mcpoauth.ErrMissing, endpoint)
	}
	if err != nil {
		return mcpoauth.Binding{}, unavailable("lookup binding", err)
	}
	if resource == nil || issuer == nil || boundGeneration == nil {
		return mcpoauth.Binding{}, fmt.Errorf("%w: endpoint %q has no live binding", mcpoauth.ErrMissing, endpoint)
	}

	var generation int64
	var invalidated bool
	err = s.pool.QueryRow(ctx, `
		SELECT generation, invalidated_at IS NOT NULL FROM conversations.mcp_credential
		 WHERE owner_user_id = $1::uuid AND resource = $2`, owner, *resource).
		Scan(&generation, &invalidated)
	if errors.Is(err, pgx.ErrNoRows) {
		return mcpoauth.Binding{}, fmt.Errorf("%w: binding for endpoint %q has no grant", mcpoauth.ErrLoginRequired, endpoint)
	}
	if err != nil {
		return mcpoauth.Binding{}, unavailable("lookup binding", err)
	}
	if invalidated {
		return mcpoauth.Binding{}, fmt.Errorf("%w: grant for endpoint %q was invalidated", mcpoauth.ErrLoginRequired, endpoint)
	}
	if generation != *boundGeneration {
		return mcpoauth.Binding{}, fmt.Errorf("%w: binding generation %d, grant generation %d",
			mcpoauth.ErrLoginRequired, *boundGeneration, generation)
	}
	return mcpoauth.Binding{
		OwnerUserID:     canonicalOwner,
		Endpoint:        endpoint,
		Resource:        *resource,
		Issuer:          *issuer,
		GrantGeneration: *boundGeneration,
	}, nil
}

// Acquire returns a usable grant for one exact endpoint binding, consulting the
// live user, the binding and the grant on EVERY call -- including when the
// access token has not expired -- so a logout or a newer login is seen
// immediately rather than after a cache expires.
//
// The refresh path is the durable intent protocol, never a cached token source:
//
//  1. Under the owner lock, re-read the grant and its expiry and commit
//     refresh_state='refreshing' with a new refresh_version. The transaction
//     commits BEFORE the upstream request, so a crash cannot lose the fact that
//     the single-use token is being spent.
//  2. Call refresh outside every transaction. Another acquisition of this
//     grant now observes the intent and is refused (ErrLoginRequired) instead
//     of spending the same token a second time.
//  3. Reacquire the owner lock and finalize with a compare-and-swap against the
//     same generation and intent version. A logout or newer login in between
//     fails the CAS and returns no token.
//
// A refresh that fails, returns no access token, or whose rotation cannot be
// committed leaves the intent committed: that generation is login-required and
// there is no automatic replay, even after a restart.
func (s *Store) Acquire(ctx context.Context, owner, endpoint string, expectedGeneration int64, refresh mcpoauth.RefreshFunc) (mcpoauth.Grant, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return mcpoauth.Grant{}, unavailable("acquire", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	if err := lockLiveOwner(ctx, tx, owner); err != nil {
		return mcpoauth.Grant{}, err
	}

	var resource string
	var boundGeneration int64
	err = tx.QueryRow(ctx, `
		SELECT bound_resource, bound_grant_generation
		  FROM conversations.mcp_endpoint
		 WHERE owner_user_id = $1::uuid AND endpoint = $2
		   AND bound_resource IS NOT NULL FOR UPDATE`, owner, endpoint).
		Scan(&resource, &boundGeneration)
	if errors.Is(err, pgx.ErrNoRows) {
		return mcpoauth.Grant{}, fmt.Errorf("%w: endpoint %q has no live binding", mcpoauth.ErrMissing, endpoint)
	}
	if err != nil {
		return mcpoauth.Grant{}, unavailable("acquire", err)
	}
	if boundGeneration != expectedGeneration {
		return mcpoauth.Grant{}, fmt.Errorf("%w: binding generation %d, expected %d",
			mcpoauth.ErrStaleFence, boundGeneration, expectedGeneration)
	}

	row, err := readCredential(ctx, tx, owner, resource, true)
	if err != nil {
		return mcpoauth.Grant{}, err
	}
	if row.invalidated {
		return mcpoauth.Grant{}, fmt.Errorf("%w: grant for resource %q was invalidated", mcpoauth.ErrLoginRequired, resource)
	}
	// Open the blob only after the tombstone check: a logged-out row has had
	// its ciphertext wiped, and the honest answer is "invalidated", not
	// "unreadable under this key".
	cur, err := s.openRow(row)
	if err != nil {
		return mcpoauth.Grant{}, err
	}
	if cur.Generation != expectedGeneration {
		return mcpoauth.Grant{}, fmt.Errorf("%w: grant generation %d, expected %d",
			mcpoauth.ErrStaleFence, cur.Generation, expectedGeneration)
	}
	if cur.RefreshState == mcpoauth.RefreshRefreshing {
		return mcpoauth.Grant{}, fmt.Errorf("%w: a refresh for resource %q is already in flight", mcpoauth.ErrLoginRequired, resource)
	}
	// The expiry is re-checked HERE, after the lock: a second acquisition that
	// waited for the first's intent sees either the intent or the rotation,
	// never a stale "still valid" answer.
	if !expired(cur.Token.Expiry, time.Now()) {
		return cur, nil
	}
	if refresh == nil || cur.Token.RefreshToken == "" {
		return mcpoauth.Grant{}, fmt.Errorf("%w: access token for resource %q expired and no refresh is possible", mcpoauth.ErrLoginRequired, resource)
	}

	var intentVersion int64
	err = tx.QueryRow(ctx, `
		UPDATE conversations.mcp_credential
		   SET refresh_state = 'refreshing', refresh_version = refresh_version + 1, updated_at = now()
		 WHERE owner_user_id = $1::uuid AND resource = $2 AND generation = $3
		   AND invalidated_at IS NULL AND refresh_state = 'idle'
		RETURNING refresh_version`, owner, resource, expectedGeneration).Scan(&intentVersion)
	if errors.Is(err, pgx.ErrNoRows) {
		// Somebody moved the row under us; the owner lock should make this
		// unreachable, so answer conservatively rather than retrying a token
		// that may already have been spent.
		return mcpoauth.Grant{}, fmt.Errorf("%w: refresh intent for resource %q was refused", mcpoauth.ErrLoginRequired, resource)
	}
	if err != nil {
		return mcpoauth.Grant{}, unavailable("commit refresh intent", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return mcpoauth.Grant{}, unavailable("commit refresh intent", err)
	}

	// Outside every transaction: the intent is durable and no lock is held.
	cur.RefreshState = mcpoauth.RefreshRefreshing
	cur.RefreshVersion = intentVersion
	result, fnErr := refresh(ctx, cur)
	if fnErr != nil {
		// The intent stays committed. The stored refresh token may already be
		// spent upstream, so replaying it is never safe.
		return mcpoauth.Grant{}, fmt.Errorf("%w: token refresh failed: %w", mcpoauth.ErrLoginRequired, fnErr)
	}
	if result.AccessToken == "" {
		return mcpoauth.Grant{}, fmt.Errorf("%w: token refresh returned no access token", mcpoauth.ErrLoginRequired)
	}
	return s.finalizeRefresh(ctx, owner, resource, expectedGeneration, intentVersion, result)
}

// finalizeRefresh commits a rotation against the intent it belongs to. The
// identity, registration, redirect URL and (unless the result supplies one) the
// distributed scopes come from the row just re-read, so a refresh answer can
// never change what the grant is or whose client registration it uses.
func (s *Store) finalizeRefresh(ctx context.Context, owner, resource string, generation, intentVersion int64, result mcpoauth.RefreshResult) (mcpoauth.Grant, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return mcpoauth.Grant{}, unavailable("finalize refresh", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	if err := lockLiveOwner(ctx, tx, owner); err != nil {
		return mcpoauth.Grant{}, err
	}
	row, err := readCredential(ctx, tx, owner, resource, true)
	if err != nil {
		if errors.Is(err, mcpoauth.ErrMissing) {
			return mcpoauth.Grant{}, fmt.Errorf("%w: grant for resource %q disappeared during refresh", mcpoauth.ErrStaleFence, resource)
		}
		return mcpoauth.Grant{}, err
	}
	if row.invalidated {
		return mcpoauth.Grant{}, fmt.Errorf("%w: grant for resource %q was invalidated during refresh", mcpoauth.ErrLoginRequired, resource)
	}
	if row.generation != generation {
		return mcpoauth.Grant{}, fmt.Errorf("%w: grant generation moved to %d during refresh", mcpoauth.ErrStaleFence, row.generation)
	}
	if row.refreshState != string(mcpoauth.RefreshRefreshing) || row.refreshVersion != intentVersion {
		return mcpoauth.Grant{}, fmt.Errorf("%w: refresh intent is no longer current", mcpoauth.ErrStaleFence)
	}

	next, err := s.openRow(row)
	if err != nil {
		return mcpoauth.Grant{}, err
	}
	next.Token.AccessToken = result.AccessToken
	if result.RefreshToken != "" {
		next.Token.RefreshToken = result.RefreshToken
	}
	if result.TokenType != "" {
		next.Token.TokenType = result.TokenType
	}
	if result.Expiry.IsZero() {
		// NULL means "the issuer reported no expiry", which never refreshes on
		// expiry and would pin this access token forever. A refresh that
		// omitted expires_in must not become that: record the rotation as
		// already expired, so the next acquisition refreshes again with the
		// refresh token that was just persisted.
		next.Token.Expiry = time.Now().Add(-time.Second).Truncate(time.Microsecond)
	} else {
		next.Token.Expiry = result.Expiry
	}
	if result.Scopes != nil {
		next.Scopes = result.Scopes
	}
	sealed, err := s.seal(next)
	if err != nil {
		return mcpoauth.Grant{}, err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE conversations.mcp_credential
		   SET key_id = $3, credential_sealed = $4, token_type = $5,
		       access_expiry = $6, scopes = $7,
		       refresh_state = 'idle', refresh_version = refresh_version + 1, updated_at = now()
		 WHERE owner_user_id = $1::uuid AND resource = $2`,
		owner, resource, s.box.KeyID(), sealed, next.Token.TokenType,
		expiryArg(next.Token.Expiry), scopesArg(next.Scopes)); err != nil {
		return mcpoauth.Grant{}, unavailable("persist refreshed token", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return mcpoauth.Grant{}, unavailable("commit refreshed token", err)
	}
	next.RefreshState = mcpoauth.RefreshIdle
	next.RefreshVersion = intentVersion + 1
	return next, nil
}

// invalidateGrant tombstones one grant under the caller's transaction and
// returns the detached material to revoke. An already-invalidated grant is a
// no-op (live=false) and yields no revocation, so a repeated logout neither
// re-revokes nor advances the tombstone again. An absent grant is ErrMissing.
//
// The sealed blob is WIPED as part of the tombstone: a resurrectable token must
// not outlive the logout that invalidated it, even in a backup. That is also
// why an undecryptable blob does not fail the logout -- the local invalidation
// has already happened and only the upstream revoke is lost.
func (s *Store) invalidateGrant(ctx context.Context, tx pgx.Tx, owner, resource string) (mcpoauth.Revocation, bool, error) {
	row, err := readCredential(ctx, tx, owner, resource, true)
	if err != nil {
		return mcpoauth.Revocation{}, false, err
	}
	rev := mcpoauth.Revocation{
		OwnerUserID: row.owner,
		Resource:    row.resource,
		Issuer:      row.issuer,
		Generation:  row.generation + 1,
	}
	if row.invalidated {
		return mcpoauth.Revocation{}, false, nil
	}
	if g, openErr := s.openRow(row); openErr != nil {
		slog.Warn("mcpauthdb: invalidated a grant that could not be decrypted; revocation skipped",
			"owner", row.owner, "resource", row.resource, "error", openErr)
	} else {
		rev.Registration = g.Registration
		rev.RefreshToken = g.Token.RefreshToken
		rev.AccessToken = g.Token.AccessToken
	}
	if _, err := tx.Exec(ctx, `
		UPDATE conversations.mcp_credential
		   SET generation = generation + 1, invalidated_at = now(),
		       key_id = '', credential_sealed = '', refresh_state = 'idle',
		       refresh_version = refresh_version + 1, updated_at = now()
		 WHERE owner_user_id = $1::uuid AND resource = $2`, owner, resource); err != nil {
		return mcpoauth.Revocation{}, false, unavailable("invalidate grant", err)
	}
	return rev, true, nil
}

// fenceEndpointsForResource advances the fence of every endpoint bound or
// pending to one resource and clears the half that belongs to it -- the live
// binding only when bound_resource is this resource, the pending login only
// when pending_resource is. An endpoint bound to one live resource while a
// different resource is being logged out keeps its binding. The rows are locked
// in deterministic URL order before any is updated: all of this owner's
// mutations already serialize on the users row, but the ordering keeps the rule
// explicit and survives a future path that does not take that lock.
func (s *Store) fenceEndpointsForResource(ctx context.Context, tx pgx.Tx, owner, resource string) error {
	rows, err := tx.Query(ctx, `
		SELECT endpoint FROM conversations.mcp_endpoint
		 WHERE owner_user_id = $1::uuid
		   AND (bound_resource = $2 OR pending_resource = $2)
		 ORDER BY endpoint FOR UPDATE`, owner, resource)
	if err != nil {
		return unavailable("fence endpoints", err)
	}
	var endpoints []string
	for rows.Next() {
		var endpoint string
		if err := rows.Scan(&endpoint); err != nil {
			rows.Close()
			return unavailable("fence endpoints", err)
		}
		endpoints = append(endpoints, endpoint)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return unavailable("fence endpoints", err)
	}
	for _, endpoint := range endpoints {
		// Clear each half only when IT is the logged-out resource. An endpoint
		// bound to live resource R1 with a pending login for R2 must keep its
		// R1 binding when R2 is logged out; its generation still advances, so
		// the pending login is fenced either way.
		if _, err := tx.Exec(ctx, `
			UPDATE conversations.mcp_endpoint
			   SET generation = generation + 1,
			       bound_resource = CASE WHEN bound_resource = $3 THEN NULL ELSE bound_resource END,
			       bound_issuer = CASE WHEN bound_resource = $3 THEN NULL ELSE bound_issuer END,
			       bound_grant_generation = CASE WHEN bound_resource = $3 THEN NULL ELSE bound_grant_generation END,
			       pending_resource = CASE WHEN pending_resource = $3 THEN NULL ELSE pending_resource END,
			       pending_issuer = CASE WHEN pending_resource = $3 THEN NULL ELSE pending_issuer END,
			       pending_grant_generation = CASE WHEN pending_resource = $3 THEN NULL ELSE pending_grant_generation END,
			       updated_at = now()
			 WHERE owner_user_id = $1::uuid AND endpoint = $2`, owner, endpoint, resource); err != nil {
			return unavailable("fence endpoint", err)
		}
	}
	return nil
}

// InvalidateResource logs out one canonical resource. The grant is tombstoned
// with an incremented generation and its blobs wiped, and every endpoint bound
// or PENDING to that resource has its own generation advanced -- which is what
// makes a login already in flight fail its CommitLogin instead of recreating
// the grant.
//
// An absent grant is ErrMissing (the caller's resource selector found nothing),
// but the pending-fence pass still runs first: an in-flight first login for
// that resource must not survive the logout just because no grant existed yet.
func (s *Store) InvalidateResource(ctx context.Context, owner, resource string) (mcpoauth.Revocation, error) {
	if resource == "" {
		return mcpoauth.Revocation{}, errors.New("mcpauthdb: invalidate resource: resource is required")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return mcpoauth.Revocation{}, unavailable("invalidate resource", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	found, err := lockOwner(ctx, tx, owner)
	if err != nil {
		return mcpoauth.Revocation{}, err
	}
	if !found {
		return mcpoauth.Revocation{}, fmt.Errorf("%w: owner %s does not exist", mcpoauth.ErrMissing, owner)
	}

	rev, _, grantErr := s.invalidateGrant(ctx, tx, owner, resource)
	missing := errors.Is(grantErr, mcpoauth.ErrMissing)
	if grantErr != nil && !missing {
		return mcpoauth.Revocation{}, grantErr
	}
	if err := s.fenceEndpointsForResource(ctx, tx, owner, resource); err != nil {
		return mcpoauth.Revocation{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return mcpoauth.Revocation{}, unavailable("commit invalidate resource", err)
	}
	if missing {
		return mcpoauth.Revocation{}, fmt.Errorf("%w: no grant for resource %q", mcpoauth.ErrMissing, resource)
	}
	return rev, nil
}

// InvalidateEndpoint logs out one canonical endpoint. An unbound endpoint still
// advances its own generation, so a login that began there cannot commit
// afterwards, and asking again is locally successful/idempotent. A bound
// endpoint also invalidates its grant and every other binding to that grant,
// advancing all of their fences, and returns the detached revocation payload.
func (s *Store) InvalidateEndpoint(ctx context.Context, owner, endpoint string) (mcpoauth.Revocation, error) {
	if endpoint == "" {
		return mcpoauth.Revocation{}, errors.New("mcpauthdb: invalidate endpoint: endpoint is required")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return mcpoauth.Revocation{}, unavailable("invalidate endpoint", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	found, err := lockOwner(ctx, tx, owner)
	if err != nil {
		return mcpoauth.Revocation{}, err
	}
	if !found {
		// No owner row means no endpoint or grant rows (the FK guarantees it).
		return mcpoauth.Revocation{}, nil
	}

	var bound *string
	err = tx.QueryRow(ctx, `
		SELECT bound_resource FROM conversations.mcp_endpoint
		 WHERE owner_user_id = $1::uuid AND endpoint = $2 FOR UPDATE`, owner, endpoint).Scan(&bound)
	if errors.Is(err, pgx.ErrNoRows) {
		// Nothing has ever been begun or logged in here; there is no fence to
		// advance and the logout is locally successful.
		return mcpoauth.Revocation{}, nil
	}
	if err != nil {
		return mcpoauth.Revocation{}, unavailable("invalidate endpoint", err)
	}

	if bound != nil && *bound != "" {
		rev, _, grantErr := s.invalidateGrant(ctx, tx, owner, *bound)
		if grantErr != nil && !errors.Is(grantErr, mcpoauth.ErrMissing) {
			return mcpoauth.Revocation{}, grantErr
		}
		// This endpoint is one of the resource's bindings, so this advances its
		// own generation and clears the binding too.
		if err := s.fenceEndpointsForResource(ctx, tx, owner, *bound); err != nil {
			return mcpoauth.Revocation{}, err
		}
		if err := tx.Commit(ctx); err != nil {
			return mcpoauth.Revocation{}, unavailable("commit invalidate endpoint", err)
		}
		return rev, nil
	}

	if _, err := tx.Exec(ctx, `
		UPDATE conversations.mcp_endpoint
		   SET generation = generation + 1,
		       pending_resource = NULL, pending_issuer = NULL, pending_grant_generation = NULL,
		       updated_at = now()
		 WHERE owner_user_id = $1::uuid AND endpoint = $2`, owner, endpoint); err != nil {
		return mcpoauth.Revocation{}, unavailable("invalidate endpoint", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return mcpoauth.Revocation{}, unavailable("commit invalidate endpoint", err)
	}
	return mcpoauth.Revocation{}, nil
}

// InvalidateOwner invalidates every grant, binding and pending fence the owner
// has. It is the user-removal path, so it deliberately does not require a live
// user: the row is tombstoned first, and this cleanup runs against it after.
// Credential use, login and refresh refuse a tombstoned owner on their own, so
// a failure here cannot resurrect anything; it can only leave rows behind.
func (s *Store) InvalidateOwner(ctx context.Context, owner string) ([]mcpoauth.Revocation, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, unavailable("invalidate owner", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	found, err := lockOwner(ctx, tx, owner)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, nil
	}

	grantRows, err := tx.Query(ctx, `
		SELECT resource FROM conversations.mcp_credential
		 WHERE owner_user_id = $1::uuid ORDER BY resource FOR UPDATE`, owner)
	if err != nil {
		return nil, unavailable("invalidate owner", err)
	}
	var resources []string
	for grantRows.Next() {
		var resource string
		if err := grantRows.Scan(&resource); err != nil {
			grantRows.Close()
			return nil, unavailable("invalidate owner", err)
		}
		resources = append(resources, resource)
	}
	grantRows.Close()
	if err := grantRows.Err(); err != nil {
		return nil, unavailable("invalidate owner", err)
	}
	var revocations []mcpoauth.Revocation
	for _, resource := range resources {
		rev, live, err := s.invalidateGrant(ctx, tx, owner, resource)
		if err != nil {
			if errors.Is(err, mcpoauth.ErrMissing) {
				continue
			}
			return nil, err
		}
		if live {
			revocations = append(revocations, rev)
		}
	}

	endpointRows, err := tx.Query(ctx, `
		SELECT endpoint FROM conversations.mcp_endpoint
		 WHERE owner_user_id = $1::uuid ORDER BY endpoint FOR UPDATE`, owner)
	if err != nil {
		return nil, unavailable("invalidate owner", err)
	}
	var endpoints []string
	for endpointRows.Next() {
		var endpoint string
		if err := endpointRows.Scan(&endpoint); err != nil {
			endpointRows.Close()
			return nil, unavailable("invalidate owner", err)
		}
		endpoints = append(endpoints, endpoint)
	}
	endpointRows.Close()
	if err := endpointRows.Err(); err != nil {
		return nil, unavailable("invalidate owner", err)
	}
	for _, endpoint := range endpoints {
		if _, err := tx.Exec(ctx, `
			UPDATE conversations.mcp_endpoint
			   SET generation = generation + 1,
			       bound_resource = NULL, bound_issuer = NULL, bound_grant_generation = NULL,
			       pending_resource = NULL, pending_issuer = NULL, pending_grant_generation = NULL,
			       updated_at = now()
			 WHERE owner_user_id = $1::uuid AND endpoint = $2`, owner, endpoint); err != nil {
			return nil, unavailable("invalidate owner", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, unavailable("commit invalidate owner", err)
	}
	return revocations, nil
}
