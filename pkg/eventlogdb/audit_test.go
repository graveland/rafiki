// SPDX-License-Identifier: Apache-2.0

package eventlogdb_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/multigres/testkit/assert"
)

// allowedProtoMarshalers is the allow-list of non-test files in cmd/ and pkg/
// permitted to marshal a proto directly. Marshalling a proto is how a proto
// gets PERSISTED, so a new site must be a deliberate decision — this test fails
// the day one appears that nobody added here.
//
// The design's audit named exactly three (pkg/eventlogdb/postgres.go,
// pkg/eventlog/memory.go, cmd/rafiki/cmd_conversations.go). The tree also holds
// a fourth, cmd/rafiki/protoout.go, the generic CLI emitter every Connect verb
// prints through — the same kind of transient CLI output as
// cmd_conversations.go, so it is allow-listed too rather than flagged. See the
// report for this deviation.
var allowedProtoMarshalers = map[string]bool{
	"pkg/eventlogdb/postgres.go":      true, // conversations.event_log.payload (JSONB)
	"pkg/eventlog/memory.go":          true, // the same payload, in-memory store
	"cmd/rafiki/cmd_conversations.go": true, // CLI output
	"cmd/rafiki/protoout.go":          true, // CLI output, the generic emitter
}

// TestPersistedProtoMarshalSitesAreAudited walks every non-test .go file under
// cmd/ and pkg/ (excluding generated code) and fails if any of them calls
// protojson.Marshal / proto.Marshal outside the allow-list.
func TestPersistedProtoMarshalSitesAreAudited(t *testing.T) {
	c := assert.NewAborting(t)
	root := repoRoot(t)

	var found []string
	for _, base := range []string{"cmd", "pkg"} {
		err := filepath.WalkDir(filepath.Join(root, base), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			rel, relErr := filepath.Rel(root, path)
			if relErr != nil {
				return relErr
			}
			rel = filepath.ToSlash(rel)
			if d.IsDir() {
				if excludedDir(rel) {
					return fs.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(rel, ".go") || strings.HasSuffix(rel, "_test.go") {
				return nil
			}
			if excludedDir(filepath.ToSlash(filepath.Dir(rel))) {
				return nil
			}
			if marshalsProto(path) {
				found = append(found, rel)
			}
			return nil
		})
		c.NoError(err, "walk %s", base)
	}

	// Guard against the walk silently finding nothing (a broken path, an
	// exclusion gone too wide): the two persisted stores must always be seen.
	for _, must := range []string{"pkg/eventlogdb/postgres.go", "pkg/eventlog/memory.go", "cmd/rafiki/protoout.go"} {
		c.True(containsString(found, must), "audit missed %s; the walk is broken", must)
	}

	for _, f := range found {
		c.True(allowedProtoMarshalers[f],
			"unexpected proto marshal site %s: a new persisted proto must be a deliberate decision (add it to allowedProtoMarshalers, or route it through pkg/eventlog's Record.Decode)", f)
	}
}

// excludedDir reports whether rel is generated code the audit does not read:
// pkg/gen, any pkg/*pb package, and the Python generator.
func excludedDir(rel string) bool {
	if rel == "pkg/gen" || strings.HasPrefix(rel, "pkg/gen/") {
		return true
	}
	if rel == "cmd/protoc-gen-rafikipy" || strings.HasPrefix(rel, "cmd/protoc-gen-rafikipy/") {
		return true
	}
	parts := strings.Split(rel, "/")
	return len(parts) >= 2 && parts[0] == "pkg" && strings.HasSuffix(parts[1], "pb")
}

// marshalsProto parses path and reports whether it calls protojson.Marshal or
// proto.Marshal. Parsing (not a text grep) means a mention in a comment or a
// string cannot trip the audit.
func marshalsProto(path string) bool {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		return false
	}
	hit := false
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		id, ok := sel.X.(*ast.Ident)
		if !ok {
			return true
		}
		if (id.Name == "protojson" || id.Name == "proto") && sel.Sel.Name == "Marshal" {
			hit = true
			return false
		}
		return true
	})
	return hit
}

func containsString(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

// repoRoot is the module root: two directories up from this test file
// (pkg/eventlogdb/audit_test.go).
func repoRoot(t *testing.T) string {
	t.Helper()
	_, here, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	return filepath.Join(filepath.Dir(here), "..", "..")
}
