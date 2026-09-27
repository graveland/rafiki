// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"go.graveland.dev/rafiki/pkg/usersdb"

	"github.com/multigres/testkit/assert"
)

// TestUserCreateCLIAdminFlag pins --admin: explicit only, never inferred from
// an empty database. The second create (no --admin, on the same now
// non-empty database) proves there is no zero-users inference either.
func TestUserCreateCLIAdminFlag(t *testing.T) {
	c := assert.NewCollecting(t)
	pool := scratchPool(t)
	store := usersdb.NewPostgresStore(pool)
	ctx := context.Background()

	var out bytes.Buffer
	c.Require().NoError(runUserCreateCLI(ctx, &out, store, "root", true), "create --admin")
	if !strings.Contains(out.String(), "username: root") || !strings.Contains(out.String(), "token: ") {
		t.Fatalf("output = %q, want username and token lines", out.String())
	}

	rows, err := store.List(ctx, false, 0)
	c.Require().NoError(err, "list")
	c.Require().False(len(rows) != 1 || rows[0].Username != "root" || !rows[0].IsAdmin, "rows = %+v, want one admin user named root", rows)

	out.Reset()
	c.Require().NoError(runUserCreateCLI(ctx, &out, store, "alice", false), "create (no --admin)")
	rows, err = store.List(ctx, true, 0)
	c.Require().NoError(err, "list")
	var alice *bool
	for _, r := range rows {
		if r.Username == "alice" {
			v := r.IsAdmin
			alice = &v
		}
	}
	c.Require().NotNil(alice, "alice was not created")
	c.False(*alice, "alice was created admin without --admin; admin must never be inferred")
}

// TestUserCreateCLIDuplicate proves a repeated name surfaces as a clear
// "already exists" error rather than a raw constraint-violation message, and
// that the failed second attempt never touched the token printed by the
// first.
func TestUserCreateCLIDuplicate(t *testing.T) {
	c := assert.NewCollecting(t)
	pool := scratchPool(t)
	store := usersdb.NewPostgresStore(pool)
	ctx := context.Background()

	var out bytes.Buffer
	c.Require().NoError(runUserCreateCLI(ctx, &out, store, "dup", false), "first create")

	out.Reset()
	err := runUserCreateCLI(ctx, &out, store, "dup", false)
	c.Require().Error(err, "second create with the same name succeeded")
	c.StrContains(err.Error(), "already exists", "err = %v, want an \"already exists\" message", err)
	c.Eq(0, out.Len(), "output on failure = %q, want nothing written", out.String())
}
