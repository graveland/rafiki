// SPDX-License-Identifier: Apache-2.0

package conv_test

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/multigres/testkit/assert"
)

// pkg/conv is storage-agnostic: it must never link pgx, or the client and the
// executor would inherit a Postgres driver through it. A missing go binary
// FAILS the guard rather than skipping it — a guard that silently skips is not
// a guard.
func TestConvIsPgxFree(t *testing.T) {
	c := assert.NewAborting(t)
	goBin, err := exec.LookPath("go")
	c.NoError(err, "go must be on PATH for this guard to mean anything")

	root, err := moduleRoot()
	c.NoError(err, "find the module root")

	cmd := exec.Command(goBin, "list", "-deps", "./pkg/conv")
	cmd.Dir = root
	out, err := cmd.Output()
	c.NoError(err, "go list -deps ./pkg/conv")

	for _, line := range strings.Split(string(out), "\n") {
		pkg := strings.TrimSpace(line)
		c.False(strings.HasPrefix(pkg, "github.com/jackc/pgx"), "pkg/conv must not link pgx, found %s", pkg)
	}
}

// moduleRoot walks up from the test's directory to the directory holding
// go.mod.
func moduleRoot() (string, error) {
	dir, err := filepath.Abs(".")
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", errors.New("go.mod not found above the test directory")
		}
		dir = parent
	}
}
