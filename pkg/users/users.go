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
)

// User is one identity row. DeletedAt non-nil means tombstoned: the token no
// longer authenticates, but history still resolves the username through it.
type User struct {
	ID        string     `json:"id"`
	Username  string     `json:"username"`
	CreatedAt time.Time  `json:"created_at"`
	DeletedAt *time.Time `json:"deleted_at,omitempty"`
}

// Identity is an authenticated caller. The zero value means "not a user":
// an anonymous request, a spawned child presenting the per-boot token, or a
// locally-trusted UDS connection.
type Identity struct {
	UserID   string `json:"user_id,omitempty"`
	Username string `json:"username,omitempty"`
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
	Create(ctx context.Context, username string) (User, string, error)
	Authenticate(ctx context.Context, token string) (Identity, error)
	List(ctx context.Context, includeDeleted bool, limit int) ([]User, error)
	Delete(ctx context.Context, username string) error
	CountActive(ctx context.Context) (int, error)
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

// NewToken mints a bearer token: 256 bits of randomness behind TokenPrefix.
// The entropy is what makes a fast digest sufficient — see HashToken.
func NewToken() (string, error) {
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
