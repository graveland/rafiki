// SPDX-License-Identifier: Apache-2.0

// Package providercreds is the pgx-free half of the per-owner provider
// credential store: the types and the error sentinels. The Postgres
// implementation lives in pkg/providercredsdb.
//
// A credential here is an OAuth login a user made to a provider (today
// ChatGPT via pkg/chatgptauth) on the daemon's behalf. The tokens are usable
// secrets: they are sealed at rest by pkg/secretbox, live only in daemon
// memory in plaintext, and never reach the CLI or a child process.
//
// The split exists for the same reason pkg/users has one: cmd/rafiki must
// link zero pgx packages (TestClientDoesNotLinkPostgres), so anything a client
// might want to name cannot pull a driver in.
package providercreds

import (
	"context"
	"errors"
	"time"
)

// Credential is one (owner, provider) login. The token fields are plaintext
// and exist in memory only; the store seals them before any write.
type Credential struct {
	OwnerUserID  string
	Provider     string
	AccountID    string
	RefreshToken string
	AccessToken  string
	AccessExpiry time.Time
	// ModelsJSON is the provider's model directory as last fetched, nil when
	// it was never fetched. It is cached here rather than derived so a daemon
	// restart does not re-ask upstream on every spawn.
	//
	// It is stored as JSONB, so a value read back is Postgres's canonical
	// rendering of the document, not byte-for-byte what was written: compare
	// it as JSON (or parse it), never as bytes.
	ModelsJSON []byte
	// ModelsETag is the ETag of the response ModelsJSON came from, '' when
	// there is none to revalidate against.
	ModelsETag string
	// Dead is true once the provider refused the refresh token. A dead
	// credential is kept (it can still be revoked, and it explains why the
	// provider is unavailable) but can never be refreshed again.
	Dead bool
	// LastRefresh is when the last successful refresh happened. ListStale
	// reads it.
	LastRefresh time.Time
}

var (
	// ErrNotFound means this owner has no credential for this provider.
	ErrNotFound = errors.New("providercreds: no credential for this owner and provider")
	// ErrDead means the stored credential can no longer be used: the provider
	// refused the refresh token, or the row cannot be decrypted with the
	// current RAFIKI_SECRETS_KEY. The owner has to log in again.
	ErrDead = errors.New("providercreds: credential is dead; log in again")
	// ErrNotPersisted means fn produced a new credential but writing it
	// failed. Refresh returns the NEW credential alongside this error: the
	// upstream has already invalidated the old refresh token, so the caller
	// must keep and re-save the new one, never drop it.
	ErrNotPersisted = errors.New("providercreds: refreshed credential was not persisted")
)

// Key names one stored credential without opening it, for scans.
type Key struct {
	Owner    string
	Provider string
}

// Store is the credential store. Every method is owner-scoped: there is no
// global credential and no daemon-level fallback.
type Store interface {
	// Put upserts the whole row: a Put is a login in hand, so it replaces the
	// stored tokens and clears dead_at. It never merges -- a second login to a
	// different account must not inherit the first one's cached model list.
	Put(ctx context.Context, c Credential) error

	// Get returns the stored credential with its tokens decrypted.
	// ErrNotFound when there is no row; ErrDead when the row is dead or
	// cannot be decrypted with the current key.
	Get(ctx context.Context, owner, provider string) (Credential, error)

	// Delete removes the row and returns the deleted credential (so a caller
	// can revoke the tokens upstream). ErrNotFound if there was none.
	Delete(ctx context.Context, owner, provider string) (Credential, error)

	// Refresh runs fn under a row lock. fn receives the CURRENT stored
	// credential and returns the replacement, or (cur, nil) unchanged when it
	// decides no refresh is needed (another holder already refreshed). If fn
	// returns an error wrapping ErrDead, the row's dead_at is set and the
	// error returned. Any other fn error rolls back and is returned.
	Refresh(ctx context.Context, owner, provider string, fn func(cur Credential) (Credential, error)) (Credential, error)

	// SetModels caches the model directory for a live credential.
	// ErrNotFound when there is no row for this owner and provider.
	SetModels(ctx context.Context, owner, provider string, modelsJSON []byte, etag string) error

	// ListStale lists live (not dead) credentials whose last_refresh_at is
	// older than age.
	ListStale(ctx context.Context, age time.Duration) ([]Key, error)
}
