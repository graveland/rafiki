// SPDX-License-Identifier: Apache-2.0

package users_test

import (
	"os/exec"
	"strings"
	"testing"
)

// pkg/users is the pgx-free half of the identity model, imported by cmd/rafiki
// (the client binary, which must never open a postgres connection — see
// TestClientDoesNotLinkPostgres) and by pkg/executor. All pgx lives in
// pkg/usersdb.
//
// This guard is currently vacuous in the sense that nothing imports pkg/users
// yet, so TestClientDoesNotLinkPostgres cannot fail from a regression here.
// It is attached to the PACKAGE rather than left to be caught at the client
// binary, mirroring pkg/executor/no_postgres_test.go: the point is that a
// later convenience constructor — "just have users.NewPostgresStore call into
// usersdb for you" — fails here, in pkg/users' own test, rather than at a
// distance once something finally imports this package into cmd/rafiki.
func TestUsersPackageDoesNotLinkPostgres(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", "go.graveland.dev/rafiki/pkg/users").Output()
	if err != nil {
		t.Skipf("go list unavailable: %v", err)
	}
	for _, dep := range strings.Split(string(out), "\n") {
		if strings.Contains(dep, "jackc/pgx") || strings.Contains(dep, "lib/pq") {
			t.Errorf("pkg/users links %s; it must stay importable by cmd/rafiki, which must never "+
				"open a database. Find the path with: go list -deps go.graveland.dev/rafiki/pkg/users | grep -B5 pgx", dep)
		}
	}
}
