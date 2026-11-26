// SPDX-License-Identifier: Apache-2.0

package claudethread

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/multigres/testkit/assert"
)

// declaresFixture is one row of testdata/declares_client_tools.json: a `tools`
// value and the verdict DeclaresClientToolsFrom must give it.
//
// The table is SHARED, and deliberately not a Go literal, because two packages
// need it and only one of them can see this file's package: migration
// 0077's backfill re-states this rule in SQL, and
// TestBackfillPredicateMatchesDeclaresClientTools (pkg/store, which needs a
// database) applies the migration's own statement to every one of these bodies.
// A new case here therefore forces the SQL and the Go rule to agree on it.
type declaresFixture struct {
	Name string `json:"name"`
	Why  string `json:"why"`
	// Tools is the raw JSON of a request's `tools` value. A missing key stays
	// nil, which is how "no tools key at all" is expressed.
	Tools       json.RawMessage `json:"tools"`
	ClientTools bool            `json:"client_tools"`
}

// loadDeclaresFixtures reads the shared fixture table.
func loadDeclaresFixtures(t *testing.T) []declaresFixture {
	t.Helper()
	raw, err := os.ReadFile("testdata/declares_client_tools.json")
	assert.NewAborting(t).Require().NoError(err, "read the shared fixture table")
	var fixtures []declaresFixture
	assert.NewAborting(t).Require().NoError(json.Unmarshal(raw, &fixtures), "decode the shared fixture table")
	assert.NewAborting(t).Require().NotEmpty(fixtures, "the shared fixture table is empty")
	return fixtures
}

// TestDeclaresClientToolsMatchesTheSharedFixtures pins the Go half of the
// subagent rule against every body the SQL half is tested with. It needs no
// database, so it runs wherever this package does; the migration side is
// TestBackfillPredicateMatchesDeclaresClientTools.
func TestDeclaresClientToolsMatchesTheSharedFixtures(t *testing.T) {
	c := assert.NewAborting(t)
	for _, f := range loadDeclaresFixtures(t) {
		c.Eq(f.ClientTools, DeclaresClientToolsFrom(f.Tools),
			"%s: DeclaresClientToolsFrom(%s) — %s", f.Name, string(f.Tools), f.Why)
	}
}

// TestDeclaresClientToolsReadsTheWireShape pins the whole-body entry point
// against the same table: DeclaresClientTools unwraps the request's `tools` key
// and nothing else, so the ruling above cannot be bypassed by a caller that has
// a whole body in hand.
func TestDeclaresClientToolsReadsTheWireShape(t *testing.T) {
	c := assert.NewAborting(t)
	for _, f := range loadDeclaresFixtures(t) {
		body := []byte("{}")
		if f.Tools != nil {
			body = []byte(`{"model":"m","tools":` + string(f.Tools) + `}`)
		}
		c.Eq(f.ClientTools, DeclaresClientTools(body),
			"%s: DeclaresClientTools(%s) — %s", f.Name, string(body), f.Why)
	}
}
