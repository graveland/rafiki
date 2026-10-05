// SPDX-License-Identifier: Apache-2.0

package eventlogdb_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/multigres/testkit/assert"
)

// allowedProtoMarshalers is the allow-list of non-test files in cmd/ and pkg/
// permitted to marshal a proto. Marshalling a proto is how a proto gets
// PERSISTED (or, for the CLI emitters, written as output), so a new site must be
// a deliberate decision. This is the MINIMAL exact set: the audit fails both on
// a site that is not listed and on a listed file that no longer matches.
//
// The design's grep is the PREFIX form (protojson.Marshal / proto.Marshal), so
// it catches both protojson.Marshal(x) and (protojson.MarshalOptions{...}).Marshal(x).
// The plan narrowed it to a literal "protojson.Marshal(" by mistake, which would
// have missed cmd_conversations.go and let a new file persisting a proto through
// MarshalOptions slip past. The detector below uses the prefix form.
var allowedProtoMarshalers = map[string]bool{
	"pkg/eventlogdb/postgres.go":      true, // persisted: conversations.event_log.payload (JSONB)
	"pkg/eventlog/memory.go":          true, // persisted: the same payload, in-memory store
	"cmd/rafiki/cmd_conversations.go": true, // CLI output
	"cmd/rafiki/protoout.go":          true, // CLI output, the generic emitter
}

// TestPersistedProtoMarshalSitesAreAudited walks every non-test .go file under
// cmd/ and pkg/ (excluding generated code) and fails if the set of files that
// marshal a proto is not EXACTLY allowedProtoMarshalers.
func TestPersistedProtoMarshalSitesAreAudited(t *testing.T) {
	found := collectMarshalSites(t)

	// Guard against the walk silently finding nothing (a broken path, an
	// exclusion gone too wide).
	c := assert.NewAborting(t)
	for _, must := range []string{"pkg/eventlogdb/postgres.go", "pkg/eventlog/memory.go", "cmd/rafiki/protoout.go", "cmd/rafiki/cmd_conversations.go"} {
		c.True(found[must], "audit missed %s; the walk is broken", must)
	}

	for _, f := range auditFailures(found) {
		t.Errorf("%s", f)
	}
}

// TestAuditFlagsAnUnlistedMarshalSite pins that a NEW file marshalling a proto —
// including via the MarshalOptions form a literal "protojson.Marshal(" scan
// would miss — fails the audit.
func TestAuditFlagsAnUnlistedMarshalSite(t *testing.T) {
	c := assert.NewCollecting(t)
	found := map[string]bool{}
	for f := range allowedProtoMarshalers {
		found[f] = true
	}
	found["pkg/newwidget/store.go"] = true

	failures := auditFailures(found)
	c.Eq(1, len(failures), "failures = %v, want exactly one", failures)
	c.StrContains(failures[0], "pkg/newwidget/store.go", "failure")
}

// TestAuditFlagsAStaleAllowListEntry pins that the allow-list stays minimal: a
// listed file that no longer marshals a proto is a failure, not a silent pass.
func TestAuditFlagsAStaleAllowListEntry(t *testing.T) {
	c := assert.NewCollecting(t)
	found := map[string]bool{}
	for f := range allowedProtoMarshalers {
		found[f] = true
	}
	delete(found, "cmd/rafiki/protoout.go")

	failures := auditFailures(found)
	c.Eq(1, len(failures), "failures = %v, want exactly one", failures)
	c.StrContains(failures[0], "cmd/rafiki/protoout.go", "failure")
}

// TestMarshalDetectorCatchesEveryMarshalForm pins the detector against every
// form of the prefix (protojson.Marshal / proto.Marshal), the MarshalOptions
// form, and the two decoys (a comment, and Unmarshal).
func TestMarshalDetectorCatchesEveryMarshalForm(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want bool
	}{
		{"protojson.Marshal", "package p\nfunc f(m proto.Message) { _, _ = protojson.Marshal(m) }\n", true},
		{"protojson.MarshalOptions", "package p\nfunc f(m proto.Message) { _, _ = (protojson.MarshalOptions{Multiline: true}).Marshal(m) }\n", true},
		{"proto.Marshal", "package p\nfunc f(m proto.Message) { _, _ = proto.Marshal(m) }\n", true},
		{"comment only", "package p\n// protojson.Marshal(m)\nvar _ = \"protojson.Marshal\"\n", false},
		{"protojson.Unmarshal", "package p\nfunc f(b []byte, m proto.Message) { _ = protojson.Unmarshal(b, m) }\n", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "x.go")
			if err := os.WriteFile(path, []byte(tc.src), 0o600); err != nil {
				t.Fatalf("write fixture: %v", err)
			}
			assert.NewCollecting(t).Eq(tc.want, marshalsProto(path), "detector for")
		})
	}
}

// collectMarshalSites walks cmd/ and pkg/ and returns the set of non-test .go
// files (generated code excluded) that marshal a proto.
func collectMarshalSites(t *testing.T) map[string]bool {
	t.Helper()
	root := repoRoot(t)
	found := map[string]bool{}
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
				found[rel] = true
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", base, err)
		}
	}
	return found
}

// auditFailures returns the failures for a set of found sites against the
// allow-list: an unlisted site, or a listed file that no longer matches.
func auditFailures(found map[string]bool) []string {
	var out []string
	for f := range found {
		if !allowedProtoMarshalers[f] {
			out = append(out, "unexpected proto marshal site "+f+": a new persisted proto must be a deliberate decision (add it to allowedProtoMarshalers, or route it through pkg/eventlog's Record.Decode)")
		}
	}
	for f := range allowedProtoMarshalers {
		if !found[f] {
			out = append(out, "stale allow-list entry "+f+": it no longer marshals a proto; remove it")
		}
	}
	sort.Strings(out)
	return out
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

// marshalsProto parses path and reports whether it marshals a proto via
// protojson.Marshal or proto.Marshal — as PREFIXES, so
// (protojson.MarshalOptions{...}).Marshal(x) is caught too. Parsing (not a text
// grep) means a mention in a comment or a string cannot trip the audit.
func marshalsProto(path string) bool {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		return false
	}
	hit := false
	ast.Inspect(f, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		id, ok := sel.X.(*ast.Ident)
		if !ok {
			return true
		}
		if (id.Name == "protojson" || id.Name == "proto") && strings.HasPrefix(sel.Sel.Name, "Marshal") {
			hit = true
			return false
		}
		return true
	})
	return hit
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
