// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os/exec"
	"strings"
	"testing"
)

// The two-binary split is the whole point of this architecture and the easiest
// invariant for a future change to violate silently: Go imports whole packages,
// so one import of a package that happens to contain a pgxpool field is enough.
//
// This is a linker-level assertion, which is the only kind that cannot be
// satisfied by a convention nobody re-reads. It shells out to `go list` rather
// than importing anything, because a test that imported the offending package
// would itself be the violation.
func TestClientDoesNotLinkPostgres(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", "go.graveland.dev/rafiki/cmd/rafiki").Output()
	if err != nil {
		t.Skipf("go list unavailable: %v", err)
	}
	for _, dep := range strings.Split(string(out), "\n") {
		if strings.Contains(dep, "jackc/pgx") || strings.Contains(dep, "lib/pq") {
			t.Errorf("cmd/rafiki links %s. rafiki is a socket client and must never open a database; "+
				"find the path with: go mod why -m github.com/jackc/pgx/v5", dep)
		}
	}
}
