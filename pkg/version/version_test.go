package version

import (
	"testing"

	"github.com/multigres/testkit/assert"
)

func TestString_NonEmpty(t *testing.T) {
	s := String()
	assert.NewAborting(t).NotEq("", s, "version is empty")
	// When running under `go test`, vcs info is embedded for the test binary,
	// so we should get a real hash (not "unknown") in this repo.
	// But also accept "unknown" so the test isn't fragile in unusual contexts.
	t.Logf("version: %q", s)
}

func TestString_VersionVarTakesPriority(t *testing.T) {
	prev := Version
	t.Cleanup(func() { Version = prev })

	Version = "1.2.3-abc1234"
	assert.NewCollecting(t).Eq("1.2.3-abc1234", String(), "String()")
}

func TestString_VersionVarEmptyFallsBack(t *testing.T) {
	// When Version is empty, String() should fall back to VCS info, which
	// is non-empty in a git checkout.
	prev := Version
	t.Cleanup(func() { Version = prev })

	Version = ""
	s := String()
	assert.NewAborting(t).NotEq("", s, "String() is empty when Version is empty (no VCS fallback?)")
	t.Logf("fallback version: %q", s)
}
