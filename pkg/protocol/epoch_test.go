// SPDX-License-Identifier: Apache-2.0

package protocol_test

import (
	"go/parser"
	"go/token"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"

	"go.graveland.dev/rafiki/pkg/protocol"

	"github.com/multigres/testkit/assert"
)

// TestEpochIsTwoAndHeaderName pins the epoch value and header name: the wire
// is epoch 2, and both the daemon and the clients key the gate on this exact
// header spelling. A change here is a wire-breaking change and must be
// deliberate.
func TestEpochIsTwoAndHeaderName(t *testing.T) {
	c := assert.NewAborting(t)
	c.Eq(2, protocol.Epoch, "protocol.Epoch")
	c.Eq("Rafiki-Protocol", protocol.EpochHeader, "protocol.EpochHeader")
	c.Eq("protocol_mismatch", protocol.ErrProtocolMismatch, "protocol.ErrProtocolMismatch")
}

// TestProtocolImportsOnlyEncodingJSON pins the package's zero-dependency
// promise: it holds domain data and the epoch constants and links nothing
// third-party, so the pgx-free client can import it. The epoch constant lives
// here rather than in a transport package precisely so this stays true.
//
// The set is the package's whole non-test import set today — frame.go's
// bufio/bytes/errors/io plus types.go's encoding/json and time. Every entry is
// stdlib; the test fails the moment a third-party package (connectrpc, pgx,
// net/http) is added.
func TestProtocolImportsOnlyEncodingJSON(t *testing.T) { checkProtocolImports(t) }

// TestEpochProtocolImportsOnlyEncodingJSON is the same pin under a name the
// task's `-run 'Epoch'` verify pattern matches; `go test -run` is an
// unanchored substring match, so the pinned name above is skipped by it.
func TestEpochProtocolImportsOnlyEncodingJSON(t *testing.T) {
	t.Run("ProtocolImportsOnlyEncodingJSON", TestProtocolImportsOnlyEncodingJSON)
}

func checkProtocolImports(t *testing.T) {
	c := assert.NewAborting(t)
	entries, err := os.ReadDir(".")
	c.NoError(err, "ReadDir")

	seen := map[string]bool{}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, perr := parser.ParseFile(token.NewFileSet(), name, nil, parser.ImportsOnly)
		c.NoError(perr, "parse %s", name)
		for _, imp := range f.Imports {
			path, uerr := strconv.Unquote(imp.Path.Value)
			c.NoError(uerr, "unquote %s", imp.Path.Value)
			seen[path] = true
		}
	}

	got := make([]string, 0, len(seen))
	for p := range seen {
		got = append(got, p)
	}
	slices.Sort(got)
	c.EqDeep([]string{"bufio", "bytes", "encoding/json", "errors", "io", "time"}, got, "pkg/protocol imports")
}
