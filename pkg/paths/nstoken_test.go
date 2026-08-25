package paths

import "testing"

// TestPIDNamespaceTokenIsStable: the token must not change between calls within
// one process, or every restart would look like a new namespace and orphan
// signalling would never happen.
func TestPIDNamespaceTokenIsStable(t *testing.T) {
	first, ok1 := PIDNamespaceToken()
	second, ok2 := PIDNamespaceToken()
	if ok1 != ok2 {
		t.Fatalf("ok changed between calls: %v then %v", ok1, ok2)
	}
	if first != second {
		t.Errorf("token changed between calls: %q then %q", first, second)
	}
	if ok1 && first == "" {
		t.Error("ok=true with an empty token")
	}
}
