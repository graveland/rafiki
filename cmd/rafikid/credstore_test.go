// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/base64"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"go.graveland.dev/rafiki/pkg/secretbox"

	"github.com/multigres/testkit/assert"
)

// TestNewCredentialStoreUnsetsSecretsKey pins the least-privilege rule: once the
// box exists the key is held in memory only, and it must be gone from the
// process environment, because the environment is inherited by everything this
// daemon starts (a stdio MCP server, a child's shell, a hand-enrolled executor).
// A DSN plus that key opens every user's ChatGPT grant.
func TestNewCredentialStoreUnsetsSecretsKey(t *testing.T) {
	c := assert.NewAborting(t)
	key := base64.StdEncoding.EncodeToString(make([]byte, 32))
	t.Setenv(secretbox.EnvKey, key)

	// The store only holds the pool, so a zero one exercises the construction
	// without a database; nothing in this path dereferences it.
	store, why := newCredentialStore(&pgxpool.Pool{})

	c.Eq("", why, "a valid key must give no reason")
	c.True(store != nil, "the store must be built")
	c.Eq("", os.Getenv(secretbox.EnvKey), "the key must not stay in the process environment")
}

// TestNewCredentialStoreKeepsTheEnvironmentOnFailure is the other half: on the
// failure paths there is no box, and the variable is left exactly as it was for
// the next start (the operator's service file is the authority on it).
func TestNewCredentialStoreKeepsTheEnvironmentOnFailure(t *testing.T) {
	c := assert.NewAborting(t)

	t.Run("no database", func(t *testing.T) {
		key := base64.StdEncoding.EncodeToString(make([]byte, 32))
		t.Setenv(secretbox.EnvKey, key)
		store, why := newCredentialStore(nil)
		c.True(store == nil, "a nil pool gives no store")
		c.NotEq("", why, "a nil pool gives a reason")
		c.Eq(key, os.Getenv(secretbox.EnvKey), "a failure path leaves the environment alone")
	})

	t.Run("malformed key", func(t *testing.T) {
		t.Setenv(secretbox.EnvKey, "not-base64-of-32-bytes")
		store, why := newCredentialStore(&pgxpool.Pool{})
		c.True(store == nil, "a malformed key gives no store")
		c.NotEq("", why, "a malformed key gives a reason")
		c.Eq("not-base64-of-32-bytes", os.Getenv(secretbox.EnvKey),
			"a failure path leaves the environment alone")
	})
}
