// SPDX-License-Identifier: Apache-2.0

package skills_test

import (
	"os/exec"
	"strings"
	"testing"
)

// pkg/skills is importable by cmd/rafiki and pkg/executor, both of which must
// link zero pgx packages; the Postgres implementation belongs in pkg/skillsdb.
//
// This is a linker-level assertion, which is the only kind that cannot be
// satisfied by a convention nobody re-reads. It shells out to `go list` rather
// than importing anything, because a test that imported the offending package
// would itself be the violation. Mirrors TestClientDoesNotLinkPostgres in
// cmd/rafiki and TestExecutorPackagesDoNotLinkPostgres in pkg/executor.
func TestSkillsDoesNotLinkPostgres(t *testing.T) {
	out, err := exec.Command("go", "list", "-deps", "go.graveland.dev/rafiki/pkg/skills").Output()
	if err != nil {
		t.Skipf("go list unavailable: %v", err)
	}
	for _, dep := range strings.Split(string(out), "\n") {
		if strings.Contains(dep, "jackc/pgx") || strings.Contains(dep, "lib/pq") {
			t.Errorf("pkg/skills links %s. It is the skills domain package and must never open a "+
				"database; put the Postgres implementation in pkg/skillsdb. Find the path with: "+
				"go list -deps go.graveland.dev/rafiki/pkg/skills | grep -B5 pgx", dep)
		}
	}
}
