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
	"fmt"
	"log/slog"
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

// redacted is what a token field prints as. Nothing in this package ever
// renders a token: a Credential is a live secret, and one reaching a log line,
// an error message or a panic trace is a credential leak.
const redacted = "<redacted>"

// String redacts both tokens, so an errant %v or a value dumped into a message
// cannot leak a credential. It is also what %+v uses: fmt prefers Stringer to
// the struct's own rendering.
func (c Credential) String() string {
	return fmt.Sprintf("providercreds.Credential{owner:%s provider:%s account:%s refresh:%s access:%s expiry:%s dead:%t last_refresh:%s models:%d bytes}",
		c.OwnerUserID, c.Provider, c.AccountID, redacted, redacted,
		c.AccessExpiry.UTC().Format(time.RFC3339), c.Dead,
		c.LastRefresh.UTC().Format(time.RFC3339), len(c.ModelsJSON))
}

// GoString redacts both tokens in the Go-syntax rendering too: %#v consults
// GoStringer where %v consults Stringer, and a debug dump or a panic trace is
// exactly where a live token must not appear.
func (c Credential) GoString() string {
	return fmt.Sprintf("providercreds.Credential{owner:%q, provider:%q, account:%q, refresh_token:%q, access_token:%q, access_expiry:%s, dead:%t, last_refresh:%s, models_json:%d bytes}",
		c.OwnerUserID, c.Provider, c.AccountID, redacted, redacted,
		c.AccessExpiry.UTC().Format(time.RFC3339), c.Dead,
		c.LastRefresh.UTC().Format(time.RFC3339), len(c.ModelsJSON))
}

// LogValue keeps the tokens out of structured logs for the same reason, so
// slog.String("credential", ...) and slog.Any log a redacted group rather than
// the live secret. It satisfies slog.LogValuer.
func (c Credential) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("owner", c.OwnerUserID),
		slog.String("provider", c.Provider),
		slog.String("account", c.AccountID),
		slog.String("refresh_token", redacted),
		slog.String("access_token", redacted),
		slog.Time("access_expiry", c.AccessExpiry),
		slog.Bool("dead", c.Dead),
		slog.Time("last_refresh", c.LastRefresh),
	)
}

var (
	// ErrNotFound means this owner has no credential for this provider.
	ErrNotFound = errors.New("providercreds: no credential for this owner and provider")
	// ErrDead means the stored credential can no longer be used: the provider
	// refused the refresh token, or the row cannot be decrypted with the
	// current RAFIKI_SECRETS_KEY. The owner has to log in again.
	//
	// A dead row is normally final, with ONE sanctioned exception: Store.Revive
	// clears the tombstone for a caller that holds the rotation replacing the
	// very token the row died on. That tombstone was another holder's
	// refresh_token_reused rather than a refusal, so it is known to be false,
	// and the caller's rotation is the only working login; dropping it would
	// turn a recoverable row into a forced re-login. Every other dead row stays
	// dead.
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
	//
	// It succeeds even when the stored ciphertext cannot be opened (the
	// RAFIKI_SECRETS_KEY changed): logout must always work, so the row is
	// deleted and NO error is returned, and the returned credential carries the
	// metadata but no tokens -- a revoke the caller cannot perform is skipped,
	// never attempted with empty tokens.
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

	// Revive writes want onto a DEAD row and clears dead_at, but only when the
	// row's stored refresh token is still sent, and reports whether it wrote.
	//
	// It exists for one interleaving: this process completed a rotation whose
	// write failed, while another holder of the same row refreshed with the
	// spent token, was told refresh_token_reused, and tombstoned the row. That
	// tombstone is known to be false for the holder of the successor, so the
	// rotation is written and the row comes back to life. It is the only
	// sanctioned resurrection of a dead credential (see ErrDead): a row that is
	// gone, not dead, holds a different token, or cannot be decrypted is left
	// alone, because none of those is a tombstone this caller can contradict.
	//
	// A true answer means the row now holds want's tokens and a fresh
	// last_refresh_at. The caller must revoke nothing: the rotation is live.
	Revive(ctx context.Context, owner, provider, sent string, want Credential) (bool, error)

	// DeleteAll removes every credential belonging to owner and returns what it
	// held, so a caller can revoke the tokens upstream (removing a user). Rows
	// whose ciphertext cannot be opened are still deleted and are returned with
	// their metadata and no tokens, like Delete. An owner with no credentials
	// returns an empty slice, never an error.
	DeleteAll(ctx context.Context, owner string) ([]Credential, error)
}
