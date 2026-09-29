// SPDX-License-Identifier: Apache-2.0

// Package users is the pgx-free half of rafiki's identity model: the types,
// the token scheme, and the error sentinels. The Postgres implementation
// lives in pkg/usersdb.
//
// The split exists because cmd/rafiki (the client) and pkg/executor must link
// zero pgx packages — see TestClientDoesNotLinkPostgres. Anything here may be
// imported by either; anything in pkg/usersdb may not.
package users

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
)

// User is one identity row. DeletedAt non-nil means tombstoned: the token no
// longer authenticates, but history still resolves the username through it.
type User struct {
	ID        string     `json:"id"`
	Username  string     `json:"username"`
	IsAdmin   bool       `json:"is_admin"`
	CreatedAt time.Time  `json:"created_at"`
	DeletedAt *time.Time `json:"deleted_at,omitempty"`
	// Email is stored lowercase, and unique among ACTIVE users only — a
	// tombstoned user's address is reusable. Empty means none.
	Email string `json:"email,omitempty"`
}

// Identity is an authenticated caller. The zero value means "not a user":
// an anonymous request, a spawned child presenting the per-boot token, or a
// locally-trusted UDS connection.
type Identity struct {
	UserID   string `json:"user_id,omitempty"`
	Username string `json:"username,omitempty"`
	// IsAdmin is populated ONLY by Store.Authenticate, from the users row --
	// never a request field, never a claim. An admin sees every owner's
	// conversations; see pkg/insights.Scope.
	IsAdmin bool `json:"is_admin,omitempty"`
	// TokenID is the user_token row that authenticated, populated only by
	// Store.Authenticate. It lets callers attribute a session to a specific
	// credential and revoke it without disturbing the user's other tokens.
	TokenID string `json:"token_id,omitempty"`
}

// NewUser is the input to Store.Create. IsAdmin and MintToken are stated
// explicitly rather than defaulted: the admin bit is never inferred, and a
// create without a token is how an OIDC-provisioned user (which
// authenticates against its IdP, not a rafiki credential) arrives.
type NewUser struct {
	Username  string
	Email     string
	IsAdmin   bool
	MintToken bool
}

// TokenOrigin says what minted a credential: OriginService is a rafiki token
// handed to the user directly; OriginOIDC is minted on the user's behalf for
// a federated session. The set matches user_token's CHECK constraint.
type TokenOrigin string

const (
	OriginService TokenOrigin = "service"
	OriginOIDC    TokenOrigin = "oidc"
)

// NewToken is the input to Store.MintToken. TTL zero means the token never
// expires; a positive TTL sets expires_at = now() + TTL, exact to the
// microsecond. A negative TTL is refused.
type NewToken struct {
	Name   string
	Origin TokenOrigin
	TTL    time.Duration
}

// Token is one user credential. It is never the plaintext — MintToken
// returns the plaintext token alongside a Token exactly once, at mint time.
type Token struct {
	ID        string
	UserID    string
	Username  string
	Name      string
	Origin    TokenOrigin
	CreatedAt time.Time
	// ExpiresAt nil means never expires; RevokedAt nil means not revoked.
	// Revocation and expiry are permanent: neither is ever cleared.
	ExpiresAt *time.Time
	RevokedAt *time.Time
}

// IsUser reports whether the identity names a row in the users table. Only a
// user identity is persisted as owner_user_id / author_user_id.
func (i Identity) IsUser() bool { return i.UserID != "" }

// Store is the identity backend.
//
// Authenticate returns ErrNotFound for an unknown or tombstoned token — an
// ANSWER — and any other error for "I could not check", which callers must
// surface as 503/internal rather than 401. See the design doc: answering a
// database blip with 401 makes clients discard working credentials.
type Store interface {
	// Create mints a user and, when u.MintToken is set, its initial service
	// token — returned as plaintext exactly once, and never again. u.IsAdmin
	// is never inferred — every caller states it explicitly: the bootstrap
	// creator (the daemon's first operator) passes true, every ordinary
	// authenticated create passes false. Nothing else may set the bit.
	// u.Email is normalized first; an address an active user already holds is
	// ErrEmailTaken, a malformed one ErrInvalidEmail — both ANSWERS.
	Create(ctx context.Context, u NewUser) (User, string, error)
	Authenticate(ctx context.Context, token string) (Identity, error)
	List(ctx context.Context, includeDeleted bool, limit int) ([]User, error)
	Delete(ctx context.Context, username string) error
	CountActive(ctx context.Context) (int, error)
	// SetEmail normalizes email and stores it on the named ACTIVE user — ""
	// clears it. Returns the updated user. Unknown username is ErrNotFound;
	// an address an active user already holds is ErrEmailTaken — both
	// ANSWERS. Any other error is an outage and must not be reported as one.
	SetEmail(ctx context.Context, username, email string) (User, error)
	// MintToken adds a credential to an existing user and returns its
	// plaintext exactly once. A bad name or origin is a plain error — an
	// ANSWER; an unknown or tombstoned userID is ErrNotFound. Any other
	// error is an outage.
	MintToken(ctx context.Context, userID string, t NewToken) (Token, string, error)
	// ListTokens returns a user's credentials, newest first, joined with the
	// username. An empty userID means every user's. includeRevoked controls
	// whether tombstoned credentials appear — revocation is a filter, never
	// a deletion. A userID that names nobody is an empty list, not an error.
	ListTokens(ctx context.Context, userID string, includeRevoked bool) ([]Token, error)
	// GetToken resolves one credential by id. Unknown id is ErrNotFound —
	// an ANSWER; any other error is an outage.
	GetToken(ctx context.Context, id string) (Token, error)
	// RevokeToken tombstones one credential: it stops authenticating
	// immediately and never revives. Revoking an already-revoked token
	// returns the unchanged row and nil — idempotent by design. Unknown or
	// malformed id is ErrNotFound; any other error is an outage.
	RevokeToken(ctx context.Context, id string) (Token, error)
	// LookupUsername resolves an ACTIVE username to its id.
	//
	// Returns only the active row, deliberately: a username is unique only
	// among active users (partial index users_username_active), so one name
	// can own one active row plus any number of tombstones, and a lookup that
	// resolved to a tombstone would attribute work to a deleted account.
	// pkg/insights/search.go's ORDER BY created_at DESC LIMIT 1 is the right
	// behaviour for a human-typed FILTER (which deliberately matches
	// tombstones) and the wrong behaviour here — do not copy it.
	//
	// ErrNotFound means "no such active user" — an ANSWER. Any other error is
	// "I could not check", which a caller must never treat as a negative:
	// answering a database blip by unattributing a child is the same mistake
	// as answering it with 401 on the auth path.
	LookupUsername(ctx context.Context, username string) (string, error)
}

// MaxUsernameLen bounds a username. Generous rather than opinionated: the
// point is to stop absurd input reaching a TEXT column and an index, not to
// decide what a name may look like.
const MaxUsernameLen = 64

// ErrInvalidUsername is returned by NormalizeUsername. It is a sentinel so a
// caller can tell "you gave me a bad name" from "the store is unreachable" —
// the same distinction ErrNotFound draws on the auth path.
var ErrInvalidUsername = errors.New("users: invalid username")

// NormalizeUsername trims surrounding whitespace and validates what is left.
//
// Deliberately NO charset rule. A username here may reasonably be a handle, a
// dotted name, or an email address, and guessing a pattern now is how you end
// up migrating out of one later. What it does reject is the input that is
// certainly a mistake: empty or whitespace-only (which would otherwise create
// an invisible, unaddressable user) and anything past MaxUsernameLen.
//
// Enforced in the store rather than the CLI so every caller — including a
// future one that is not the CLI — gets it.
func NormalizeUsername(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", fmt.Errorf("%w: must not be empty or whitespace", ErrInvalidUsername)
	}
	if len(s) > MaxUsernameLen {
		return "", fmt.Errorf("%w: longer than %d bytes", ErrInvalidUsername, MaxUsernameLen)
	}
	return s, nil
}

// TokenPrefix marks a rafiki user token in logs, config files and secret
// scanners. It is part of the plaintext and therefore part of the digest.
const TokenPrefix = "rfk_"

// NewBearerToken mints a bearer token: 256 bits of randomness behind
// TokenPrefix. The entropy is what makes a fast digest sufficient — see
// HashToken. (Named for the wire form rather than NewToken because NewToken
// is the MintToken request type.)
func NewBearerToken() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return TokenPrefix + base64.RawURLEncoding.EncodeToString(b[:]), nil
}

// HashToken returns the stored form of a token: base64url(sha256(token)).
//
// Identical to executorsdb's hashToken, deliberately — one credential scheme
// in this codebase, not two. SHA-256 rather than a password hash because the
// input is NewToken's 256 random bits, against which a work factor buys
// nothing, and because a per-row salt would turn every authentication into a
// full-table scan.
func HashToken(token string) string {
	h := sha256.Sum256([]byte(token))
	return base64.RawURLEncoding.EncodeToString(h[:])
}

// maxEmailLen is RFC 5321's limit on the forward- or reverse-path — the
// practical ceiling an address must clear to be deliverable.
const maxEmailLen = 254

// NormalizeEmail trims surrounding whitespace and lowercases what is left.
// An empty (or whitespace-only) input normalizes to "" with no error — no
// address is a valid state, distinct from a malformed one.
//
// Deliberately NO deliverability check: this validates the shape an identity
// column and an index can hold, not whether mail would arrive. What it
// rejects is the input that is certainly a mistake: more than one @, an
// empty local or domain part, whitespace anywhere ("a b@c" is two tokens,
// not one address), and anything past 254 bytes.
//
// Enforced in the store rather than the CLI so every caller gets it, the
// same way NormalizeUsername is.
func NormalizeEmail(s string) (string, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" {
		return "", nil
	}
	if len(s) > maxEmailLen {
		return "", fmt.Errorf("%w: longer than %d bytes", ErrInvalidEmail, maxEmailLen)
	}
	if strings.IndexFunc(s, unicode.IsSpace) >= 0 {
		return "", fmt.Errorf("%w: must not contain whitespace", ErrInvalidEmail)
	}
	user, domain, ok := strings.Cut(s, "@")
	if !ok {
		return "", fmt.Errorf("%w: must contain an @", ErrInvalidEmail)
	}
	if strings.Contains(domain, "@") {
		return "", fmt.Errorf("%w: must contain exactly one @", ErrInvalidEmail)
	}
	if user == "" || domain == "" {
		return "", fmt.Errorf("%w: empty local or domain part", ErrInvalidEmail)
	}
	return s, nil
}
