package paths

import (
	"testing"

	"github.com/multigres/testkit/assert"
)

// TestPIDNamespaceTokenIsStable: the token must not change between calls within
// one process, or every restart would look like a new namespace and orphan
// signalling would never happen.
func TestPIDNamespaceTokenIsStable(t *testing.T) {
	c := assert.NewCollecting(t)
	first, ok1 := PIDNamespaceToken()
	second, ok2 := PIDNamespaceToken()
	c.Require().Eq(ok2, ok1, "ok changed between calls")
	c.Eq(second, first, "token changed between calls")
	c.False(ok1 && first == "", "ok=true with an empty token")
}
