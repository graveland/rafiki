// SPDX-License-Identifier: Apache-2.0

package gitpymodules

import (
	"errors"
	"strings"
	"testing"

	"go.graveland.dev/rafiki/pkg/pymodules"
)

// "local" is the sentinel the pymodule tool surface uses for the built-in
// blob store; a git source registered under it would make repo="local"
// ambiguous between the two stores. This is the validation every Store.Put
// implementation must apply (the concrete Postgres one enforces it in
// pkg/gitpymodulesdb and is pinned there by TestPostgresStorePutRejectsReservedLocalName).
func TestGitSourceRecordPutRejectsReservedLocalName(t *testing.T) {
	err := ValidateName("local")
	if !errors.Is(err, ErrReservedName) {
		t.Fatalf("ValidateName(\"local\") = %v, want ErrReservedName", err)
	}
}

// Beyond the reserved sentinel, a git source's name follows the pymodule
// name rule -- it becomes the `repo` argument's value everywhere else, and
// pymodules.ValidName is the one check that already fits (bare Python
// identifier, 1-64 chars).
func TestGitSourceRecordValidateNameMirrorsPymodulesRules(t *testing.T) {
	for _, name := range []string{"rotate_keys", "_ops_tools", "a"} {
		if err := ValidateName(name); err != nil {
			t.Errorf("ValidateName(%q) = %v, want nil", name, err)
		}
	}
	for _, name := range []string{"", "1abc", "has space", "a/b", "..", "over-64-chars-" + strings.Repeat("x", 50)} {
		err := ValidateName(name)
		if err == nil {
			t.Errorf("ValidateName(%q) = nil, want an error", name)
			continue
		}
		if errors.Is(err, ErrReservedName) {
			t.Errorf("ValidateName(%q) = ErrReservedName, want the pymodules.ValidName error", name)
		}
	}
	// The two rules must compose: the reserved check wins for "local", and
	// ValidName's own verdict is passed through untouched for the rest.
	if want := pymodules.ValidName("has space"); ValidateName("has space") == nil || want == nil {
		t.Fatalf("ValidateName must defer to pymodules.ValidName (got %v, pymodules says %v)", ValidateName("has space"), want)
	}
}
