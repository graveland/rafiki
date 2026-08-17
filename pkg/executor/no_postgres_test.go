package executor_test

import (
	"os/exec"
	"strings"
	"testing"
)

// The executor runs a model's file and shell tools, on a machine that is
// frequently not the daemon's. It has no DSN, opens no database, and must not
// link one — and `pkg/execpool`, which the executor binary links for its
// reverse-dial transport, must not either.
//
// This is a linker-level assertion because the failure mode is transitive and
// invisible: Go imports whole packages, so one import of a package that happens
// to contain a pgxpool field is enough. That is exactly how it broke before —
// `ToolOpts` carried an unread `Pool *pgxpool.Pool`, and `pkg/fundi/tools`
// reached `pkg/tasks` (whose Postgres store then shared its package) and
// `pkg/agentloop` (for a context accessor and one constant, which dragged in
// `pkg/llm` and `pkg/store`). Eleven pgx packages, none of them reachable code.
//
// It is not a security boundary — dead code nothing calls is not attack surface,
// and there is no DSN on an executor host. It is about build coupling and
// binary weight: the executor is baked into every workspace container image, and
// a pgx bump should not rebuild it.
//
// Mirrors TestClientDoesNotLinkPostgres in cmd/rafiki. Note that once the
// executor folds into the `rafiki` binary, that test covers this ground too —
// this one keeps the invariant attached to the PACKAGES, so it survives the
// binaries being rearranged.
func TestExecutorPackagesDoNotLinkPostgres(t *testing.T) {
	for _, pkg := range []string{
		"go.graveland.dev/rafiki/pkg/executor",
		"go.graveland.dev/rafiki/pkg/execpool",
		"go.graveland.dev/rafiki/pkg/fundi/tools",
	} {
		out, err := exec.Command("go", "list", "-deps", pkg).Output()
		if err != nil {
			t.Skipf("go list unavailable: %v", err)
		}
		for _, dep := range strings.Split(string(out), "\n") {
			if strings.Contains(dep, "jackc/pgx") || strings.Contains(dep, "lib/pq") {
				t.Errorf("%s links %s; it runs tools and must never open a database. "+
					"Find the path with: go list -deps %s | grep -B5 pgx, or check for a "+
					"struct field of a pgx type on an otherwise DB-free type.", pkg, dep, pkg)
			}
		}
	}
}
