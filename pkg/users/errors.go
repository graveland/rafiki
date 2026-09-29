// SPDX-License-Identifier: Apache-2.0

package users

import "errors"

var (
	// ErrNotFound means the token or username does not name an ACTIVE user.
	// It is an answer: callers may return 401. Any other error from a Store
	// is not an answer and must not be reported as an auth failure.
	ErrNotFound = errors.New("users: no such user")

	// ErrUsernameTaken means an active user already holds the name. A
	// tombstoned user does not hold it.
	ErrUsernameTaken = errors.New("users: username already taken")

	// ErrInvalidEmail is returned by NormalizeEmail. A sentinel so a caller
	// can tell "you gave me a bad address" from "the store is unreachable".
	ErrInvalidEmail = errors.New("users: invalid email")

	// ErrEmailTaken means an active user already holds the address. A
	// tombstoned user does not: the partial index users_email_active covers
	// active rows only, which is what makes an address reusable after its
	// owner is deleted.
	ErrEmailTaken = errors.New("users: email already in use")
)
