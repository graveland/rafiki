// SPDX-License-Identifier: Apache-2.0

package users

import (
	"context"
	"errors"
)

// OIDCClaims are the verified claims of one login. Email is already
// normalized (NormalizeEmail) and domain-checked by the caller.
type OIDCClaims struct {
	Issuer, Subject, Email string
}

// OIDCResolver maps a verified OIDC login to a rafiki user, binding a
// (issuer, subject) identity to a user on first login. The Postgres
// implementation lives in pkg/usersdb.
type OIDCResolver interface {
	ResolveOIDC(ctx context.Context, c OIDCClaims) (User, error)
}

var (
	// ErrOIDCNoUser means the claims do not resolve to an active rafiki
	// user — either no user has a matching binding or email, or the user
	// that does is tombstoned. It is an answer: callers may refuse the
	// login. Any other error from a Store means "I could not check".
	ErrOIDCNoUser = errors.New("users: no active rafiki user for this identity")

	// ErrOIDCConflict means the claimed email is already bound to a
	// different subject at the same issuer. It is an answer, not an
	// outage: the login must be refused, not retried.
	ErrOIDCConflict = errors.New("users: email is bound to a different subject at this issuer")
)
