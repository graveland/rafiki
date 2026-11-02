// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/multigres/testkit/assert"
)

// The reconcile is one statement: atomic, and its advisory lock is
// transaction-scoped. It names every managed extension, takes a lock of its own,
// and carries no version — the versions are the server's, so a new tree is what
// makes an installed extension out of date.
func TestEmbeddedExtensionsStatement(t *testing.T) {
	stmt := embeddedExtensionsStatement()
	c := assert.NewCollecting(t)

	for _, name := range embeddedExtensions {
		c.StrContains(stmt, "'"+name+"'", "the statement must name %s", name)
	}
	c.StrContains(stmt, "pg_advisory_xact_lock(7261666970)", "its own transaction-scoped lock")
	c.StrContains(stmt, "CREATE EXTENSION", "installs what is missing")
	c.StrContains(stmt, "ALTER EXTENSION %I UPDATE", "updates what is out of date")
	c.StrContains(stmt, "pg_available_extensions", "versions come from the server, not from Go")
	c.StrContains(stmt, "IF NOT FOUND THEN", "an extension the tree does not ship is skipped, not an error")
	c.False(strings.Contains(stmt, "VERSION"), "no version may be pinned in the statement")
	c.NotEq(embeddedExtensionsLockKey, 7261666969, "the role-defaults lock is a different one")
}

func TestReconcileExtensionsRunsTheStatementOnce(t *testing.T) {
	c := assert.NewAborting(t)
	cat := &fakeCatalog{}

	c.NoError(reconcileExtensions(context.Background(), cat), "reconcile")
	c.EqDeep([]string{embeddedExtensionsStatement()}, cat.execs, "exactly the reconcile statement")
	c.Eq(0, len(cat.queries), "nothing is read back: the statement decides inside the server")
}

func TestReconcileExtensionsWrapsAFailure(t *testing.T) {
	c := assert.NewAborting(t)
	cat := &fakeCatalog{execErr: errors.New(`extension "vector" has no update path`)}

	err := reconcileExtensions(context.Background(), cat)

	c.Error(err, "a failed install or update must stop the start")
	c.ErrorContains(err, "installing or updating the embedded server's extensions")
	c.ErrorContains(err, `extension "vector"`, "the server's own text names the extension")
}
