package embedpg

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"

	"github.com/multigres/testkit/assert"
)

// TestEverySubprocessIsBuiltByCommand is a source ratchet in the spirit of the
// SDK-import ratchet: the minimal environment is a property of this package only
// while every subprocess goes through command(). A raw exec.Command in a new
// helper compiles, runs and works — it just hands the postmaster back the
// daemon's whole environment — so nothing but reading the source notices.
//
// The walk skips test files (a test may legitimately build a throwaway process)
// and the one function allowed to call exec.
func TestEverySubprocessIsBuiltByCommand(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}

	c := assert.NewCollecting(t)
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("ParseFile(%s): %v", name, err)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || (fn.Recv == nil && fn.Name.Name == "command") {
				continue
			}
			ast.Inspect(fn, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				pkg, ok := sel.X.(*ast.Ident)
				if !ok || pkg.Name != "exec" {
					return true
				}
				if sel.Sel.Name != "Command" && sel.Sel.Name != "CommandContext" {
					return true
				}
				c.Fail("exec.%s at %s:%d bypasses command(); build the subprocess there so it gets the minimal environment",
					sel.Sel.Name, name, fset.Position(call.Pos()).Line)
				return true
			})
		}
	}
}
