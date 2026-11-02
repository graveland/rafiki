// SPDX-License-Identifier: Apache-2.0

package mcpoauth

import (
	"context"
	"errors"
	"time"
)

// This file is the pgx-free durable contract for daemon-brokered MCP OAuth:
// the owner-scoped grant, the endpoint binding, the durable fences that keep a
// logout from being undone by an older pending login, and the single-use
// refresh-intent state. pkg/mcpauthdb implements it against Postgres.
//
// Three rules shape every method here, and an implementation that breaks any of
// them breaks the engine built on top:
//
//   - The database is the authority on every call. Nothing is cached in a Go
//     map or a mutex: a grant that was logged out, replaced by a newer login or
//     has a refresh in flight is refused even when it was usable a moment ago.
//     Outcomes are distinguished by error identity (ErrMissing,
//     ErrLoginRequired, ErrStaleFence, ErrUnavailable), never by message text.
//   - A login and a logout are fenced by persisted generations. BeginFence
//     captures an endpoint generation, BindFence captures a resource and a
//     grant generation, and CommitLogin installs its grant only while both are
//     still current and the owner is still live. An older pending login can
//     therefore never recreate a grant after a logout.
//   - A refresh token is single-use, so the intent to refresh is a durable
//     row state, not an in-memory cache. It is committed before the upstream
//     request, finalized by a compare-and-swap against the same intent version,
//     and left behind (making the generation login-required) when the upstream
//     answer is ambiguous or the final write fails. There is no replay.

// ErrMissing reports that no stored credential exists for the requested owner
// and resource or endpoint: the owner never logged in, the binding was cleared
// by a logout, or the grant row is absent. It is the only error a caller may
// read as "there is genuinely nothing stored": a database failure is
// ErrUnavailable and an unusable credential is ErrLoginRequired.
var ErrMissing = errors.New("mcpoauth: no stored credential")

// ErrLoginRequired reports that a credential row exists but cannot be used
// without a fresh explicit login: it was invalidated, a refresh for this
// generation is already in flight (or was abandoned mid-flight), its stored
// ciphertext cannot be opened with the current key, the owner is tombstoned, or
// the access token expired and no usable refresh token remains.
var ErrLoginRequired = errors.New("mcpoauth: a fresh login is required")

// ErrStaleFence reports that the generation a caller captured is no longer
// current: a logout advanced an endpoint or grant fence, or another explicit
// login committed first. The operation did nothing; the caller must begin
// again. It never means "retry with the same fence".
var ErrStaleFence = errors.New("mcpoauth: generation fence is stale")

// ErrUnavailable reports that the store could not answer or could not persist
// what it was asked to: the database is unreachable, a query failed, or a
// commit failed. It is deliberately distinct from ErrMissing so that a store
// outage can never be reported -- or acted on -- as an absent credential.
var ErrUnavailable = errors.New("mcpoauth: credential store unavailable")

// AuthStyle is how a token endpoint wants its client credentials presented. It
// is persisted by NAME, never by enum ordinal, so adding a style cannot
// reinterpret a stored row.
type AuthStyle string

const (
	// AuthStyleAuto lets the OAuth client detect the style (the default).
	// The empty value of AuthStyle means the same thing, so a Grant built
	// without setting this field behaves as auto-detect rather than being
	// refused; every other unrecognised value is refused by the store.
	AuthStyleAuto AuthStyle = "auto"
	// AuthStyleInParams sends client_id/client_secret in the form body.
	AuthStyleInParams AuthStyle = "in_params"
	// AuthStyleInHeader sends them as HTTP Basic.
	AuthStyleInHeader AuthStyle = "in_header"
)

// RefreshState is the durable refresh-intent state of one grant.
type RefreshState string

const (
	// RefreshIdle means no refresh is in flight: acquisitions may use the
	// token and may start a refresh.
	RefreshIdle RefreshState = "idle"
	// RefreshRefreshing means an acquisition committed the intent to refresh
	// this generation and may be talking to the upstream. Every other
	// acquisition refuses the grant rather than replaying the single-use
	// refresh token or handing out a token that is being replaced.
	RefreshRefreshing RefreshState = "refreshing"
)

// Registration is the client registration and the validated endpoints a grant
// refreshes and revokes against. It is per owner, resource and exact issuer,
// and a new explicit login registers afresh rather than reusing an old
// registration: a changed issuer is accepted only through that login.
type Registration struct {
	ClientID     string
	ClientSecret string // secret: sealed at rest, never a plaintext column
	AuthStyle    AuthStyle
	// AuthorizationEndpoint, TokenEndpoint and RevocationEndpoint are the
	// validated metadata endpoints. RevocationEndpoint is empty when the
	// issuer advertised none; revocation is then skipped rather than guessed.
	AuthorizationEndpoint string
	TokenEndpoint         string
	RevocationEndpoint    string
}

// Token is the bearer half of a grant. RefreshToken is empty when the issuer
// issued none; the store keeps a stored refresh token when a refresh result
// omits a replacement.
type Token struct {
	AccessToken  string
	RefreshToken string
	TokenType    string
	// Expiry is the access token's expiry. The zero value means the issuer
	// reported none and is stored as NULL, which never refreshes on expiry.
	Expiry time.Time
}

// Grant is one owner's OAuth grant for one canonical MCP resource.
//
// OwnerUserID, Resource, Issuer and Generation are identity: a refresh result
// may replace the token material and the granted scopes, never these. The
// store returns them canonicalised as the row holds them.
type Grant struct {
	OwnerUserID string
	// Resource is the canonical RFC 8707 resource this grant is for.
	Resource string
	// Issuer is the exact issuer string the SDK validated, not a canonical
	// equivalent.
	Issuer     string
	Generation int64
	// Registration is the client registration the grant refreshes with.
	Registration Registration
	// Scopes are the granted scopes, in the issuer's order.
	Scopes []string
	// RedirectURL is the loopback redirect the login completed with. A later
	// login may legitimately use another one, so it is not part of identity.
	RedirectURL    string
	Token          Token
	RefreshState   RefreshState
	RefreshVersion int64
}

// RefreshResult is what a RefreshFunc hands back for the store to persist: the
// rotated token material and, optionally, the granted scopes. Everything else
// (owner, resource, issuer, generation, registration, redirect URL) stays the
// row's, so a refresh response can never change what the grant is.
//
// An empty RefreshToken keeps the stored one, and an empty TokenType keeps the
// stored one: an issuer that omits a replacement has not revoked the existing
// token, and dropping it would strand the grant.
type RefreshResult struct {
	AccessToken  string
	RefreshToken string
	TokenType    string
	Expiry       time.Time
	// Scopes replaces the granted scopes when non-nil.
	Scopes []string
}

// RefreshFunc performs one bounded upstream refresh for cur, which is the
// authoritative stored grant (client registration, endpoints, refresh token,
// expiry and scopes included). It is called outside every database transaction,
// after the intent to refresh has been committed, and at most once per intent
// version: another acquisition of the same grant refuses while it runs rather
// than spending the same single-use token again.
//
// ctx is the caller's and carries the caller's bound. A returned error leaves
// the intent committed, which makes the generation login-required; there is no
// automatic replay.
type RefreshFunc func(ctx context.Context, cur Grant) (RefreshResult, error)

// LoginFence is the persisted state a login is fenced to. BeginFence returns
// it with the endpoint generation; BindFence returns it again with the resource,
// exact issuer and grant generation that validated discovery identified, and
// that is the only fence CommitLogin accepts.
//
// OwnerLive records what BeginFence observed. It is informational: CommitLogin
// re-checks the live-owner predicate in its own transaction, because the
// authoritative check is never the one a caller holds.
type LoginFence struct {
	OwnerUserID        string
	Endpoint           string
	EndpointGeneration int64
	OwnerLive          bool
	// Resource and Issuer are empty until BindFence.
	Resource string
	Issuer   string
	// GrantGeneration is the grant generation captured by BindFence: 0 when no
	// grant row exists yet, otherwise the tombstoned or live row's generation.
	GrantGeneration int64
}

// Binding is an endpoint's live, validated link to a grant. Endpoint is the
// canonical MCP endpoint URL and Resource is the canonical resource it was
// validated for; they are frequently different strings and never derived from
// one another.
type Binding struct {
	OwnerUserID     string
	Endpoint        string
	Resource        string
	Issuer          string
	GrantGeneration int64
}

// Revocation is the detached payload a local invalidation hands back after it
// has committed, so the caller can best-effort revoke upstream. It is
// deliberately memory-only: it carries secrets and must never be logged,
// serialized or returned over RPC.
type Revocation struct {
	OwnerUserID  string
	Resource     string
	Issuer       string
	Registration Registration
	// RefreshToken and AccessToken are empty when the stored ciphertext could
	// not be opened; local invalidation still succeeded, only the revoke is
	// lost.
	RefreshToken string
	AccessToken  string
	// Generation is the incremented tombstone generation.
	Generation int64
}

// Store is the durable MCP OAuth credential store. Every method is safe for
// concurrent use and treats the database as the only authority.
//
// Every endpoint and resource argument is stored VERBATIM: the store never
// canonicalizes or rewrites one. Callers MUST canonicalize both first, using
// pkg/mcpoauth's canonicalizer, because exact binding is a byte comparison -- a
// differently-spelled URL is a different row with a different credential,
// which fails closed but silently makes a login invisible to its endpoint.
type Store interface {
	// BeginFence starts a login: it locks the owner, requires a live user and
	// returns the endpoint's current generation, creating the endpoint state
	// row (generation 1) when this is the endpoint's first login. Starting a
	// login discards any other pending fence on the same endpoint.
	BeginFence(ctx context.Context, owner, endpoint string) (LoginFence, error)

	// BindFence records the resource and exact issuer validated discovery
	// identified, together with the grant generation it observed, and returns
	// the fence CommitLogin will accept. It refuses (ErrStaleFence) when the
	// endpoint generation moved or no BeginFence created the row.
	BindFence(ctx context.Context, fence LoginFence, resource, issuer string) (LoginFence, error)

	// CommitLogin installs grant and binds fence.Endpoint to it, in one
	// transaction that also checks the owner is live. It requires the endpoint
	// generation and grant generation captured by the fence to still be
	// current, so it refuses (ErrStaleFence) after a logout or after another
	// login committed first; the loser must start a new login. A successful
	// commit advances the grant generation by one and returns the stored grant.
	CommitLogin(ctx context.Context, fence LoginFence, grant Grant) (Grant, error)

	// LookupBinding resolves one owner's exact canonical endpoint to its
	// validated resource and grant generation, with no discovery. It returns
	// ErrMissing when the endpoint has no live binding, ErrLoginRequired when
	// the owner is tombstoned or the bound grant is unusable, and ErrUnavailable
	// on a database failure.
	LookupBinding(ctx context.Context, owner, endpoint string) (Binding, error)

	// Acquire returns a usable grant for one exact endpoint binding, consulting
	// the live user, the binding and the grant on every call even when the
	// access token has not expired. expectedGeneration is the generation
	// LookupBinding (or a previous Acquire) returned; a moved binding or grant
	// is ErrStaleFence.
	//
	// When the access token has expired, Acquire commits a durable refresh
	// intent and then calls refresh outside the transaction, committing the
	// rotation before returning it. Another acquisition of a grant whose intent
	// is committed returns ErrLoginRequired rather than spending the single-use
	// token too; a refresh another session is completing right now, an intent
	// abandoned by a crash, and a failed refresh are all the SAME answer, on
	// purpose, and none of them is retryable from here: the design mandates one
	// upstream refresh per version with no automatic replay, and an explicit
	// login is the recovery. A refresh that fails, or whose rotation cannot be
	// committed, leaves the intent committed for that reason. A nil refresh for
	// an expired token is ErrLoginRequired.
	Acquire(ctx context.Context, owner, endpoint string, expectedGeneration int64, refresh RefreshFunc) (Grant, error)

	// InvalidateResource logs out one owner's canonical resource: it tombstones
	// the grant with an incremented generation, clears and advances the fence
	// of every endpoint bound or pending to that resource, and returns the
	// detached revocation payload. An endpoint bound to a DIFFERENT live
	// resource keeps that binding (its fence still advances if the pending half
	// was for this resource). An absent grant row is ErrMissing (after any
	// pending fence was advanced). It is idempotent: an already-invalidated
	// grant returns an empty Revocation and no error.
	InvalidateResource(ctx context.Context, owner, resource string) (Revocation, error)

	// InvalidateEndpoint logs out one owner's canonical endpoint, whether or
	// not it is bound. An unbound endpoint still advances its own fence so an
	// in-flight first login cannot survive it, and the call succeeds
	// idempotently when there is nothing to invalidate. A bound endpoint also
	// invalidates its grant and every other binding to that grant, returning
	// the detached revocation payload (empty when the grant was already
	// invalidated or absent).
	InvalidateEndpoint(ctx context.Context, owner, endpoint string) (Revocation, error)

	// InvalidateOwner invalidates every grant, binding and pending fence the
	// owner has, returning the revocations. It works for an already-tombstoned
	// user, because it is the cleanup path that must not require a live owner;
	// credential use, login and refresh separately refuse a tombstoned owner.
	InvalidateOwner(ctx context.Context, owner string) ([]Revocation, error)
}
