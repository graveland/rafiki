// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/multigres/testkit/assert"

	"go.graveland.dev/rafiki/pkg/embedpg"
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

// envCount counts the entries of name in env, so a test can tell "the DSN's value
// is present" from "the inherited one is still there too".
func envCount(env []string, name string) int {
	prefix := name + "="
	n := 0
	for _, kv := range env {
		if _, found := strings.CutPrefix(kv, prefix); found {
			n++
		}
	}
	return n
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

// An inherited PGHOST would otherwise be what psql dials, an inherited PGDATABASE
// would name the wrong database, an inherited PGSERVICE could pull a whole
// connection out of a service file, and an inherited PGPASSFILE would supply a
// password the DSN never named.
func TestPsqlInvocationDropsInheritedPGVars(t *testing.T) {
	t.Setenv("PATH", fakePsqlDir(t))
	c := assert.NewAborting(t)

	inv, err := psqlInvocation("postgres://alice@real.example:6543/app", "postgres://alice@real.example:6543/app",
		nil, []string{"PGHOST=evil", "PGSERVICE=x", "PGDATABASE=wrong", "PGPASSFILE=/tmp/pgpass", "HOME=/home/alice"})
	c.NoError(err, "psqlInvocation returned error")

	got, ok := envValue(inv.env, "PGHOST")
	c.True(ok, "PGHOST must be set from the DSN, got: %v", inv.env)
	c.Eq("real.example", got, "PGHOST must be the DSN's host, got")
	c.Eq(1, envCount(inv.env, "PGHOST"), "the inherited PGHOST must be gone, not merely outranked, got: %v", inv.env)

	// The DSN names a dbname, so the planted one must have been dropped rather
	// than left beside the DSN's value.
	got, ok = envValue(inv.env, "PGDATABASE")
	c.True(ok, "PGDATABASE must be set from the DSN, got: %v", inv.env)
	c.Eq("app", got, "PGDATABASE must be the DSN's dbname, got")
	c.Eq(1, envCount(inv.env, "PGDATABASE"), "the inherited PGDATABASE must be gone, got: %v", inv.env)

	for _, name := range []string{"PGSERVICE", "PGPASSFILE"} {
		_, ok := envValue(inv.env, name)
		c.False(ok, "an inherited %s must be dropped, got: %v", name, inv.env)
	}
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

// The missing-client error must win over resolving the DSN: on RAFIKI_DB=embedded
// resolving downloads tens of megabytes and runs initdb, and a user with no psql
// should not sit through that to be told to install it.
func TestPsqlCmdChecksTheClientBeforeResolving(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	orig := newEmbeddedServer
	t.Cleanup(func() { newEmbeddedServer = orig })
	newEmbeddedServer = func() embedpg.Server {
		t.Fatal("the DSN was resolved before psql was looked up")
		return embedpg.Server{}
	}

	cmd := newPsqlCmd()
	cmd.SetArgs([]string{"--db", embeddedDSN})
	err := cmd.Execute()

	c := assert.NewAborting(t)
	c.Error(err, "want the missing-psql error, got nil")
	c.StrContains(err.Error(), "brew install libpq", "error must be the install remedy, got: %v", err)
}

// A parse error must never echo any part of a password. The two hard cases are
// a password whose value is cut short by an unquoted space (making the rest of
// it a bare token) and one followed immediately by a closing quote and more text
// (making the next byte look like a stray character) — both used to be echoed
// verbatim.
func TestParseConninfoErrorDoesNotEchoPassword(t *testing.T) {
	c := assert.NewAborting(t)

	cases := []struct {
		dsn    string
		forbid []string // substrings that must not appear in the error
	}{
		// `s3cret` becomes a bare token after the unquoted value `my`.
		{"password=my s3cret", []string{"s3cret", "my "}},
		// The byte after the closing quote is the password's continuation.
		{`password='abc'def`, []string{"abc", "def", `"d"`}},
		// Same shape, hitting the "not a keyword" branch instead.
		{"password=abc 1x=y", []string{"abc", "1x"}},
		// A value that is quoted and never closed.
		{"host=/tmp password='s3cret", []string{"s3cret"}},
		// A well-formed password followed by a bare token.
		{"password=s3cret dbname", []string{"s3cret"}},
		// A URL whose port is invalid: url.Parse's own error embeds the URL.
		{"postgres://alice:s3cret@db.example:notaport/app", []string{"s3cret"}},
	}
	for _, tc := range cases {
		_, err := parseConninfo(tc.dsn)
		c.Error(err, "want a parse error for a malformed DSN, got nil")
		for _, bad := range tc.forbid {
			c.NotStrContains(err.Error(), bad, "error must not echo %q, got: %v", bad, err)
		}
	}
}

// The errors still have to be usable: with the token's text withheld, the byte
// offset and the keyword it follows are what an operator has to go on.
func TestParseConninfoErrorLocatesTheToken(t *testing.T) {
	c := assert.NewAborting(t)

	_, err := parseConninfo("password=my s3cret")
	c.Error(err, "want a parse error, got nil")
	// "s3cret" starts at byte 12.
	c.StrContains(err.Error(), "offset 12", "error must locate the token, got: %v", err)
	c.StrContains(err.Error(), `after the keyword "password"`, "error must name the preceding keyword, got: %v", err)
}

// Keyword values are quoted with \' and \\ escapes, and an unquoted value takes
// the same escapes. The embedded DSN's host is a filesystem path, which is
// exactly where those characters turn up.
func TestParseConninfoKeywordEscapes(t *testing.T) {
	c := assert.NewAborting(t)

	cases := []struct{ dsn, key, want string }{
		{`host='/tmp/it\'s' dbname=x`, "host", `/tmp/it's`},
		{`password='a\\b'`, "password", `a\b`},
		{`host=/tmp/a\ b`, "host", "/tmp/a b"},
	}
	for _, tc := range cases {
		kw, err := parseConninfo(tc.dsn)
		c.NoError(err, "parseConninfo(%q) returned error", tc.dsn)
		c.Eq(tc.want, kw[tc.key], "%s from %q, got", tc.key, tc.dsn)
	}
}

// A percent-encoded URL password is decoded by net/url, and the decoded value is
// what psql must receive.
func TestParseConninfoURLPercentEncodedPassword(t *testing.T) {
	c := assert.NewAborting(t)

	const dsn = "postgres://alice:p%40ss%3Aword@db.example:6543/app"
	kw, err := parseConninfo(dsn)
	c.NoError(err, "parseConninfo returned error")
	c.Eq("p@ss:word", kw["password"], "password, got")
	c.Eq("alice", kw["user"], "user, got")
	c.Eq("db.example", kw["host"], "host, got")
	c.Eq("6543", kw["port"], "port, got")
	c.Eq("app", kw["dbname"], "dbname, got")

	t.Setenv("PATH", fakePsqlDir(t))
	inv, err := psqlInvocation(dsn, dsn, nil, nil)
	c.NoError(err, "psqlInvocation returned error")
	got, ok := envValue(inv.env, "PGPASSWORD")
	c.True(ok, "PGPASSWORD must be set, got: %v", inv.env)
	c.Eq("p@ss:word", got, "PGPASSWORD must carry the decoded password, got")
}

// A unix-socket URL names its directory in the query string, leaving the host
// empty. That is the shape `rafiki setup` would produce for a socket server.
func TestParseConninfoSocketURL(t *testing.T) {
	c := assert.NewAborting(t)

	kw, err := parseConninfo("postgres:///app?host=/tmp/x")
	c.NoError(err, "parseConninfo returned error")
	c.Eq("/tmp/x", kw["host"], "host, got")
	c.Eq("app", kw["dbname"], "dbname, got")
	_, ok := kw["port"]
	c.False(ok, "an absent port must not be invented, got: %v", kw)
}
