// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/multigres/testkit/assert"
)

// fakePsqlDir makes a directory holding an executable named psql and returns
// it, so exec.LookPath finds a client without one being installed.
func fakePsqlDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "psql")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatalf("write fake psql: %v", err)
	}
	return dir
}

// envValue reports the last value of name in env.
func envValue(env []string, name string) (string, bool) {
	prefix := name + "="
	val, ok := "", false
	for _, kv := range env {
		if rest, found := strings.CutPrefix(kv, prefix); found {
			val, ok = rest, true
		}
	}
	return val, ok
}

// The DSN reaches psql through the environment only: argv is world-readable in
// the process table, and this DSN holds a password.
func TestPsqlInvocationPutsConnectionInEnvNotArgv(t *testing.T) {
	t.Setenv("PATH", fakePsqlDir(t))
	c := assert.NewAborting(t)

	inv, err := psqlInvocation("postgres://alice:s3cret@db.example:6543/app?sslmode=require",
		"postgres://alice:s3cret@db.example:6543/app?sslmode=require",
		[]string{"-c", "select 1"}, []string{"HOME=/home/alice"})
	c.NoError(err, "psqlInvocation returned error")

	c.EqDeep([]string{"psql", "-c", "select 1"}, inv.argv, "argv must carry no DSN part, got")
	for _, a := range inv.argv {
		c.NotStrContains(a, "s3cret", "argv element must not hold the password, got: %v", a)
		c.NotStrContains(a, "db.example", "argv element must not hold the host, got: %v", a)
	}

	for _, want := range [][2]string{
		{"PGHOST", "db.example"},
		{"PGPORT", "6543"},
		{"PGUSER", "alice"},
		{"PGPASSWORD", "s3cret"},
		{"PGDATABASE", "app"},
		{"PGSSLMODE", "require"},
	} {
		got, ok := envValue(inv.env, want[0])
		c.True(ok, "env must set %s, got: %v", want[0], inv.env)
		c.Eq(want[1], got, "%s value, got", want[0])
	}
	c.NotStrContains(inv.target, "s3cret", "target must not hold the password, got: %v", inv.target)
}

// An inherited PGHOST would otherwise be what psql dials, and an inherited
// PGSERVICE could pull a whole connection out of a service file.
func TestPsqlInvocationDropsInheritedPGVars(t *testing.T) {
	t.Setenv("PATH", fakePsqlDir(t))
	c := assert.NewAborting(t)

	inv, err := psqlInvocation("postgres://alice@real.example:6543/app", "postgres://alice@real.example:6543/app",
		nil, []string{"PGHOST=evil", "PGSERVICE=x", "PGDATABASE=wrong", "HOME=/home/alice"})
	c.NoError(err, "psqlInvocation returned error")

	got, ok := envValue(inv.env, "PGHOST")
	c.True(ok, "PGHOST must be set from the DSN, got: %v", inv.env)
	c.Eq("real.example", got, "PGHOST must be the DSN's host, got")

	_, ok = envValue(inv.env, "PGSERVICE")
	c.False(ok, "an inherited PGSERVICE must be dropped, got: %v", inv.env)
	_, ok = envValue(inv.env, "HOME")
	c.True(ok, "non-PG variables must survive, got: %v", inv.env)
}

// The embedded server's DSN is libpq keyword syntax whose host is a data
// directory — a path with a space in it, which is why the quoting matters.
func TestPsqlInvocationKeywordDSN(t *testing.T) {
	t.Setenv("PATH", fakePsqlDir(t))
	c := assert.NewAborting(t)

	dsn := "host='/tmp/a b' port=5432 user=rafiki dbname=postgres sslmode=disable"
	inv, err := psqlInvocation("embedded", dsn, nil, nil)
	c.NoError(err, "psqlInvocation returned error")

	got, ok := envValue(inv.env, "PGHOST")
	c.True(ok, "PGHOST must be set, got: %v", inv.env)
	c.Eq("/tmp/a b", got, "PGHOST must be the socket directory, got")
	c.Eq("rafikid psql: embedded database at /tmp/a b", inv.target, "target, got")
}

func TestPsqlInvocationRejectsUnknownParam(t *testing.T) {
	t.Setenv("PATH", fakePsqlDir(t))
	c := assert.NewAborting(t)

	_, err := psqlInvocation("postgres://u@h/db?foo=1", "postgres://u@h/db?foo=1", nil, nil)
	c.Error(err, "want an error for an unmappable keyword, got nil")
	c.StrContains(err.Error(), `cannot pass DSN parameter "foo"`, "error must name the keyword, got: %v", err)

	// pgx pool settings are not libpq parameters; a DSN carrying them still
	// reaches psql, they are just skipped.
	_, err = psqlInvocation("postgres://u@h/db?pool_max_conns=4", "postgres://u@h/db?pool_max_conns=4", nil, nil)
	c.NoError(err, "pool_ settings must be skipped, got error")
}

func TestPsqlInvocationMissingPsql(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	c := assert.NewAborting(t)

	_, err := psqlInvocation("postgres://u@h/db", "postgres://u@h/db", nil, nil)
	c.Error(err, "want an error when psql is not on PATH, got nil")
	c.StrContains(err.Error(), "brew install libpq", "error must name the install remedy, got: %v", err)
}

// A parse error must never echo the DSN: it is the one place a password is
// guaranteed to be present in a form nobody meant to publish.
func TestParseConninfoErrorDoesNotEchoPassword(t *testing.T) {
	c := assert.NewAborting(t)

	for _, dsn := range []string{
		"host=/tmp password='s3cret",
		"password=s3cret dbname",
		"postgres://alice:s3cret@db.example:notaport/app",
	} {
		_, err := parseConninfo(dsn)
		c.Error(err, "want a parse error for %q, got nil", "the malformed DSN")
		c.NotStrContains(err.Error(), "s3cret", "error must not echo the password, got: %v", err)
	}
}
