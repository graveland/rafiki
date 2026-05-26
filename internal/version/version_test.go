package version

import "testing"

func TestString_NonEmpty(t *testing.T) {
	s := String()
	if s == "" {
		t.Fatal("version is empty")
	}
	// When running under `go test`, vcs info is embedded for the test binary,
	// so we should get a real hash (not "unknown") in this repo.
	// But also accept "unknown" so the test isn't fragile in unusual contexts.
	t.Logf("version: %q", s)
}
