// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"go.graveland.dev/rafiki/pkg/usersdb"
)

// TestUserCreateCLIAdminFlag pins --admin: explicit only, never inferred from
// an empty database. The second create (no --admin, on the same now
// non-empty database) proves there is no zero-users inference either.
func TestUserCreateCLIAdminFlag(t *testing.T) {
	pool := scratchPool(t)
	store := usersdb.NewPostgresStore(pool)
	ctx := context.Background()

	var out bytes.Buffer
	if err := runUserCreateCLI(ctx, &out, store, "root", true); err != nil {
		t.Fatalf("create --admin: %v", err)
	}
	if !strings.Contains(out.String(), "username: root") || !strings.Contains(out.String(), "token: ") {
		t.Fatalf("output = %q, want username and token lines", out.String())
	}

	rows, err := store.List(ctx, false, 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 1 || rows[0].Username != "root" || !rows[0].IsAdmin {
		t.Fatalf("rows = %+v, want one admin user named root", rows)
	}

	out.Reset()
	if err := runUserCreateCLI(ctx, &out, store, "alice", false); err != nil {
		t.Fatalf("create (no --admin): %v", err)
	}
	rows, err = store.List(ctx, true, 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	var alice *bool
	for _, r := range rows {
		if r.Username == "alice" {
			v := r.IsAdmin
			alice = &v
		}
	}
	if alice == nil {
		t.Fatal("alice was not created")
	}
	if *alice {
		t.Error("alice was created admin without --admin; admin must never be inferred")
	}
}

// TestUserCreateCLIDuplicate proves a repeated name surfaces as a clear
// "already exists" error rather than a raw constraint-violation message, and
// that the failed second attempt never touched the token printed by the
// first.
func TestUserCreateCLIDuplicate(t *testing.T) {
	pool := scratchPool(t)
	store := usersdb.NewPostgresStore(pool)
	ctx := context.Background()

	var out bytes.Buffer
	if err := runUserCreateCLI(ctx, &out, store, "dup", false); err != nil {
		t.Fatalf("first create: %v", err)
	}

	out.Reset()
	err := runUserCreateCLI(ctx, &out, store, "dup", false)
	if err == nil {
		t.Fatal("second create with the same name succeeded")
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Errorf("err = %v, want an \"already exists\" message", err)
	}
	if out.Len() != 0 {
		t.Errorf("output on failure = %q, want nothing written", out.String())
	}
}
