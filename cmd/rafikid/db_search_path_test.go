// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/multigres/testkit/assert"
)

func TestProvisionEmbeddedDatabaseEnsuresRoleDefaults(t *testing.T) {
	for _, tc := range []struct {
		name       string
		catalog    fakeCatalog
		wantCreate bool
	}{
		{"fresh", fakeCatalog{}, true},
		{"existing", fakeCatalog{exists: true}, false},
		{"duplicate database race", fakeCatalog{execErrs: []error{&pgconn.PgError{Code: sqlstateDuplicateDatabase}, nil}}, true},
		{"catalog index race", fakeCatalog{execErrs: []error{&pgconn.PgError{Code: sqlstateUniqueViolation, ConstraintName: pgDatabaseNameIndex}, nil}}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := assert.NewAborting(t)
			c.NoError(provisionEmbeddedDatabase(context.Background(), &tc.catalog), "provision")
			want := []string{embeddedDatabaseDefaultsStatement()}
			if tc.wantCreate {
				want = append([]string{embeddedDatabaseStatement()}, want...)
			}
			c.EqDeep(want, tc.catalog.execs, "creation must precede defaults, including after a race")
		})
	}
}

func TestProvisionEmbeddedDatabasePropagatesFailures(t *testing.T) {
	fail := errors.New("permission denied")
	for _, tc := range []struct {
		name      string
		catalog   fakeCatalog
		wantExecs int
	}{
		{"existence check", fakeCatalog{existsErr: fail}, 0},
		{"create", fakeCatalog{execErr: fail}, 1},
		{"existing role defaults", fakeCatalog{exists: true, execErr: fail}, 1},
		{"fresh role defaults", fakeCatalog{execErrs: []error{nil, fail}}, 2},
		{"raced role defaults", fakeCatalog{execErrs: []error{&pgconn.PgError{Code: sqlstateDuplicateDatabase}, fail}}, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := provisionEmbeddedDatabase(context.Background(), &tc.catalog)
			if !errors.Is(err, fail) {
				t.Fatalf("error = %v, want original failure", err)
			}
			if len(tc.catalog.execs) != tc.wantExecs {
				t.Fatalf("executed %d statements, want %d", len(tc.catalog.execs), tc.wantExecs)
			}
		})
	}
}

// Only the provisioner's CREATE step can tolerate a duplicate-database error.
// The resolver must not swallow a later failure while configuring defaults.
func TestResolveDSNEmbeddedDoesNotSwallowDefaultsFailure(t *testing.T) {
	pointResolverAt(t, fakeEmbeddedServer(t))
	fail := &pgconn.PgError{Code: sqlstateDuplicateDatabase, Message: "defaults failed"}
	stubCreateDatabase(t, fail)
	dsn, err := resolveDSN(context.Background(), embeddedDSN)
	if dsn != "" || !errors.Is(err, fail) {
		t.Fatalf("resolveDSN = (%q, %v), want no DSN and the defaults failure", dsn, err)
	}
}

func TestPsqlSearchPathDefaultPreservesExplicitOptions(t *testing.T) {
	t.Setenv("PATH", fakePsqlDir(t))
	for _, tc := range []struct{ name, dsn, want string }{
		{"URL", "postgres://alice@host/app", "-c search_path=" + rafikiSearchPath},
		{"embedded keyword DSN", "host=/tmp/private port=5432 user=rafiki dbname=rafiki", "-c search_path=" + rafikiSearchPath},
		{"DSN options", `host=host dbname=app options='-c statement_timeout=500 -c search_path=custom'`, "-c search_path=" + rafikiSearchPath + " -c statement_timeout=500 -c search_path=custom"},
		{"URL options", "postgres://alice@host/app?options=-c%20search_path%3Dcustom", "-c search_path=" + rafikiSearchPath + " -c search_path=custom"},
		{"escaped options", `host=host options='-c application_name=two\\ words'`, "-c search_path=" + rafikiSearchPath + ` -c application_name=two\ words`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			inv, err := psqlInvocation(tc.dsn, tc.dsn, nil, []string{"PGOPTIONS=-c search_path=ambient", "HOME=/tmp"})
			if err != nil {
				t.Fatal(err)
			}
			got, _ := envValue(inv.env, "PGOPTIONS")
			if got != tc.want || envCount(inv.env, "PGOPTIONS") != 1 {
				t.Fatalf("PGOPTIONS = %q, want %q (exactly once)", got, tc.want)
			}
			for _, arg := range inv.argv {
				if strings.Contains(arg, "search_path") {
					t.Fatal("default must not alter user argv")
				}
			}
		})
	}
}

func TestOtherPgToolsDoNotGetPsqlSearchPath(t *testing.T) {
	for _, name := range []string{"pg_dump", "pg_restore"} {
		inv, err := pgToolInvocation("test", "/unused", name, "postgres://u@h/db", "postgres://u@h/db", nil, []string{"PGOPTIONS=-c search_path=ambient"})
		if err != nil {
			t.Fatal(err)
		}
		if _, ok := envValue(inv.env, "PGOPTIONS"); ok {
			t.Fatalf("%s inherited a psql-specific default", name)
		}
	}
}
