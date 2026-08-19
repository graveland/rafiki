package lspadapter_test

import (
	"os/exec"
	"strings"
	"testing"
)

// pkg/executor builds an adapter from this package and is required to link no
// pgx driver at all (pkg/executor/no_postgres_test.go). This package sits on
// that import path, so the invariant has to be attached here too — otherwise
// the first import of a store helper into the adapter would be caught only by
// a test in a package three hops away, if at all.
func TestAdapterDoesNotLinkPostgres(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", "go.graveland.dev/rafiki/pkg/fundi/lspadapter").Output()
	if err != nil {
		t.Skipf("go list unavailable: %v", err)
	}
	for _, dep := range strings.Split(string(out), "\n") {
		if strings.Contains(dep, "jackc/pgx") || strings.Contains(dep, "lib/pq") {
			t.Errorf("pkg/fundi/lspadapter links %s; the executor imports this package and must link no database driver", dep)
		}
	}
}
