package paths

import (
	"testing"

	"github.com/multigres/testkit/assert"
)

// The FUNDI_* and PIC_*/PI_CONTROLLER_* spellings are gone. Get reads exactly
// one name — a fallback chain three renames deep is the drift this
// consolidation exists to end.
func TestGet_ReadsOnlyTheCurrentName(t *testing.T) {
	t.Setenv("RAFIKI_SOCKET", "/run/new.sock")
	t.Setenv("FUNDI_SOCKET", "/run/old.sock")
	t.Setenv("PI_CONTROLLER_SOCKET", "/run/ancient.sock")

	assert.NewCollecting(t).Eq("/run/new.sock", Get(Socket), "Get(Socket)")
}

func TestGet_DoesNotFallBackToRetiredSpellings(t *testing.T) {
	t.Setenv("FUNDI_SOCKET", "/run/old.sock")
	t.Setenv("PI_CONTROLLER_SOCKET", "/run/ancient.sock")

	got := Get(Socket)
	assert.NewCollecting(t).Eq("", got, "Get(Socket) = %q with only retired spellings set, want \"\"", got)
}

// RAFIKI_URL/RAFIKI_TOKEN are client-side — what this process presents — and
// now serve both surfaces a client reaches: an LLM proxy and (https:// only)
// a remote daemon's control plane. The separate control-only URL, token and
// server-accept-token variables are retired; these two names carry all of it.
func TestOwnedVariableNames(t *testing.T) {
	cases := map[string]string{
		Socket:       "RAFIKI_SOCKET",
		ChildID:      "RAFIKI_CHILD_ID",
		DB:           "RAFIKI_DB",
		URL:          "RAFIKI_URL",
		Token:        "RAFIKI_TOKEN",
		DefaultModel: "RAFIKI_DEFAULT_MODEL",
		Instructions: "RAFIKI_INSTRUCTIONS",
	}
	for got, want := range cases {
		assert.NewCollecting(t).Eq(want, got, "variable constant")
	}
}
