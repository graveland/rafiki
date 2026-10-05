// SPDX-License-Identifier: Apache-2.0

package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/multigres/testkit/assert"
)

// TestProductionKillGracePeriods pins the exact spans the four production
// Controller.Kill call sites intend: the respawn path, the claude abort path,
// the lease-loss stop and failChild's force-kill. Controller.Kill takes a
// time.Duration, so these were once bare millisecond literals that silently
// became nanoseconds; the named constants make the intent explicit and this
// test keeps their values from drifting.
func TestProductionKillGracePeriods(t *testing.T) {
	c := assert.NewAborting(t)
	for _, tc := range []struct {
		name string
		got  time.Duration
		want time.Duration
	}{
		{"respawn shutdown", respawnShutdownGrace, 3 * time.Second},
		{"respawn kill", respawnKillGrace, 500 * time.Millisecond},
		{"abort shutdown", abortShutdownGrace, time.Second},
		{"abort kill", abortKillGrace, 500 * time.Millisecond},
		{"lease-loss shutdown", leaseLossShutdownGrace, 5 * time.Second},
		{"lease-loss kill", leaseLossKillGrace, 5 * time.Second},
		{"failChild shutdown", failChildShutdownGrace, 5 * time.Second},
		{"failChild kill", failChildKillGrace, time.Second},
	} {
		c.Eq(tc.want, tc.got, "%s grace", tc.name)
	}
}

// TestNoBareNumericDurationsInKillCalls is the general guard for the wave's
// signature change: Controller.Kill and Controller.CloseAllExited take
// time.Duration, so a bare integer literal argument (3000, 500, 5_000…) means
// NANOSECONDS, not the milliseconds the author meant. This parses every Go file
// in the package (production and tests) and fails on any such literal passed to
// a Kill/CloseAllExited call, so the regression the reviewer caught cannot
// creep back in. Zero is allowed (0 is 0 in every unit); syscall.Kill is not a
// Controller call and is skipped.
func TestNoBareNumericDurationsInKillCalls(t *testing.T) {
	c := assert.NewAborting(t)
	entries, err := os.ReadDir(".")
	c.NoError(err, "ReadDir")

	fset := token.NewFileSet()
	var bad []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") {
			continue
		}
		f, perr := parser.ParseFile(fset, name, nil, 0)
		c.NoError(perr, "parse %s", name)
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if sel.Sel.Name != "Kill" && sel.Sel.Name != "CloseAllExited" {
				return true
			}
			if id, ok := sel.X.(*ast.Ident); ok && id.Name == "syscall" {
				return true
			}
			for _, a := range call.Args {
				lit, ok := a.(*ast.BasicLit)
				if !ok || lit.Kind != token.INT {
					continue
				}
				v, _ := strconv.Atoi(strings.ReplaceAll(lit.Value, "_", ""))
				if v != 0 {
					bad = append(bad, fset.Position(lit.Pos()).String()+": "+lit.Value)
				}
			}
			return true
		})
	}
	c.Empty(bad, "bare numeric duration argument(s) passed to Kill/CloseAllExited; "+
		"write 3*time.Second, not 3000: %v", bad)
}
