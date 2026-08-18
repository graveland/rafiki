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
)
