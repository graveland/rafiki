// SPDX-License-Identifier: Apache-2.0

package repos

import (
	"errors"
	"strings"
	"testing"

	"github.com/multigres/testkit/assert"
)

// "local" is the sentinel the pymodule tool surface uses for the built-in
// blob store; a repo registered under it would make repo="local"
// ambiguous between the two stores. This is the validation every Store.Put
// implementation must apply (the concrete Postgres one enforces it in
// pkg/reposdb and is pinned there by TestPostgresStorePutRejectsReservedLocalName).
func TestRecordPutRejectsReservedLocalName(t *testing.T) {
	err := ValidateName("local")
	assert.NewAborting(t).ErrorIs(err, ErrReservedName, "ValidateName(\"local\") = %v, want ErrReservedName", err)
}

// Beyond the reserved sentinel, a repo's name is a single safe path
// segment, not a Python identifier: it names a checkout directory and is never
// imported, so "review-swarm" is accepted where pymodules.ValidName would
// refuse it.
func TestRecordValidateNameIsAPathSegment(t *testing.T) {
	c := assert.NewCollecting(t)
	for _, name := range []string{"rotate_keys", "_ops_tools", "a", "review-swarm", "1abc", strings.Repeat("x", 64)} {
		err := ValidateName(name)
		c.NoError(err, "ValidateName(%q) = %v, want nil", name, err)
	}
	for _, name := range []string{"", ".", "..", ".hidden", "v1.2", "evil.py", "-rf", "has space", "a/b", `a\b`, "a:b", "a\x00b", strings.Repeat("x", 65)} {
		err := ValidateName(name)
		if err == nil {
			t.Errorf("ValidateName(%q) = nil, want an error", name)
			continue
		}
		c.False(errors.Is(err, ErrReservedName), "ValidateName(%q) = ErrReservedName, want the path-segment error", name)
	}
}
