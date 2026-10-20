// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os/exec"
	"strings"
	"testing"
)

// rafiki-hooks is the one rafiki binary meant to face the internet, so its
// dependency closure is a security boundary rather than a build tidiness
// question: a single import of a package that happens to carry a pgxpool field
// would put a database DSN in the image behind the webhook listener.
//
// These are linker-level assertions, which are the only kind that cannot be
// satisfied by a convention nobody re-reads. They shell out to `go list` rather
// than importing anything, because a test that imported the offending package
// would itself be the violation.
func TestHooksDoesNotLinkPostgres(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", "go.graveland.dev/rafiki/cmd/rafiki-hooks").Output()
	if err != nil {
		// FAIL, do not skip: this test is the ENFORCEMENT of "cmd/rafiki-hooks
		// links zero pgx packages", and a guard that silently skips on a
		// missing toolchain is not a guard. The repo cannot be built without
		// a Go toolchain, so `go` being absent is not a legitimate
		// environment for this suite.
		t.Fatalf("go list -deps go.graveland.dev/rafiki/cmd/rafiki-hooks could not run; "+
			"the pgx-link guard cannot be evaluated without the Go toolchain on PATH: %v", err)
	}
	for _, dep := range strings.Split(string(out), "\n") {
		if strings.Contains(dep, "jackc/pgx") || strings.Contains(dep, "lib/pq") {
			t.Errorf("cmd/rafiki-hooks links %s. The webhook gateway talks to the daemon over the "+
				"Control plane and never opens a database; find the path with: "+
				"go mod why -m github.com/jackc/pgx/v5", dep)
		}
	}
}

// daemonOnlyPackages are the importable packages that carry the daemon's own
// machinery — executors, capture, the inbox, and every database handle. None of
// them belongs in a process reachable from outside.
//
// The list is by PACKAGE, not by binary: cmd/rafikid is `package main` and can
// never be a dependency of anything, so asserting on it (as this test once did)
// is an assertion that cannot fail. Only importable packages can be linked.
var daemonOnlyPackages = []string{
	"go.graveland.dev/rafiki/pkg/inbox",
	"go.graveland.dev/rafiki/pkg/inboxdb",
	"go.graveland.dev/rafiki/pkg/childstoredb",
	"go.graveland.dev/rafiki/pkg/usersdb",
	"go.graveland.dev/rafiki/pkg/recalldb",
	"go.graveland.dev/rafiki/pkg/store",
	"go.graveland.dev/rafiki/pkg/execpool",
	"go.graveland.dev/rafiki/pkg/server",
	"go.graveland.dev/rafiki/pkg/rawtrace",
}

// TestHooksDoesNotLinkDaemon pins the other half: the gateway links no daemon
// code. It is a client of one RPC, and the daemon's own tree carries executors,
// capture and every database handle — none of which belongs in a process
// reachable from outside.
func TestHooksDoesNotLinkDaemon(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", "go.graveland.dev/rafiki/cmd/rafiki-hooks").Output()
	if err != nil {
		// FAIL, do not skip, for the reason above.
		t.Fatalf("go list -deps go.graveland.dev/rafiki/cmd/rafiki-hooks could not run; "+
			"the daemon-link guard cannot be evaluated without the Go toolchain on PATH: %v", err)
	}
	deps := make(map[string]bool)
	for _, dep := range strings.Split(string(out), "\n") {
		deps[dep] = true
	}

	// A guard that has drifted out of date looks exactly like a passing one, so
	// fail rather than pass when the list names a package this module does not
	// have: a renamed package must not silently leave nothing to check.
	if err := exec.Command("go", append([]string{"list"}, daemonOnlyPackages...)...).Run(); err != nil {
		t.Errorf("daemonOnlyPackages names a package this module does not have; "+
			"update the list rather than letting the guard check a package that no longer exists: %v", err)
	}

	for _, banned := range daemonOnlyPackages {
		if deps[banned] {
			t.Errorf("cmd/rafiki-hooks links %s. The gateway is a Connect client of rafikid and must "+
				"not import daemon code; move what both need into a package under pkg/ (pkg/notify is "+
				"the one such share)", banned)
		}
	}
}
