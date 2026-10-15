package tools

import (
	"strings"
	"testing"

	"github.com/multigres/testkit/assert"
)

// TestNonInteractiveEnvAddsMissingDefaults: every default absent from the
// input is added.
func TestNonInteractiveEnvAddsMissingDefaults(t *testing.T) {
	c := assert.NewAborting(t)
	got := NonInteractiveEnv([]string{"PATH=/bin"})
	for _, want := range nonInteractiveDefaults {
		c.StrContains(strings.Join(got, "\n"), want, "missing default %q in", want)
	}
}

// TestNonInteractiveEnvKeepsCallerValues: a caller-supplied value always wins,
// and the key is not duplicated.
func TestNonInteractiveEnvKeepsCallerValues(t *testing.T) {
	c := assert.NewAborting(t)
	got := NonInteractiveEnv([]string{"PAGER=less"})
	c.Eq("less", envOf(got)["PAGER"], "caller PAGER=less should win; env was")
	c.Eq(1, countKey(got, "PAGER"), "PAGER appears more than once in")
}

// TestNonInteractiveEnvNilInheritsProcessEnv: a nil env expands from the
// process environment, so the defaults join it rather than replacing it.
func TestNonInteractiveEnvNilInheritsProcessEnv(t *testing.T) {
	c := assert.NewAborting(t)
	t.Setenv("RAFIKI_NI_PROBE", "1")
	got := NonInteractiveEnv(nil)
	c.StrContains(strings.Join(got, "\n"), "RAFIKI_NI_PROBE=1", "process environment was not inherited, got")
}

// TestNonInteractiveEnvDoesNotAliasInput: the result is a new slice, so
// mutating it leaves the caller's slice — including spare capacity beyond its
// length — untouched.
func TestNonInteractiveEnvDoesNotAliasInput(t *testing.T) {
	c := assert.NewAborting(t)
	base := make([]string, 1, 8)
	base[0] = "PAGER=less"

	got := NonInteractiveEnv(base)
	got[0] = "PAGER=clobbered"
	_ = append(got, "EXTRA=1")

	c.Eq("PAGER=less", base[0], "input slice was mutated through an aliased backing array, got")
	// The caller's slice still has zero length beyond index 0, so an entry
	// written into shared spare capacity would show up at base[:2].
	extended := base[:2]
	c.NotStrContains(strings.Join(extended, "\n"), "EXTRA=1", "result appended into the caller's backing array, got")
}
